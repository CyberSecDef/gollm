package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/cybersecdef/gollm/internal/agents"
	"github.com/cybersecdef/gollm/internal/bus"
)

// focusedPane tracks which pane has keyboard focus.
type focusedPane int

const (
	paneConversation focusedPane = iota
	paneAgentStatus
	paneEventLog
	paneInput
)

// tickMsg is sent on each spinner tick.
type tickMsg time.Time

// Model is the top-level bubbletea model for the TUI.
type Model struct {
	// Viewports for scrollable panes
	conversationViewport viewport.Model
	agentStatusViewport  viewport.Model
	eventLogViewport     viewport.Model
	inputTextarea        textarea.Model

	// Content state
	conversation  []ConversationEntry
	agentStatuses map[string]AgentInfo
	events        []EventEntry

	// Window dimensions
	width, height int

	// Agent system
	msgBus       *bus.Bus
	orchestrator interface {
		Send(msg agents.Message)
		GetWorkerStatuses() map[string]agents.AgentStatus
		Status() agents.AgentStatus
	}
	auditor interface {
		GetEvents() []agents.Message
	}

	// Channel for agent messages routed to TUI
	msgChan <-chan agents.Message

	// If an agent posted a question, hold it here until user answers
	pendingQuestion *agents.Message

	// UI state
	focused    focusedPane
	processing bool
	spinTick   int
	ready      bool
}

// NewModel creates the initial TUI model.
func NewModel(
	b *bus.Bus,
	orch interface {
		Send(msg agents.Message)
		GetWorkerStatuses() map[string]agents.AgentStatus
		Status() agents.AgentStatus
	},
	aud interface {
		GetEvents() []agents.Message
	},
) Model {
	ta := textarea.New()
	ta.Placeholder = "Ask anything... (Enter to send, Ctrl+C to quit)"
	ta.Focus()
	ta.SetWidth(80)
	ta.SetHeight(3)
	ta.ShowLineNumbers = false
	ta.CharLimit = 4000

	m := Model{
		conversationViewport: viewport.New(0, 0),
		agentStatusViewport:  viewport.New(0, 0),
		eventLogViewport:     viewport.New(0, 0),
		inputTextarea:        ta,
		agentStatuses:        make(map[string]AgentInfo),
		msgBus:               b,
		orchestrator:         orch,
		auditor:              aud,
		msgChan:              b.Subscribe("tui"),
		focused:              paneInput,
	}

	// Seed conversation with a welcome message
	m.conversation = append(m.conversation, ConversationEntry{
		Role:      "system",
		Content:   "Welcome to gollm – multi-agent AI terminal. Type your request below.",
		Timestamp: time.Now(),
	})

	// Register core agents in status pane
	m.agentStatuses["orchestrator"] = AgentInfo{ID: "orchestrator", Role: "Orchestrator", Status: agents.StatusIdle}
	m.agentStatuses["auditor"] = AgentInfo{ID: "auditor", Role: "Auditor", Status: agents.StatusWorking}
	m.agentStatuses["synthesizer"] = AgentInfo{ID: "synthesizer", Role: "Synthesizer", Status: agents.StatusIdle}

	return m
}

// Init starts the spinner ticker and begins listening for agent messages.
func (m Model) Init() tea.Cmd {
	return tea.Batch(
		textarea.Blink,
		waitForAgentMsg(m.msgChan),
		tickCmd(),
	)
}

// waitForAgentMsg returns a Cmd that blocks until an agent message arrives.
func waitForAgentMsg(ch <-chan agents.Message) tea.Cmd {
	return func() tea.Msg {
		return <-ch
	}
}

func tickCmd() tea.Cmd {
	return tea.Tick(100*time.Millisecond, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}

// Update handles all incoming messages.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.ready = true
		m.relayout()

	case tickMsg:
		m.spinTick++
		m.refreshAgentStatuses()
		cmds = append(cmds, tickCmd())

	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c":
			return m, tea.Quit
		case "tab":
			m.focused = (m.focused + 1) % 4
			m.syncFocus()
		case "shift+tab":
			m.focused = (m.focused + 3) % 4
			m.syncFocus()
		case "enter":
			if m.focused == paneInput {
				return m.handleSubmit()
			}
		default:
			if m.focused == paneInput {
				var cmd tea.Cmd
				m.inputTextarea, cmd = m.inputTextarea.Update(msg)
				cmds = append(cmds, cmd)
			} else {
				m = m.handlePaneNav(msg)
			}
		}

	case agents.Message:
		m = m.handleAgentMessage(msg)
		// Keep listening
		cmds = append(cmds, waitForAgentMsg(m.msgChan))
	}

	// Update viewports
	var c tea.Cmd
	m.conversationViewport, c = m.conversationViewport.Update(msg)
	cmds = append(cmds, c)
	m.agentStatusViewport, c = m.agentStatusViewport.Update(msg)
	cmds = append(cmds, c)
	m.eventLogViewport, c = m.eventLogViewport.Update(msg)
	cmds = append(cmds, c)

	return m, tea.Batch(cmds...)
}

