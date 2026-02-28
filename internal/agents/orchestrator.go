package agents

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cybersecdef/gollm/internal/bus"
	"github.com/cybersecdef/gollm/internal/llm"
	"github.com/cybersecdef/gollm/internal/tools"
)

const (
	orchestratorID    = "orchestrator"
	clarificationRole = "Clarification"
	maxIterations     = 3
	// MaxIterations is exported so external packages (e.g. the TUI) can display
	// iteration progress without hardcoding the value.
	MaxIterations  = maxIterations
	maxRetries     = 3
	baseRetryDelay = time.Second
)

// OrchestratorHook is a callback invoked at key lifecycle events in the loop.
// event is a short identifier (e.g. "pre_synthesize"); metadata carries context.
type OrchestratorHook func(event string, metadata map[string]string)

// OrchestratorConfig holds optional runtime configuration for the Orchestrator.
type OrchestratorConfig struct {
	// MaxWorkers limits the number of concurrently running workers across all
	// iterations. Zero (default) means unlimited.
	MaxWorkers int
	// TaskTimeout caps the total time allowed for a single handleUserInput
	// execution. Zero means no extra limit beyond individual worker timeouts.
	TaskTimeout time.Duration
}

// workerState tracks a spawned worker and its result.
type workerState struct {
	worker *Worker
	result string
	done   bool
}

// retryWithBackoff runs fn up to maxAttempts times with exponential backoff.
// Delays: base, 2*base, 4*base, …
func retryWithBackoff(ctx context.Context, maxAttempts int, base time.Duration, fn func() error) error {
	var err error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err = fn(); err == nil {
			return nil
		}
		if attempt < maxAttempts-1 {
			delay := base * (1 << uint(attempt))
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(delay):
			}
		}
	}
	return err
}

// Orchestrator owns the main control loop. It decomposes user requests into
// subtasks, spawns Worker agents, collects results, checks acceptance criteria,
// and forwards results to the Synthesizer.
type Orchestrator struct {
	id       string
	bus      *bus.Bus
	llm      llm.Client
	registry *tools.Registry
	factory  *WorkerFactory // optional; used when spawning workers
	inbox    <-chan Message

	mu              sync.RWMutex
	status          AgentStatus
	workers         map[string]*workerState
	resultCh        chan struct{}
	workerSeq       atomic.Int64
	currentIter     atomic.Int32
	taskCancel      context.CancelFunc // cancels the running task; nil when idle
	scratchpad      strings.Builder
	clarificationCh chan string       // receives user answers to questions
	synthHook       OrchestratorHook // called just before handing off to synthesizer

	semaphore chan struct{} // bounded worker pool; nil = unlimited
	cfg       OrchestratorConfig

	cancel context.CancelFunc
	done   chan struct{}
}

// NewOrchestrator creates an Orchestrator and subscribes it to the bus.
func NewOrchestrator(b *bus.Bus, client llm.Client, registry *tools.Registry) *Orchestrator {
	return NewOrchestratorWithConfig(b, client, registry, OrchestratorConfig{})
}

// NewOrchestratorWithConfig creates an Orchestrator with the supplied configuration.
func NewOrchestratorWithConfig(b *bus.Bus, client llm.Client, registry *tools.Registry, cfg OrchestratorConfig) *Orchestrator {
	o := &Orchestrator{
		id:              orchestratorID,
		bus:             b,
		llm:             client,
		registry:        registry,
		cfg:             cfg,
		inbox:           b.Subscribe(orchestratorID),
		workers:         make(map[string]*workerState),
		resultCh:        make(chan struct{}, 64),
		clarificationCh: make(chan string, 1),
		done:            make(chan struct{}),
	}
	if cfg.MaxWorkers > 0 {
		o.semaphore = make(chan struct{}, cfg.MaxWorkers)
		// Pre-fill so acquire = receive (takes a slot) and release = send (returns a slot).
		for i := 0; i < cfg.MaxWorkers; i++ {
			o.semaphore <- struct{}{}
		}
	}
	return o
}

// RegisterSynthesizerHook sets a hook called just before results are sent to
// the synthesizer. Replaces any previously registered hook.
func (o *Orchestrator) RegisterSynthesizerHook(h OrchestratorHook) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.synthHook = h
}

