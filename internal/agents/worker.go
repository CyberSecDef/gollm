package agents

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/cybersecdef/gollm/internal/bus"
	"github.com/cybersecdef/gollm/internal/llm"
	"github.com/cybersecdef/gollm/internal/tools"
)

// Worker is a domain-expert agent that handles a single delegated subtask.
// It maintains a private scratchpad and can invoke tools via the registry.
type Worker struct {
	id       string
	role     string
	task     string
	bus      *bus.Bus
	llm      llm.Client
	registry *tools.Registry
	inbox    <-chan Message

	mu         sync.RWMutex
	status     AgentStatus
	scratchpad strings.Builder

	cancel context.CancelFunc
	done   chan struct{}
}

// NewWorker constructs a Worker and subscribes it to the bus.
func NewWorker(id, role, task string, b *bus.Bus, client llm.Client, registry *tools.Registry) *Worker {
	return &Worker{
		id:       id,
		role:     role,
		task:     task,
		bus:      b,
		llm:      client,
		registry: registry,
		inbox:    b.Subscribe(id),
		done:     make(chan struct{}),
	}
}

func (w *Worker) ID() string          { return w.id }
func (w *Worker) Role() string        { return w.role }
func (w *Worker) Status() AgentStatus { w.mu.RLock(); defer w.mu.RUnlock(); return w.status }
func (w *Worker) Send(msg Message)    { w.bus.Publish(msg) }

// Start begins the worker's processing loop.
func (w *Worker) Start(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	w.cancel = cancel
	w.setStatus(StatusWorking)

	go w.run(ctx)
	return nil
}

// Stop cancels the worker and waits for it to finish.
func (w *Worker) Stop() {
	if w.cancel != nil {
		w.cancel()
	}
	<-w.done
}

func (w *Worker) run(ctx context.Context) {
	defer close(w.done)
	defer w.bus.Unsubscribe(w.id)

	w.emit(ctx, MsgTypeStatusUpdate, fmt.Sprintf("[%s] Starting task: %s", w.role, w.task))

	result, err := w.processTask(ctx)
	if err != nil {
		w.setStatus(StatusError)
		w.emit(ctx, MsgTypeError, fmt.Sprintf("[%s] Error: %v", w.role, err))
		// Still send a result so the orchestrator doesn't hang.
		result = fmt.Sprintf("Error encountered: %v", err)
	}

	// Broadcast result to orchestrator (empty To = broadcast; orchestrator listens).
	msg := NewMessage(MsgTypeTaskResult, w.id, orchestratorID, result)
	msg.Metadata["worker_id"] = w.id
	msg.Metadata["role"] = w.role
	msg.Metadata["task"] = w.task
	w.bus.Publish(msg)

	w.setStatus(StatusCompleted)
	w.emit(ctx, MsgTypeStatusUpdate, fmt.Sprintf("[%s] Task completed", w.role))
}

func (w *Worker) processTask(ctx context.Context) (string, error) {
	systemPrompt := fmt.Sprintf(
		"You are a %s specialist agent. Your task is: %s\n\n"+
			"Work through the task step by step. Use your expertise to provide a detailed, "+
			"actionable response. Document your reasoning in a structured way.",
		w.role, w.task,
	)

	w.scratchPad("Starting analysis of task: " + w.task)
	w.emit(ctx, MsgTypeStatusUpdate, fmt.Sprintf("[%s] Calling LLM...", w.role))

	messages := []llm.ChatMessage{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: w.task},
	}

	response, err := w.llm.Complete(ctx, messages)
	if err != nil {
		return "", fmt.Errorf("worker LLM call: %w", err)
	}

	w.scratchPad("LLM response received, length: " + fmt.Sprintf("%d chars", len(response)))
	w.emit(ctx, MsgTypeStatusUpdate, fmt.Sprintf("[%s] Response received, processing...", w.role))

	return response, nil
}

func (w *Worker) emit(ctx context.Context, msgType MessageType, content string) {
	select {
	case <-ctx.Done():
		return
	default:
	}
	msg := NewMessage(msgType, w.id, "", content)
	msg.Metadata["role"] = w.role
	w.bus.Publish(msg)
}

func (w *Worker) scratchPad(note string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.scratchpad.WriteString(note)
	w.scratchpad.WriteString("\n")
}

func (w *Worker) setStatus(s AgentStatus) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.status = s
}
