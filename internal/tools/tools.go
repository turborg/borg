// Package tools holds the capabilities the agent can invoke (read/write files,
// run commands, search), each described by a JSON schema for tool-calling.
package tools

import (
	"context"
	"encoding/json"
)

// Tool is a single capability the agent can invoke. Mutating tools (writes,
// shell) are gated behind a user permission prompt before Execute runs.
type Tool interface {
	Name() string
	Description() string
	Schema() json.RawMessage
	Mutating() bool
	Execute(ctx context.Context, args json.RawMessage) (string, error)
}

// Definition is the OpenAI tool wire shape advertised to the model.
type Definition struct {
	Type     string      `json:"type"`
	Function FunctionDef `json:"function"`
}

// FunctionDef names + describes a tool and its JSON-schema parameters.
type FunctionDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// ToolInfo contains human-readable information about a registered tool.
type ToolInfo struct {
	Name        string
	Description string
	Mutating    bool
}

// Registry holds the tools available to the agent, preserving order.
type Registry struct {
	tools map[string]Tool
	order []string
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry { return &Registry{tools: map[string]Tool{}} }

// Register adds (or replaces) a tool.
func (r *Registry) Register(t Tool) {
	if _, ok := r.tools[t.Name()]; !ok {
		r.order = append(r.order, t.Name())
	}
	r.tools[t.Name()] = t
}

// Get returns a tool by name.
func (r *Registry) Get(name string) (Tool, bool) {
	t, ok := r.tools[name]
	return t, ok
}

// Names returns the registered tool names in registration order.
func (r *Registry) Names() []string {
	out := make([]string, len(r.order))
	copy(out, r.order)
	return out
}

// Definitions returns the registered tools in their advertised wire shape.
func (r *Registry) Definitions() []Definition {
	defs := make([]Definition, 0, len(r.order))
	for _, name := range r.order {
		t := r.tools[name]
		defs = append(defs, Definition{
			Type:     "function",
			Function: FunctionDef{Name: t.Name(), Description: t.Description(), Parameters: t.Schema()},
		})
	}
	return defs
}

// ToolList returns the registered tools in registration order.
func (r *Registry) ToolList() []ToolInfo {
	out := make([]ToolInfo, 0, len(r.order))

	for _, name := range r.order {
		t := r.tools[name]

		out = append(out, ToolInfo{
			Name:        t.Name(),
			Description: t.Description(),
			Mutating:    t.Mutating(),
		})
	}

	return out
}

// DefaultRegistry returns the standard coding-agent toolset.
func DefaultRegistry() *Registry {
	bg := newBGManager() // shared by bash / bash_output / kill_shell
	r := NewRegistry()
	r.Register(readFile{})
	r.Register(listDir{})
	r.Register(grepTool{})
	r.Register(globTool{})
	r.Register(writeFile{})
	r.Register(editFile{})
	r.Register(editLines{})
	r.Register(verifyTool{})
	r.Register(bashTool{bg: bg})
	r.Register(bashOutputTool{bg: bg})
	r.Register(killShellTool{bg: bg})
	r.Register(askUser{})    // interactive sentinel — the loop drives the UI prompt
	r.Register(finishTool{}) // terminal sentinel for guided/required tool-calling
	return r
}

// truncate caps a tool's output so a single result can't blow the context.
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "\n… [truncated]"
}