// SetWorkerFactory configures the factory used when spawning workers.
// When set, workers are created via the factory and inherit the registered
// skill profile for their role (tools, rate limit, timeout, etc.).
// Can be called at any time before or after Start.
func (o *Orchestrator) SetWorkerFactory(f *WorkerFactory) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.factory = f
}

func (o *Orchestrator) ID() string            { return o.id }
func (o *Orchestrator) Role() string          { return "Orchestrator" }
func (o *Orchestrator) Status() AgentStatus   { o.mu.RLock(); defer o.mu.RUnlock(); return o.status }
func (o *Orchestrator) Inbox() <-chan Message  { return o.inbox }
func (o *Orchestrator) Send(msg Message)       { o.bus.Publish(msg) }

// Scratchpad returns a snapshot of the orchestrator's planning notes.
func (o *Orchestrator) Scratchpad() string {
	o.mu.RLock()
	defer o.mu.RUnlock()
	return o.scratchpad.String()
}

// Start launches the orchestrator's event loop.
func (o *Orchestrator) Start(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	o.cancel = cancel
	o.setStatus(StatusIdle)
	go o.loop(ctx)
	return nil
}

// Stop shuts down the orchestrator and all spawned workers.
func (o *Orchestrator) Stop() {
	if o.cancel != nil {
		o.cancel()
	}
	<-o.done

	o.mu.Lock()
	defer o.mu.Unlock()
	for _, ws := range o.workers {
		ws.worker.Stop()
	}
}

func (o *Orchestrator) loop(ctx context.Context) {
	defer close(o.done)
	for {
		select {
		case <-ctx.Done():
			o.setStatus(StatusCompleted)
			return
		case msg, ok := <-o.inbox:
			if !ok {
				return
			}
			switch msg.Type {
			case MsgTypeShutdown:
				o.setStatus(StatusCompleted)
				return
			case MsgTypeUserInput:
				go o.handleUserInput(ctx, msg)
			case MsgTypeTaskResult:
				o.handleTaskResult(msg)
			case MsgTypeAnswer:
				// Deliver the answer to any goroutine blocked in askClarification.
				select {
				case o.clarificationCh <- msg.Content:
				default:
				}
			case MsgTypeStopRequest:
				// Cancel the currently running task if any.
				o.mu.Lock()
				cancel := o.taskCancel
				o.mu.Unlock()
				if cancel != nil {
					cancel()
				}
			case MsgTypeFinalResponse:
				// Confirm completion and emit a final event with exit code.
				correlationID := msg.CorrelationID
				if correlationID == "" {
					correlationID = msg.Metadata["correlation_id"]
				}
				done := NewMessage(MsgTypeEvent, o.id, "", "Task complete")
				done.Metadata["exit_code"] = "0"
				done.Metadata["correlation_id"] = correlationID
				done.CorrelationID = correlationID
				o.bus.Publish(done)
			}
		}
	}
}

