// Package tools defines the tool interface and registry the agent calls into.
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"

	"github.com/MavrkAI/Mirrin/internal/llm"
)

// Risk classifies what a tool can do to the world.
type Risk int

const (
	// RiskRead only observes: reading a calendar, fetching a page.
	RiskRead Risk = iota
	// RiskWrite changes state in a recoverable way: creating an event, sending an email.
	RiskWrite
	// RiskDangerous can cause hard-to-reverse harm: shell commands, deleting files, payments.
	RiskDangerous
)

func (r Risk) String() string {
	switch r {
	case RiskRead:
		return "read"
	case RiskWrite:
		return "write"
	default:
		return "dangerous"
	}
}

// Call carries per-invocation context to a tool.
type Call struct {
	// ChatKey identifies the conversation that triggered the call.
	ChatKey string
	Input   json.RawMessage
}

// Tool is one capability the agent can invoke.
type Tool interface {
	Spec() llm.ToolSpec
	Risk() Risk
	Run(ctx context.Context, call Call) (string, error)
}

// CallRisker is a Tool whose risk depends on the call (a browser click that
// turns out to be "Pay now").
type CallRisker interface {
	RiskFor(ctx context.Context, call Call) Risk
}

// Func is a Tool built from a closure.
type Func struct {
	spec    llm.ToolSpec
	risk    Risk
	fn      func(ctx context.Context, call Call) (string, error)
	riskFor func(ctx context.Context, call Call) Risk
	hidden  bool
}

// Hider is a Tool that may be kept from the model: registered and runnable
// by the daemon (an approval the owner said yes to), never offered in
// Specs or Names. The agent must also refuse a call the model makes to it
// by name (Offered).
type Hider interface {
	Hidden() bool
}

// Hide keeps the tool from the model wherever it is registered.
func (f *Func) Hide() *Func {
	f.hidden = true
	return f
}

// Hidden implements Hider.
func (f *Func) Hidden() bool { return f.hidden }

// WithRiskFor lets the tool raise its risk for particular calls.
func (f *Func) WithRiskFor(fn func(ctx context.Context, call Call) Risk) *Func {
	f.riskFor = fn
	return f
}

// RiskFor implements CallRisker.
func (f *Func) RiskFor(ctx context.Context, call Call) Risk {
	if f.riskFor == nil {
		return f.risk
	}
	if r := f.riskFor(ctx, call); r > f.risk {
		return r
	}
	return f.risk
}

// New builds a Tool from a spec and function.
func New(name, description string, schema map[string]any, risk Risk, fn func(ctx context.Context, call Call) (string, error)) *Func {
	if schema == nil {
		schema = Schema(nil)
	}
	return &Func{spec: llm.ToolSpec{Name: name, Description: description, Schema: schema}, risk: risk, fn: fn}
}

func (f *Func) Spec() llm.ToolSpec { return f.spec }
func (f *Func) Risk() Risk         { return f.risk }
func (f *Func) Run(ctx context.Context, call Call) (string, error) {
	return f.fn(ctx, call)
}

// Prop describes one JSON Schema property.
type Prop struct {
	Type        string
	Description string
	Required    bool
	Enum        []string
}

// Schema builds a JSON Schema object from named properties.
func Schema(props map[string]Prop) map[string]any {
	properties := map[string]any{}
	required := []string{}
	names := make([]string, 0, len(props))
	for n := range props {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		p := props[n]
		def := map[string]any{"type": p.Type, "description": p.Description}
		if len(p.Enum) > 0 {
			def["enum"] = p.Enum
		}
		properties[n] = def
		if p.Required {
			required = append(required, n)
		}
	}
	return map[string]any{"type": "object", "properties": properties, "required": required}
}

// Decode unmarshals a call's input into v.
func Decode(call Call, v any) error {
	if len(call.Input) == 0 {
		return nil
	}
	return json.Unmarshal(call.Input, v)
}

// Registry holds every tool available to the agent.
type Registry struct {
	mu    sync.RWMutex
	tools map[string]Tool
}

// NewRegistry creates an empty registry.
func NewRegistry() *Registry { return &Registry{tools: map[string]Tool{}} }

// Register adds tools, replacing any with the same name.
func (r *Registry) Register(ts ...Tool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, t := range ts {
		r.tools[t.Spec().Name] = t
	}
}

// isHidden reports whether t is kept from the model (Hider).
func isHidden(t Tool) bool {
	h, ok := t.(Hider)
	return ok && h.Hidden()
}

// Offered reports whether the model is offered the named tool: it is
// registered and not hidden. A call the model makes to a tool it wasn't
// offered should be refused as unknown.
func (r *Registry) Offered(name string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tools[name]
	return ok && !isHidden(t)
}

// Unregister removes a tool by name.
func (r *Registry) Unregister(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.tools, name)
}

// Get returns a tool by name.
func (r *Registry) Get(name string) (Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tools[name]
	return t, ok
}

// Specs returns the specs of the tools the model is offered (not hidden
// ones) in a stable order.
func (r *Registry) Specs() []llm.ToolSpec {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.tools))
	for n, t := range r.tools {
		if isHidden(t) {
			continue
		}
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]llm.ToolSpec, 0, len(names))
	for _, n := range names {
		out = append(out, r.tools[n].Spec())
	}
	return out
}

// Names lists the names of the tools the model is offered.
func (r *Registry) Names() []string {
	specs := r.Specs()
	out := make([]string, len(specs))
	for i, s := range specs {
		out[i] = s.Name
	}
	return out
}

// Run executes a tool by name.
func (r *Registry) Run(ctx context.Context, name string, call Call) (string, error) {
	t, ok := r.Get(name)
	if !ok {
		return "", fmt.Errorf("unknown tool %q", name)
	}
	return t.Run(ctx, call)
}
