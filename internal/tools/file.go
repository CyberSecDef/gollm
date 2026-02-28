package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// FileConfig holds optional constraints applied by all file-manipulation tools.
type FileConfig struct {
	// AllowedDirs restricts file access to paths within these directories.
	// An empty slice means no directory restrictions are enforced.
	AllowedDirs []string
	// MaxReadBytes caps the size of files that read_file will load.
	// Zero means no limit.
	MaxReadBytes int64
	// MaxWriteBytes caps the size of content that write_file will save.
	// Zero means no limit.
	MaxWriteBytes int64
	// CallTimeout is an additional per-call deadline applied on top of any
	// caller-supplied context deadline. Zero means no additional timeout.
	CallTimeout time.Duration
}

// FileSystem abstracts file-system operations so tools can be tested without
// touching the real file system.
type FileSystem interface {
	ReadFile(name string) ([]byte, error)
	WriteFile(name string, data []byte, perm os.FileMode) error
	Stat(name string) (os.FileInfo, error)
}

// osFileSystem is the production FileSystem backed by the real OS.
type osFileSystem struct{}

func (osFileSystem) ReadFile(name string) ([]byte, error)                    { return os.ReadFile(name) }
func (osFileSystem) WriteFile(name string, d []byte, p os.FileMode) error    { return os.WriteFile(name, d, p) }
func (osFileSystem) Stat(name string) (os.FileInfo, error)                   { return os.Stat(name) }

// safePath validates and cleans a file path, enforcing the optional allowlist.
// It always applies filepath.Clean; when AllowedDirs is non-empty the cleaned
// path must fall within one of the listed directories.
func safePath(raw string, allowedDirs []string) (string, error) {
	if raw == "" {
		return "", fmt.Errorf("empty path")
	}
	clean := filepath.Clean(raw)
	if len(allowedDirs) == 0 {
		return clean, nil
	}
	for _, dir := range allowedDirs {
		allowed := filepath.Clean(dir)
		rel, err := filepath.Rel(allowed, clean)
		// rel must be relative (not absolute) and must not start with ".."
		// to be safely contained within the allowed directory.
		if err == nil && !filepath.IsAbs(rel) && !strings.HasPrefix(rel, "..") {
			return clean, nil
		}
	}
	return "", fmt.Errorf("path %q is not within any allowed directory", raw)
}

// withCallTimeout wraps ctx with the config-level timeout when set.
func withCallTimeout(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout > 0 {
		return context.WithTimeout(ctx, timeout)
	}
	return context.WithCancel(ctx)
}

// ---- ReadFile ------------------------------------------------------------

type readFileTool struct {
	cfg FileConfig
	fs  FileSystem
}

// NewReadFile returns a ReadFile tool with default (unrestricted) configuration.
func NewReadFile() Tool { return NewReadFileWithConfig(FileConfig{}, osFileSystem{}) }

// NewReadFileWithConfig returns a ReadFile tool with the given configuration and FileSystem.
func NewReadFileWithConfig(cfg FileConfig, fs FileSystem) Tool {
	return &readFileTool{cfg: cfg, fs: fs}
}

func (t *readFileTool) Name() string { return "read_file" }
func (t *readFileTool) Description() string {
	return "Read the contents of a file. params: {\"path\":\"...\"}"
}

func (t *readFileTool) Execute(ctx context.Context, params map[string]string) (string, error) {
	rawPath, ok := params["path"]
	if !ok || rawPath == "" {
		return "", fmt.Errorf("read_file: missing required param 'path'")
	}
	path, err := safePath(rawPath, t.cfg.AllowedDirs)
	if err != nil {
		return "", fmt.Errorf("read_file: %w", err)
	}

	ctx, cancel := withCallTimeout(ctx, t.cfg.CallTimeout)
	defer cancel()

	if ctx.Err() != nil {
		return "", ctx.Err()
	}

	if t.cfg.MaxReadBytes > 0 {
		info, statErr := t.fs.Stat(path)
		if statErr != nil {
			return "", fmt.Errorf("read_file stat: %w", statErr)
		}
		if info.Size() > t.cfg.MaxReadBytes {
			return "", fmt.Errorf("read_file: file size %d exceeds limit %d", info.Size(), t.cfg.MaxReadBytes)
		}
	}

	data, err := t.fs.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read_file: %w", err)
	}
	return string(data), nil
}

// ---- WriteFile -----------------------------------------------------------

type writeFileTool struct {
	cfg FileConfig
	fs  FileSystem
}

// NewWriteFile returns a WriteFile tool with default (unrestricted) configuration.
func NewWriteFile() Tool { return NewWriteFileWithConfig(FileConfig{}, osFileSystem{}) }

// NewWriteFileWithConfig returns a WriteFile tool with the given configuration and FileSystem.
func NewWriteFileWithConfig(cfg FileConfig, fs FileSystem) Tool {
	return &writeFileTool{cfg: cfg, fs: fs}
}

func (t *writeFileTool) Name() string { return "write_file" }
func (t *writeFileTool) Description() string {
	return "Write content to a file. params: {\"path\":\"...\",\"content\":\"...\"}"
}

