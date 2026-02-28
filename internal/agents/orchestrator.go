package agents

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/cybersecdef/gollm/internal/bus"
	"github.com/cybersecdef/gollm/internal/llm"
	"github.com/cybersecdef/gollm/internal/tools"
)

const orchestratorID = "orchestrator"

// workerState tracks a spawned worker and its result.
type workerState struct {
	worker *Worker
	result string
	done   bool
}

// Orchestrator owns the main control loop. It decomposes user requests into
// subtasks, spawns Worker agents, collects results, and forwards them to the
// Synthesizer.
type Orchestrator struct {
	id       string
	bus      *bus.Bus
	llm      llm.Client
	registry *tools.Registry
	inbox    <-chan Message

	mu        sync.RWMutex
	status    AgentStatus
	workers   map[string]*workerState
	resultCh  chan struct{}
	workerSeq atomic.Int64

	cancel context.CancelFunc
	done   chan struct{}
}

// NewOrchestrator creates an Orchestrator and subscribes it to the bus.
func NewOrchestrator(b *bus.Bus, client llm.Client, registry *tools.Registry) *Orchestrator {
	return &Orchestrator{
		id:       orchestratorID,
		bus:      b,
		llm:      client,
		registry: registry,
		inbox:    b.Subscribe(orchestratorID),
		workers:  make(map[string]*workerState),
		resultCh: make(chan struct{}, 64),
		done:     make(chan struct{}),
	}
}

func (o *Orchestrator) ID() string          { return o.id }
func (o *Orchestrator) Role() string        { return "Orchestrator" }
func (o *Orchestrator) Status() AgentStatus { o.mu.RLock(); defer o.mu.RUnlock(); return o.status }
func (o *Orchestrator) Send(msg Message)    { o.bus.Publish(msg) }

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
				// Future: resume branched workflows.
			}
		}
	}
}

// handleUserInput decomposes the request into subtasks and spawns workers.
func (o *Orchestrator) handleUserInput(ctx context.Context, msg Message) {
	o.setStatus(StatusWorking)
	correlationID := msg.ID

	o.emitEvent(ctx, "Received user input, decomposing task...")

	subtasks, err := o.decompose(ctx, msg.Content)
	if err != nil {
		o.emitError(ctx, fmt.Sprintf("Decomposition failed: %v", err))
		o.setStatus(StatusIdle)
		return
	}
	if len(subtasks) == 0 {
		o.emitError(ctx, "No subtasks generated")
		o.setStatus(StatusIdle)
		return
	}

	o.emitEvent(ctx, fmt.Sprintf("Decomposed into %d subtask(s)", len(subtasks)))

	// Spawn workers and record their IDs.
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

	// Start workers.
	for _, wid := range workerIDs {
		o.mu.RLock()
		ws := o.workers[wid]
		o.mu.RUnlock()
		o.emitEvent(ctx, fmt.Sprintf("Spawning %s agent: %s", ws.worker.role, ws.worker.task))
		if err := ws.worker.Start(ctx); err != nil {
			o.emitError(ctx, fmt.Sprintf("Failed to start worker %s: %v", wid, err))
		}
	}

	// Wait for all workers, then synthesize.
	go o.waitAndSynthesize(ctx, workerIDs, correlationID)
}

// waitAndSynthesize waits until all workers finish and then forwards results to synthesizer.
func (o *Orchestrator) waitAndSynthesize(ctx context.Context, workerIDs []string, correlationID string) {
	for {
		if ctx.Err() != nil {
			return
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
			return
		case <-o.resultCh:
		}
	}

	// Collect results.
	o.mu.RLock()
	var sb strings.Builder
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

	o.emitEvent(ctx, "All workers complete, sending to synthesizer...")
	o.setStatus(StatusWaiting)

	synthMsg := NewMessage(MsgTypeTaskAssign, o.id, synthesizerID, sb.String())
	synthMsg.Metadata["correlation_id"] = correlationID
	o.bus.Publish(synthMsg)
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

	// Notify waitAndSynthesize.
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

// decompose calls the LLM to break the user request into role/task pairs.
func (o *Orchestrator) decompose(ctx context.Context, userRequest string) ([]subtask, error) {
	systemPrompt := `You are an orchestration agent. Decompose the user's request into 2-3 specialist subtasks.
Format each subtask on its own line as:
SUBTASK_N|Role|Task description

Where Role is one of: Researcher, Analyst, Coder, Writer, Reviewer
Example:
SUBTASK_1|Researcher|Research the history of X
SUBTASK_2|Analyst|Analyze the requirements for Y`

	messages := []llm.ChatMessage{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: "Decompose this request into subtasks: " + userRequest},
	}

	response, err := o.llm.Complete(ctx, messages)
	if err != nil {
		return nil, err
	}
	return parseSubtasks(response), nil
}

// parseSubtasks extracts SUBTASK_N|Role|Task lines from LLM output.
func parseSubtasks(response string) []subtask {
	var tasks []subtask
	for _, line := range strings.Split(response, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "SUBTASK_") {
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

func (o *Orchestrator) emitEvent(ctx context.Context, content string) {
	if ctx.Err() != nil {
		return
	}
	o.bus.Publish(NewMessage(MsgTypeEvent, o.id, "", content))
}

func (o *Orchestrator) emitError(ctx context.Context, content string) {
	if ctx.Err() != nil {
		return
	}
	o.bus.Publish(NewMessage(MsgTypeError, o.id, "", content))
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
