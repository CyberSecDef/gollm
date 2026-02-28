package tools

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"time"
)

type execCommandTool struct{}

func NewExecCommand() Tool { return &execCommandTool{} }

func (t *execCommandTool) Name() string { return "exec_command" }
func (t *execCommandTool) Description() string {
	return "Execute a shell command and return combined stdout+stderr. params: {\"command\":\"...\"}"
}

func (t *execCommandTool) Execute(ctx context.Context, params map[string]string) (string, error) {
	command, ok := params["command"]
	if !ok || command == "" {
		return "", fmt.Errorf("exec_command: missing required param 'command'")
	}

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf

	if err := cmd.Run(); err != nil {
		// Include output even on non-zero exit so callers can inspect errors.
		return buf.String(), fmt.Errorf("exec_command: %w", err)
	}
	return buf.String(), nil
}
