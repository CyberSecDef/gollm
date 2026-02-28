package agents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/cybersecdef/gollm/internal/bus"
	"github.com/cybersecdef/gollm/internal/llm"
	"github.com/cybersecdef/gollm/internal/tools"
)

// workerTaskTimeout is the default maximum time allowed for a single LLM task.
const workerTaskTimeout = 5 * time.Minute

// maxToolIterations is the maximum number of tool-calling rounds per task.
const maxToolIterations = 3

// WorkerSkills describes the capability profile of a worker agent (its SME role).
type WorkerSkills struct {
	// Role is the SME role label (e.g., "Researcher", "Coder").
	Role string
	// Description is an extended prose description embedded in the system prompt.
	Description string
	// AllowedTools lists the names of tools this worker may invoke.
	// An empty slice means no tool access.
	AllowedTools []string
	// RateLimit is the minimum duration between consecutive tool invocations.
	// Zero means no rate limiting.
	RateLimit time.Duration
	// Timeout overrides the default per-task timeout. Zero falls back to workerTaskTimeout.
	Timeout time.Duration
}

// toolCall holds a parsed tool invocation extracted from an LLM response.
type toolCall struct {
	name   string
	params map[string]string
}

// Worker is a domain-expert agent that handles a single delegated subtask.
// It maintains a private scratchpad and can invoke tools via the registry.
type Worker struct {
	id       string
	role     string
	task     string
	skills   WorkerSkills
	bus      *bus.Bus
	llm      llm.Client
	registry *tools.Registry
	inbox    <-chan Message

	// limiter gates tool invocations; nil means no rate limiting.
	limiter <-chan time.Time

	mu         sync.RWMutex
	status     AgentStatus
	scratchpad strings.Builder

	cancel context.CancelFunc
	done   chan struct{}
}

// NewWorker constructs a Worker with default skills derived from the role name.
func NewWorker(id, role, task string, b *bus.Bus, client llm.Client, registry *tools.Registry) *Worker {
	return NewWorkerWithSkills(id, task, WorkerSkills{Role: role}, b, client, registry)
}

// NewWorkerWithSkills constructs a Worker with the provided skill profile.
func NewWorkerWithSkills(id, task string, skills WorkerSkills, b *bus.Bus, client llm.Client, registry *tools.Registry) *Worker {
	if skills.Role == "" {
		skills.Role = "Analyst"
	}
	w := &Worker{
		id:       id,
		role:     skills.Role,
		task:     task,
		skills:   skills,
		bus:      b,
		llm:      client,
		registry: registry,
		inbox:    b.Subscribe(id),
		done:     make(chan struct{}),
	}
	if skills.RateLimit > 0 {
		ticker := time.NewTicker(skills.RateLimit)
		w.limiter = ticker.C
	}
	return w
}

func (w *Worker) ID() string            { return w.id }
func (w *Worker) Role() string          { return w.role }
func (w *Worker) Status() AgentStatus   { w.mu.RLock(); defer w.mu.RUnlock(); return w.status }
func (w *Worker) Inbox() <-chan Message  { return w.inbox }
func (w *Worker) Send(msg Message)       { w.bus.Publish(msg) }
func (w *Worker) Skills() WorkerSkills  { return w.skills }

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

// Done returns a channel that is closed when the worker has finished.
func (w *Worker) Done() <-chan struct{} { return w.done }