func (m Model) handleSubmit() (Model, tea.Cmd) {
	input := strings.TrimSpace(m.inputTextarea.Value())
	if input == "" {
		return m, nil
	}

	m.inputTextarea.Reset()

	if m.pendingQuestion != nil {
		// Send answer back to orchestrator
		answerMsg := agents.NewMessage(agents.MsgTypeAnswer, "user", "orchestrator", input)
		m.msgBus.Publish(answerMsg)
		m.pendingQuestion = nil
		m.conversation = append(m.conversation, ConversationEntry{
			Role:      "user",
			Content:   input,
			Timestamp: time.Now(),
		})
	} else {
		// Normal user input
		m.conversation = append(m.conversation, ConversationEntry{
			Role:      "user",
			Content:   input,
			Timestamp: time.Now(),
		})
		m.processing = true

		userMsg := agents.NewMessage(agents.MsgTypeUserInput, "user", "orchestrator", input)
		m.msgBus.Publish(userMsg)
	}

	m.updateConversationViewport()
	return m, nil
}

func (m Model) handleAgentMessage(msg agents.Message) Model {
	// Update agent status map
	if msg.From != "" {
		info := m.agentStatuses[msg.From]
		info.ID = msg.From
		if info.Role == "" {
			info.Role = msg.From
		}
		if len(msg.Content) > 0 {
			info.LastMsg = msg.Content
			if len(info.LastMsg) > 80 {
				info.LastMsg = info.LastMsg[:77] + "..."
			}
		}
		// Infer status from message type
		switch msg.Type {
		case agents.MsgTypeStatusUpdate:
			info.Status = agents.StatusWorking
		case agents.MsgTypeTaskResult:
			info.Status = agents.StatusCompleted
		case agents.MsgTypeError:
			info.Status = agents.StatusError
		case agents.MsgTypeFinalResponse:
			info.Status = agents.StatusCompleted
		}
		m.agentStatuses[msg.From] = info
	}

	// Role metadata override
	if role, ok := msg.Metadata["role"]; ok && msg.From != "" {
		info := m.agentStatuses[msg.From]
		info.Role = role
		m.agentStatuses[msg.From] = info
	}

	// Add to event log
	entry := EventEntry{
		Type:      string(msg.Type),
		From:      msg.From,
		Content:   msg.Content,
		Timestamp: msg.Timestamp,
	}
	m.events = append(m.events, entry)
	if len(m.events) > 200 {
		m.events = m.events[1:]
	}

	// Handle specific message types
	switch msg.Type {
	case agents.MsgTypeFinalResponse:
		m.conversation = append(m.conversation, ConversationEntry{
			Role:      "assistant",
			Content:   msg.Content,
			Timestamp: msg.Timestamp,
		})
		m.processing = false
		m.agentStatuses["orchestrator"] = AgentInfo{
			ID:     "orchestrator",
			Role:   "Orchestrator",
			Status: agents.StatusIdle,
		}

	case agents.MsgTypeQuestion:
		m.pendingQuestion = &msg
		m.conversation = append(m.conversation, ConversationEntry{
			Role:      "system",
			Content:   "❓ " + msg.Content,
			Timestamp: msg.Timestamp,
		})

	case agents.MsgTypeError:
		m.conversation = append(m.conversation, ConversationEntry{
			Role:      "system",
			Content:   "⚠ Error: " + msg.Content,
			Timestamp: msg.Timestamp,
		})
		if msg.From == "orchestrator" {
			m.processing = false
		}
	}

	m.updateConversationViewport()
	m.updateEventLogViewport()
	m.updateAgentStatusViewport()
	return m
}

func (m *Model) handlePaneNav(msg tea.KeyMsg) Model {
	switch m.focused {
	case paneConversation:
		switch msg.String() {
		case "up", "k":
			m.conversationViewport.LineUp(1)
		case "down", "j":
			m.conversationViewport.LineDown(1)
		}
	case paneAgentStatus:
		switch msg.String() {
		case "up", "k":
			m.agentStatusViewport.LineUp(1)
		case "down", "j":
			m.agentStatusViewport.LineDown(1)
		}
	case paneEventLog:
		switch msg.String() {
		case "up", "k":
			m.eventLogViewport.LineUp(1)
		case "down", "j":
			m.eventLogViewport.LineDown(1)
		}
	}
	return *m
}

func (m *Model) syncFocus() {
	if m.focused == paneInput {
		m.inputTextarea.Focus()
	} else {
		m.inputTextarea.Blur()
	}
}

