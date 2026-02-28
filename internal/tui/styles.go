package tui

import (
	"github.com/charmbracelet/lipgloss"
)

var (
	// Colors
	colorPrimary   = lipgloss.Color("#7C3AED") // purple
	colorSecondary = lipgloss.Color("#2563EB") // blue
	colorSuccess   = lipgloss.Color("#16A34A") // green
	colorWarning   = lipgloss.Color("#D97706") // amber
	colorError     = lipgloss.Color("#DC2626") // red
	colorMuted     = lipgloss.Color("#6B7280") // gray
	colorBg        = lipgloss.Color("#1F2937") // dark bg
	colorText      = lipgloss.Color("#F9FAFB") // near-white
	colorBorder    = lipgloss.Color("#374151") // gray border
	colorHighlight = lipgloss.Color("#8B5CF6") // lighter purple

	// Base styles
	baseStyle = lipgloss.NewStyle().
			Foreground(colorText).
			Background(colorBg)

	// Pane border style
	paneStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(colorBorder).
			Padding(0, 1)

	focusedPaneStyle = lipgloss.NewStyle().
				Border(lipgloss.RoundedBorder()).
				BorderForeground(colorPrimary).
				Padding(0, 1)

	// Title bar for each pane
	titleStyle = lipgloss.NewStyle().
			Foreground(colorPrimary).
			Bold(true).
			MarginBottom(1)

	// Conversation styles
	userMsgStyle = lipgloss.NewStyle().
			Foreground(colorSecondary).
			Bold(true)

	assistantMsgStyle = lipgloss.NewStyle().
				Foreground(colorText)

	// Agent status styles
	statusIdleStyle = lipgloss.NewStyle().
			Foreground(colorMuted)

	statusWorkingStyle = lipgloss.NewStyle().
				Foreground(colorSuccess).
				Bold(true)

	statusWaitingStyle = lipgloss.NewStyle().
				Foreground(colorWarning)

	statusErrorStyle = lipgloss.NewStyle().
				Foreground(colorError).
				Bold(true)

	statusCompletedStyle = lipgloss.NewStyle().
				Foreground(colorMuted)

	// Event log styles
	eventTypeStyle = lipgloss.NewStyle().
			Foreground(colorHighlight).
			Bold(true)

	eventContentStyle = lipgloss.NewStyle().
				Foreground(colorMuted)

	errorEventStyle = lipgloss.NewStyle().
			Foreground(colorError)

	// Input bar
	inputBarStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(colorSecondary).
			Padding(0, 1)

	// Help text
	helpStyle = lipgloss.NewStyle().
			Foreground(colorMuted).
			Italic(true)

	// Spinner / waiting indicator
	spinnerStyle = lipgloss.NewStyle().
			Foreground(colorPrimary).
			Bold(true)
)
