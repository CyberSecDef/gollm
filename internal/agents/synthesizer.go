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

// workerSection holds a parsed worker output block extracted from the accumulated content.
type workerSection struct {
	role    string
	id      string
	content string
}

// fileEdit holds a file path and content extracted from a FILE_EDIT directive.
type fileEdit struct {
	path    string
	content string
}

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
// all worker results (and optional orchestrator plan) in the Content/Metadata fields.
func (s *Synthesizer) synthesize(ctx context.Context, assignMsg Message) {
	s.setStatus(StatusWorking)
	s.emitStatus(ctx, "Synthesizing worker outputs...")

	correlationID := assignMsg.CorrelationID
	if correlationID == "" {
		correlationID = assignMsg.Metadata["correlation_id"]
	}

	s.mu.Lock()
	s.scratchpad.WriteString(fmt.Sprintf("Synthesizing correlation=%s\n", correlationID))
	s.mu.Unlock()

	// Extract optional orchestrator plan passed in metadata.
	orchPlan := assignMsg.Metadata["orchestrator_plan"]

	// Parse worker output sections for traceability attribution.
	sections := parseWorkerSections(assignMsg.Content)

	// Parse any FILE_EDIT directives embedded in worker outputs.
	fileEdits := parseFileEdits(assignMsg.Content)

	systemPrompt := `You are a synthesis specialist. You will receive expert agent outputs,
an orchestrator plan, and worker context.
Your job is to:
1. Write a concise executive summary under the heading ## Summary
2. Synthesize all inputs into a unified response under ## Synthesized Response
3. Eliminate redundancy while preserving all important information
4. Present the final answer clearly and actionably
Do NOT include traceability, citations, or file-edit sections — these are appended automatically.`

	var userPromptBuilder strings.Builder
	if orchPlan != "" {
		userPromptBuilder.WriteString("=== ORCHESTRATOR PLAN ===\n")
		userPromptBuilder.WriteString(orchPlan)
		userPromptBuilder.WriteString("\n\n")
	}
	userPromptBuilder.WriteString("=== WORKER OUTPUTS ===\n")
	userPromptBuilder.WriteString(assignMsg.Content)

	messages := []llm.ChatMessage{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: userPromptBuilder.String()},
	}

	llmResult, err := s.llm.Complete(ctx, messages)
	if err != nil {
		s.setStatus(StatusError)
		s.emitStatus(ctx, fmt.Sprintf("Synthesis error: %v", err))
		llmResult = fallbackSynthesize(assignMsg.Content)
	}

	// Apply deterministic formatting: append traceability, citations, and file edits.
	result := buildTracedResponse(llmResult, sections, fileEdits, correlationID)

	// Post the final response so the TUI and orchestrator can display it.
	finalMsg := NewMessage(MsgTypeFinalResponse, s.id, "", result)
	finalMsg.Metadata["correlation_id"] = correlationID
	finalMsg.CorrelationID = assignMsg.CorrelationID
	s.bus.Publish(finalMsg)

	s.setStatus(StatusIdle)
	s.emitStatus(ctx, "Synthesis complete")
}

// parseWorkerSections extracts "=== Role Agent (workerID) ===" blocks from
// accumulated orchestrator content and returns them with role, ID, and output.
func parseWorkerSections(content string) []workerSection {
	var sections []workerSection
	lines := strings.Split(content, "\n")

	var current *workerSection
	var body strings.Builder

	for _, line := range lines {
		if strings.HasPrefix(line, "=== ") && strings.HasSuffix(line, " ===") {
			// Flush previous section.
			if current != nil {
				current.content = strings.TrimSpace(body.String())
				sections = append(sections, *current)
				body.Reset()
			}
			current = nil

			// Parse: "=== Researcher Agent (worker-1) ==="
			inner := strings.TrimPrefix(strings.TrimSuffix(line, " ==="), "=== ")
			// Skip iteration and scratchpad headers.
			if strings.HasPrefix(inner, "Iteration ") || strings.Contains(inner, "Scratchpad") {
				continue
			}
			if idx := strings.LastIndex(inner, " ("); idx > 0 {
				roleLabel := strings.TrimSuffix(inner[:idx], " Agent")
				workerID := strings.TrimSuffix(inner[idx+2:], ")")
				current = &workerSection{role: roleLabel, id: workerID}
			}
		} else if current != nil {
			body.WriteString(line)
			body.WriteString("\n")
		}
	}
	// Flush last section.
	if current != nil {
		current.content = strings.TrimSpace(body.String())
		sections = append(sections, *current)
	}
	return sections
}