func (m *Model) refreshAgentStatuses() {
	statuses := m.orchestrator.GetWorkerStatuses()
	for id, status := range statuses {
		info := m.agentStatuses[id]
		info.ID = id
		info.Status = status
		if info.Role == "" {
			info.Role = id
		}
		m.agentStatuses[id] = info
	}
	// Update orchestrator's own status
	orchInfo := m.agentStatuses["orchestrator"]
	orchInfo.Status = m.orchestrator.Status()
	m.agentStatuses["orchestrator"] = orchInfo
	m.updateAgentStatusViewport()
}

// relayout recalculates viewport dimensions based on terminal size.
func (m *Model) relayout() {
	if m.width == 0 || m.height == 0 {
		return
	}

	// Reserve space: 2 border lines + 1 title per pane, plus input bar + help
	inputHeight := 5
	helpHeight := 1
	topSectionHeight := m.height - inputHeight - helpHeight - 2

	// Three top panes side by side
	convWidth := m.width * 50 / 100
	rightWidth := m.width - convWidth
	statusWidth := rightWidth / 2
	evtWidth := rightWidth - statusWidth

	// Inner heights = total - 2 (borders) - 1 (title)
	innerH := topSectionHeight - 3
	if innerH < 2 {
		innerH = 2
	}

	m.conversationViewport.Width = convWidth - 4
	m.conversationViewport.Height = innerH

	m.agentStatusViewport.Width = statusWidth - 4
	m.agentStatusViewport.Height = innerH

	m.eventLogViewport.Width = evtWidth - 4
	m.eventLogViewport.Height = innerH

	m.inputTextarea.SetWidth(m.width - 4)

	m.updateConversationViewport()
	m.updateAgentStatusViewport()
	m.updateEventLogViewport()
}

func (m *Model) updateConversationViewport() {
	content := renderConversation(m.conversation)
	m.conversationViewport.SetContent(content)
	m.conversationViewport.GotoBottom()
}

func (m *Model) updateAgentStatusViewport() {
	content := renderAgentStatus(m.agentStatuses)
	m.agentStatusViewport.SetContent(content)
}

func (m *Model) updateEventLogViewport() {
	content := renderEventLog(m.events)
	m.eventLogViewport.SetContent(content)
}

// View renders the entire TUI.
func (m Model) View() string {
	if !m.ready {
		return "\n  Initializing gollm...\n"
	}

	convWidth := m.width * 50 / 100
	rightWidth := m.width - convWidth
	statusWidth := rightWidth / 2
	evtWidth := rightWidth - statusWidth

	inputHeight := 5
	helpHeight := 1
	topH := m.height - inputHeight - helpHeight - 2

	// Determine focused pane border styles
	convBorder := paneStyle
	statusBorder := paneStyle
	evtBorder := paneStyle
	inputBorder := inputBarStyle

	switch m.focused {
	case paneConversation:
		convBorder = focusedPaneStyle
	case paneAgentStatus:
		statusBorder = focusedPaneStyle
	case paneEventLog:
		evtBorder = focusedPaneStyle
	case paneInput:
		inputBorder = inputBarStyle.BorderForeground(colorPrimary)
	}

	// Spinner or idle indicator
	processingIndicator := ""
	if m.processing {
		processingIndicator = " " + renderSpinner(m.spinTick) + " Processing..."
	}

	// Pending question banner
	questionBanner := ""
	if m.pendingQuestion != nil {
		questionBanner = "\n" + renderPendingQuestion(m.pendingQuestion)
	}

	// Build pane contents with titles
	convTitle := titleStyle.Render("💬 Conversation")
	if m.processing {
		convTitle += processingIndicator
	}
	convContent := convTitle + "\n" + m.conversationViewport.View()
	convPane := convBorder.
		Width(convWidth - 2).
		Height(topH - 2).
		Render(convContent)

	statusTitle := titleStyle.Render("🤖 Agents")
	statusContent := statusTitle + "\n" + m.agentStatusViewport.View()
	statusPane := statusBorder.
		Width(statusWidth - 2).
		Height(topH - 2).
		Render(statusContent)

	evtTitle := titleStyle.Render("📋 Events")
	evtContent := evtTitle + "\n" + m.eventLogViewport.View()
	evtPane := evtBorder.
		Width(evtWidth - 2).
		Height(topH - 2).
		Render(evtContent)

	// Layout top row
	topRow := lipgloss.JoinHorizontal(lipgloss.Top, convPane, statusPane, evtPane)

	// Input section
	inputContent := m.inputTextarea.View()
	if questionBanner != "" {
		inputContent = questionBanner + "\n" + inputContent
	}
	inputSection := inputBorder.Width(m.width - 4).Render(inputContent)

	// Help line
	help := helpStyle.Render("Tab: focus  •  Enter: send  •  ↑↓/jk: scroll  •  Ctrl+C: quit")
	helpLine := fmt.Sprintf("  %s", help)

	return lipgloss.JoinVertical(lipgloss.Left, topRow, inputSection, helpLine)
}
