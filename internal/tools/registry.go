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

// EventEmitter receives audit notifications before and after each tool execution.
// Implementations must be safe to call concurrently.
type EventEmitter interface {
	// EmitToolCall is invoked immediately before a tool executes.
	EmitToolCall(toolName string, params map[string]string)
	// EmitToolResult is invoked immediately after a tool finishes.
	EmitToolResult(toolName string, result string, err error)
}

// Registry holds all registered tools and dispatches executions.
type Registry struct {
	mu      sync.RWMutex
	tools   map[string]Tool
	emitter EventEmitter
}

// NewRegistry creates an empty Registry.
func NewRegistry() *Registry {
	return &Registry{tools: make(map[string]Tool)}
}

// SetEventEmitter configures the EventEmitter that will receive audit
// notifications for every tool execution routed through this Registry.
// Pass nil to disable event emission.
func (r *Registry) SetEventEmitter(e EventEmitter) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.emitter = e
}

// Register adds a tool to the registry. Overwrites any existing tool with the same name.
func (r *Registry) Register(tool Tool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tools[tool.Name()] = tool
}

// Execute runs the named tool with the given parameters.
// It emits an EmitToolCall event before execution and an EmitToolResult event
// after execution when an EventEmitter has been configured.
func (r *Registry) Execute(ctx context.Context, name string, params map[string]string) (string, error) {
	r.mu.RLock()
	t, ok := r.tools[name]
	e := r.emitter
	r.mu.RUnlock()
	if !ok {
		return "", fmt.Errorf("tool %q not found", name)
	}
	if e != nil {
		e.EmitToolCall(name, params)
	}
	result, err := t.Execute(ctx, params)
	if e != nil {
		e.EmitToolResult(name, result, err)
	}
	return result, err
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
