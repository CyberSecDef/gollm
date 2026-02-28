package agents

import (
	"fmt"
	"sync"
	"time"

	"github.com/cybersecdef/gollm/internal/bus"
	"github.com/cybersecdef/gollm/internal/llm"
	"github.com/cybersecdef/gollm/internal/tools"
)

// builtinProfiles lists the pre-defined SME skill profiles shipped with gollm.
var builtinProfiles = []WorkerSkills{
	{
		Role: "Researcher",
		Description: "You are a research specialist. Your job is to gather, verify, and summarise " +
			"information from available sources. Provide well-sourced, factual answers with clear citations.",
		AllowedTools: []string{"fetch_url", "read_file"},
		RateLimit:    200 * time.Millisecond,
		Timeout:      3 * time.Minute,
	},
	{
		Role: "Analyst",
		Description: "You are a data and requirements analyst. Your job is to examine information, " +
			"identify patterns, and produce structured findings with actionable recommendations.",
		AllowedTools: []string{"read_file"},
		RateLimit:    0,
		Timeout:      3 * time.Minute,
	},
	{
		Role: "Coder",
		Description: "You are a software engineering specialist. Your job is to write, review, and " +
			"execute code. Prefer idiomatic, well-tested, and secure implementations.",
		AllowedTools: []string{"exec_command", "read_file", "write_file"},
		RateLimit:    500 * time.Millisecond,
		Timeout:      5 * time.Minute,
	},
	{
		Role: "Writer",
		Description: "You are a technical writing specialist. Your job is to draft, edit, and " +
			"structure documentation, reports, and communications that are clear and concise.",
		AllowedTools: []string{},
		RateLimit:    0,
		Timeout:      3 * time.Minute,
	},
	{
		Role: "Reviewer",
		Description: "You are a quality-assurance specialist. Your job is to critically evaluate " +
			"work products, identify issues, and suggest concrete improvements.",
		AllowedTools: []string{},
		RateLimit:    0,
		Timeout:      3 * time.Minute,
	},
	{
		Role: "FileOps",
		Description: "You are a file-operations specialist. Your job is to read, write, and patch " +
			"files on disk as directed.",
		AllowedTools: []string{"read_file", "write_file", "patch_file"},
		RateLimit:    100 * time.Millisecond,
		Timeout:      2 * time.Minute,
	},
}

// WorkerFactory creates role-specific Worker agents from registered skill profiles.
// It ships with built-in profiles for common SME roles and supports custom profiles
// via Register.
type WorkerFactory struct {
	mu       sync.RWMutex
	profiles map[string]WorkerSkills

	bus      *bus.Bus
	llm      llm.Client
	registry *tools.Registry
}

// NewWorkerFactory creates a WorkerFactory pre-loaded with the built-in SME profiles.
func NewWorkerFactory(b *bus.Bus, client llm.Client, registry *tools.Registry) *WorkerFactory {
	f := &WorkerFactory{
		profiles: make(map[string]WorkerSkills, len(builtinProfiles)),
		bus:      b,
		llm:      client,
		registry: registry,
	}
	for _, p := range builtinProfiles {
		f.profiles[p.Role] = p
	}
	return f
}

// Register adds or replaces a skill profile. Use this to extend the factory with
// domain-specific SME definitions.
func (f *WorkerFactory) Register(skills WorkerSkills) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.profiles[skills.Role] = skills
}

// Profiles returns a copy of all registered skill profiles keyed by role name.
func (f *WorkerFactory) Profiles() map[string]WorkerSkills {
	f.mu.RLock()
	defer f.mu.RUnlock()
	out := make(map[string]WorkerSkills, len(f.profiles))
	for k, v := range f.profiles {
		out[k] = v
	}
	return out
}

// NewWorker creates a Worker for the given role using the matching registered
// skill profile. If no exact match is found the role is used as a bare label
// (no tools, default timeout). The task and id identify the specific work item.
func (f *WorkerFactory) NewWorker(id, role, task string) *Worker {
	f.mu.RLock()
	skills, ok := f.profiles[role]
	f.mu.RUnlock()

	if !ok {
		// Fallback: bare role, no tools.
		skills = WorkerSkills{Role: role}
	}

	return NewWorkerWithSkills(id, task, skills, f.bus, f.llm, f.registry)
}

// KnownRoles returns the names of all registered skill profiles.
func (f *WorkerFactory) KnownRoles() []string {
	f.mu.RLock()
	defer f.mu.RUnlock()
	roles := make([]string, 0, len(f.profiles))
	for role := range f.profiles {
		roles = append(roles, role)
	}
	return roles
}

// newWorkerID returns a sequential worker ID from a shared counter.
// Exposed so the orchestrator can use the same naming scheme.
func newWorkerID(seq int64) string {
	return fmt.Sprintf("worker-%d", seq)
}
