package tools_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cybersecdef/gollm/internal/tools"
)

// ---- mock FileSystem --------------------------------------------------------

type mockFS struct {
	files map[string][]byte
	// statErr forces Stat to return an error for a specific file.
	statErr map[string]error
	// writeErr forces WriteFile to return an error for a specific file.
	writeErr map[string]error
}

func newMockFS() *mockFS {
	return &mockFS{
		files:    make(map[string][]byte),
		statErr:  make(map[string]error),
		writeErr: make(map[string]error),
	}
}

func (m *mockFS) ReadFile(name string) ([]byte, error) {
	data, ok := m.files[name]
	if !ok {
		return nil, &os.PathError{Op: "open", Path: name, Err: os.ErrNotExist}
	}
	return append([]byte(nil), data...), nil
}

func (m *mockFS) WriteFile(name string, data []byte, _ os.FileMode) error {
	if err, ok := m.writeErr[name]; ok {
		return err
	}
	m.files[name] = append([]byte(nil), data...)
	return nil
}

func (m *mockFS) Stat(name string) (os.FileInfo, error) {
	if err, ok := m.statErr[name]; ok {
		return nil, err
	}
	data, ok := m.files[name]
	if !ok {
		return nil, &os.PathError{Op: "stat", Path: name, Err: os.ErrNotExist}
	}
	return &fakeFileInfo{name: filepath.Base(name), size: int64(len(data))}, nil
}

type fakeFileInfo struct {
	name string
	size int64
}

func (f *fakeFileInfo) Name() string      { return f.name }
func (f *fakeFileInfo) Size() int64       { return f.size }
func (f *fakeFileInfo) Mode() os.FileMode { return 0o644 }
func (f *fakeFileInfo) ModTime() time.Time { return time.Time{} }
func (f *fakeFileInfo) IsDir() bool       { return false }
func (f *fakeFileInfo) Sys() any          { return nil }

// ---- mock HTTPDoer ----------------------------------------------------------

type mockHTTPDoer struct {
	resp *http.Response
	err  error
}

func newHTTPDoer(statusCode int, body string) *mockHTTPDoer {
	return &mockHTTPDoer{
		resp: &http.Response{
			StatusCode: statusCode,
			Body:       io.NopCloser(strings.NewReader(body)),
		},
	}
}

func (m *mockHTTPDoer) Do(_ *http.Request) (*http.Response, error) {
	if m.err != nil {
		return nil, m.err
	}
	// Reset the body so it can be read once.
	bodyBytes, _ := io.ReadAll(m.resp.Body)
	m.resp.Body = io.NopCloser(bytes.NewReader(bodyBytes))
	return m.resp, nil
}

// ---- mock EventEmitter -------------------------------------------------------

type captureEmitter struct {
	calls   []string
	results []string
}

func (c *captureEmitter) EmitToolCall(toolName string, _ map[string]string) {
	c.calls = append(c.calls, toolName)
}
func (c *captureEmitter) EmitToolResult(toolName string, _ string, _ error) {
	c.results = append(c.results, toolName)
}

// ============================================================================
// Registry tests
// ============================================================================

func TestRegistry_ToolNotFound(t *testing.T) {
	reg := tools.NewRegistry()
	_, err := reg.Execute(context.Background(), "nonexistent", nil)
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected not-found error, got %v", err)
	}
}

func TestRegistry_EventEmitter_CalledBeforeAndAfter(t *testing.T) {
	reg := tools.NewRegistry()

	// Register a simple tool.
	reg.Register(tools.NewReadFile())

	em := &captureEmitter{}
	reg.SetEventEmitter(em)

	// Execute a tool that will fail (no file), but the emitter should still fire.
	_, _ = reg.Execute(context.Background(), "read_file", map[string]string{"path": "/nonexistent/file"})

	if len(em.calls) != 1 || em.calls[0] != "read_file" {
		t.Errorf("EmitToolCall not called correctly: %v", em.calls)
	}
	if len(em.results) != 1 || em.results[0] != "read_file" {
		t.Errorf("EmitToolResult not called correctly: %v", em.results)
	}
}