func (t *writeFileTool) Execute(ctx context.Context, params map[string]string) (string, error) {
	rawPath, ok := params["path"]
	if !ok || rawPath == "" {
		return "", fmt.Errorf("write_file: missing required param 'path'")
	}
	path, err := safePath(rawPath, t.cfg.AllowedDirs)
	if err != nil {
		return "", fmt.Errorf("write_file: %w", err)
	}

	ctx, cancel := withCallTimeout(ctx, t.cfg.CallTimeout)
	defer cancel()

	if ctx.Err() != nil {
		return "", ctx.Err()
	}

	content := params["content"]
	if t.cfg.MaxWriteBytes > 0 && int64(len(content)) > t.cfg.MaxWriteBytes {
		return "", fmt.Errorf("write_file: content size %d exceeds limit %d", len(content), t.cfg.MaxWriteBytes)
	}
	if err := t.fs.WriteFile(path, []byte(content), 0o644); err != nil {
		return "", fmt.Errorf("write_file: %w", err)
	}
	return fmt.Sprintf("wrote %d bytes to %s", len(content), path), nil
}

// ---- PatchFile -----------------------------------------------------------

type patchFileTool struct {
	cfg FileConfig
	fs  FileSystem
}

// NewPatchFile returns a PatchFile tool with default (unrestricted) configuration.
func NewPatchFile() Tool { return NewPatchFileWithConfig(FileConfig{}, osFileSystem{}) }

// NewPatchFileWithConfig returns a PatchFile tool with the given configuration and FileSystem.
func NewPatchFileWithConfig(cfg FileConfig, fs FileSystem) Tool {
	return &patchFileTool{cfg: cfg, fs: fs}
}

func (t *patchFileTool) Name() string { return "patch_file" }
func (t *patchFileTool) Description() string {
	return "Apply a unified diff patch or replace a line range in a file. " +
		"Diff params: {\"path\":\"...\",\"patch\":\"...\"}. " +
		"Range params: {\"path\":\"...\",\"start_line\":\"1\",\"end_line\":\"3\",\"replacement\":\"...\"}"
}

func (t *patchFileTool) Execute(ctx context.Context, params map[string]string) (string, error) {
	rawPath, ok := params["path"]
	if !ok || rawPath == "" {
		return "", fmt.Errorf("patch_file: missing required param 'path'")
	}
	path, err := safePath(rawPath, t.cfg.AllowedDirs)
	if err != nil {
		return "", fmt.Errorf("patch_file: %w", err)
	}

	ctx, cancel := withCallTimeout(ctx, t.cfg.CallTimeout)
	defer cancel()

	if ctx.Err() != nil {
		return "", ctx.Err()
	}

	data, err := t.fs.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("patch_file read: %w", err)
	}

	var result string

	// Range-replace mode: start_line and end_line are provided.
	if startStr, hasStart := params["start_line"]; hasStart {
		endStr, hasEnd := params["end_line"]
		if !hasEnd {
			return "", fmt.Errorf("patch_file: 'end_line' required when 'start_line' is set")
		}
		replacement := params["replacement"]

		startLine, err := strconv.Atoi(startStr)
		if err != nil || startLine < 1 {
			return "", fmt.Errorf("patch_file: invalid start_line %q", startStr)
		}
		endLine, err := strconv.Atoi(endStr)
		if err != nil || endLine < startLine {
			return "", fmt.Errorf("patch_file: invalid end_line %q", endStr)
		}

		result = applyRangeReplace(string(data), startLine, endLine, replacement)
	} else {
		// Unified-diff mode.
		patch, ok := params["patch"]
		if !ok || patch == "" {
			return "", fmt.Errorf("patch_file: missing required param 'patch' (or use start_line/end_line/replacement)")
		}
		result = applySimplePatch(string(data), patch)
	}

	if t.cfg.MaxWriteBytes > 0 && int64(len(result)) > t.cfg.MaxWriteBytes {
		return "", fmt.Errorf("patch_file: result size %d exceeds limit %d", len(result), t.cfg.MaxWriteBytes)
	}

	if err := t.fs.WriteFile(path, []byte(result), 0o644); err != nil {
		return "", fmt.Errorf("patch_file write: %w", err)
	}
	return fmt.Sprintf("patched %s successfully", path), nil
}

// applyRangeReplace replaces lines [startLine, endLine] (1-indexed, inclusive)
// with the provided replacement string.
func applyRangeReplace(original string, startLine, endLine int, replacement string) string {
	lines := strings.Split(original, "\n")
	// Clamp to valid range.
	if startLine > len(lines) {
		startLine = len(lines)
	}
	if endLine > len(lines) {
		endLine = len(lines)
	}
	// Convert to 0-indexed.
	start := startLine - 1
	end := endLine // exclusive upper bound

	var out []string
	out = append(out, lines[:start]...)
	if replacement != "" {
		out = append(out, strings.Split(replacement, "\n")...)
	}
	if end < len(lines) {
		out = append(out, lines[end:]...)
	}
	return strings.Join(out, "\n")
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
