package agents

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteInstructionsBlockIdempotent(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "AGENTS.md")
	if err := os.WriteFile(p, []byte("# My rules\n\nkeep me\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := WriteInstructionsBlock(p); err != nil {
			t.Fatal(err)
		}
	}
	b, _ := os.ReadFile(p)
	s := string(b)
	if !strings.Contains(s, "keep me") {
		t.Errorf("clobbered existing content:\n%s", s)
	}
	if strings.Count(s, recartoBlockBegin) != 1 || strings.Count(s, recartoBlockEnd) != 1 {
		t.Errorf("block not idempotent:\n%s", s)
	}
	if !strings.Contains(s, "record_memory") {
		t.Errorf("missing recarto instructions body:\n%s", s)
	}
}

func TestWriteInstructionsBlockCreatesFileWithParents(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "nested", "AGENTS.md")
	if err := WriteInstructionsBlock(p); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), recartoBlockBegin) {
		t.Errorf("new file missing block:\n%s", b)
	}
}

func TestScopePathsRepoVsUser(t *testing.T) {
	p := Paths{Scope: ScopeRepo, Dir: "/repo", Home: "/home/u"}
	if got := p.RepoFile("AGENTS.md"); got != filepath.FromSlash("/repo/AGENTS.md") {
		t.Errorf("RepoFile = %s", got)
	}
	if got := p.UserFile(".copilot", "mcp-config.json"); got != filepath.FromSlash("/home/u/.copilot/mcp-config.json") {
		t.Errorf("UserFile = %s", got)
	}
	if ScopeUser.String() != "user" || ScopeRepo.String() != "repo" {
		t.Errorf("scope strings wrong")
	}
}
