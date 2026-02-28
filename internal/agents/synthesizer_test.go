package agents

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/cybersecdef/gollm/internal/bus"
	"github.com/cybersecdef/gollm/internal/llm"
)

// ---- parseWorkerSections ----------------------------------------------------

func TestParseWorkerSections_Empty(t *testing.T) {
	sections := parseWorkerSections("")
	if len(sections) != 0 {
		t.Errorf("expected 0 sections, got %d", len(sections))
	}
}

func TestParseWorkerSections_SingleSection(t *testing.T) {
	content := "=== Researcher Agent (worker-1) ===\nFound lots of info.\n"
	sections := parseWorkerSections(content)
	if len(sections) != 1 {
		t.Fatalf("expected 1 section, got %d", len(sections))
	}
	if sections[0].role != "Researcher" {
		t.Errorf("expected role Researcher, got %q", sections[0].role)
	}
	if sections[0].id != "worker-1" {
		t.Errorf("expected id worker-1, got %q", sections[0].id)
	}
	if !strings.Contains(sections[0].content, "Found lots of info.") {
		t.Errorf("unexpected content: %q", sections[0].content)
	}
}

func TestParseWorkerSections_MultipleSections(t *testing.T) {
	content := `=== Iteration 1 ===
=== Researcher Agent (worker-1) ===
Research findings here.

=== Analyst Agent (worker-2) ===
Analysis conclusions here.
`
	sections := parseWorkerSections(content)
	if len(sections) != 2 {
		t.Fatalf("expected 2 sections, got %d: %+v", len(sections), sections)
	}
	if sections[0].role != "Researcher" || sections[0].id != "worker-1" {
		t.Errorf("section[0] mismatch: %+v", sections[0])
	}
	if sections[1].role != "Analyst" || sections[1].id != "worker-2" {
		t.Errorf("section[1] mismatch: %+v", sections[1])
	}
}

func TestParseWorkerSections_SkipsScratchpadHeaders(t *testing.T) {
	content := `=== Researcher Agent (worker-1) ===
Output here.

=== WORKER SCRATCHPADS ===
=== Researcher (worker-1) Scratchpad ===
Private notes.
`
	sections := parseWorkerSections(content)
	// Only the worker output section should be captured, not the scratchpad.
	if len(sections) != 1 {
		t.Fatalf("expected 1 section, got %d", len(sections))
	}
	if sections[0].role != "Researcher" {
		t.Errorf("expected Researcher, got %q", sections[0].role)
	}
}

// ---- parseFileEdits ---------------------------------------------------------

func TestParseFileEdits_NoEdits(t *testing.T) {
	edits := parseFileEdits("Just some normal text\nno file edits here")
	if len(edits) != 0 {
		t.Errorf("expected 0 edits, got %d", len(edits))
	}
}

func TestParseFileEdits_SingleLine(t *testing.T) {
	edits := parseFileEdits("FILE_EDIT|/tmp/foo.txt|hello world")
	if len(edits) != 1 {
		t.Fatalf("expected 1 edit, got %d", len(edits))
	}
	if edits[0].path != "/tmp/foo.txt" {
		t.Errorf("path mismatch: %q", edits[0].path)
	}
	if edits[0].content != "hello world" {
		t.Errorf("content mismatch: %q", edits[0].content)
	}
}

func TestParseFileEdits_MultiLine(t *testing.T) {
	content := "FILE_EDIT_START|src/main.go\npackage main\n\nfunc main() {}\nFILE_EDIT_END"
	edits := parseFileEdits(content)
	if len(edits) != 1 {
		t.Fatalf("expected 1 edit, got %d", len(edits))
	}
	if edits[0].path != "src/main.go" {
		t.Errorf("path mismatch: %q", edits[0].path)
	}
	if !strings.Contains(edits[0].content, "package main") {
		t.Errorf("content missing 'package main': %q", edits[0].content)
	}
}

func TestParseFileEdits_MultipleEdits(t *testing.T) {
	content := "FILE_EDIT|/a.txt|content a\nFILE_EDIT|/b.txt|content b"
	edits := parseFileEdits(content)
	if len(edits) != 2 {
		t.Fatalf("expected 2 edits, got %d", len(edits))
	}
}

