package agents

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/cybersecdef/gollm/internal/bus"
	"github.com/cybersecdef/gollm/internal/llm"
	"github.com/cybersecdef/gollm/internal/tools"
)

// TestFullMultiAgentWorkflow_Integration exercises the complete pipeline:
//
//	Bus → Orchestrator → Worker(s) → Synthesizer → FinalResponse
//
// It uses the MockLLM client so no API key is required, and a temporary
// directory for the Auditor log so the test is hermetic.
func TestFullMultiAgentWorkflow_Integration(t *testing.T) {
	const testTimeout = 30 * time.Second

	// ---- Infrastructure -------------------------------------------------------
	b := bus.New()
	reg := tools.NewRegistry()
	client := &llm.MockClient{}

	// ---- Agents ---------------------------------------------------------------
	auditCfg := AuditorConfig{LogPath: t.TempDir() + "/audit.jsonl"}
	auditor := NewAuditorWithConfig(b, auditCfg)
	orchestrator := NewOrchestrator(b, client, reg)
	synthesizer := NewSynthesizer(b, client)

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	for _, ag := range []interface {
		Start(ctx context.Context) error
		ID() string
	}{auditor, orchestrator, synthesizer} {
		if err := ag.Start(ctx); err != nil {
			t.Fatalf("failed to start %s: %v", ag.ID(), err)
		}
	}
	defer func() {
		synthesizer.Stop()
		orchestrator.Stop()
		auditor.Stop()
	}()

	// ---- Subscribe for results ------------------------------------------------
	tuiCh := b.Subscribe("integration-tui")

	// ---- Trigger the workflow -------------------------------------------------
	userMsg := NewMessage(MsgTypeUserInput, "integration-test", orchestratorID,
		"Summarise the benefits of multi-agent systems")
	b.Publish(userMsg)

	// ---- Wait for final response -----------------------------------------------
	deadline := time.After(testTimeout - 2*time.Second)
	var finalMsg Message
	for {
		select {
		case msg := <-tuiCh:
			if msg.Type == MsgTypeFinalResponse {
				finalMsg = msg
				goto done
			}
		case <-deadline:
			t.Fatal("integration test timed out waiting for MsgTypeFinalResponse")
		}
	}
done:

	// ---- Assertions -----------------------------------------------------------

	if finalMsg.Content == "" {
		t.Error("final response content is empty")
	}
	if !strings.Contains(finalMsg.Content, "##") {
		t.Error("final response should contain markdown sections")
	}
	if !strings.Contains(finalMsg.Content, "## Traceability") {
		t.Error("final response missing ## Traceability section")
	}
	if !strings.Contains(finalMsg.Content, "## Citations") {
		t.Error("final response missing ## Citations section")
	}

	// Auditor should have recorded at least the user input and some events.
	events := auditor.GetEvents()
	if len(events) == 0 {
		t.Error("auditor recorded no events during the workflow")
	}

	// Orchestrator should be back to idle after completing the task.
	deadline2 := time.After(3 * time.Second)
	for {
		if orchestrator.Status() == StatusIdle {
			break
		}
		select {
		case <-deadline2:
			t.Errorf("orchestrator status after completion: %s (expected idle)", orchestrator.Status())
			goto checkStatus
		case <-time.After(50 * time.Millisecond):
		}
	}
checkStatus:
}

// TestWorkerPool_MaxWorkers verifies that setting MaxWorkers limits the number
// of concurrently running workers.
func TestWorkerPool_MaxWorkers(t *testing.T) {
	const maxWorkers = 2
	const testTimeout = 20 * time.Second

	b := bus.New()
	reg := tools.NewRegistry()

	// Slow LLM: blocks until unblocked so we can observe concurrency.
	gate := make(chan struct{})
	var concurrent int32
	var peakConcurrent int32
	var concSem = make(chan struct{}, 1)
	concSem <- struct{}{}

	slowLLM := &funcClient{fn: func(ctx context.Context, msgs []llm.ChatMessage) (string, error) {
		// Tally concurrent calls.
		<-concSem
		concurrent++
		if concurrent > peakConcurrent {
			peakConcurrent = concurrent
		}
		concSem <- struct{}{}

		select {
		case <-gate:
		case <-ctx.Done():
			<-concSem
			concurrent--
			concSem <- struct{}{}
			return "", ctx.Err()
		}

		<-concSem
		concurrent--
		concSem <- struct{}{}

		// Determine the LLM role from messages.
		for _, m := range msgs {
			if m.Role == "system" {
				if strings.Contains(m.Content, "acceptance criteria evaluator") {
					return "ACCEPTED", nil
				}
			}
		}
		return "SUBTASK_1|Analyst|task A\nSUBTASK_2|Analyst|task B\nSUBTASK_3|Analyst|task C", nil
	}}

	orchCfg := OrchestratorConfig{MaxWorkers: maxWorkers}
	orch := NewOrchestratorWithConfig(b, slowLLM, reg, orchCfg)

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	if err := orch.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer orch.Stop()

	synthCh := b.Subscribe(synthesizerID)

	// Unblock the LLM after a brief pause so workers can accumulate.
	go func() {
		time.Sleep(500 * time.Millisecond)
		close(gate)
	}()

	userMsg := NewMessage(MsgTypeUserInput, "test", orchestratorID, "pool test goal")
	b.Publish(userMsg)

	deadline := time.After(testTimeout - 3*time.Second)
	for {
		select {
		case msg := <-synthCh:
			if msg.Type == MsgTypeTaskAssign {
				// Workflow completed. The peak concurrency recorded by the slow LLM
				// should not exceed maxWorkers (the orchestrator decompose call itself
				// doesn't count against the worker pool – only worker LLM calls do).
				if peakConcurrent > int32(maxWorkers) {
					t.Errorf("peak concurrent workers %d exceeded MaxWorkers %d",
						peakConcurrent, maxWorkers)
				}
				return
			}
		case <-deadline:
			t.Fatal("timed out waiting for synthesis after pool test")
		}
	}
}

// TestWorker_PanicRecovery verifies that a panicking worker sends a
// MsgTypeTaskResult so the orchestrator is not left waiting indefinitely.
func TestWorker_PanicRecovery(t *testing.T) {
	const testTimeout = 10 * time.Second

	b := bus.New()
	reg := tools.NewRegistry()

	panicLLM := &funcClient{fn: func(_ context.Context, _ []llm.ChatMessage) (string, error) {
		panic("simulated worker panic")
	}}

	skills := WorkerSkills{Role: "Tester", Timeout: 5 * time.Second}
	w := NewWorkerWithSkills("w-panic", "panic task", skills, b, panicLLM, reg)

	orchCh := b.Subscribe(orchestratorID)
	spyCh := b.Subscribe("panic-spy")

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	if err := w.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer w.Stop()

	var sawError, sawResult bool
	deadline := time.After(testTimeout - 2*time.Second)
	for !sawError || !sawResult {
		select {
		case msg := <-spyCh:
			if msg.Type == MsgTypeError && msg.Metadata["panic"] == "true" {
				sawError = true
			}
		case msg := <-orchCh:
			if msg.Type == MsgTypeTaskResult {
				sawResult = true
			}
		case <-deadline:
			t.Fatalf("timed out: sawError=%v sawResult=%v", sawError, sawResult)
		}
	}

	if w.Status() != StatusError {
		t.Errorf("expected worker status Error after panic, got %s", w.Status())
	}
}