// handleUserInput runs the orchestration loop: decompose → spawn workers →
// collect results → check acceptance criteria, iterating up to maxIterations.
// It requests clarifications from the user when the LLM needs more context and
// spawns new agents mid-loop as needed.
func (o *Orchestrator) handleUserInput(ctx context.Context, msg Message) {
	defer func() {
		if r := recover(); r != nil {
			o.setStatus(StatusError)
			errMsg := NewMessage(MsgTypeError, o.id, "", fmt.Sprintf("orchestrator panic: %v", r))
			o.bus.Publish(errMsg)
			o.setStatus(StatusIdle)
		}
	}()

	// Create a task-scoped context so individual tasks can be stopped via
	// MsgTypeStopRequest without affecting the agent's overall lifecycle.
	taskCtx, taskCancel := context.WithCancel(ctx)
	o.mu.Lock()
	o.taskCancel = taskCancel
	o.mu.Unlock()
	defer func() {
		taskCancel()
		o.mu.Lock()
		o.taskCancel = nil
		o.mu.Unlock()
		o.currentIter.Store(0)
		o.setStatus(StatusIdle)
	}()

	o.setStatus(StatusWorking)
	correlationID := msg.ID
	goal := msg.Content

	o.mu.Lock()
	o.scratchpad.WriteString(fmt.Sprintf("[%s] Goal: %s\n", correlationID, goal))
	o.mu.Unlock()

	o.emitEvent(taskCtx, correlationID, "Received user goal, starting orchestration loop...")

	var accumulated strings.Builder

	for iter := 1; iter <= maxIterations; iter++ {
		if taskCtx.Err() != nil {
			return
		}

		o.currentIter.Store(int32(iter))
		o.emitEvent(taskCtx, correlationID, fmt.Sprintf("[%d/%d] Decomposing tasks...", iter, maxIterations))

		// Decompose with automatic retry/backoff.
		subtasks, err := o.decomposeWithRetry(taskCtx, goal, accumulated.String())
		if err != nil {
			o.emitError(taskCtx, correlationID, fmt.Sprintf("Decomposition failed: %v", err))
			break
		}
		if len(subtasks) == 0 {
			o.emitError(taskCtx, correlationID, "No subtasks generated")
			break
		}

		// If the LLM requests a user clarification, pause and ask.
		if len(subtasks) == 1 && subtasks[0].role == clarificationRole {
			o.emitEvent(taskCtx, correlationID, "Requesting clarification from user...")
			answer, ok := o.askClarification(taskCtx, subtasks[0].task)
			if !ok {
				return
			}
			goal = fmt.Sprintf("%s\nUser clarification: %s", goal, answer)
			continue
		}

		o.emitEvent(taskCtx, correlationID, fmt.Sprintf("[%d/%d] Spawning %d worker(s)...", iter, maxIterations, len(subtasks)))

		// Spawn a new set of workers for this iteration.
		iterWorkerIDs := o.spawnIterWorkers(taskCtx, subtasks, correlationID)

		// Wait for all iteration workers to finish.
		iterResults := o.collectWorkerResults(taskCtx, iterWorkerIDs)
		accumulated.WriteString(fmt.Sprintf("=== Iteration %d ===\n%s\n\n", iter, iterResults))

		o.mu.Lock()
		o.scratchpad.WriteString(fmt.Sprintf("[iter %d] collected %d chars\n", iter, len(iterResults)))
		o.mu.Unlock()

		o.emitEvent(taskCtx, correlationID, fmt.Sprintf("[%d/%d] Checking acceptance criteria...", iter, maxIterations))

		accepted, acceptErr := o.checkAcceptanceCriteria(taskCtx, goal, accumulated.String())
		if acceptErr != nil {
			// Treat a failed acceptance check as "not yet accepted" and continue.
			o.emitEvent(taskCtx, correlationID, fmt.Sprintf("[%d/%d] Acceptance check error: %v, continuing...", iter, maxIterations, acceptErr))
			continue
		}
		if accepted {
			o.emitEvent(taskCtx, correlationID, fmt.Sprintf("Acceptance criteria met after iteration %d", iter))
			break
		}

		o.emitEvent(taskCtx, correlationID, fmt.Sprintf("[%d/%d] Criteria not yet met, refining...", iter, maxIterations))
	}

	// Bail out if the task was stopped mid-execution.
	if taskCtx.Err() != nil {
		return
	}

	// Invoke pre-synthesis hook if registered.
	o.mu.RLock()
	hook := o.synthHook
	o.mu.RUnlock()
	if hook != nil {
		hook("pre_synthesize", map[string]string{
			"correlation_id": correlationID,
			"goal":           goal,
		})
	}

	o.emitEvent(taskCtx, correlationID, "All iterations complete, sending to synthesizer...")
	o.setStatus(StatusWaiting)

	// Append worker scratchpads to the content for traceability.
	var padBuilder strings.Builder
	o.mu.RLock()
	for id, ws := range o.workers {
		if pad := ws.worker.Scratchpad(); pad != "" {
			padBuilder.WriteString(fmt.Sprintf("=== %s (%s) Scratchpad ===\n%s\n\n", ws.worker.role, id, pad))
		}
	}
	o.mu.RUnlock()
	content := accumulated.String()
	if padBuilder.Len() > 0 {
		content += "\n=== WORKER SCRATCHPADS ===\n" + padBuilder.String()
	}

	synthMsg := NewMessage(MsgTypeTaskAssign, o.id, synthesizerID, content)
	synthMsg.Metadata["correlation_id"] = correlationID
	synthMsg.Metadata["orchestrator_plan"] = o.Scratchpad()
	synthMsg.CorrelationID = correlationID
	o.bus.Publish(synthMsg)
	// setStatus(StatusIdle) is handled by the deferred cleanup above.
}