// ---- buildTracedResponse ----------------------------------------------------

func TestBuildTracedResponse_IncludesTraceability(t *testing.T) {
	sections := []workerSection{
		{role: "Researcher", id: "worker-1", content: "Found info."},
		{role: "Analyst", id: "worker-2", content: "Analysed data."},
	}
	result := buildTracedResponse("## Summary\n\nGreat answer.", sections, nil, "corr-123")

	if !strings.Contains(result, "## Traceability") {
		t.Error("expected ## Traceability section")
	}
	if !strings.Contains(result, "Researcher") {
		t.Error("expected Researcher in traceability table")
	}
	if !strings.Contains(result, "worker-1") {
		t.Error("expected worker-1 in traceability table")
	}
	if !strings.Contains(result, "Analyst") {
		t.Error("expected Analyst in traceability table")
	}
}

func TestBuildTracedResponse_IncludesCitations(t *testing.T) {
	result := buildTracedResponse("body", nil, nil, "corr-abc")
	if !strings.Contains(result, "## Citations") {
		t.Error("expected ## Citations section")
	}
	if !strings.Contains(result, "corr-abc") {
		t.Error("expected correlation ID in citations")
	}
	if !strings.Contains(result, "logs/audit.jsonl") {
		t.Error("expected log reference in citations")
	}
}

func TestBuildTracedResponse_CitationsNoCorrelationID(t *testing.T) {
	result := buildTracedResponse("body", nil, nil, "")
	if !strings.Contains(result, "## Citations") {
		t.Error("expected ## Citations section even without correlation ID")
	}
	if !strings.Contains(result, "logs/audit.jsonl") {
		t.Error("expected log reference")
	}
}

func TestBuildTracedResponse_IncludesFileEdits(t *testing.T) {
	edits := []fileEdit{
		{path: "src/foo.go", content: "package main"},
	}
	result := buildTracedResponse("body", nil, edits, "")
	if !strings.Contains(result, "## File Edits") {
		t.Error("expected ## File Edits section")
	}
	if !strings.Contains(result, "src/foo.go") {
		t.Error("expected file path in file edits section")
	}
	if !strings.Contains(result, "package main") {
		t.Error("expected file content in file edits section")
	}
	if !strings.Contains(result, "```go") {
		t.Error("expected go language tag on code fence")
	}
}

func TestBuildTracedResponse_NoFileEditsSection(t *testing.T) {
	result := buildTracedResponse("body", nil, nil, "")
	if strings.Contains(result, "## File Edits") {
		t.Error("unexpected ## File Edits section when there are no edits")
	}
}

func TestBuildTracedResponse_SummaryTruncated(t *testing.T) {
	longContent := strings.Repeat("x", 200)
	sections := []workerSection{{role: "Coder", id: "worker-3", content: longContent}}
	result := buildTracedResponse("body", sections, nil, "")
	// The truncated summary should end with "..." in the table.
	if !strings.Contains(result, "...") {
		t.Error("expected truncated content to end with '...' in traceability table")
	}
}

// ---- fallbackSynthesize -----------------------------------------------------

func TestFallbackSynthesize_IncludesStructure(t *testing.T) {
	result := fallbackSynthesize("worker output here")
	if !strings.Contains(result, "## Summary") {
		t.Error("expected ## Summary in fallback output")
	}
	if !strings.Contains(result, "## Synthesized Response") {
		t.Error("expected ## Synthesized Response in fallback output")
	}
	if !strings.Contains(result, "worker output here") {
		t.Error("expected original content in fallback output")
	}
}

// ---- fileEditLang -----------------------------------------------------------

