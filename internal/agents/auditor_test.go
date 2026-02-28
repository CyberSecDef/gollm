package agents

import (
"bufio"
"context"
"encoding/json"
"os"
"strings"
"sync"
"testing"
"time"

"github.com/cybersecdef/gollm/internal/bus"
)

// newTestAuditor creates a temporary-file-backed Auditor for tests.
func newTestAuditor(t *testing.T, cfg AuditorConfig) (*Auditor, *bus.Bus, string) {
t.Helper()
f, err := os.CreateTemp(t.TempDir(), "audit-*.jsonl")
if err != nil {
t.Fatalf("create temp file: %v", err)
}
logPath := f.Name()
f.Close()

if cfg.LogPath == "" {
cfg.LogPath = logPath
}
b := bus.New()
aud := NewAuditorWithConfig(b, cfg)
return aud, b, logPath
}

// readJSONLFile parses all JSON objects from a JSONL file.
func readJSONLFile(t *testing.T, path string) []map[string]any {
t.Helper()
data, err := os.ReadFile(path)
if err != nil {
t.Fatalf("read log file: %v", err)
}
var records []map[string]any
scanner := bufio.NewScanner(strings.NewReader(string(data)))
for scanner.Scan() {
line := strings.TrimSpace(scanner.Text())
if line == "" {
continue
}
var rec map[string]any
if err := json.Unmarshal([]byte(line), &rec); err != nil {
t.Fatalf("unmarshal JSONL line %q: %v", line, err)
}
records = append(records, rec)
}
return records
}

// --- Subscribe / fan-out ---

func TestAuditorSubscribe_ReceivesMessages(t *testing.T) {
aud, b, _ := newTestAuditor(t, AuditorConfig{})

ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
defer cancel()

if err := aud.Start(ctx); err != nil {
t.Fatalf("Start: %v", err)
}
defer aud.Stop()

subCh := aud.Subscribe()
defer aud.Unsubscribe(subCh)

b.Publish(NewMessage(MsgTypeEvent, "test-agent", "", "hello from subscriber"))

select {
case msg := <-subCh:
if msg.Content != "hello from subscriber" {
t.Errorf("unexpected content: %q", msg.Content)
}
case <-time.After(2 * time.Second):
t.Fatal("timed out waiting for subscribed message")
}
}

func TestAuditorSubscribe_MultipleSubscribers(t *testing.T) {
aud, b, _ := newTestAuditor(t, AuditorConfig{})

ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
defer cancel()

if err := aud.Start(ctx); err != nil {
t.Fatalf("Start: %v", err)
}
defer aud.Stop()

ch1 := aud.Subscribe()
ch2 := aud.Subscribe()
defer aud.Unsubscribe(ch1)
defer aud.Unsubscribe(ch2)

b.Publish(NewMessage(MsgTypeEvent, "src", "", "broadcast"))

for _, ch := range []<-chan Message{ch1, ch2} {
select {
case msg := <-ch:
if msg.Content != "broadcast" {
t.Errorf("unexpected content: %q", msg.Content)
}
case <-time.After(2 * time.Second):
t.Fatal("timed out waiting for one of the subscribers")
}
}
}

func TestAuditorUnsubscribe_ChannelClosed(t *testing.T) {
aud, _, _ := newTestAuditor(t, AuditorConfig{})

ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
defer cancel()

if err := aud.Start(ctx); err != nil {
t.Fatalf("Start: %v", err)
}
defer aud.Stop()

ch := aud.Subscribe()
aud.Unsubscribe(ch)

// The channel should now be closed.
select {
case _, ok := <-ch:
if ok {
t.Error("expected channel to be closed after Unsubscribe")
}
default:
// Channel is empty and closed — drain it.
}
}

func TestAuditorStop_ClosesSubscriberChannels(t *testing.T) {
aud, _, _ := newTestAuditor(t, AuditorConfig{})

ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
defer cancel()

if err := aud.Start(ctx); err != nil {
t.Fatalf("Start: %v", err)
}

ch := aud.Subscribe()
aud.Stop()

// After Stop, the channel should be closed.
select {
case _, ok := <-ch:
if ok {
t.Error("expected subscriber channel to be closed after Stop")
}
case <-time.After(time.Second):
t.Fatal("timed out waiting for channel close after Stop")
}
cancel()
}

// --- Structured JSONL fields ---

