package tools

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

var webClient = &http.Client{Timeout: 10 * time.Second}

type fetchURLTool struct{}

func NewFetchURL() Tool { return &fetchURLTool{} }

func (t *fetchURLTool) Name() string { return "fetch_url" }
func (t *fetchURLTool) Description() string {
	return "Fetch the text content of a URL. params: {\"url\":\"...\"}"
}

func (t *fetchURLTool) Execute(ctx context.Context, params map[string]string) (string, error) {
	url, ok := params["url"]
	if !ok || url == "" {
		return "", fmt.Errorf("fetch_url: missing required param 'url'")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("fetch_url build request: %w", err)
	}
	req.Header.Set("User-Agent", "gollm/1.0")

	resp, err := webClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetch_url http: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("fetch_url: HTTP %d for %s", resp.StatusCode, url)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 1 MiB cap
	if err != nil {
		return "", fmt.Errorf("fetch_url read: %w", err)
	}

	// Return the raw body; callers can handle HTML stripping.
	return strings.TrimSpace(string(body)), nil
}