func TestRegistry_SetEventEmitter_Nil(t *testing.T) {
	reg := tools.NewRegistry()
	em := &captureEmitter{}
	reg.SetEventEmitter(em)
	reg.SetEventEmitter(nil) // disable

	reg.Register(tools.NewReadFile())
	reg.Execute(context.Background(), "read_file", map[string]string{"path": "/nonexistent"}) //nolint:errcheck

	if len(em.calls) != 0 {
		t.Errorf("expected no emitter calls after setting nil, got %d", len(em.calls))
	}
}

// ============================================================================
// safePath / path safety tests (via ReadFile with AllowedDirs)
// ============================================================================

func TestReadFile_PathSafety_AllowedDir(t *testing.T) {
	fs := newMockFS()
	fs.files["/allowed/file.txt"] = []byte("hello")

	tool := tools.NewReadFileWithConfig(tools.FileConfig{AllowedDirs: []string{"/allowed"}}, fs)
	out, err := tool.Execute(context.Background(), map[string]string{"path": "/allowed/file.txt"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out != "hello" {
		t.Errorf("unexpected output: %q", out)
	}
}

func TestReadFile_PathSafety_DeniedDir(t *testing.T) {
	fs := newMockFS()
	fs.files["/etc/passwd"] = []byte("secret")

	tool := tools.NewReadFileWithConfig(tools.FileConfig{AllowedDirs: []string{"/allowed"}}, fs)
	_, err := tool.Execute(context.Background(), map[string]string{"path": "/etc/passwd"})
	if err == nil {
		t.Fatal("expected error for path outside allowed dir")
	}
}

func TestReadFile_PathSafety_TraversalBlocked(t *testing.T) {
	fs := newMockFS()
	fs.files["/etc/passwd"] = []byte("secret")

	// Even with /allowed set, a path that traverses out should be denied.
	tool := tools.NewReadFileWithConfig(tools.FileConfig{AllowedDirs: []string{"/allowed"}}, fs)
	_, err := tool.Execute(context.Background(), map[string]string{"path": "/allowed/../etc/passwd"})
	if err == nil {
		t.Fatal("expected error for traversal path")
	}
}

func TestReadFile_PathSafety_NoRestriction(t *testing.T) {
	fs := newMockFS()
	fs.files["/any/path.txt"] = []byte("data")

	tool := tools.NewReadFileWithConfig(tools.FileConfig{}, fs)
	out, err := tool.Execute(context.Background(), map[string]string{"path": "/any/path.txt"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out != "data" {
		t.Errorf("unexpected output: %q", out)
	}
}

// ============================================================================
// ReadFile tests
// ============================================================================

func TestReadFile_MissingParam(t *testing.T) {
	tool := tools.NewReadFileWithConfig(tools.FileConfig{}, newMockFS())
	_, err := tool.Execute(context.Background(), map[string]string{})
	if err == nil || !strings.Contains(err.Error(), "missing required param") {
		t.Fatalf("expected missing-param error, got %v", err)
	}
}

func TestReadFile_SizeLimit_Exceeded(t *testing.T) {
	fs := newMockFS()
	fs.files["/big.txt"] = bytes.Repeat([]byte("x"), 100)

	tool := tools.NewReadFileWithConfig(tools.FileConfig{MaxReadBytes: 50}, fs)
	_, err := tool.Execute(context.Background(), map[string]string{"path": "/big.txt"})
	if err == nil || !strings.Contains(err.Error(), "exceeds limit") {
		t.Fatalf("expected size-limit error, got %v", err)
	}
}

func TestReadFile_SizeLimit_OK(t *testing.T) {
	fs := newMockFS()
	fs.files["/small.txt"] = []byte("small")

	tool := tools.NewReadFileWithConfig(tools.FileConfig{MaxReadBytes: 1000}, fs)
	out, err := tool.Execute(context.Background(), map[string]string{"path": "/small.txt"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out != "small" {
		t.Errorf("unexpected output: %q", out)
	}
}

func TestReadFile_CallTimeout(t *testing.T) {
	// Use a very short timeout and a context that is already cancelled.
	tool := tools.NewReadFileWithConfig(tools.FileConfig{CallTimeout: 1 * time.Nanosecond}, newMockFS())
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already done

	_, err := tool.Execute(ctx, map[string]string{"path": "/any"})
	if err == nil {
		t.Fatal("expected timeout/cancel error")
	}
}

// ============================================================================
// WriteFile tests
// ============================================================================

func TestWriteFile_Basic(t *testing.T) {
	fs := newMockFS()
	tool := tools.NewWriteFileWithConfig(tools.FileConfig{}, fs)
	out, err := tool.Execute(context.Background(), map[string]string{"path": "/out.txt", "content": "hello"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "5 bytes") {
		t.Errorf("unexpected output: %q", out)
	}
	if string(fs.files["/out.txt"]) != "hello" {
		t.Errorf("file content mismatch: %q", fs.files["/out.txt"])
	}
}

func TestWriteFile_SizeLimit_Exceeded(t *testing.T) {
	fs := newMockFS()
	tool := tools.NewWriteFileWithConfig(tools.FileConfig{MaxWriteBytes: 3}, fs)
	_, err := tool.Execute(context.Background(), map[string]string{"path": "/out.txt", "content": "hello"})
	if err == nil || !strings.Contains(err.Error(), "exceeds limit") {
		t.Fatalf("expected size-limit error, got %v", err)
	}
}

func TestWriteFile_MissingPath(t *testing.T) {
	tool := tools.NewWriteFileWithConfig(tools.FileConfig{}, newMockFS())
	_, err := tool.Execute(context.Background(), map[string]string{"content": "data"})
	if err == nil {
		t.Fatal("expected error for missing path")
	}
}

func TestWriteFile_PathSafety(t *testing.T) {
	fs := newMockFS()
	tool := tools.NewWriteFileWithConfig(tools.FileConfig{AllowedDirs: []string{"/sandbox"}}, fs)
	_, err := tool.Execute(context.Background(), map[string]string{"path": "/etc/cron", "content": "evil"})
	if err == nil {
		t.Fatal("expected error for path outside allowed dir")
	}
}

// ============================================================================
// PatchFile tests
// ============================================================================

func TestPatchFile_UnifiedDiff(t *testing.T) {
	fs := newMockFS()
	fs.files["/f.txt"] = []byte("line1\nline2\nline3")

	tool := tools.NewPatchFileWithConfig(tools.FileConfig{}, fs)
	// Proper unified-diff: context line advances the pointer to line2 before removing it.
	patch := " line1\n-line2\n+lineB\n line3"
	_, err := tool.Execute(context.Background(), map[string]string{"path": "/f.txt", "patch": patch})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := string(fs.files["/f.txt"])
	if !strings.Contains(got, "lineB") || strings.Contains(got, "line2") {
		t.Errorf("patch not applied correctly; got %q", got)
	}
}

func TestPatchFile_RangeReplace(t *testing.T) {
	fs := newMockFS()
	fs.files["/f.txt"] = []byte("a\nb\nc\nd")

	tool := tools.NewPatchFileWithConfig(tools.FileConfig{}, fs)
	_, err := tool.Execute(context.Background(), map[string]string{
		"path":        "/f.txt",
		"start_line":  "2",
		"end_line":    "3",
		"replacement": "X\nY",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := string(fs.files["/f.txt"])
	if got != "a\nX\nY\nd" {
		t.Errorf("range replace gave %q", got)
	}
}

func TestPatchFile_RangeReplace_MissingEndLine(t *testing.T) {
	fs := newMockFS()
	fs.files["/f.txt"] = []byte("a\nb")

	tool := tools.NewPatchFileWithConfig(tools.FileConfig{}, fs)
	_, err := tool.Execute(context.Background(), map[string]string{
		"path":       "/f.txt",
		"start_line": "1",
	})
	if err == nil || !strings.Contains(err.Error(), "end_line") {
		t.Fatalf("expected end_line error, got %v", err)
	}
}

func TestPatchFile_MissingPatch(t *testing.T) {
	fs := newMockFS()
	fs.files["/f.txt"] = []byte("data")
	tool := tools.NewPatchFileWithConfig(tools.FileConfig{}, fs)
	_, err := tool.Execute(context.Background(), map[string]string{"path": "/f.txt"})
	if err == nil {
		t.Fatal("expected error for missing patch param")
	}
}

func TestPatchFile_PathSafety(t *testing.T) {
	fs := newMockFS()
	tool := tools.NewPatchFileWithConfig(tools.FileConfig{AllowedDirs: []string{"/safe"}}, fs)
	_, err := tool.Execute(context.Background(), map[string]string{"path": "/unsafe/f.txt", "patch": "+x"})
	if err == nil {
		t.Fatal("expected error for path outside allowed dir")
	}
}

// ============================================================================
// ExecCommand tests
// ============================================================================

func TestExecCommand_Basic(t *testing.T) {
	tool := tools.NewExecCommand()
	out, err := tool.Execute(context.Background(), map[string]string{"command": "echo hello"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "hello") {
		t.Errorf("expected 'hello' in output, got %q", out)
	}
	if !strings.Contains(out, "exit_code: 0") {
		t.Errorf("expected exit_code: 0 in output, got %q", out)
	}
}

func TestExecCommand_Stdout_Stderr_ExitCode(t *testing.T) {
	tool := tools.NewExecCommand()
	// Command that writes to stderr and exits non-zero.
	out, err := tool.Execute(context.Background(), map[string]string{
		"command": "echo out && echo err >&2 && exit 1",
	})
	if err == nil {
		t.Fatal("expected non-zero exit error")
	}
	if !strings.Contains(out, "stdout:") {
		t.Errorf("expected 'stdout:' section in output, got %q", out)
	}
	if !strings.Contains(out, "stderr:") {
		t.Errorf("expected 'stderr:' section in output, got %q", out)
	}
	if !strings.Contains(out, "exit_code: 1") {
		t.Errorf("expected exit_code: 1, got %q", out)
	}
}

func TestExecCommand_AllowList_Allowed(t *testing.T) {
	tool := tools.NewExecCommandWithConfig(tools.ExecConfig{AllowedCommands: []string{"echo"}})
	out, err := tool.Execute(context.Background(), map[string]string{"command": "echo allowed"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "allowed") {
		t.Errorf("expected 'allowed' in output, got %q", out)
	}
}

func TestExecCommand_AllowList_Denied(t *testing.T) {
	tool := tools.NewExecCommandWithConfig(tools.ExecConfig{AllowedCommands: []string{"echo"}})
	_, err := tool.Execute(context.Background(), map[string]string{"command": "rm -rf /"})
	if err == nil || !strings.Contains(err.Error(), "not in the allowlist") {
		t.Fatalf("expected allowlist error, got %v", err)
	}
}

func TestExecCommand_MissingCommand(t *testing.T) {
	tool := tools.NewExecCommand()
	_, err := tool.Execute(context.Background(), map[string]string{})
	if err == nil {
		t.Fatal("expected error for missing command param")
	}
}

func TestExecCommand_Timeout(t *testing.T) {
	tool := tools.NewExecCommandWithConfig(tools.ExecConfig{CallTimeout: 100 * time.Millisecond})
	_, err := tool.Execute(context.Background(), map[string]string{"command": "sleep 10"})
	if err == nil {
		t.Fatal("expected timeout error")
	}
}

// ============================================================================
// FetchURL tests
// ============================================================================

func TestFetchURL_Basic(t *testing.T) {
	doer := newHTTPDoer(200, "<html><body>Hello world</body></html>")
	tool := tools.NewFetchURLWithConfig(tools.FetchConfig{}, doer)
	out, err := tool.Execute(context.Background(), map[string]string{"url": "https://example.com"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// No sanitize → raw HTML returned.
	if !strings.Contains(out, "Hello world") {
		t.Errorf("unexpected output: %q", out)
	}
}

func TestFetchURL_Sanitize(t *testing.T) {
	doer := newHTTPDoer(200, "<html><body><h1>Title</h1><p>Content here.</p></body></html>")
	tool := tools.NewFetchURLWithConfig(tools.FetchConfig{Sanitize: true}, doer)
	out, err := tool.Execute(context.Background(), map[string]string{"url": "https://example.com"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(out, "<") || strings.Contains(out, ">") {
		t.Errorf("HTML tags not stripped: %q", out)
	}
	if !strings.Contains(out, "Title") || !strings.Contains(out, "Content here") {
		t.Errorf("text content missing: %q", out)
	}
}

func TestFetchURL_InvalidScheme(t *testing.T) {
	doer := newHTTPDoer(200, "data")
	tool := tools.NewFetchURLWithConfig(tools.FetchConfig{}, doer)
	_, err := tool.Execute(context.Background(), map[string]string{"url": "ftp://example.com/file"})
	if err == nil || !strings.Contains(err.Error(), "unsupported scheme") {
		t.Fatalf("expected scheme error, got %v", err)
	}
}

func TestFetchURL_HostAllowlist_Allowed(t *testing.T) {
	doer := newHTTPDoer(200, "ok")
	tool := tools.NewFetchURLWithConfig(tools.FetchConfig{AllowedHosts: []string{"example.com"}}, doer)
	_, err := tool.Execute(context.Background(), map[string]string{"url": "https://example.com/page"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestFetchURL_HostAllowlist_Subdomain(t *testing.T) {
	doer := newHTTPDoer(200, "ok")
	tool := tools.NewFetchURLWithConfig(tools.FetchConfig{AllowedHosts: []string{"example.com"}}, doer)
	_, err := tool.Execute(context.Background(), map[string]string{"url": "https://sub.example.com/page"})
	if err != nil {
		t.Fatalf("unexpected error for subdomain: %v", err)
	}
}

func TestFetchURL_HostAllowlist_Denied(t *testing.T) {
	doer := newHTTPDoer(200, "ok")
	tool := tools.NewFetchURLWithConfig(tools.FetchConfig{AllowedHosts: []string{"example.com"}}, doer)
	_, err := tool.Execute(context.Background(), map[string]string{"url": "https://evil.com/page"})
	if err == nil || !strings.Contains(err.Error(), "not in the allowlist") {
		t.Fatalf("expected allowlist error, got %v", err)
	}
}

func TestFetchURL_HTTP4xx(t *testing.T) {
	doer := newHTTPDoer(404, "not found")
	tool := tools.NewFetchURLWithConfig(tools.FetchConfig{}, doer)
	_, err := tool.Execute(context.Background(), map[string]string{"url": "https://example.com"})
	if err == nil || !strings.Contains(err.Error(), "HTTP 404") {
		t.Fatalf("expected 404 error, got %v", err)
	}
}

func TestFetchURL_NetworkError(t *testing.T) {
	doer := &mockHTTPDoer{err: errors.New("connection refused")}
	tool := tools.NewFetchURLWithConfig(tools.FetchConfig{}, doer)
	_, err := tool.Execute(context.Background(), map[string]string{"url": "https://example.com"})
	if err == nil {
		t.Fatal("expected network error")
	}
}

func TestFetchURL_MissingURL(t *testing.T) {
	tool := tools.NewFetchURLWithConfig(tools.FetchConfig{}, newHTTPDoer(200, ""))
	_, err := tool.Execute(context.Background(), map[string]string{})
	if err == nil {
		t.Fatal("expected error for missing URL")
	}
}

func TestFetchURL_MaxBodyBytes(t *testing.T) {
	body := strings.Repeat("x", 200)
	doer := newHTTPDoer(200, body)
	// Cap at 10 bytes.
	tool := tools.NewFetchURLWithConfig(tools.FetchConfig{MaxBodyBytes: 10}, doer)
	out, err := tool.Execute(context.Background(), map[string]string{"url": "https://example.com"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out) > 10 {
		t.Errorf("response not capped: got %d bytes", len(out))
	}
}

// ============================================================================
// Research tool tests
// ============================================================================

func TestResearch_Basic(t *testing.T) {
	doer := newHTTPDoer(200, "<html><body>Research content here.</body></html>")
	tool := tools.NewResearchWithConfig(tools.FetchConfig{}, doer)
	out, err := tool.Execute(context.Background(), map[string]string{"url": "https://example.com"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "Research source:") {
		t.Errorf("expected research source header, got %q", out)
	}
	if strings.Contains(out, "<html>") {
		t.Errorf("HTML not stripped in research output: %q", out)
	}
	if !strings.Contains(out, "Research content here") {
		t.Errorf("text content missing: %q", out)
	}
}

func TestResearch_WithQuery(t *testing.T) {
	doer := newHTTPDoer(200, "<p>Findings</p>")
	tool := tools.NewResearchWithConfig(tools.FetchConfig{}, doer)
	out, err := tool.Execute(context.Background(), map[string]string{
		"url":   "https://example.com",
		"query": "what is X",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "Query: what is X") {
		t.Errorf("query not included in output: %q", out)
	}
}

func TestResearch_MissingURL(t *testing.T) {
	doer := newHTTPDoer(200, "")
	tool := tools.NewResearchWithConfig(tools.FetchConfig{}, doer)
	_, err := tool.Execute(context.Background(), map[string]string{})
	if err == nil {
		t.Fatal("expected error for missing URL")
	}
}

func TestResearch_AlwaysSanitizes(t *testing.T) {
	doer := newHTTPDoer(200, "<b>bold</b> text")
	// Explicitly pass Sanitize: false — research tool overrides it.
	tool := tools.NewResearchWithConfig(tools.FetchConfig{Sanitize: false}, doer)
	out, err := tool.Execute(context.Background(), map[string]string{"url": "https://example.com"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(out, "<b>") {
		t.Errorf("HTML not stripped even with Sanitize:false override: %q", out)
	}
}

// ============================================================================
// stripHTML tests (internal via FetchURL with Sanitize:true)
// ============================================================================

func TestStripHTML_NestedTags(t *testing.T) {
	doer := newHTTPDoer(200, "<div><span>Hello</span> <em>world</em>!</div>")
	tool := tools.NewFetchURLWithConfig(tools.FetchConfig{Sanitize: true}, doer)
	out, err := tool.Execute(context.Background(), map[string]string{"url": "https://example.com"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(out, "<") {
		t.Errorf("tags remaining: %q", out)
	}
	if !strings.Contains(out, "Hello") || !strings.Contains(out, "world") {
		t.Errorf("text stripped incorrectly: %q", out)
	}
}

// ============================================================================
// NewResearch default constructor
// ============================================================================

func TestNewResearch_NameAndDescription(t *testing.T) {
	tool := tools.NewResearch()
	if tool.Name() != "research" {
		t.Errorf("expected name 'research', got %q", tool.Name())
	}
	if tool.Description() == "" {
		t.Error("expected non-empty description")
	}
}

// ============================================================================
// Integration: Registry + EventEmitter + real file tools
// ============================================================================

func TestRegistry_RealReadFile_AuditEvents(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.txt")
	if err := os.WriteFile(path, []byte("content"), 0o644); err != nil {
		t.Fatal(err)
	}

	reg := tools.NewRegistry()
	reg.Register(tools.NewReadFile())
	em := &captureEmitter{}
	reg.SetEventEmitter(em)

	out, err := reg.Execute(context.Background(), "read_file", map[string]string{"path": path})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out != "content" {
		t.Errorf("unexpected output: %q", out)
	}
	if len(em.calls) != 1 {
		t.Errorf("expected 1 EmitToolCall, got %d", len(em.calls))
	}
	if len(em.results) != 1 {
		t.Errorf("expected 1 EmitToolResult, got %d", len(em.results))
	}
}
