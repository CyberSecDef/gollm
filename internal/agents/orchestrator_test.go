package agents

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cybersecdef/gollm/internal/bus"
	"github.com/cybersecdef/gollm/internal/llm"
	"github.com/cybersecdef/gollm/internal/tools"
)

// ---- helpers ----------------------------------------------------------------

// newTestOrchestrator builds an Orchestrator backed by a mock LLM.
func newTestOrchestrator(client llm.Client) (*Orchestrator, *bus.Bus) {
	b := bus.New()
	reg := tools.NewRegistry()
	return NewOrchestrator(b, client, reg), b
}

// drain reads all messages from the TUI subscription until the channel is quiet.
func drain(ch <-chan Message, timeout time.Duration) []Message {
	var msgs []Message
	deadline := time.After(timeout)
	for {
		select {
		case m, ok := <-ch:
			if !ok {
				return msgs
			}
			msgs = append(msgs, m)
		case <-deadline:
			return msgs
		}
	}
}

// ---- parseSubtasks ----------------------------------------------------------

func TestParseSubtasks_Normal(t *testing.T) {
	input := `Here are the subtasks:
SUBTASK_1|Researcher|Research topic A
SUBTASK_2|Analyst|Analyse the findings
SUBTASK_3|Coder|Implement solution`

	tasks := parseSubtasks(input)
	if len(tasks) != 3 {
		t.Fatalf("expected 3 tasks, got %d", len(tasks))
	}
	if tasks[0].role != "Researcher" || tasks[0].task != "Research topic A" {
		t.Errorf("task[0] mismatch: %+v", tasks[0])
	}
	if tasks[1].role != "Analyst" {
		t.Errorf("task[1] role mismatch: %+v", tasks[1])
	}
}

func TestParseSubtasks_Fallback(t *testing.T) {
	// When no SUBTASK_ lines are found a single Analyst task is created.
	tasks := parseSubtasks("no special lines here")
	if len(tasks) != 1 {
		t.Fatalf("expected 1 fallback task, got %d", len(tasks))
	}
	if tasks[0].role != "Analyst" {
		t.Errorf("expected Analyst fallback, got %q", tasks[0].role)
	}
}

func TestParseSubtasks_Clarification(t *testing.T) {
	input := "CLARIFICATION|Clarification|What is the target audience?"
	tasks := parseSubtasks(input)
	if len(tasks) != 1 {
		t.Fatalf("expected 1 task, got %d", len(tasks))
	}
	if tasks[0].role != clarificationRole {
		t.Errorf("expected role %q, got %q", clarificationRole, tasks[0].role)
	}
	if tasks[0].task != "What is the target audience?" {
		t.Errorf("unexpected task: %q", tasks[0].task)
	}
}

// ---- retryWithBackoff -------------------------------------------------------

