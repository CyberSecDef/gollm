package tools

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

const defaultExecTimeout = 30 * time.Second

// ExecConfig holds optional constraints for the exec_command tool.
type ExecConfig struct {
	// AllowedCommands is a set of permitted command prefixes (e.g. "go", "ls").
	// When non-empty the command string must begin with one of these tokens.
	// An empty slice means no command restrictions are enforced.
	AllowedCommands []string
	// CallTimeout overrides the default 30-second per-call timeout.
	// Zero falls back to the default.
	CallTimeout time.Duration
}

type execCommandTool struct {
	cfg ExecConfig
}

// NewExecCommand returns an ExecCommand tool with default (unrestricted) configuration.
func NewExecCommand() Tool { return NewExecCommandWithConfig(ExecConfig{}) }

// NewExecCommandWithConfig returns an ExecCommand tool with the given configuration.
func NewExecCommandWithConfig(cfg ExecConfig) Tool {
	return &execCommandTool{cfg: cfg}
}

func (t *execCommandTool) Name() string { return "exec_command" }
func (t *execCommandTool) Description() string {
	return "Execute a shell command and return stdout, stderr, and exit code. params: {\"command\":\"...\"}"
}

func (t *execCommandTool) Execute(ctx context.Context, params map[string]string) (string, error) {
	command, ok := params["command"]
	if !ok || command == "" {
		return "", fmt.Errorf("exec_command: missing required param 'command'")
	}

	// Enforce command allowlist.
	// The first whitespace-delimited token of the command must appear in the
	// allowlist.  Note: the full command string is still passed to the shell,
	// so allowlist entries should be chosen with the understanding that shell
	// metacharacters in the argument portion are not further restricted.
	if len(t.cfg.AllowedCommands) > 0 {
		allowed := false
		cmdToken := strings.Fields(command)
		if len(cmdToken) > 0 {
			for _, a := range t.cfg.AllowedCommands {
				if cmdToken[0] == a {
					allowed = true
					break
				}
			}
		}
		if !allowed {
			return "", fmt.Errorf("exec_command: command %q is not in the allowlist", command)
		}
	}

	timeout := t.cfg.CallTimeout
	if timeout <= 0 {
		timeout = defaultExecTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.CommandContext(ctx, "cmd", "/c", command)
	} else {
		cmd = exec.CommandContext(ctx, "sh", "-c", command)
	}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	runErr := cmd.Run()

	exitCode := 0
	if runErr != nil {
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else {
			// Non-exit error (e.g. context timeout, exec not found).
			// No meaningful exit code is available; omit it from the result.
			return fmt.Sprintf("stdout:\n%s\nstderr:\n%s",
				stdout.String(), stderr.String()), fmt.Errorf("exec_command: %w", runErr)
		}
	}

	result := fmt.Sprintf("stdout:\n%s\nstderr:\n%s\nexit_code: %d",
		stdout.String(), stderr.String(), exitCode)
	if exitCode != 0 {
		return result, fmt.Errorf("exec_command: exited with code %d", exitCode)
	}
	return result, nil
}
