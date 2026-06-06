package claudecode

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"git.gerwim.nl/gerwim/recarto/internal/agents"
)

func TestInstallRepoScopeWritesConfigAndIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	home := t.TempDir()

	// A pre-existing, unrelated hook must be preserved across install.
	if err := os.MkdirAll(filepath.Join(dir, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	pre := `{"hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"keep-me"}]}]}}`
	if err := os.WriteFile(filepath.Join(dir, ".claude", "settings.json"), []byte(pre), 0o644); err != nil {
		t.Fatal(err)
	}

	p := agents.Paths{Scope: agents.ScopeRepo, Dir: dir, Home: home}
	res, err := Install(p, "recarto")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(res.Hooks, "installed") {
		t.Errorf("Hooks = %q, want installed N", res.Hooks)
	}
	if res.MCP != "configured" {
		t.Errorf("MCP = %q, want configured", res.MCP)
	}

	mcp, _ := os.ReadFile(filepath.Join(dir, ".mcp.json"))
	if !strings.Contains(string(mcp), `"recarto"`) || !strings.Contains(string(mcp), `"serve"`) {
		t.Errorf(".mcp.json missing recarto serve:\n%s", mcp)
	}

	settings, _ := os.ReadFile(filepath.Join(dir, ".claude", "settings.json"))
	for _, want := range []string{
		"keep-me",
		"recarto hook session-start --agent claude",
		"recarto hook post-read --agent claude",
		"recarto hook post-edit --agent claude",
		"recarto hook augment --agent claude",
		"recarto hook stop --agent claude",
	} {
		if !strings.Contains(string(settings), want) {
			t.Errorf("settings.json missing %q:\n%s", want, settings)
		}
	}

	agentsmd, _ := os.ReadFile(filepath.Join(dir, "AGENTS.md"))
	if !strings.Contains(string(agentsmd), "record_memory") {
		t.Errorf("AGENTS.md missing instructions:\n%s", agentsmd)
	}

	// Re-running installs nothing new.
	res2, err := Install(p, "recarto")
	if err != nil {
		t.Fatal(err)
	}
	if res2.Hooks != "already present" || res2.MCP != "already present" {
		t.Errorf("second install not idempotent: %+v", res2)
	}
}

func TestInstallReplacesStaleHooks(t *testing.T) {
	dir := t.TempDir()
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Pre-existing settings: a foreign hook to preserve, plus a STALE recarto hook
	// shipped by an older version (old binary path + renamed event).
	pre := `{"hooks":{` +
		`"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"keep-me"}]}],` +
		`"PostToolUse":[{"matcher":"Read","hooks":[{"type":"command","command":"/old/recarto hook post-read-LEGACY --agent claude"}]}]` +
		`}}`
	if err := os.WriteFile(filepath.Join(dir, ".claude", "settings.json"), []byte(pre), 0o644); err != nil {
		t.Fatal(err)
	}

	p := agents.Paths{Scope: agents.ScopeRepo, Dir: dir, Home: home}
	if _, err := Install(p, "recarto"); err != nil {
		t.Fatal(err)
	}

	b, _ := os.ReadFile(filepath.Join(dir, ".claude", "settings.json"))
	s := string(b)
	if strings.Contains(s, "post-read-LEGACY") {
		t.Errorf("stale recarto hook not pruned:\n%s", s)
	}
	if strings.Contains(s, "/old/recarto") {
		t.Errorf("stale recarto binary path not pruned:\n%s", s)
	}
	if !strings.Contains(s, "keep-me") {
		t.Errorf("foreign hook not preserved:\n%s", s)
	}
	for _, want := range []string{
		"recarto hook session-start --agent claude",
		"recarto hook post-read --agent claude",
		"recarto hook stop --agent claude",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("current hook %q missing:\n%s", want, s)
		}
	}
}

func TestInstallUserScopeTargets(t *testing.T) {
	dir := t.TempDir()
	home := t.TempDir()
	if _, err := Install(agents.Paths{Scope: agents.ScopeUser, Dir: dir, Home: home}, "recarto"); err != nil {
		t.Fatal(err)
	}
	for _, rel := range [][]string{
		{".claude.json"},
		{".claude", "settings.json"},
		{".claude", "CLAUDE.md"},
	} {
		if _, err := os.Stat(filepath.Join(append([]string{home}, rel...)...)); err != nil {
			t.Errorf("user target %v missing: %v", rel, err)
		}
	}
}
