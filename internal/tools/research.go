package tools

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

// researchTool fetches a URL, sanitizes the HTML response to plain text, and
// returns a structured research result with the source URL attributed.
// It always sanitizes output and is intended for use by Researcher agents.
type researchTool struct {
	cfg  FetchConfig
	doer HTTPDoer
}

// NewResearch returns a Research tool with default (unrestricted) configuration.
// HTML sanitization is always enabled.
func NewResearch() Tool {
	return NewResearchWithConfig(FetchConfig{})
}

// NewResearchWithConfig returns a Research tool with the given configuration and HTTPDoer.
// HTML sanitization is always enabled regardless of cfg.Sanitize.
func NewResearchWithConfig(cfg FetchConfig, doer ...HTTPDoer) Tool {
	cfg.Sanitize = true // research always sanitizes
	var d HTTPDoer
	if len(doer) > 0 && doer[0] != nil {
		d = doer[0]
	} else {
		d = &http.Client{Timeout: 15 * time.Second}
	}
	return &researchTool{cfg: cfg, doer: d}
}

func (t *researchTool) Name() string { return "research" }
func (t *researchTool) Description() string {
	return "Fetch a URL and return sanitized plain-text content for research. params: {\"url\":\"...\",\"query\":\"<optional focus>\"}"
}

func (t *researchTool) Execute(ctx context.Context, params map[string]string) (string, error) {
	rawURL, ok := params["url"]
	if !ok || rawURL == "" {
		return "", fmt.Errorf("research: missing required param 'url'")
	}

	fetcher := &fetchURLTool{cfg: t.cfg, doer: t.doer}
	text, err := fetcher.Execute(ctx, map[string]string{"url": rawURL})
	if err != nil {
		return "", fmt.Errorf("research: %w", err)
	}

	query := params["query"]
	if query != "" {
		return fmt.Sprintf("Research source: %s\nQuery: %s\n\n%s", rawURL, query, text), nil
	}
	return fmt.Sprintf("Research source: %s\n\n%s", rawURL, text), nil
}
