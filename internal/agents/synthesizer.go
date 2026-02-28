package agents

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/cybersecdef/gollm/internal/bus"
	"github.com/cybersecdef/gollm/internal/llm"
)

const synthesizerID = "synthesizer"

// Synthesizer merges worker task results into a single coherent final response
// with citations referencing each contributing worker.
type Synthesizer struct {
	id         string
	bus        *bus.Bus
	llm        llm.Client
	inbox      <-chan Message
	mu         sync.RWMutex
	status     AgentStatus
	scratchpad strings.Builder
	cancel     context.CancelFunc
	done       chan struct{}
}

// NewSynthesizer creates a Synthesizer and subscribes it to the bus.
func NewSynthesizer(b *bus.Bus, client llm.Client) *Synthesizer {
	return &Synthesizer{
		id:    synthesizerID,
		bus:   b,
		llm:   client,
		inbox: b.Subscribe(synthesizerID),
		done:  make(chan struct{}),
	}
}

func (s *Synthesizer) ID() string            { return s.id }
func (s *Synthesizer) Role() string          { return "Synthesizer" }
func (s *Synthesizer) Status() AgentStatus   { s.mu.RLock(); defer s.mu.RUnlock(); return s.status }
func (s *Synthesizer) Inbox() <-chan Message  { return s.inbox }
func (s *Synthesizer) Send(msg Message)       { s.bus.Publish(msg) }

// Scratchpad returns a snapshot of the synthesizer's working notes.
func (s *Synthesizer) Scratchpad() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.scratchpad.String()
}

// Start begins the synthesizer's event loop.
func (s *Synthesizer) Start(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	s.cancel = cancel
	s.setStatus(StatusIdle)
	go s.loop(ctx)
	return nil
}

// Stop shuts down the synthesizer.
func (s *Synthesizer) Stop() {
	if s.cancel != nil {
		s.cancel()
	}
	<-s.done
}

func (s *Synthesizer) loop(ctx context.Context) {
	defer close(s.done)
	for {
		select {
		case <-ctx.Done():
			s.setStatus(StatusCompleted)
			return
		case msg, ok := <-s.inbox:
			if !ok {
				return
			}
			if msg.Type == MsgTypeShutdown {
				s.setStatus(StatusCompleted)
				return
			}
			if msg.Type == MsgTypeTaskAssign {
				go s.synthesize(ctx, msg)
			}
		}
	}
}

// synthesize is called when the orchestrator sends a MsgTypeTaskAssign with
// all worker results bundled in the Content field.
func (s *Synthesizer) synthesize(ctx context.Context, assignMsg Message) {
	s.setStatus(StatusWorking)
	s.emitStatus(ctx, "Synthesizing worker outputs...")

	s.mu.Lock()
	s.scratchpad.WriteString(fmt.Sprintf("Synthesizing correlation=%s\n", assignMsg.Metadata["correlation_id"]))
	s.mu.Unlock()

	systemPrompt := `You are a synthesis specialist. You will receive multiple expert agent outputs.
Your job is to:
1. Merge them into a single, coherent, well-structured response
2. Eliminate redundancy while preserving all important information
3. Add a citations section at the end referencing each agent's contribution
4. Present the final answer clearly and actionably`

	userPrompt := fmt.Sprintf(
		"Please synthesize the following expert agent outputs into a final response:\n\n%s",
		assignMsg.Content,
	)

	messages := []llm.ChatMessage{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: userPrompt},
	}

	result, err := s.llm.Complete(ctx, messages)
	if err != nil {
		s.setStatus(StatusError)
		s.emitStatus(ctx, fmt.Sprintf("Synthesis error: %v", err))
		result = fallbackSynthesize(assignMsg.Content)
	}

	// Post the final response so the TUI and orchestrator can display it.
	finalMsg := NewMessage(MsgTypeFinalResponse, s.id, "", result)
	finalMsg.Metadata["correlation_id"] = assignMsg.Metadata["correlation_id"]
	s.bus.Publish(finalMsg)

	s.setStatus(StatusIdle)
	s.emitStatus(ctx, "Synthesis complete")
}

// fallbackSynthesize builds a simple combined response when the LLM fails.
func fallbackSynthesize(content string) string {
	var sb strings.Builder
	sb.WriteString("## Combined Agent Response\n\n")
	sb.WriteString(content)
	sb.WriteString("\n\n---\n*Synthesized by gollm*")
	return sb.String()
}

func (s *Synthesizer) emitStatus(ctx context.Context, content string) {
	select {
	case <-ctx.Done():
		return
	default:
	}
	s.bus.Publish(NewMessage(MsgTypeStatusUpdate, s.id, "", content))
}

func (s *Synthesizer) setStatus(st AgentStatus) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status = st
}
