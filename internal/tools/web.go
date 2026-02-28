package tools

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const defaultFetchMaxBytes = 1 << 20 // 1 MiB

// FetchConfig holds optional constraints for the fetch_url tool.
type FetchConfig struct {
	// AllowedHosts restricts fetching to these hostnames (e.g. "example.com").
	// An empty slice means no host restrictions are enforced.
	AllowedHosts []string
	// MaxBodyBytes caps the response body size. Zero falls back to 1 MiB.
	MaxBodyBytes int64
	// CallTimeout is applied as an additional per-call deadline.
	// Zero means no additional timeout.
	CallTimeout time.Duration
	// Sanitize strips HTML tags from the response body when true.
	Sanitize bool
}

// HTTPDoer abstracts the HTTP round-trip so the tool can be tested without
// making real network requests.
type HTTPDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

type fetchURLTool struct {
	cfg  FetchConfig
	doer HTTPDoer
}

// NewFetchURL returns a FetchURL tool with default (unrestricted) configuration.
func NewFetchURL() Tool {
	return NewFetchURLWithConfig(FetchConfig{}, &http.Client{Timeout: 10 * time.Second})
}

// NewFetchURLWithConfig returns a FetchURL tool with the given configuration and HTTPDoer.
func NewFetchURLWithConfig(cfg FetchConfig, doer HTTPDoer) Tool {
	if doer == nil {
		doer = &http.Client{Timeout: 10 * time.Second}
	}
	return &fetchURLTool{cfg: cfg, doer: doer}
}

func (t *fetchURLTool) Name() string { return "fetch_url" }
func (t *fetchURLTool) Description() string {
	return "Fetch the text content of a URL. params: {\"url\":\"...\"}"
}

func (t *fetchURLTool) Execute(ctx context.Context, params map[string]string) (string, error) {
	rawURL, ok := params["url"]
	if !ok || rawURL == "" {
		return "", fmt.Errorf("fetch_url: missing required param 'url'")
	}

	// Validate URL scheme.
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("fetch_url: invalid URL: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf("fetch_url: unsupported scheme %q (only http and https are allowed)", parsed.Scheme)
	}

	// Enforce host allowlist.
	// An exact hostname match or a match against any subdomain of an allowed
	// host (using the "." prefix to ensure boundary safety) is accepted.
	if len(t.cfg.AllowedHosts) > 0 {
		allowed := false
		host := parsed.Hostname()
		for _, h := range t.cfg.AllowedHosts {
			if host == h || strings.HasSuffix(host, "."+h) {
				allowed = true
				break
			}
		}
		if !allowed {
			return "", fmt.Errorf("fetch_url: host %q is not in the allowlist", host)
		}
	}

	if t.cfg.CallTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, t.cfg.CallTimeout)
		defer cancel()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", fmt.Errorf("fetch_url build request: %w", err)
	}
	req.Header.Set("User-Agent", "gollm/1.0")

	resp, err := t.doer.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetch_url http: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("fetch_url: HTTP %d for %s", resp.StatusCode, rawURL)
	}

	maxBytes := t.cfg.MaxBodyBytes
	if maxBytes <= 0 {
		maxBytes = defaultFetchMaxBytes
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes))
	if err != nil {
		return "", fmt.Errorf("fetch_url read: %w", err)
	}

	text := strings.TrimSpace(string(body))
	if t.cfg.Sanitize {
		text = stripHTML(text)
	}
	return text, nil
}

// stripHTML removes HTML tags from s and normalises whitespace.
// This is a best-effort plain-text extractor for well-formed HTML.
// It does not handle HTML entities, CDATA sections, or malformed markup.
func stripHTML(s string) string {
	var b strings.Builder
	inTag := false
	for _, r := range s {
		switch {
		case r == '<':
			inTag = true
		case r == '>':
			inTag = false
		case !inTag:
			b.WriteRune(r)
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}
