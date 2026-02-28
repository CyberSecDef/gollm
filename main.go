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
	"github.com/cybersecdef/gollm/internal/tools"
	"github.com/cybersecdef/gollm/internal/tui"
)

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
