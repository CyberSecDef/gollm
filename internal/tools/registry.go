package tools

import (
	"context"
	"fmt"
	"sync"
)

// Tool defines a capability that agents can invoke.
type Tool interface {
	Name() string
	Description() string
	Execute(ctx context.Context, params map[string]string) (string, error)
}

// Registry holds all registered tools and dispatches executions.
type Registry struct {
	mu    sync.RWMutex
	tools map[string]Tool
}

// NewRegistry creates an empty Registry.
func NewRegistry() *Registry {
	return &Registry{tools: make(map[string]Tool)}
}

// Register adds a tool to the registry. Overwrites any existing tool with the same name.
func (r *Registry) Register(tool Tool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tools[tool.Name()] = tool
}

// Execute runs the named tool with the given parameters.
func (r *Registry) Execute(ctx context.Context, name string, params map[string]string) (string, error) {
	r.mu.RLock()
	t, ok := r.tools[name]
	r.mu.RUnlock()
	if !ok {
		return "", fmt.Errorf("tool %q not found", name)
	}
	return t.Execute(ctx, params)
}

// List returns all registered tools in an unspecified order.
func (r *Registry) List() []Tool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Tool, 0, len(r.tools))
	for _, t := range r.tools {
		out = append(out, t)
	}
	return out
}
