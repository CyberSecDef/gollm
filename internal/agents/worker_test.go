package agents

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/cybersecdef/gollm/internal/bus"
	"github.com/cybersecdef/gollm/internal/llm"
	"github.com/cybersecdef/gollm/internal/tools"
)

// ---- parseToolCalls ---------------------------------------------------------

func TestParseToolCalls_NoDirectives(t *testing.T) {
	calls := parseToolCalls("just a normal response\nno tool calls here")
	if len(calls) != 0 {
		t.Errorf("expected 0 calls, got %d", len(calls))
	}
}

func TestParseToolCalls_SingleNoParams(t *testing.T) {
	calls := parseToolCalls("TOOL_CALL|fetch_url|")
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	if calls[0].name != "fetch_url" {
		t.Errorf("unexpected name: %q", calls[0].name)
	}
}

func TestParseToolCalls_WithParams(t *testing.T) {
	calls := parseToolCalls("TOOL_CALL|fetch_url|url=https://example.com")
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	if calls[0].params["url"] != "https://example.com" {
		t.Errorf("unexpected url param: %q", calls[0].params["url"])
	}
}

func TestParseToolCalls_MultipleParams(t *testing.T) {
	calls := parseToolCalls("TOOL_CALL|write_file|path=/tmp/x.txt&content=hello")
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	if calls[0].params["path"] != "/tmp/x.txt" {
		t.Errorf("path param: %q", calls[0].params["path"])
	}
	if calls[0].params["content"] != "hello" {
		t.Errorf("content param: %q", calls[0].params["content"])
	}
}

func TestParseToolCalls_MultipleCalls(t *testing.T) {
	resp := "First do this:\nTOOL_CALL|read_file|path=/a.txt\nThen:\nTOOL_CALL|read_file|path=/b.txt"
	calls := parseToolCalls(resp)
	if len(calls) != 2 {
		t.Fatalf("expected 2 calls, got %d", len(calls))
	}
}

// ---- WorkerSkills / filterAllowedTools -------------------------------------

func TestFilterAllowedTools_EmptyAllowedList(t *testing.T) {
	w := &Worker{skills: WorkerSkills{AllowedTools: nil}}
	calls := []toolCall{{name: "fetch_url"}}
	if got := w.filterAllowedTools(calls); got != nil {
		t.Errorf("expected nil, got %v", got)
	}
}

func TestFilterAllowedTools_Filters(t *testing.T) {
	w := &Worker{skills: WorkerSkills{AllowedTools: []string{"read_file"}}}
	calls := []toolCall{{name: "read_file"}, {name: "exec_command"}}
	got := w.filterAllowedTools(calls)
	if len(got) != 1 || got[0].name != "read_file" {
		t.Errorf("unexpected result: %v", got)
	}
}

// ---- retryableError / retryHint --------------------------------------------

func TestRetryHint_NonRetryableError(t *testing.T) {
	retryable, hint := retryHint(errors.New("plain error"))
	if retryable || hint != "" {
		t.Errorf("expected non-retryable, no hint; got retryable=%v hint=%q", retryable, hint)
	}
}

func TestRetryHint_RetryableError(t *testing.T) {
	err := &retryableError{err: errors.New("boom"), retryable: true, hint: "try again"}
	retryable, hint := retryHint(err)
	if !retryable {
		t.Error("expected retryable=true")
	}
	if hint != "try again" {
		t.Errorf("unexpected hint: %q", hint)
	}
}

func TestRetryHint_WrappedRetryableError(t *testing.T) {
	inner := &retryableError{err: errors.New("inner"), retryable: true, hint: "wrapped hint"}
	wrapped := fmt.Errorf("outer: %w", inner)
	retryable, hint := retryHint(wrapped)
	if !retryable {
		t.Error("expected retryable=true for wrapped error")
	}
	if hint != "wrapped hint" {
		t.Errorf("unexpected hint: %q", hint)
	}
}

// ---- WorkerFactory ---------------------------------------------------------

