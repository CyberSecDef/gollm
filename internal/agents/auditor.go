package agents

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/cybersecdef/gollm/internal/bus"
)

const (
	auditorID        = "auditor"
	maxEventBuffer   = 1000
	logsDir          = "logs"
	auditLogFilename = "logs/audit.jsonl"
)

// Auditor subscribes to all broadcast messages, writes them to a JSONL audit
// log, and maintains an in-memory ring buffer for the TUI event pane.
type Auditor struct {
	id         string
	bus        *bus.Bus
	inbox      <-chan Message
	status     AgentStatus
	mu         sync.RWMutex
	events     []Message
	scratchpad strings.Builder
	logF       *os.File
	cancel     context.CancelFunc
	done       chan struct{}
}

// NewAuditor creates an Auditor but does not start it.
func NewAuditor(b *bus.Bus) *Auditor {
	return &Auditor{
		id:    auditorID,
		bus:   b,
		inbox: b.Subscribe(auditorID),
		done:  make(chan struct{}),
	}
}

func (a *Auditor) ID() string            { return a.id }
func (a *Auditor) Role() string          { return "Auditor" }
func (a *Auditor) Status() AgentStatus   { a.mu.RLock(); defer a.mu.RUnlock(); return a.status }
func (a *Auditor) Inbox() <-chan Message  { return a.inbox }

func (a *Auditor) Send(msg Message) { a.bus.Publish(msg) }

// Scratchpad returns a summary of audit notes written during message recording.
func (a *Auditor) Scratchpad() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.scratchpad.String()
}

// Start opens the audit log file and begins consuming broadcast messages.
func (a *Auditor) Start(ctx context.Context) error {
	if err := os.MkdirAll(logsDir, 0o755); err != nil {
		return fmt.Errorf("auditor: mkdir logs: %w", err)
	}

	f, err := os.OpenFile(auditLogFilename, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
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

// Stop signals the auditor to shut down.
func (a *Auditor) Stop() {
	if a.cancel != nil {
		a.cancel()
	}
	<-a.done
	if a.logF != nil {
		_ = a.logF.Close()
	}
}

// GetEvents returns a copy of the in-memory event buffer (most recent last).
func (a *Auditor) GetEvents() []Message {
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := make([]Message, len(a.events))
	copy(out, a.events)
	return out
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

func (a *Auditor) record(msg Message) {
	// Write to JSONL file.
	if a.logF != nil {
		line, err := json.Marshal(map[string]any{
			"id":        msg.ID,
			"type":      msg.Type,
			"from":      msg.From,
			"to":        msg.To,
			"content":   msg.Content,
			"metadata":  msg.Metadata,
			"timestamp": msg.Timestamp.Format(time.RFC3339Nano),
		})
		if err == nil {
			_, _ = a.logF.Write(append(line, '\n'))
		}
	}

	slog.Debug("audit", "type", msg.Type, "from", msg.From, "to", msg.To)

	// Append to in-memory buffer, evicting oldest if full.
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.events) >= maxEventBuffer {
		a.events = a.events[1:]
	}
	a.events = append(a.events, msg)
	a.scratchpad.WriteString(fmt.Sprintf("[%s] %s → %s: %s\n",
		msg.Timestamp.Format(time.RFC3339), msg.From, msg.To, msg.Type))
}

func (a *Auditor) setStatus(s AgentStatus) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.status = s
}
