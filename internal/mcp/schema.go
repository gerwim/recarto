package mcp

import (
	"github.com/google/jsonschema-go/jsonschema"

	"github.com/gerwim/recarto/internal/features"
	"github.com/gerwim/recarto/internal/memory"
)

// schemaFor builds the auto-derived input schema for In, panicking on the
// (compile-time-impossible) reflection error so callers stay expression-simple.
func schemaFor[In any]() *jsonschema.Schema {
	s, err := jsonschema.For[In](nil)
	if err != nil {
		panic(err)
	}
	return s
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

func recordMemorySchema(f features.Set) *jsonschema.Schema {
	s := withEnum(schemaFor[RecordMemoryInput](), "kind", kindStrings(f)...)
	return withEnum(s, "scope", scopeRepo, scopeWorkspace)
}

func traceCallPathSchema() *jsonschema.Schema {
	return withEnum(schemaFor[TraceCallPathInput](), "direction", "callers", "callees")
}

func queryGraphSchema() *jsonschema.Schema {
	return withEnum(schemaFor[QueryGraphInput](), "edge_direction", "in", "out")
}

func reportRecartoIssueSchema() *jsonschema.Schema {
	return withEnum(schemaFor[ReportRecartoIssueInput](), "signal_type",
		"error", "quality", "friction", "gap")
}