// parseFileEdits extracts FILE_EDIT directives from content.
// Single-line format:  FILE_EDIT|<path>|<content>
// Multi-line format:   FILE_EDIT_START|<path> … FILE_EDIT_END
func parseFileEdits(content string) []fileEdit {
	var edits []fileEdit
	lines := strings.Split(content, "\n")

	var inEdit bool
	var editPath string
	var editBody strings.Builder

	for _, line := range lines {
		if inEdit {
			if strings.TrimSpace(line) == "FILE_EDIT_END" {
				edits = append(edits, fileEdit{path: editPath, content: strings.TrimRight(editBody.String(), "\n")})
				inEdit = false
				editPath = ""
				editBody.Reset()
			} else {
				editBody.WriteString(line)
				editBody.WriteString("\n")
			}
			continue
		}
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "FILE_EDIT_START|") {
			editPath = strings.TrimPrefix(trimmed, "FILE_EDIT_START|")
			inEdit = true
			editBody.Reset()
		} else if strings.HasPrefix(trimmed, "FILE_EDIT|") {
			parts := strings.SplitN(trimmed, "|", 3)
			if len(parts) == 3 {
				edits = append(edits, fileEdit{path: parts[1], content: parts[2]})
			}
		}
	}
	return edits
}

// buildTracedResponse wraps synthesized LLM output with deterministic
// traceability, citation, and file-edit sections.
func buildTracedResponse(llmOutput string, sections []workerSection, edits []fileEdit, correlationID string) string {
	var sb strings.Builder
	sb.WriteString(llmOutput)

	// Traceability table.
	sb.WriteString("\n\n## Traceability\n\n")
	sb.WriteString("| Agent | ID | Key Contribution |\n")
	sb.WriteString("|-------|-----|------------------|\n")
	for _, sec := range sections {
		summary := strings.ReplaceAll(sec.content, "\n", " ")
		summary = strings.ReplaceAll(summary, "|", "\\|")
		if len(summary) > 100 {
			summary = summary[:97] + "..."
		}
		sb.WriteString(fmt.Sprintf("| %s | %s | %s |\n", sec.role, sec.id, summary))
	}

	// Citations section.
	sb.WriteString("\n## Citations\n\n")
	if correlationID != "" {
		sb.WriteString(fmt.Sprintf("- Log reference: `logs/audit.jsonl` (correlation: `%s`)\n", correlationID))
		sb.WriteString(fmt.Sprintf("- Audit trail: all agent activity recorded under correlation ID `%s`\n", correlationID))
	} else {
		sb.WriteString("- Log reference: `logs/audit.jsonl`\n")
	}

	// File edits section.
	if len(edits) > 0 {
		sb.WriteString("\n## File Edits\n")
		for _, e := range edits {
			sb.WriteString(fmt.Sprintf("\n### %s\n\n```%s\n%s\n```\n", e.path, fileEditLang(e.path), e.content))
		}
	}

	return sb.String()
}

// fileEditLang returns a markdown code fence language identifier based on the
// file extension, falling back to an empty string for unknown types.
func fileEditLang(path string) string {
	if idx := strings.LastIndex(path, "."); idx >= 0 {
		switch strings.ToLower(path[idx:]) {
		case ".go":
			return "go"
		case ".py":
			return "python"
		case ".js":
			return "javascript"
		case ".ts":
			return "typescript"
		case ".sh", ".bash":
			return "bash"
		case ".json":
			return "json"
		case ".yaml", ".yml":
			return "yaml"
		case ".md":
			return "markdown"
		}
	}
	return ""
}

// fallbackSynthesize builds a simple combined response when the LLM fails.
func fallbackSynthesize(content string) string {
	var sb strings.Builder
	sb.WriteString("## Summary\n\nCombined agent outputs (LLM synthesis unavailable).\n\n")
	sb.WriteString("## Synthesized Response\n\n")
	sb.WriteString(content)
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
