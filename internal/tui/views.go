package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/cybersecdef/gollm/internal/agents"
)

// renderConversation renders the conversation pane content.
func renderConversation(entries []ConversationEntry) string {
	if len(entries) == 0 {
		return helpStyle.Render("No messages yet. Type a message below and press Enter.")
	}

	var sb strings.Builder
	for _, entry := range entries {
		ts := entry.Timestamp.Format("15:04:05")
		switch entry.Role {
		case "user":
			sb.WriteString(userMsgStyle.Render(fmt.Sprintf("▶ You [%s]", ts)))
			sb.WriteString("\n")
			sb.WriteString(entry.Content)
			sb.WriteString("\n\n")
		case "assistant":
			sb.WriteString(assistantMsgStyle.Render(fmt.Sprintf("◀ Assistant [%s]", ts)))
			sb.WriteString("\n")
			sb.WriteString(entry.Content)
			sb.WriteString("\n\n")
		case "system":
			sb.WriteString(helpStyle.Render(fmt.Sprintf("  ℹ %s [%s]", entry.Content, ts)))
			sb.WriteString("\n\n")
		}
	}
	return sb.String()
}

// renderAgentStatus renders the agent status pane content.
func renderAgentStatus(agents map[string]AgentInfo) string {
	if len(agents) == 0 {
		return helpStyle.Render("No agents active.")
	}

	var sb strings.Builder
	for id, info := range agents {
		statusStr := renderStatus(info.Status)
		role := info.Role
		if role == "" {
			role = id
		}
		sb.WriteString(fmt.Sprintf("%s %s\n", statusStr, lipgloss.NewStyle().Bold(true).Render(role)))
		if info.LastMsg != "" {
			msg := info.LastMsg
			if len(msg) > 60 {
				msg = msg[:57] + "..."
			}
			sb.WriteString(eventContentStyle.Render("  "+msg) + "\n")
		}
		sb.WriteString("\n")
	}
	return sb.String()
}

func renderStatus(status agents.AgentStatus) string {
	switch status {
	case agents.StatusIdle:
		return statusIdleStyle.Render("○")
	case agents.StatusWorking:
		return statusWorkingStyle.Render("●")
	case agents.StatusWaiting:
		return statusWaitingStyle.Render("◐")
	case agents.StatusCompleted:
		return statusCompletedStyle.Render("✓")
	case agents.StatusError:
		return statusErrorStyle.Render("✗")
	default:
		return "?"
	}
}

// renderEventLog renders the event log pane content.
func renderEventLog(events []EventEntry) string {
	if len(events) == 0 {
		return helpStyle.Render("No events yet.")
	}

	var sb strings.Builder
	// Show most recent events first (reversed).
	for i := len(events) - 1; i >= 0; i-- {
		e := events[i]
		ts := e.Timestamp.Format("15:04:05")
		content := e.Content
		if len(content) > 80 {
			content = content[:77] + "..."
		}

		var line string
		switch e.Type {
		case string(agents.MsgTypeError):
			line = errorEventStyle.Render(fmt.Sprintf("[%s] ✗ %s", ts, content))
		case string(agents.MsgTypeFinalResponse):
			line = statusWorkingStyle.Render(fmt.Sprintf("[%s] ✓ final_response", ts))
		default:
			typeLabel := eventTypeStyle.Render(fmt.Sprintf("[%s]", e.Type))
			contentLabel := eventContentStyle.Render(content)
			line = fmt.Sprintf("%s %s %s", ts, typeLabel, contentLabel)
		}
		sb.WriteString(line + "\n")
	}
	return sb.String()
}

// renderPendingQuestion renders a question awaiting user response.
func renderPendingQuestion(q *agents.Message) string {
	if q == nil {
		return ""
	}
	return statusWaitingStyle.Render(fmt.Sprintf("❓ Agent question: %s", q.Content))
}

// renderSpinner returns an animated spinner frame.
func renderSpinner(tick int) string {
	frames := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
	return spinnerStyle.Render(frames[tick%len(frames)])
}

// ConversationEntry is a single message in the conversation history.
type ConversationEntry struct {
	Role      string // "user", "assistant", "system"
	Content   string
	Timestamp time.Time
}

// AgentInfo summarizes a single agent for the status pane.
type AgentInfo struct {
	ID      string
	Role    string
	Status  agents.AgentStatus
	LastMsg string
}

// EventEntry is a display-ready event for the event log pane.
type EventEntry struct {
	Type      string
	From      string
	Content   string
	Timestamp time.Time
}
