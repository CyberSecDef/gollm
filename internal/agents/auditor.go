package agents

import (
"context"
"encoding/json"
"fmt"
"log/slog"
"os"
"path/filepath"
"strings"
"sync"
"time"

"github.com/cybersecdef/gollm/internal/bus"
)

const (
	auditorID            = "auditor"
	maxEventBuffer       = 1000
	subscriberChanBuffer = 256
	logsDir              = "logs"
	auditLogFilename     = "logs/audit.jsonl"
)

// AuditHook is a callback invoked for every message recorded by the Auditor.
type AuditHook func(msg Message)

// AuditorConfig holds optional configuration for the Auditor.
type AuditorConfig struct {
// LogPath overrides the default audit JSONL log path.
LogPath string
// MaxFileSizeBytes enables log rotation: when the log file exceeds this
// size it is renamed with a timestamp suffix and a new file is opened.
// Zero disables rotation.
MaxFileSizeBytes int64
}

// Auditor subscribes to all broadcast messages, writes them to a JSONL audit
// log, and maintains an in-memory ring buffer for the TUI event pane.
type Auditor struct {
id      string
bus     *bus.Bus
inbox   <-chan Message
status  AgentStatus
logPath string
maxSize int64

mu         sync.RWMutex
events     []Message
scratchpad strings.Builder
hook       AuditHook
logF       *os.File

subsMu sync.Mutex
subs   []chan Message

cancel context.CancelFunc
done   chan struct{}
}

// NewAuditor creates an Auditor with default configuration.
func NewAuditor(b *bus.Bus) *Auditor {
return NewAuditorWithConfig(b, AuditorConfig{})
}

// NewAuditorWithConfig creates an Auditor with the given configuration.
func NewAuditorWithConfig(b *bus.Bus, cfg AuditorConfig) *Auditor {
logPath := cfg.LogPath
if logPath == "" {
logPath = auditLogFilename
}
return &Auditor{
id:      auditorID,
bus:     b,
inbox:   b.Subscribe(auditorID),
done:    make(chan struct{}),
logPath: logPath,
maxSize: cfg.MaxFileSizeBytes,
}
}

// Subscribe returns a channel that receives a copy of every audited message for
// live display (e.g. in a TUI). The caller must drain this channel promptly to
// avoid blocking the Auditor. Call Unsubscribe when done.
func (a *Auditor) Subscribe() <-chan Message {
ch := make(chan Message, subscriberChanBuffer)
a.subsMu.Lock()
a.subs = append(a.subs, ch)
a.subsMu.Unlock()
return ch
}

// Unsubscribe removes a previously subscribed channel and closes it.
func (a *Auditor) Unsubscribe(ch <-chan Message) {
a.subsMu.Lock()
defer a.subsMu.Unlock()
for i, c := range a.subs {
if c == ch {
a.subs = append(a.subs[:i], a.subs[i+1:]...)
close(c)
return
}
}
}

func (a *Auditor) ID() string           { return a.id }
func (a *Auditor) Role() string         { return "Auditor" }
func (a *Auditor) Status() AgentStatus  { a.mu.RLock(); defer a.mu.RUnlock(); return a.status }
func (a *Auditor) Inbox() <-chan Message { return a.inbox }

func (a *Auditor) Send(msg Message) { a.bus.Publish(msg) }

// Scratchpad returns a summary of audit notes written during message recording.
func (a *Auditor) Scratchpad() string {
a.mu.RLock()
defer a.mu.RUnlock()
return a.scratchpad.String()
}

// Start opens the audit log file and begins consuming broadcast messages.
func (a *Auditor) Start(ctx context.Context) error {
if err := os.MkdirAll(filepath.Dir(a.logPath), 0o755); err != nil {
return fmt.Errorf("auditor: mkdir logs: %w", err)
}

f, err := os.OpenFile(a.logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
if err != nil {
return fmt.Errorf("auditor: open log: %w", err)
}
a.logF = f

ctx, cancel := context.WithCancel(ctx)
a.cancel = cancel
a.setStatus(StatusWorking)

go a.loop(ctx)
return nil
}

// Stop signals the auditor to shut down and closes all subscriber channels.
func (a *Auditor) Stop() {
if a.cancel != nil {
a.cancel()
}
<-a.done
if a.logF != nil {
_ = a.logF.Close()
}
// Close all subscriber channels so callers unblock.
a.subsMu.Lock()
defer a.subsMu.Unlock()
for _, ch := range a.subs {
close(ch)
}
a.subs = nil
}

// GetEvents returns a copy of the in-memory event buffer (most recent last).
func (a *Auditor) GetEvents() []Message {
a.mu.RLock()
defer a.mu.RUnlock()
out := make([]Message, len(a.events))
copy(out, a.events)
return out
}

// RegisterHook sets a callback invoked for every message recorded by the Auditor.
// Replaces any previously registered hook. Safe to call at any time.
func (a *Auditor) RegisterHook(h AuditHook) {
a.mu.Lock()
defer a.mu.Unlock()
a.hook = h
}

func (a *Auditor) loop(ctx context.Context) {
defer close(a.done)
for {
select {
case <-ctx.Done():
a.setStatus(StatusCompleted)
return
case msg, ok := <-a.inbox:
if !ok {
return
}
a.record(msg)
}
}
}

// rotateIfNeeded renames the current log file and opens a fresh one when the
// configured MaxFileSizeBytes limit is reached.
func (a *Auditor) rotateIfNeeded() {
if a.maxSize <= 0 || a.logF == nil {
return
}
info, err := a.logF.Stat()
if err != nil || info.Size() < a.maxSize {
return
}
_ = a.logF.Close()
rotated := a.logPath + "." + time.Now().UTC().Format("20060102150405")
_ = os.Rename(a.logPath, rotated)
f, err := os.OpenFile(a.logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
if err != nil {
slog.Error("auditor: failed to open new log after rotation", "err", err)
a.logF = nil
return
}
a.logF = f
}

func (a *Auditor) record(msg Message) {
a.rotateIfNeeded()

// Write structured JSONL event: timestamp, agent_id, type, payload, correlation_id.
if a.logF != nil {
line, err := json.Marshal(map[string]any{
"id":             msg.ID,
"type":           msg.Type,
"agent_id":       msg.From,
"to":             msg.To,
"payload":        msg.Content,
"metadata":       msg.Metadata,
"timestamp":      msg.Timestamp.Format(time.RFC3339Nano),
"correlation_id": msg.CorrelationID,
})
if err == nil {
_, _ = a.logF.Write(append(line, '\n'))
}
}

slog.Debug("audit", "type", msg.Type, "from", msg.From, "to", msg.To, "correlation_id", msg.CorrelationID)

// Append to in-memory ring buffer, evicting oldest when full.
a.mu.Lock()
if len(a.events) >= maxEventBuffer {
a.events = a.events[1:]
}
a.events = append(a.events, msg)
a.scratchpad.WriteString(fmt.Sprintf("[%s] %s → %s: %s\n",
msg.Timestamp.Format(time.RFC3339), msg.From, msg.To, msg.Type))
h := a.hook
a.mu.Unlock()

// Fan-out to live subscribers (non-blocking: slow consumers are skipped).
a.subsMu.Lock()
for _, ch := range a.subs {
select {
case ch <- msg:
default:
}
}
a.subsMu.Unlock()

// Invoke hook outside the lock to prevent potential deadlocks.
if h != nil {
h(msg)
}
}

func (a *Auditor) setStatus(s AgentStatus) {
a.mu.Lock()
defer a.mu.Unlock()
a.status = s
}
