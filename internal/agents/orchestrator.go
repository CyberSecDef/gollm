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
	orchestratorID  = "orchestrator"
	clarificationRole = "Clarification"
	maxIterations   = 3
	maxRetries      = 3
	baseRetryDelay  = time.Second
)

// OrchestratorHook is a callback invoked at key lifecycle events in the loop.
// event is a short identifier (e.g. "pre_synthesize"); metadata carries context.
type OrchestratorHook func(event string, metadata map[string]string)

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
	inbox    <-chan Message

	mu              sync.RWMutex
	status          AgentStatus
	workers         map[string]*workerState
	resultCh        chan struct{}
	workerSeq       atomic.Int64
	scratchpad      strings.Builder
	clarificationCh chan string       // receives user answers to questions
	synthHook       OrchestratorHook // called just before handing off to synthesizer

	cancel context.CancelFunc
	done   chan struct{}
}

// NewOrchestrator creates an Orchestrator and subscribes it to the bus.
func NewOrchestrator(b *bus.Bus, client llm.Client, registry *tools.Registry) *Orchestrator {
	return &Orchestrator{
		id:              orchestratorID,
		bus:             b,
		llm:             client,
		registry:        registry,
		inbox:           b.Subscribe(orchestratorID),
		workers:         make(map[string]*workerState),
		resultCh:        make(chan struct{}, 64),
		clarificationCh: make(chan string, 1),
		done:            make(chan struct{}),
	}
}

// RegisterSynthesizerHook sets a hook called just before results are sent to
// the synthesizer. Replaces any previously registered hook.
func (o *Orchestrator) RegisterSynthesizerHook(h OrchestratorHook) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.synthHook = h
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
			}
		}
	}
}

// handleUserInput runs the orchestration loop: decompose → spawn workers →
// collect results → check acceptance criteria, iterating up to maxIterations.
// It requests clarifications from the user when the LLM needs more context and
// spawns new agents mid-loop as needed.
func (o *Orchestrator) handleUserInput(ctx context.Context, msg Message) {
	o.setStatus(StatusWorking)
	correlationID := msg.ID
	goal := msg.Content

	o.mu.Lock()
	o.scratchpad.WriteString(fmt.Sprintf("[%s] Goal: %s\n", correlationID, goal))
	o.mu.Unlock()

	o.emitEvent(ctx, correlationID, "Received user goal, starting orchestration loop...")

	var accumulated strings.Builder

	for iter := 1; iter <= maxIterations; iter++ {
		if ctx.Err() != nil {
			return
		}

		o.emitEvent(ctx, correlationID, fmt.Sprintf("[%d/%d] Decomposing tasks...", iter, maxIterations))

		// Decompose with automatic retry/backoff.
		subtasks, err := o.decomposeWithRetry(ctx, goal, accumulated.String())
		if err != nil {
			o.emitError(ctx, correlationID, fmt.Sprintf("Decomposition failed: %v", err))
			break
		}
		if len(subtasks) == 0 {
			o.emitError(ctx, correlationID, "No subtasks generated")
			break
		}

		// If the LLM requests a user clarification, pause and ask.
		if len(subtasks) == 1 && subtasks[0].role == clarificationRole {
			o.emitEvent(ctx, correlationID, "Requesting clarification from user...")
			answer, ok := o.askClarification(ctx, subtasks[0].task)
			if !ok {
				return
			}
			goal = fmt.Sprintf("%s\nUser clarification: %s", goal, answer)
			continue
		}

		o.emitEvent(ctx, correlationID, fmt.Sprintf("[%d/%d] Spawning %d worker(s)...", iter, maxIterations, len(subtasks)))

		// Spawn a new set of workers for this iteration.
		iterWorkerIDs := o.spawnIterWorkers(ctx, subtasks, correlationID)

		// Wait for all iteration workers to finish.
		iterResults := o.collectWorkerResults(ctx, iterWorkerIDs)
		accumulated.WriteString(fmt.Sprintf("=== Iteration %d ===\n%s\n\n", iter, iterResults))

		o.mu.Lock()
		o.scratchpad.WriteString(fmt.Sprintf("[iter %d] collected %d chars\n", iter, len(iterResults)))
		o.mu.Unlock()

		o.emitEvent(ctx, correlationID, fmt.Sprintf("[%d/%d] Checking acceptance criteria...", iter, maxIterations))

		accepted, acceptErr := o.checkAcceptanceCriteria(ctx, goal, accumulated.String())
		if acceptErr != nil {
			// Treat a failed acceptance check as "not yet accepted" and continue.
			o.emitEvent(ctx, correlationID, fmt.Sprintf("[%d/%d] Acceptance check error: %v, continuing...", iter, maxIterations, acceptErr))
			continue
		}
		if accepted {
			o.emitEvent(ctx, correlationID, fmt.Sprintf("Acceptance criteria met after iteration %d", iter))
			break
		}

		o.emitEvent(ctx, correlationID, fmt.Sprintf("[%d/%d] Criteria not yet met, refining...", iter, maxIterations))
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

	o.emitEvent(ctx, correlationID, "All iterations complete, sending to synthesizer...")
	o.setStatus(StatusWaiting)

	synthMsg := NewMessage(MsgTypeTaskAssign, o.id, synthesizerID, accumulated.String())
	synthMsg.Metadata["correlation_id"] = correlationID
	synthMsg.CorrelationID = correlationID
	o.bus.Publish(synthMsg)

	o.setStatus(StatusIdle)
}

// spawnIterWorkers creates and starts a set of workers for one loop iteration.
// It registers them in the shared workers map and returns their IDs.
func (o *Orchestrator) spawnIterWorkers(ctx context.Context, subtasks []subtask, correlationID string) []string {
	workerIDs := make([]string, 0, len(subtasks))

	o.mu.Lock()
	for _, st := range subtasks {
		wid := fmt.Sprintf("worker-%d", o.workerSeq.Add(1))
		ws := &workerState{
			worker: NewWorker(wid, st.role, st.task, o.bus, o.llm, o.registry),
		}
		o.workers[wid] = ws
		workerIDs = append(workerIDs, wid)
	}
	o.mu.Unlock()

	for _, wid := range workerIDs {
		o.mu.RLock()
		ws := o.workers[wid]
		o.mu.RUnlock()
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