func (w *Worker) run(ctx context.Context) {
	defer close(w.done)
	defer w.bus.Unsubscribe(w.id)
	defer func() {
		if r := recover(); r != nil {
			w.setStatus(StatusError)
			errMsg := NewMessage(MsgTypeError, w.id, "", fmt.Sprintf("[%s] panic: %v", w.role, r))
			errMsg.Metadata["role"] = w.role
			errMsg.Metadata["panic"] = "true"
			w.bus.Publish(errMsg)
			// Still report a result so the orchestrator is not left waiting.
			res := NewMessage(MsgTypeTaskResult, w.id, orchestratorID, fmt.Sprintf("panic recovered: %v", r))
			res.Metadata["worker_id"] = w.id
			res.Metadata["role"] = w.role
			w.bus.Publish(res)
		}
	}()

	w.emit(ctx, MsgTypeStatusUpdate, fmt.Sprintf("[%s] Starting task: %s", w.role, w.task))

	result, err := w.processTask(ctx)
	if err != nil {
		w.setStatus(StatusError)
		// Build error message with retry hint metadata.
		errMsg := NewMessage(MsgTypeError, w.id, "", fmt.Sprintf("[%s] Error: %v", w.role, err))
		errMsg.Metadata["role"] = w.role
		retryable, hint := retryHint(err)
		errMsg.Metadata["retryable"] = fmt.Sprintf("%t", retryable)
		if hint != "" {
			errMsg.Metadata["retry_hint"] = hint
		}
		w.bus.Publish(errMsg)
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
	// Enforce a per-task timeout; use skill override when set.
	timeout := w.skills.Timeout
	if timeout == 0 {
		timeout = workerTaskTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	systemPrompt := w.buildSystemPrompt()

	w.scratchPad("Starting analysis of task: " + w.task)
	w.emit(ctx, MsgTypeStatusUpdate, fmt.Sprintf("[%s] Calling LLM...", w.role))

	messages := []llm.ChatMessage{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: w.task},
	}

	// Tool-calling agentic loop: iterate up to maxToolIterations rounds.
	for iter := 0; iter < maxToolIterations; iter++ {
		response, err := w.llm.Complete(ctx, messages)
		if err != nil {
			return "", &retryableError{
				err:       fmt.Errorf("worker LLM call (iter %d): %w", iter+1, err),
				retryable: true,
				hint:      "transient LLM error – retry after a short delay",
			}
		}

		w.scratchPad(fmt.Sprintf("[iter %d] LLM response: %d chars", iter+1, len(response)))
		w.emit(ctx, MsgTypeStatusUpdate, fmt.Sprintf("[%s] Response received (iter %d), processing...", w.role, iter+1))

		// Parse tool calls from the response.
		calls := parseToolCalls(response)
		allowedCalls := w.filterAllowedTools(calls)
		if len(allowedCalls) == 0 || w.registry == nil {
			// No tool calls – return the response directly.
			return response, nil
		}

		// Execute each tool call, honouring the per-worker rate limit.
		var toolResults strings.Builder
		for _, tc := range allowedCalls {
			if err := w.applyRateLimit(ctx); err != nil {
				return "", err
			}

			// Emit tool call notification on the bus.
			paramsJSON, _ := json.Marshal(tc.params)
			callMsg := NewMessage(MsgTypeToolCall, w.id, "", fmt.Sprintf("%s(%s)", tc.name, paramsJSON))
			callMsg.Metadata["role"] = w.role
			callMsg.Metadata["tool"] = tc.name
			w.bus.Publish(callMsg)

			result, toolErr := w.registry.Execute(ctx, tc.name, tc.params)
			resultContent := result
			if toolErr != nil {
				resultContent = fmt.Sprintf("error: %v", toolErr)
			}

			// Emit tool result notification.
			resMsg := NewMessage(MsgTypeToolResult, w.id, "", resultContent)
			resMsg.Metadata["role"] = w.role
			resMsg.Metadata["tool"] = tc.name
			w.bus.Publish(resMsg)

			w.scratchPad(fmt.Sprintf("Tool %s → %s", tc.name, resultContent))
			toolResults.WriteString(fmt.Sprintf("Tool: %s\nResult: %s\n\n", tc.name, resultContent))
		}

		// Feed tool results back to the conversation for the next iteration.
		messages = append(messages,
			llm.ChatMessage{Role: "assistant", Content: response},
			llm.ChatMessage{Role: "user", Content: "Tool results:\n" + toolResults.String() + "\nPlease continue with your analysis."},
		)
	}

	// Final LLM call after exhausting tool iterations.
	response, err := w.llm.Complete(ctx, messages)
	if err != nil {
		return "", &retryableError{
			err:       fmt.Errorf("worker LLM final call: %w", err),
			retryable: true,
			hint:      "transient LLM error – retry after a short delay",
		}
	}
	w.scratchPad(fmt.Sprintf("Final response: %d chars", len(response)))
	return response, nil
}

// buildSystemPrompt constructs the LLM system prompt from the worker's skill profile.
func (w *Worker) buildSystemPrompt() string {
	var sb strings.Builder
	if w.skills.Description != "" {
		sb.WriteString(w.skills.Description)
	} else {
		sb.WriteString(fmt.Sprintf(
			"You are a %s specialist agent. Your task is: %s\n\n"+
				"Work through the task step by step. Use your expertise to provide a detailed, "+
				"actionable response. Document your reasoning in a structured way.",
			w.role, w.task,
		))
	}

	// Append available tool descriptions when tools are accessible.
	if w.registry != nil && len(w.skills.AllowedTools) > 0 {
		sb.WriteString("\n\nYou have access to the following tools. To invoke a tool, include a line in your response with the format:\n")
		sb.WriteString("TOOL_CALL|<tool_name>|<key>=<value>[&<key>=<value>...]\n\nAvailable tools:\n")
		for _, t := range w.registry.List() {
			for _, allowed := range w.skills.AllowedTools {
				if t.Name() == allowed {
					sb.WriteString(fmt.Sprintf("- %s: %s\n", t.Name(), t.Description()))
					break
				}
			}
		}
	}

	return sb.String()
}

// applyRateLimit blocks until the rate-limit window elapses, or ctx is done.
func (w *Worker) applyRateLimit(ctx context.Context) error {
	if w.limiter == nil {
		return nil
	}
	select {
	case <-w.limiter:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// filterAllowedTools returns only the tool calls whose tool name appears in AllowedTools.
func (w *Worker) filterAllowedTools(calls []toolCall) []toolCall {
	if len(w.skills.AllowedTools) == 0 {
		return nil
	}
	allowed := make(map[string]bool, len(w.skills.AllowedTools))
	for _, name := range w.skills.AllowedTools {
		allowed[name] = true
	}
	var out []toolCall
	for _, tc := range calls {
		if allowed[tc.name] {
			out = append(out, tc)
		}
	}
	return out
}

// parseToolCalls extracts TOOL_CALL directives from an LLM response.
// Format: TOOL_CALL|<tool_name>|<key>=<value>[&<key>=<value>...]
func parseToolCalls(response string) []toolCall {
	var calls []toolCall
	for _, line := range strings.Split(response, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "TOOL_CALL|") {
			continue
		}
		parts := strings.SplitN(line, "|", 3)
		if len(parts) < 2 {
			continue
		}
		name := strings.TrimSpace(parts[1])
		if name == "" {
			continue
		}
		params := make(map[string]string)
		if len(parts) == 3 {
			for _, kv := range strings.Split(parts[2], "&") {
				kv = strings.TrimSpace(kv)
				if idx := strings.IndexByte(kv, '='); idx > 0 {
					params[kv[:idx]] = kv[idx+1:]
				}
			}
		}
		calls = append(calls, toolCall{name: name, params: params})
	}
	return calls
}

// retryableError is an error that carries a retry recommendation.
type retryableError struct {
	err       error
	retryable bool
	hint      string
}

func (e *retryableError) Error() string { return e.err.Error() }
func (e *retryableError) Unwrap() error { return e.err }

// retryHint extracts retry metadata from an error.
// Returns (retryable, hint).
func retryHint(err error) (bool, string) {
	var re *retryableError
	if errors.As(err, &re) {
		return re.retryable, re.hint
	}
	return false, ""
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

// Scratchpad returns a snapshot of the worker's private working notes.
func (w *Worker) Scratchpad() string {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.scratchpad.String()
}

func (w *Worker) setStatus(s AgentStatus) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.status = s
}
