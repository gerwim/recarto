package agents

import (
	"fmt"
	"testing"
)

func TestSearchIdentifiers(t *testing.T) {
	cases := []struct {
		pattern string
		want    []string
	}{
		{"handleAugment", []string{"handleAugment"}},
		{"AGENTS.md|CLAUDE.md", []string{"AGENTS.md", "CLAUDE.md"}},
		{"ProjectReference", []string{"ProjectReference"}},
		{`\.Source\b`, []string{"Source"}},
		{`\bfoo\s+bar\b`, []string{"foo", "bar"}},
		{"Add.*Projection", []string{"Add", "Projection"}},
		{`Engine\.Recall`, []string{"Engine.Recall"}},
		{"recarto init", []string{"recarto", "init"}},
		{"internal/agents/handle.go", nil},
		{"search-hints", nil},
		{`[a-z]+_\d{2}`, nil},
		{"404", nil},
		{"**/*Projection*.cs", []string{"Projection"}},
		{"Address*.cs", []string{"Address"}},
		{"*.cs", nil},
		{"src/**/*.go", nil},
		{"internal/**", nil},
	}
	for _, tc := range cases {
		if got := searchIdentifiers(tc.pattern); fmt.Sprint(got) != fmt.Sprint(tc.want) {
			t.Errorf("searchIdentifiers(%q) = %q, want %q", tc.pattern, got, tc.want)
		}
	}
}