func TestWorkerFactory_BuiltinProfiles(t *testing.T) {
	b := bus.New()
	reg := tools.NewRegistry()
	f := NewWorkerFactory(b, &llm.MockClient{}, reg)

	roles := f.KnownRoles()
	expected := []string{"Researcher", "Analyst", "Coder", "Writer", "Reviewer", "FileOps"}
	roleSet := make(map[string]bool, len(roles))
	for _, r := range roles {
		roleSet[r] = true
	}
	for _, e := range expected {
		if !roleSet[e] {
			t.Errorf("expected built-in role %q not found in factory", e)
		}
	}
}

func TestWorkerFactory_Register(t *testing.T) {
	b := bus.New()
	reg := tools.NewRegistry()
	f := NewWorkerFactory(b, &llm.MockClient{}, reg)

	custom := WorkerSkills{
		Role:         "SecurityAuditor",
		Description:  "You audit code for vulnerabilities.",
		AllowedTools: []string{"read_file"},
		Timeout:      2 * time.Minute,
	}
	f.Register(custom)

	profiles := f.Profiles()
	got, ok := profiles["SecurityAuditor"]
	if !ok {
		t.Fatal("expected SecurityAuditor profile to be registered")
	}
	if got.Description != custom.Description {
		t.Errorf("description mismatch: got %q", got.Description)
	}
}

func TestWorkerFactory_NewWorker_KnownRole(t *testing.T) {
	b := bus.New()
	reg := tools.NewRegistry()
	f := NewWorkerFactory(b, &llm.MockClient{}, reg)

	w := f.NewWorker("w-1", "Researcher", "research topic X")
	if w.Role() != "Researcher" {
		t.Errorf("expected role Researcher, got %q", w.Role())
	}
	skills := w.Skills()
	if len(skills.AllowedTools) == 0 {
		t.Error("Researcher should have allowed tools")
	}
}

func TestWorkerFactory_NewWorker_UnknownRole(t *testing.T) {
	b := bus.New()
	reg := tools.NewRegistry()
	f := NewWorkerFactory(b, &llm.MockClient{}, reg)

	w := f.NewWorker("w-2", "Astronaut", "fly to the moon")
	if w.Role() != "Astronaut" {
		t.Errorf("expected role Astronaut, got %q", w.Role())
	}
	// Unknown role → no tools.
	if len(w.Skills().AllowedTools) != 0 {
		t.Errorf("expected no allowed tools for unknown role, got %v", w.Skills().AllowedTools)
	}
}

// ---- Worker with tool calling ----------------------------------------------

// echoTool is a test Tool that returns its input params as a string.
type echoTool struct{ callCount int }

func (e *echoTool) Name() string        { return "echo" }
func (e *echoTool) Description() string { return "echo params back" }
func (e *echoTool) Execute(_ context.Context, params map[string]string) (string, error) {
	e.callCount++
	return fmt.Sprintf("echo: %v", params), nil
}

// toolCallingMockClient returns a TOOL_CALL on the first call, then a plain response.
type toolCallingMockClient struct {
	calls int
}

func (c *toolCallingMockClient) Complete(_ context.Context, _ []llm.ChatMessage) (string, error) {
	c.calls++
	if c.calls == 1 {
		return "Thinking...\nTOOL_CALL|echo|key=value\nWait for result.", nil
	}
	return "Final answer after using the echo tool.", nil
}

