package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/cybersecdef/gollm/internal/agents"
	"github.com/cybersecdef/gollm/internal/bus"
	"github.com/cybersecdef/gollm/internal/llm"
	"github.com/cybersecdef/gollm/internal/msgs"
	"github.com/cybersecdef/gollm/internal/tools"
	"github.com/cybersecdef/gollm/internal/tui"
)

// busEventEmitter adapts the message bus to the tools.EventEmitter interface
// so that the tool registry emits audit events on the bus before and after
// every tool execution.
type busEventEmitter struct {
	b    *bus.Bus
	from string
}

func (e *busEventEmitter) EmitToolCall(toolName string, params map[string]string) {
	msg := agents.NewMessage(msgs.MsgTypeToolCall, e.from, "", fmt.Sprintf("pre-exec: %s", toolName))
	msg.Metadata["tool"] = toolName
	msg.Metadata["phase"] = "before"
	e.b.Publish(msg)
}

func (e *busEventEmitter) EmitToolResult(toolName string, result string, err error) {
	content := result
	if err != nil {
		content = fmt.Sprintf("error: %v", err)
	}
	msg := agents.NewMessage(msgs.MsgTypeToolResult, e.from, "", content)
	msg.Metadata["tool"] = toolName
	msg.Metadata["phase"] = "after"
	e.b.Publish(msg)
}

func main() {
	// Structured logger (logs go to a file so they don't interfere with TUI).
	if err := os.MkdirAll("logs", 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "failed to create logs dir: %v\n", err)
		os.Exit(1)
	}
	logFile, err := os.OpenFile("logs/gollm.log", os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to open log file: %v\n", err)
		os.Exit(1)
	}
	defer logFile.Close()

	logger := slog.New(slog.NewJSONHandler(logFile, &slog.HandlerOptions{Level: slog.LevelDebug}))
	slog.SetDefault(logger)

	slog.Info("gollm starting")

	// --- Message Bus ---
	msgBus := bus.New()

	// --- Tool Registry ---
	registry := tools.NewRegistry()
	registry.Register(tools.NewReadFile())
	registry.Register(tools.NewWriteFile())
	registry.Register(tools.NewPatchFile())
	registry.Register(tools.NewFetchURL())
	registry.Register(tools.NewExecCommand())
	registry.Register(tools.NewResearch())

	// Wire the bus as an audit event emitter so the registry publishes
	// tool_call / tool_result events before and after every tool execution.
	registry.SetEventEmitter(&busEventEmitter{b: msgBus, from: "tool_registry"})

	// --- LLM Client ---
	llmClient := llm.NewOpenAIClient()

	// --- Agents ---
	auditor := agents.NewAuditor(msgBus)
	orchestrator := agents.NewOrchestrator(msgBus, llmClient, registry)
	synthesizer := agents.NewSynthesizer(msgBus, llmClient)

	// --- Shared context for all agents ---
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start agents
	for _, ag := range []interface {
		Start(ctx context.Context) error
		ID() string
	}{auditor, orchestrator, synthesizer} {
		if err := ag.Start(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "failed to start agent %s: %v\n", ag.ID(), err)
			os.Exit(1)
		}
	}

	slog.Info("all agents started")

	// --- TUI ---
	model := tui.NewModel(msgBus, orchestrator, auditor)

	program := tea.NewProgram(
		model,
		tea.WithAltScreen(),
		tea.WithMouseCellMotion(),
	)

	slog.Info("starting TUI")
	if _, err := program.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "TUI error: %v\n", err)
		os.Exit(1)
	}

	// --- Graceful shutdown ---
	cancel()

	// Signal agents to stop.
	shutdownMsg := agents.NewMessage(agents.MsgTypeShutdown, "main", "", "shutdown")
	msgBus.Publish(shutdownMsg)

	synthesizer.Stop()
	orchestrator.Stop()
	auditor.Stop()

	slog.Info("gollm shutdown complete")
}