// spawnIterWorkers creates and starts a set of workers for one loop iteration.
// It registers them in the shared workers map and returns their IDs.
func (o *Orchestrator) spawnIterWorkers(ctx context.Context, subtasks []subtask, correlationID string) []string {
	workerIDs := make([]string, 0, len(subtasks))

	o.mu.Lock()
	fac := o.factory
	for _, st := range subtasks {
		wid := fmt.Sprintf("worker-%d", o.workerSeq.Add(1))
		var w *Worker
		if fac != nil {
			w = fac.NewWorker(wid, st.role, st.task)
		} else {
			w = NewWorker(wid, st.role, st.task, o.bus, o.llm, o.registry)
		}
		o.workers[wid] = &workerState{worker: w}
		workerIDs = append(workerIDs, wid)
	}
	o.mu.Unlock()

	for _, wid := range workerIDs {
		o.mu.RLock()
		ws := o.workers[wid]
		o.mu.RUnlock()

		// Acquire semaphore before starting each worker (backpressure / bounded pool).
		if o.semaphore != nil {
			select {
			case <-o.semaphore: // acquire a slot
			case <-ctx.Done():
				return workerIDs
			}
			// Release the slot asynchronously when this worker finishes.
			done := ws.worker.Done()
			sem := o.semaphore
			go func() {
				select {
				case <-done:
				case <-ctx.Done():
				}
				select {
				case sem <- struct{}{}: // release slot
				default:
				}
			}()
		}

		o.emitEvent(ctx, correlationID, fmt.Sprintf("Spawning %s agent: %s", ws.worker.role, ws.worker.task))
		if err := ws.worker.Start(ctx); err != nil {
			o.emitError(ctx, correlationID, fmt.Sprintf("Failed to start worker %s: %v", wid, err))
		}
	}

	return workerIDs
}

// collectWorkerResults blocks until every worker in the given set has finished,
// then returns their combined output as a formatted string.
func (o *Orchestrator) collectWorkerResults(ctx context.Context, workerIDs []string) string {
	for {
		if ctx.Err() != nil {
			return ""
		}

		o.mu.RLock()
		allDone := true
		for _, wid := range workerIDs {
			ws := o.workers[wid]
			if ws == nil || !ws.done {
				allDone = false
				break
			}
		}
		o.mu.RUnlock()

		if allDone {
			break
		}

		select {
		case <-ctx.Done():
			return ""
		case <-o.resultCh:
		}
	}

	var sb strings.Builder
	o.mu.RLock()
	for _, wid := range workerIDs {
		ws := o.workers[wid]
		if ws == nil {
			continue
		}
		sb.WriteString(fmt.Sprintf("=== %s Agent (%s) ===\n", ws.worker.role, wid))
		sb.WriteString(ws.result)
		sb.WriteString("\n\n")
	}
	o.mu.RUnlock()

	return sb.String()
}

// askClarification broadcasts a question to the user and blocks until an answer
// arrives via MsgTypeAnswer or the context is cancelled.
func (o *Orchestrator) askClarification(ctx context.Context, question string) (string, bool) {
	o.setStatus(StatusWaiting)
	o.bus.Publish(NewMessage(MsgTypeQuestion, o.id, "", question))

	select {
	case <-ctx.Done():
		return "", false
	case answer := <-o.clarificationCh:
		o.setStatus(StatusWorking)
		return answer, true
	}
}

// checkAcceptanceCriteria asks the LLM whether the accumulated results satisfy
// the user's original goal. Returns true when the goal is met.
func (o *Orchestrator) checkAcceptanceCriteria(ctx context.Context, goal, results string) (bool, error) {
	systemPrompt := `You are an acceptance criteria evaluator for a multi-agent system.
Determine whether the collected results fully address the user's goal.
Respond with exactly one of:
ACCEPTED   – the goal is fully addressed
NEEDS_MORE_WORK – more work is required`

	userContent := fmt.Sprintf("Goal: %s\n\nCollected results:\n%s", goal, results)

	var response string
	err := retryWithBackoff(ctx, maxRetries, baseRetryDelay, func() error {
		var callErr error
		response, callErr = o.llm.Complete(ctx, []llm.ChatMessage{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: userContent},
		})
		return callErr
	})
	if err != nil {
		return false, err
	}

	return strings.Contains(strings.ToUpper(response), "ACCEPTED"), nil
}