func TestWorker_ToolCallingLoop(t *testing.T) {
	const testTimeout = 10 * time.Second
	b := bus.New()
	reg := tools.NewRegistry()
	echo := &echoTool{}
	reg.Register(echo)

	mockLLM := &toolCallingMockClient{}
	skills := WorkerSkills{
		Role:         "Tester",
		AllowedTools: []string{"echo"},
		Timeout:      testTimeout,
	}
	w := NewWorkerWithSkills("w-tool", "test echo tool", skills, b, mockLLM, reg)

	// Subscribe as "orchestrator" to receive the directed task-result message,
	// and as "spy" to also capture broadcast tool call/result messages.
	orchCh := b.Subscribe(orchestratorID)
	spyCh := b.Subscribe("spy")

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	if err := w.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer w.Stop()

	// Collect broadcast messages (tool call/result) until task result arrives.
	var msgs []Message
	deadline := time.After(testTimeout - 2*time.Second)
	// Fan broadcast messages into msgs slice.
	go func() {
		for {
			select {
			case msg, ok := <-spyCh:
				if !ok {
					return
				}
				msgs = append(msgs, msg)
			case <-ctx.Done():
				return
			}
		}
	}()

	// Wait for the directed task-result message.
	for {
		select {
		case msg, ok := <-orchCh:
			if !ok {
				t.Fatal("orchestrator channel closed")
			}
			if msg.Type == MsgTypeTaskResult {
				goto done
			}
		case <-deadline:
			t.Fatal("timed out waiting for task result")
		}
	}
done:

	// Verify tool was called.
	if echo.callCount == 0 {
		t.Error("expected echo tool to be called at least once")
	}

	// Give broadcast collector a moment to drain.
	time.Sleep(50 * time.Millisecond)

	// Verify tool call/result messages were emitted on the bus.
	var sawToolCall, sawToolResult bool
	for _, m := range msgs {
		if m.Type == MsgTypeToolCall {
			sawToolCall = true
		}
		if m.Type == MsgTypeToolResult {
			sawToolResult = true
		}
	}
	if !sawToolCall {
		t.Error("expected MsgTypeToolCall message on bus")
	}
	if !sawToolResult {
		t.Error("expected MsgTypeToolResult message on bus")
	}

	// Verify scratchpad contains tool output.
	if !strings.Contains(w.Scratchpad(), "echo") {
		t.Errorf("scratchpad should mention echo tool; got: %q", w.Scratchpad())
	}
}

// ---- Worker error propagation with retry hints -----------------------------

type errorLLMClient struct{ err error }

func (c *errorLLMClient) Complete(_ context.Context, _ []llm.ChatMessage) (string, error) {
	return "", c.err
}

func TestWorker_ErrorPropagation_RetryHint(t *testing.T) {
	b := bus.New()
	reg := tools.NewRegistry()
	subCh := b.Subscribe("spy")

	lErr := fmt.Errorf("simulated LLM failure")
	mockLLM := &errorLLMClient{err: lErr}

	skills := WorkerSkills{Role: "Tester", Timeout: 5 * time.Second}
	w := NewWorkerWithSkills("w-err", "failing task", skills, b, mockLLM, reg)

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	if err := w.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer w.Stop()

	// Wait for an error message.
	deadline := time.After(6 * time.Second)
	for {
		select {
		case msg := <-subCh:
			if msg.Type == MsgTypeError {
				// Verify retry metadata is present.
				if _, ok := msg.Metadata["retryable"]; !ok {
					t.Error("error message missing 'retryable' metadata key")
				}
				if _, ok := msg.Metadata["retry_hint"]; !ok {
					t.Error("error message missing 'retry_hint' metadata key")
				}
				return
			}
		case <-deadline:
			t.Fatal("timed out waiting for error message")
		}
	}
}

// ---- Worker skills timeout override ----------------------------------------

func TestWorkerSkills_TimeoutOverride(t *testing.T) {
	skills := WorkerSkills{
		Role:    "Quick",
		Timeout: 30 * time.Second,
	}
	b := bus.New()
	reg := tools.NewRegistry()
	w := NewWorkerWithSkills("w-timeout", "quick task", skills, b, &llm.MockClient{}, reg)

	if w.Skills().Timeout != 30*time.Second {
		t.Errorf("expected 30s timeout, got %v", w.Skills().Timeout)
	}
}

// ---- Orchestrator with factory ---------------------------------------------

func TestOrchestratorWithFactory_SpawnsFactoryWorkers(t *testing.T) {
	const testTimeout = 15 * time.Second
	b := bus.New()
	reg := tools.NewRegistry()
	client := &llm.MockClient{}

	fac := NewWorkerFactory(b, client, reg)

	orch := NewOrchestrator(b, client, reg)
	orch.SetWorkerFactory(fac)

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	if err := orch.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer orch.Stop()

	// Subscribe as "synthesizer" to receive the directed task-assign message.
	synthCh := b.Subscribe(synthesizerID)

	userMsg := NewMessage(MsgTypeUserInput, "user", orchestratorID, "test factory spawning")
	b.Publish(userMsg)

	deadline := time.After(testTimeout - 3*time.Second)
	for {
		select {
		case msg := <-synthCh:
			if msg.Type == MsgTypeTaskAssign {
				// Orchestration completed — factory workers ran.
				return
			}
		case <-deadline:
			t.Fatal("timed out waiting for synthesis after factory-based orchestration")
		}
	}
}
