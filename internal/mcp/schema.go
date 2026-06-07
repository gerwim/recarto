package mcp

import (
	"github.com/google/jsonschema-go/jsonschema"

	"git.gerwim.nl/gerwim/recarto/internal/memory"
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

func kindStrings() []string {
	out := make([]string, len(memory.AllKinds))
	for i, k := range memory.AllKinds {
		out[i] = string(k)
	}
	return out
}

func recordMemorySchema() *jsonschema.Schema {
	return withEnum(schemaFor[RecordMemoryInput](), "kind", kindStrings()...)
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