func TestAuditorJSONL_StructuredFields(t *testing.T) {
aud, b, logPath := newTestAuditor(t, AuditorConfig{})

ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
defer cancel()

if err := aud.Start(ctx); err != nil {
t.Fatalf("Start: %v", err)
}

msg := NewMessage(MsgTypeEvent, "orchestrator", "", "test payload")
msg.CorrelationID = "corr-123"
b.Publish(msg)

time.Sleep(200 * time.Millisecond)
aud.Stop()

records := readJSONLFile(t, logPath)
if len(records) == 0 {
t.Fatal("expected at least one JSONL record")
}

rec := records[0]
fields := []string{"id", "type", "agent_id", "payload", "correlation_id", "timestamp"}
for _, f := range fields {
if _, ok := rec[f]; !ok {
t.Errorf("JSONL record missing field %q", f)
}
}
if rec["agent_id"] != "orchestrator" {
t.Errorf("agent_id: got %v, want orchestrator", rec["agent_id"])
}
if rec["payload"] != "test payload" {
t.Errorf("payload: got %v, want %q", rec["payload"], "test payload")
}
if rec["correlation_id"] != "corr-123" {
t.Errorf("correlation_id: got %v, want corr-123", rec["correlation_id"])
}
}

// --- CorrelationID propagation ---

func TestAuditorCorrelationID_PreservedInRingBuffer(t *testing.T) {
aud, b, _ := newTestAuditor(t, AuditorConfig{})

ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
defer cancel()

if err := aud.Start(ctx); err != nil {
t.Fatalf("Start: %v", err)
}
defer aud.Stop()

msg := NewMessage(MsgTypeEvent, "agent-x", "", "payload")
msg.CorrelationID = "trace-abc"
b.Publish(msg)

time.Sleep(200 * time.Millisecond)

events := aud.GetEvents()
found := false
for _, e := range events {
if e.CorrelationID == "trace-abc" {
found = true
break
}
}
if !found {
t.Error("CorrelationID not preserved in ring buffer events")
}
}

// --- Log rotation ---

func TestAuditorLogRotation(t *testing.T) {
dir := t.TempDir()
logPath := dir + "/audit.jsonl"

cfg := AuditorConfig{
LogPath:          logPath,
MaxFileSizeBytes: 100, // rotate after 100 bytes
}
b := bus.New()
aud := NewAuditorWithConfig(b, cfg)

ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
defer cancel()

if err := aud.Start(ctx); err != nil {
t.Fatalf("Start: %v", err)
}

// Publish enough messages to trigger rotation.
for i := 0; i < 10; i++ {
b.Publish(NewMessage(MsgTypeEvent, "src", "", "rotation-test-message-payload"))
}

time.Sleep(500 * time.Millisecond)
aud.Stop()

// At least one rotated file should exist alongside the active log.
entries, err := os.ReadDir(dir)
if err != nil {
t.Fatalf("readdir: %v", err)
}
var rotated int
for _, e := range entries {
if strings.HasPrefix(e.Name(), "audit.jsonl.") {
rotated++
}
}
if rotated == 0 {
t.Error("expected at least one rotated log file")
}
}

// --- Ring buffer eviction ---

func TestAuditorRingBuffer_Eviction(t *testing.T) {
aud, b, _ := newTestAuditor(t, AuditorConfig{})

ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
defer cancel()

if err := aud.Start(ctx); err != nil {
t.Fatalf("Start: %v", err)
}
defer aud.Stop()

// Flood with more than maxEventBuffer messages.
const excessMessages = 50
total := maxEventBuffer + excessMessages
var wg sync.WaitGroup
for i := 0; i < total; i++ {
wg.Add(1)
go func() {
defer wg.Done()
b.Publish(NewMessage(MsgTypeEvent, "flood", "", "x"))
}()
}
wg.Wait()
time.Sleep(300 * time.Millisecond)

events := aud.GetEvents()
if len(events) > maxEventBuffer {
t.Errorf("ring buffer exceeded maxEventBuffer: got %d", len(events))
}
}

// --- NewAuditorWithConfig default path ---

func TestNewAuditorWithConfig_DefaultPath(t *testing.T) {
b := bus.New()
aud := NewAuditorWithConfig(b, AuditorConfig{})
if aud.logPath != auditLogFilename {
t.Errorf("expected default log path %q, got %q", auditLogFilename, aud.logPath)
}
}
