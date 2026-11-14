package mcp

import (
	"slices"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/gerwim/recarto/internal/app"
	"github.com/gerwim/recarto/internal/features"
	"github.com/gerwim/recarto/internal/memory"
)

// inputSchema builds the input schema a's server offers for In, panicking on the
// (compile-time-impossible) reflection error so callers stay expression-simple.
func inputSchema[In any](a *app.App) *jsonschema.Schema {
	s, err := jsonschema.For[In](nil)
	if err != nil {
		panic(err)
	}
	// Without RECARTO_REPOS extras, repo can only name the current repo, so it
	// would cost every tool definition context for nothing.
	if len(a.Repos) == 0 {
		dropProperty(s, "repo")
	}
	return s
}

// dropProperty removes one property from s. Callers only drop omitempty
// fields, so Required never lists them.
func dropProperty(s *jsonschema.Schema, name string) {
	delete(s.Properties, name)
	s.PropertyOrder = slices.DeleteFunc(s.PropertyOrder, func(p string) bool { return p == name })
}

// withEnum sets a JSON-Schema enum on one property, turning a free-text string
// field into a closed set the client validates and the model sees explicitly.
func withEnum(s *jsonschema.Schema, field string, vals ...string) *jsonschema.Schema {
	if p := s.Properties[field]; p != nil {
		for _, v := range vals {
			p.Enum = append(p.Enum, any(v))
		}
	}
	return s
}

// kindStrings lists the kinds record_memory takes: todo only while f has the
// todos feature on.
func kindStrings(f features.Set) []string {
	var out []string
	for _, k := range memory.AllKinds {
		if k != memory.KindTodo || f.On(features.Todos) {
			out = append(out, string(k))
		}
	}
	return out
}

func recordMemorySchema(a *app.App) *jsonschema.Schema {
	s := withEnum(inputSchema[RecordMemoryInput](a), "kind", kindStrings(a.Features)...)
	// scope only chooses a store when a workspace store exists.
	if a.Workspace == nil {
		dropProperty(s, "scope")
		return s
	}
	return withEnum(s, "scope", scopeRepo, scopeWorkspace)
}

func traceCallPathSchema(a *app.App) *jsonschema.Schema {
	return withEnum(inputSchema[TraceCallPathInput](a), "direction", "callers", "callees")
}

func queryGraphSchema(a *app.App) *jsonschema.Schema {
	return withEnum(inputSchema[QueryGraphInput](a), "edge_direction", "in", "out")
}

func reportRecartoIssueSchema(a *app.App) *jsonschema.Schema {
	return withEnum(inputSchema[ReportRecartoIssueInput](a), "signal_type",
		"error", "quality", "friction", "gap")
}
