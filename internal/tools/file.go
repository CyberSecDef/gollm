package tools

import (
	"context"
	"fmt"
	"os"
	"strings"
)

// ---- ReadFile ------------------------------------------------------------

type readFileTool struct{}

func NewReadFile() Tool { return &readFileTool{} }

func (t *readFileTool) Name() string { return "read_file" }
func (t *readFileTool) Description() string {
	return "Read the contents of a file. params: {\"path\":\"...\"}"
}

func (t *readFileTool) Execute(_ context.Context, params map[string]string) (string, error) {
	path, ok := params["path"]
	if !ok || path == "" {
		return "", fmt.Errorf("read_file: missing required param 'path'")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read_file: %w", err)
	}
	return string(data), nil
}

// ---- WriteFile -----------------------------------------------------------

type writeFileTool struct{}

func NewWriteFile() Tool { return &writeFileTool{} }

func (t *writeFileTool) Name() string { return "write_file" }
func (t *writeFileTool) Description() string {
	return "Write content to a file. params: {\"path\":\"...\",\"content\":\"...\"}"
}

func (t *writeFileTool) Execute(_ context.Context, params map[string]string) (string, error) {
	path, ok := params["path"]
	if !ok || path == "" {
		return "", fmt.Errorf("write_file: missing required param 'path'")
	}
	content := params["content"]
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return "", fmt.Errorf("write_file: %w", err)
	}
	return fmt.Sprintf("wrote %d bytes to %s", len(content), path), nil
}

// ---- PatchFile -----------------------------------------------------------

type patchFileTool struct{}

func NewPatchFile() Tool { return &patchFileTool{} }

func (t *patchFileTool) Name() string { return "patch_file" }
func (t *patchFileTool) Description() string {
	return "Apply a unified diff patch to a file. params: {\"path\":\"...\",\"patch\":\"...\"}"
}

// Execute applies a simplified line-by-line patch to path.
// The patch format is: lines starting with '-' are removed, lines starting with
// '+' are added, and context lines (space-prefixed or unchanged) are matched.
func (t *patchFileTool) Execute(_ context.Context, params map[string]string) (string, error) {
	path, ok := params["path"]
	if !ok || path == "" {
		return "", fmt.Errorf("patch_file: missing required param 'path'")
	}
	patch, ok := params["patch"]
	if !ok || patch == "" {
		return "", fmt.Errorf("patch_file: missing required param 'patch'")
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("patch_file read: %w", err)
	}

	result := applySimplePatch(string(data), patch)
	if err := os.WriteFile(path, []byte(result), 0o644); err != nil {
		return "", fmt.Errorf("patch_file write: %w", err)
	}
	return fmt.Sprintf("patched %s successfully", path), nil
}

// applySimplePatch applies a naive unified-diff-style patch.
func applySimplePatch(original, patch string) string {
	lines := strings.Split(original, "\n")
	patchLines := strings.Split(patch, "\n")

	var result []string
	lineIdx := 0

	for _, pl := range patchLines {
		if strings.HasPrefix(pl, "---") || strings.HasPrefix(pl, "+++") || strings.HasPrefix(pl, "@@") {
			continue
		}
		switch {
		case strings.HasPrefix(pl, "-"):
			// Remove matching line
			target := strings.TrimPrefix(pl, "-")
			if lineIdx < len(lines) && lines[lineIdx] == target {
				lineIdx++
			}
		case strings.HasPrefix(pl, "+"):
			result = append(result, strings.TrimPrefix(pl, "+"))
		default:
			// Context line: copy from original
			if lineIdx < len(lines) {
				result = append(result, lines[lineIdx])
				lineIdx++
			}
		}
	}
	// Append remaining original lines
	result = append(result, lines[lineIdx:]...)
	return strings.Join(result, "\n")
}