// decomposeWithRetry wraps decomposeWithContext with retry/backoff.
func (o *Orchestrator) decomposeWithRetry(ctx context.Context, goal, previousResults string) ([]subtask, error) {
	var tasks []subtask
	err := retryWithBackoff(ctx, maxRetries, baseRetryDelay, func() error {
		var callErr error
		tasks, callErr = o.decomposeWithContext(ctx, goal, previousResults)
		return callErr
	})
	return tasks, err
}

// decomposeWithContext calls the LLM to plan subtasks, optionally using
// results from previous iterations to refine the plan.
func (o *Orchestrator) decomposeWithContext(ctx context.Context, goal, previousResults string) ([]subtask, error) {
	systemPrompt := `You are an orchestration agent. Decompose the user's goal into 2-3 specialist subtasks.
Format each subtask on its own line as:
SUBTASK_N|Role|Task description

Where Role is one of: Researcher, Analyst, Coder, Writer, Reviewer
If you need a clarification from the user before proceeding, respond with exactly one line:
CLARIFICATION|Clarification|<your question>

Example:
SUBTASK_1|Researcher|Research the history of X
SUBTASK_2|Analyst|Analyze the requirements for Y`

	var userContent strings.Builder
	userContent.WriteString("Goal: ")
	userContent.WriteString(goal)
	if previousResults != "" {
		userContent.WriteString("\n\nPrevious iteration results:\n")
		userContent.WriteString(previousResults)
		userContent.WriteString("\n\nCreate subtasks to address any gaps or improve the results.")
	}

	response, err := o.llm.Complete(ctx, []llm.ChatMessage{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: userContent.String()},
	})
	if err != nil {
		return nil, err
	}
	return parseSubtasks(response), nil
}

func (o *Orchestrator) handleTaskResult(msg Message) {
	workerID := msg.Metadata["worker_id"]
	if workerID == "" {
		workerID = msg.From
	}

	o.mu.Lock()
	if ws, ok := o.workers[workerID]; ok {
		ws.result = msg.Content
		ws.done = true
	}
	o.mu.Unlock()

// Notify collectWorkerResults.
	select {
	case o.resultCh <- struct{}{}:
	default:
	}
}

// subtask holds a parsed role and task description.
type subtask struct {
	role string
	task string
}

// parseSubtasks extracts SUBTASK_N|Role|Task and CLARIFICATION|Clarification|Q lines.
func parseSubtasks(response string) []subtask {
	var tasks []subtask
	for _, line := range strings.Split(response, "\n") {
		line = strings.TrimSpace(line)
		// Accept both SUBTASK_ and CLARIFICATION prefixes.
		if !strings.HasPrefix(line, "SUBTASK_") && !strings.HasPrefix(line, "CLARIFICATION") {
			continue
		}
		parts := strings.SplitN(line, "|", 3)
		if len(parts) < 3 {
			continue
		}
		role := strings.TrimSpace(parts[1])
		task := strings.TrimSpace(parts[2])
		if role != "" && task != "" {
			tasks = append(tasks, subtask{role: role, task: task})
		}
	}
	// Fallback: single general worker.
	if len(tasks) == 0 {
		tasks = append(tasks, subtask{role: "Analyst", task: response})
	}
	return tasks
}

func (o *Orchestrator) emitEvent(ctx context.Context, correlationID, content string) {
	if ctx.Err() != nil {
		return
	}
	msg := NewMessage(MsgTypeEvent, o.id, "", content)
	msg.CorrelationID = correlationID
	o.bus.Publish(msg)
}

func (o *Orchestrator) emitError(ctx context.Context, correlationID, content string) {
	if ctx.Err() != nil {
		return
	}
	msg := NewMessage(MsgTypeError, o.id, "", content)
	msg.CorrelationID = correlationID
	o.bus.Publish(msg)
}

func (o *Orchestrator) setStatus(s AgentStatus) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.status = s
}

// GetCurrentIteration returns the iteration number currently being executed
// (1-based), or 0 when the orchestrator is idle.
func (o *Orchestrator) GetCurrentIteration() int {
	return int(o.currentIter.Load())
}

// GetWorkerStatuses returns a snapshot of all spawned workers.
func (o *Orchestrator) GetWorkerStatuses() map[string]AgentStatus {
	o.mu.RLock()
	defer o.mu.RUnlock()
	out := make(map[string]AgentStatus, len(o.workers))
	for id, ws := range o.workers {
		out[id] = ws.worker.Status()
	}
	return out
}