func TestRetryWithBackoff_SucceedsOnFirstAttempt(t *testing.T) {
	calls := 0
	err := retryWithBackoff(context.Background(), 3, time.Millisecond, func() error {
		calls++
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls != 1 {
		t.Errorf("expected 1 call, got %d", calls)
	}
}

func TestRetryWithBackoff_RetriesOnError(t *testing.T) {
	calls := 0
	sentinel := errors.New("transient")
	err := retryWithBackoff(context.Background(), 3, time.Millisecond, func() error {
		calls++
		if calls < 3 {
			return sentinel
		}
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls != 3 {
		t.Errorf("expected 3 calls, got %d", calls)
	}
}

func TestRetryWithBackoff_ExhaustsAttempts(t *testing.T) {
	calls := 0
	sentinel := errors.New("permanent")
	err := retryWithBackoff(context.Background(), 2, time.Millisecond, func() error {
		calls++
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected sentinel error, got %v", err)
	}
	if calls != 2 {
		t.Errorf("expected 2 calls, got %d", calls)
	}
}

func TestRetryWithBackoff_ContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately
	err := retryWithBackoff(ctx, 3, time.Millisecond, func() error {
		return errors.New("should not reach")
	})
	if err == nil {
		t.Fatal("expected error for cancelled context")
	}
}

// ---- OrchestratorHook -------------------------------------------------------

func TestRegisterSynthesizerHook_Called(t *testing.T) {
	orch, b := newTestOrchestrator(&llm.MockClient{})
	tuiCh := b.Subscribe("tui")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := orch.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer orch.Stop()

	var hookCalled bool
	var hookMu sync.Mutex
	orch.RegisterSynthesizerHook(func(event string, meta map[string]string) {
		hookMu.Lock()
		defer hookMu.Unlock()
		if event == "pre_synthesize" {
			hookCalled = true
		}
	})

	userMsg := NewMessage(MsgTypeUserInput, "user", orchestratorID, "test goal")
	b.Publish(userMsg)

	// Wait until the synthesizer receives MsgTypeTaskAssign (which happens after
	// the hook fires).
	synthCh := b.Subscribe("synthesizer")
	deadline := time.After(8 * time.Second)
	for {
		select {
		case msg := <-synthCh:
			if msg.Type == MsgTypeTaskAssign {
				hookMu.Lock()
				called := hookCalled
				hookMu.Unlock()
				if !called {
					t.Error("synthesizer hook was not called before MsgTypeTaskAssign")
				}
				_ = tuiCh
				return
			}
		case <-deadline:
			t.Fatal("timed out waiting for synthesizer task assign")
		}
	}
}

// ---- AuditHook --------------------------------------------------------------

func TestAuditorRegisterHook(t *testing.T) {
	b := bus.New()
	aud := NewAuditor(b)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := aud.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer aud.Stop()

	var received []Message
	var mu sync.Mutex
	aud.RegisterHook(func(msg Message) {
		mu.Lock()
		defer mu.Unlock()
		received = append(received, msg)
	})

	// Publish a few broadcast messages.
	for i := 0; i < 3; i++ {
		b.Publish(NewMessage(MsgTypeEvent, "test", "", fmt.Sprintf("msg %d", i)))
	}

	// Give the auditor loop time to process.
	time.Sleep(200 * time.Millisecond)

	mu.Lock()
	n := len(received)
	mu.Unlock()

	if n < 3 {
		t.Errorf("expected at least 3 hook calls, got %d", n)
	}
}

// ---- Clarification flow -----------------------------------------------------

func TestClarificationFlow(t *testing.T) {
	// Mock LLM: first call returns a CLARIFICATION; subsequent calls return
	// normal subtasks so the loop can proceed.
	callCount := 0
	var callMu sync.Mutex
	mockLLM := &funcClient{fn: func(_ context.Context, messages []llm.ChatMessage) (string, error) {
		callMu.Lock()
		count := callCount
		callCount++
		callMu.Unlock()

		// Inspect system prompt to determine request type.
		system := ""
		for _, m := range messages {
			if m.Role == "system" {
				system = m.Content
			}
		}
		if strings.Contains(system, "acceptance criteria evaluator") {
			return "ACCEPTED", nil
		}
		if count == 0 {
			// First decompose → ask for clarification.
			return "CLARIFICATION|Clarification|What is the scope?", nil
		}
		// Second decompose → normal subtasks.
		return "SUBTASK_1|Analyst|Analyse the scope\nSUBTASK_2|Researcher|Research background", nil
	}}

	orch, b := newTestOrchestrator(mockLLM)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := orch.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer orch.Stop()

	// Subscribe to detect the Question being broadcast.
	tuiCh := b.Subscribe("tui")

	userMsg := NewMessage(MsgTypeUserInput, "user", orchestratorID, "write a report")
	b.Publish(userMsg)

	// Wait for the MsgTypeQuestion.
	var question Message
	deadline := time.After(5 * time.Second)
waitQuestion:
	for {
		select {
		case msg := <-tuiCh:
			if msg.Type == MsgTypeQuestion {
				question = msg
				break waitQuestion
			}
		case <-deadline:
			t.Fatal("timed out waiting for clarification question")
		}
	}

	if question.Content == "" {
		t.Error("received empty clarification question")
	}

	// Answer the question.
	answerMsg := NewMessage(MsgTypeAnswer, "user", orchestratorID, "global scope")
	b.Publish(answerMsg)

	// Now wait for the synthesizer to receive results.
	synthCh := b.Subscribe("synthesizer")
	deadline2 := time.After(8 * time.Second)
	for {
		select {
		case msg := <-synthCh:
			if msg.Type == MsgTypeTaskAssign {
				// Loop completed after clarification — success.
				return
			}
		case <-deadline2:
			t.Fatal("timed out waiting for synthesis after clarification")
		}
	}
}

// ---- Acceptance criteria loop -----------------------------------------------

func TestAcceptanceCriteriaLoop(t *testing.T) {
	// Mock LLM: acceptance check fails on the first iteration, passes on the second.
	iterCount := 0
	acceptCalls := 0
	var mu sync.Mutex

	mockLLM := &funcClient{fn: func(_ context.Context, messages []llm.ChatMessage) (string, error) {
		mu.Lock()
		defer mu.Unlock()

		system := ""
		for _, m := range messages {
			if m.Role == "system" {
				system = m.Content
			}
		}
		if strings.Contains(system, "acceptance criteria evaluator") {
			acceptCalls++
			if acceptCalls == 1 {
				return "NEEDS_MORE_WORK", nil
			}
			return "ACCEPTED", nil
		}
		// Decompose call — always return 1 simple subtask.
		iterCount++
		return fmt.Sprintf("SUBTASK_1|Analyst|Analyse iteration %d", iterCount), nil
	}}

	orch, b := newTestOrchestrator(mockLLM)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if err := orch.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer orch.Stop()

	userMsg := NewMessage(MsgTypeUserInput, "user", orchestratorID, "iterative goal")
	b.Publish(userMsg)

	synthCh := b.Subscribe("synthesizer")
	deadline := time.After(12 * time.Second)
	for {
		select {
		case msg := <-synthCh:
			if msg.Type == MsgTypeTaskAssign {
				mu.Lock()
				iters := iterCount
				accepts := acceptCalls
				mu.Unlock()
				if iters < 2 {
					t.Errorf("expected at least 2 decompose calls (iterations), got %d", iters)
				}
				if accepts < 2 {
					t.Errorf("expected at least 2 acceptance checks, got %d", accepts)
				}
				return
			}
		case <-deadline:
			t.Fatal("timed out waiting for synthesis after multiple iterations")
		}
	}
}

// ---- funcClient helper ------------------------------------------------------

// funcClient is a test llm.Client backed by a plain function.
type funcClient struct {
	fn func(ctx context.Context, messages []llm.ChatMessage) (string, error)
}

func (f *funcClient) Complete(ctx context.Context, messages []llm.ChatMessage) (string, error) {
	return f.fn(ctx, messages)
}

// ---- GetCurrentIteration / MsgTypeStopRequest -------------------------------

func TestGetCurrentIteration_IdleIsZero(t *testing.T) {
	orch, _ := newTestOrchestrator(&llm.MockClient{})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := orch.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer orch.Stop()

	if got := orch.GetCurrentIteration(); got != 0 {
		t.Errorf("expected GetCurrentIteration() == 0 when idle, got %d", got)
	}
}

func TestGetCurrentIteration_ReturnsNonZeroWhileRunning(t *testing.T) {
	// LLM that blocks until the context is done so we can observe the iteration.
	ready := make(chan struct{})
	block := make(chan struct{})

	mockLLM := &funcClient{fn: func(ctx context.Context, messages []llm.ChatMessage) (string, error) {
		// Signal that the LLM call is in progress.
		select {
		case ready <- struct{}{}:
		default:
		}
		// Block until the test unblocks or ctx is cancelled.
		select {
		case <-block:
		case <-ctx.Done():
		}
		return "SUBTASK_1|Analyst|task", nil
	}}

	orch, b := newTestOrchestrator(mockLLM)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := orch.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer orch.Stop()

	userMsg := NewMessage(MsgTypeUserInput, "user", orchestratorID, "test goal")
	b.Publish(userMsg)

	// Wait for the first LLM call to start.
	select {
	case <-ready:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for first LLM call")
	}

	if got := orch.GetCurrentIteration(); got == 0 {
		t.Error("expected GetCurrentIteration() > 0 while orchestrator is running")
	}

	// Unblock the LLM and let the orchestrator finish.
	close(block)
}

func TestStopRequest_CancelsRunningTask(t *testing.T) {
	// LLM that blocks until signalled – used to hold the orchestrator in iteration 1.
	ready := make(chan struct{})
	block := make(chan struct{})

	mockLLM := &funcClient{fn: func(ctx context.Context, messages []llm.ChatMessage) (string, error) {
		select {
		case ready <- struct{}{}:
		default:
		}
		select {
		case <-block:
		case <-ctx.Done():
			return "", ctx.Err()
		}
		return "SUBTASK_1|Analyst|task", nil
	}}

	orch, b := newTestOrchestrator(mockLLM)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := orch.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer orch.Stop()

	// Start a task.
	userMsg := NewMessage(MsgTypeUserInput, "user", orchestratorID, "blocking goal")
	b.Publish(userMsg)

	// Wait until the task is in progress.
	select {
	case <-ready:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for task to start")
	}

	// Verify orchestrator is working.
	if orch.Status() != StatusWorking {
		t.Errorf("expected StatusWorking, got %s", orch.Status())
	}

	// Send a stop request.
	stopMsg := NewMessage(MsgTypeStopRequest, "user", orchestratorID, "stop")
	b.Publish(stopMsg)

	// Wait for the orchestrator to return to idle (max 5 s).
	deadline := time.After(5 * time.Second)
	for {
		if orch.Status() == StatusIdle {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("orchestrator did not return to idle after stop request; status=%s", orch.Status())
		case <-time.After(50 * time.Millisecond):
		}
	}

	// currentIter should be reset to 0 after stop.
	if got := orch.GetCurrentIteration(); got != 0 {
		t.Errorf("expected GetCurrentIteration() == 0 after stop, got %d", got)
	}

	// Unblock any pending LLM calls so goroutines can clean up.
	close(block)
}
