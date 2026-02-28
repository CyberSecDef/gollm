package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// ChatMessage is a single turn in a conversation.
type ChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Client defines the interface for calling a language model.
type Client interface {
	Complete(ctx context.Context, messages []ChatMessage) (string, error)
}

// ---- OpenAI implementation -----------------------------------------------

type openAIClient struct {
	apiKey  string
	baseURL string
	model   string
	http    *http.Client
	limiter <-chan time.Time
}

type openAIRequest struct {
	Model    string        `json:"model"`
	Messages []ChatMessage `json:"messages"`
}

type openAIResponse struct {
	Choices []struct {
		Message ChatMessage `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// NewOpenAIClient creates a client backed by any OpenAI-compatible API.
// Configuration is read from environment variables:
//
//	OPENAI_API_KEY   – required for real calls
//	OPENAI_BASE_URL  – defaults to https://api.openai.com/v1
//	OPENAI_MODEL     – defaults to gpt-4o-mini
//
// When OPENAI_API_KEY is empty a MockClient is returned instead.
func NewOpenAIClient() Client {
	key := os.Getenv("OPENAI_API_KEY")
	if key == "" {
		return &MockClient{}
	}

	base := os.Getenv("OPENAI_BASE_URL")
	if base == "" {
		base = "https://api.openai.com/v1"
	}
	model := os.Getenv("OPENAI_MODEL")
	if model == "" {
		model = "gpt-4o-mini"
	}

	// Simple rate limiter: max 10 requests per second.
	ticker := time.NewTicker(100 * time.Millisecond)

	return &openAIClient{
		apiKey:  key,
		baseURL: strings.TrimRight(base, "/"),
		model:   model,
		http:    &http.Client{Timeout: 120 * time.Second},
		limiter: ticker.C,
	}
}

func (c *openAIClient) Complete(ctx context.Context, messages []ChatMessage) (string, error) {
	// Rate limiting
	select {
	case <-c.limiter:
	case <-ctx.Done():
		return "", ctx.Err()
	}

	payload, err := json.Marshal(openAIRequest{
		Model:    c.model,
		Messages: messages,
	})
	if err != nil {
		return "", fmt.Errorf("llm marshal: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return "", fmt.Errorf("llm request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("llm http: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("llm read body: %w", err)
	}

	var oaiResp openAIResponse
	if err := json.Unmarshal(body, &oaiResp); err != nil {
		return "", fmt.Errorf("llm unmarshal: %w", err)
	}
	if oaiResp.Error != nil {
		return "", fmt.Errorf("llm api error: %s", oaiResp.Error.Message)
	}
	if len(oaiResp.Choices) == 0 {
		return "", fmt.Errorf("llm: no choices returned")
	}
	return oaiResp.Choices[0].Message.Content, nil
}

// ---- Mock implementation -------------------------------------------------

// MockClient returns deterministic fake responses so the application is fully
// functional even without an API key.
type MockClient struct{}

func (m *MockClient) Complete(_ context.Context, messages []ChatMessage) (string, error) {
	// Extract the last user message to craft a contextual reply.
	userMsg := ""
	systemMsg := ""
	for _, msg := range messages {
		switch msg.Role {
		case "user":
			userMsg = msg.Content
		case "system":
			systemMsg = msg.Content
		}
	}

	lower := strings.ToLower(userMsg + systemMsg)

	switch {
	case strings.Contains(lower, "decompose") || strings.Contains(lower, "subtask"):
		return mockDecompose(userMsg), nil
	case strings.Contains(lower, "researcher") || strings.Contains(lower, "research"):
		return mockResearch(userMsg), nil
	case strings.Contains(lower, "coder") || strings.Contains(lower, "code"):
		return mockCode(userMsg), nil
	case strings.Contains(lower, "analyst") || strings.Contains(lower, "analy"):
		return mockAnalysis(userMsg), nil
	case strings.Contains(lower, "synthesize") || strings.Contains(lower, "merge") || strings.Contains(lower, "combine"):
		return mockSynthesize(userMsg), nil
	default:
		return mockGeneral(userMsg), nil
	}
}

func mockDecompose(input string) string {
	return fmt.Sprintf(`I'll break this task into subtasks:

SUBTASK_1|Researcher|Research background information and context for: %s
SUBTASK_2|Analyst|Analyze the requirements and identify key considerations for: %s
SUBTASK_3|Coder|Provide implementation details or technical solution for: %s`, input, input, input)
}

func mockResearch(input string) string {
	return fmt.Sprintf(`## Research Findings

**Topic:** %s

### Background
Based on available knowledge, this topic involves several important considerations. The domain has evolved significantly and there are established best practices to follow.

### Key Points
1. Foundational concepts are well-documented in literature
2. Modern approaches favor modularity and extensibility
3. Performance considerations are critical at scale

### Sources
- General domain knowledge
- Best practices from industry standards
- Empirical observations from similar systems

**Confidence:** High`, input)
}

func mockCode(input string) string {
	return fmt.Sprintf(`## Technical Implementation

**Task:** %s

### Approach
A clean, idiomatic implementation following best practices:

`+"```"+`
// Implementation outline
func Solution(input string) (string, error) {
    // 1. Validate input
    // 2. Process core logic
    // 3. Return result
    return process(input), nil
}
`+"```"+`

### Key Considerations
- Error handling at every boundary
- Context propagation for cancellation
- Clean separation of concerns

**Complexity:** O(n) time, O(1) space`, input)
}

func mockAnalysis(input string) string {
	return fmt.Sprintf(`## Analysis Report

**Subject:** %s

### Findings
After thorough analysis, several key patterns emerge:

**Strengths:**
- Well-defined problem space
- Clear success criteria
- Established tooling available

**Risks:**
- Complexity may increase with scale
- Integration points need careful handling

**Recommendations:**
1. Start with a minimal viable approach
2. Iterate based on feedback
3. Document decisions for future reference

**Overall Assessment:** Feasible with moderate effort`, input)
}

func mockSynthesize(input string) string {
	_ = input
	return `## Synthesized Response

Based on the contributions from all specialist agents, here is the consolidated answer:

### Executive Summary
The multi-agent analysis has produced a comprehensive view of the request, drawing on research, analysis, and technical expertise.

### Combined Findings

**From Research Agent:** Provided foundational context and background knowledge establishing the theoretical basis.

**From Analysis Agent:** Identified key requirements, risks, and strategic recommendations for moving forward.

**From Technical Agent:** Delivered concrete implementation guidance with code examples and architectural patterns.

### Conclusion
The requested task is well-understood and actionable. The recommended approach combines the research insights with the technical implementation strategy, validated through the analytical framework provided.

*This response synthesizes outputs from all active worker agents.*`
}

func mockGeneral(input string) string {
	return fmt.Sprintf(`I understand your request: "%s"

I'll process this through the multi-agent system. The orchestrator will decompose this into specialized tasks, assign them to worker agents, and synthesize the results into a comprehensive response.

Please wait while the agents work on your request.`, input)
}
