package memory

import (
	"strings"
	"testing"
)

func TestExpandSynonymsBidirectional(t *testing.T) {
	got := strings.Join(expandSynonyms([]string{"cfg"}), " ")
	if !strings.Contains(got, "config") {
		t.Errorf("cfg should expand to config: %q", got)
	}
	got = strings.Join(expandSynonyms([]string{"config"}), " ")
	if !strings.Contains(got, "cfg") {
		t.Errorf("config should expand to cfg (bidirectional): %q", got)
	}
}

func TestBuildSearchTextExpandsAndSplits(t *testing.T) {
	st := BuildSearchText("Fix authCfg bug", "the db connection", []Ref{
		{Type: RefFile, Value: "internal/auth/session.go"},
	})
	for _, want := range []string{"auth", "cfg", "config", "database", "session"} {
		if !strings.Contains(st, want) {
			t.Errorf("search_text missing %q: %q", want, st)
		}
	}
}
