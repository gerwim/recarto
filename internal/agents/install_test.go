package agents

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/gerwim/recarto/internal/features"
)

func TestWriteInstructionsBlockIdempotent(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "AGENTS.md")
	if err := os.WriteFile(p, []byte("# My rules\n\nkeep me\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := WriteInstructionsBlock(p, nil); err != nil {
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

	// A different feature set replaces the block in place rather than appending
	// a second one.
	if err := WriteInstructionsBlock(p, off(features.GraphTools)); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(p)
	s = string(b)
	if strings.Count(s, recartoBlockBegin) != 1 || strings.Count(s, recartoBlockEnd) != 1 {
		t.Errorf("feature change appended a second block:\n%s", s)
	}
	if !strings.Contains(s, "keep me") || !strings.Contains(s, "record_memory") {
		t.Errorf("feature change clobbered content:\n%s", s)
	}
	if strings.Contains(s, "search_graph") {
		t.Errorf("graph-tools off, block still names search_graph:\n%s", s)
	}

	if err := WriteInstructionsBlock(p, nil); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(p)
	s = string(b)
	if strings.Count(s, recartoBlockBegin) != 1 || strings.Count(s, recartoBlockEnd) != 1 {
		t.Errorf("switching back appended a second block:\n%s", s)
	}
	if !strings.Contains(s, "search_graph") {
		t.Errorf("switching back did not restore the graph bullet:\n%s", s)
	}
}

func TestWriteInstructionsBlockReplaces17Block(t *testing.T) {
	old, err := os.ReadFile("testdata/instructions-1.7.md")
	if err != nil {
		t.Fatal(err)
	}
	before, after := "# My rules\n\nkeep me\n\n", "\n\n## Also mine\n\nand me\n"
	p := filepath.Join(t.TempDir(), "CLAUDE.md")
	if err := os.WriteFile(p, []byte(before+recartoBlockBegin+"\n"+string(old)+recartoBlockEnd+after), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteInstructionsBlock(p, nil); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if want := before + recartoBlockBegin + "\n" + Instructions(nil) + "\n" + recartoBlockEnd + after; string(got) != want {
		t.Errorf("1.7.x block not replaced cleanly:\ngot  %q\nwant %q", got, want)
	}
}

func TestWriteInstructionsBlockCreatesFileWithParents(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "nested", "AGENTS.md")
	if err := WriteInstructionsBlock(p, nil); err != nil {
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

func TestDetectFindsOpenCode2Alias(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PATH lookup needs an .exe on Windows")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "opencode2"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	if got := Detect(); !reflect.DeepEqual(got, []string{"opencode"}) {
		t.Errorf("Detect() = %v, want [opencode]", got)
	}
}