func TestFileEditLang_KnownExtensions(t *testing.T) {
	cases := []struct {
		path string
		want string
	}{
		{"main.go", "go"},
		{"script.py", "python"},
		{"app.js", "javascript"},
		{"types.ts", "typescript"},
		{"run.sh", "bash"},
		{"config.json", "json"},
		{"deploy.yaml", "yaml"},
		{"README.md", "markdown"},
		{"binary", ""},
		{"no_ext", ""},
	}
	for _, tc := range cases {
		got := fileEditLang(tc.path)
		if got != tc.want {
			t.Errorf("fileEditLang(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}
}



func TestSynthesizer_ProducesFinalResponse(t *testing.T) {
	b := bus.New()
	client := &llm.MockClient{}

	synth := NewSynthesizer(b, client)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := synth.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer synth.Stop()

	// Subscribe to receive the broadcast MsgTypeFinalResponse.
	tuiCh := b.Subscribe("tui")

	// Simulate orchestrator sending task assign.
	content := "=== Researcher Agent (worker-1) ===\nResearch output here.\n\n" +
		"=== Analyst Agent (worker-2) ===\nAnalysis output here.\n"
	assignMsg := NewMessage(MsgTypeTaskAssign, orchestratorID, synthesizerID, content)
	assignMsg.Metadata["correlation_id"] = "test-corr-1"
	assignMsg.Metadata["orchestrator_plan"] = "[corr-test-1] Goal: test\n"
	assignMsg.CorrelationID = "test-corr-1"
	b.Publish(assignMsg)

	// Wait for final response.
	deadline := time.After(8 * time.Second)
	for {
		select {
		case msg := <-tuiCh:
			if msg.Type == MsgTypeFinalResponse {
				if !strings.Contains(msg.Content, "## Traceability") {
					t.Error("final response missing ## Traceability section")
				}
				if !strings.Contains(msg.Content, "## Citations") {
					t.Error("final response missing ## Citations section")
				}
				if msg.CorrelationID != "test-corr-1" {
					t.Errorf("correlation ID mismatch: got %q, want %q", msg.CorrelationID, "test-corr-1")
				}
				return
			}
		case <-deadline:
			t.Fatal("timed out waiting for MsgTypeFinalResponse")
		}
	}
}

func TestSynthesizer_FileEditsInContent(t *testing.T) {
	b := bus.New()
	client := &llm.MockClient{}

	synth := NewSynthesizer(b, client)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := synth.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer synth.Stop()

	tuiCh := b.Subscribe("tui")

	// Worker output contains a FILE_EDIT directive.
	content := "=== Coder Agent (worker-1) ===\n" +
		"Here is the code:\nFILE_EDIT|src/hello.go|package main\n"
	assignMsg := NewMessage(MsgTypeTaskAssign, orchestratorID, synthesizerID, content)
	assignMsg.CorrelationID = "test-corr-2"
	b.Publish(assignMsg)

	deadline := time.After(8 * time.Second)
	for {
		select {
		case msg := <-tuiCh:
			if msg.Type == MsgTypeFinalResponse {
				if !strings.Contains(msg.Content, "## File Edits") {
					t.Error("final response missing ## File Edits section")
				}
				if !strings.Contains(msg.Content, "src/hello.go") {
					t.Error("final response missing file path")
				}
				return
			}
		case <-deadline:
			t.Fatal("timed out waiting for MsgTypeFinalResponse")
		}
	}
}

// ---- Orchestrator emits completion event on FinalResponse -------------------

func TestOrchestrator_EmitsCompletionEventOnFinalResponse(t *testing.T) {
	orch, _ := newTestOrchestrator(&llm.MockClient{})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := orch.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer orch.Stop()

	// Use the orchestrator's own bus subscription directly.
	orchBus := orch.bus
	eventCh := orchBus.Subscribe("event-spy")

	// Publish a MsgTypeFinalResponse broadcast to trigger the completion event.
	finalMsg := NewMessage(MsgTypeFinalResponse, synthesizerID, "", "The final answer.")
	finalMsg.Metadata["correlation_id"] = "corr-event-test"
	finalMsg.CorrelationID = "corr-event-test"
	orchBus.Publish(finalMsg)

	// Wait for the MsgTypeEvent with exit_code=0.
	deadline := time.After(5 * time.Second)
	for {
		select {
		case msg := <-eventCh:
			if msg.Type == MsgTypeEvent &&
				msg.Metadata["exit_code"] == "0" &&
				msg.Metadata["correlation_id"] == "corr-event-test" {
				return
			}
		case <-deadline:
			t.Fatal("timed out waiting for completion event from orchestrator")
		}
	}
}
