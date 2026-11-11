package claudecode

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gerwim/recarto/internal/agents"
	"github.com/gerwim/recarto/internal/features"
)

func TestInstallRepoScopeWritesConfigAndIsIdempotent(t *testing.T) {
	t.Setenv("RECARTO_CONFIG", filepath.Join(t.TempDir(), "config.toml"))
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

// TestInstallInstructionsFollowConfig: the block init writes names only the MCP
// tools the user's config leaves on.
func TestInstallInstructionsFollowConfig(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "config.toml")
	if err := features.Save(cfg, features.Set{features.GraphTools: false}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RECARTO_CONFIG", cfg)
	dir := t.TempDir()
	if _, err := Install(agents.Paths{Scope: agents.ScopeRepo, Dir: dir, Home: t.TempDir()}, "recarto"); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "AGENTS.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "record_memory") {
		t.Errorf("AGENTS.md missing the memory tools that are on:\n%s", b)
	}
	if strings.Contains(string(b), "search_graph") {
		t.Errorf("AGENTS.md names graph tools that are off:\n%s", b)
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

// setGOOS overrides the package's OS detection for one test and restores it.
func setGOOS(t *testing.T, v string) {
	t.Helper()
	old := goos
	goos = v
	t.Cleanup(func() { goos = old })
}

func TestHookSpecsPinPowershellOnWindows(t *testing.T) {
	setGOOS(t, "windows")
	for _, hs := range hookSpecs("recarto") {
		if hs.shell != "powershell" {
			t.Errorf("hook %q shell = %q, want powershell", hs.command, hs.shell)
		}
	}
}

func TestHookSpecsLeaveShellDefaultElsewhere(t *testing.T) {
	setGOOS(t, "linux")
	for _, hs := range hookSpecs("recarto") {
		if hs.shell != "" {
			t.Errorf("hook %q shell = %q, want empty on linux", hs.command, hs.shell)
		}
	}
}

func TestInstallPinsPowershellShellOnWindows(t *testing.T) {
	setGOOS(t, "windows")
	dir := t.TempDir()
	home := t.TempDir()
	p := agents.Paths{Scope: agents.ScopeRepo, Dir: dir, Home: home}
	if _, err := Install(p, "recarto"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, ".claude", "settings.json"))
	s := string(b)
	// Every recarto-owned hook entry must carry the powershell shell so the
	// command does not get launched under bash/Git Bash on Windows. The on-disk
	// JSON is indented, so match the spaced form.
	if n := strings.Count(s, `"shell": "powershell"`); n != len(hookSpecs("recarto")) {
		t.Errorf("got %d powershell shells, want %d:\n%s", n, len(hookSpecs("recarto")), s)
	}
}

func TestInstallNoShellFieldOnNonWindows(t *testing.T) {
	setGOOS(t, "linux")
	dir := t.TempDir()
	home := t.TempDir()
	p := agents.Paths{Scope: agents.ScopeRepo, Dir: dir, Home: home}
	if _, err := Install(p, "recarto"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, ".claude", "settings.json"))
	if strings.Contains(string(b), `"shell"`) {
		t.Errorf("non-windows install should not write a shell field:\n%s", b)
	}
}

// A Windows user who installed with an older version has the recarto hooks but
// no shell field (so they break under bash). Re-running init must add the shell
// rather than report the hooks already present.
func TestSyncHooksAddsMissingShellOnWindows(t *testing.T) {
	setGOOS(t, "windows")
	settings := map[string]any{}
	// Seed the current commands WITHOUT a shell, as an older installer would.
	for _, hs := range hookSpecs("recarto") {
		hs.shell = ""
		appendHook(settings, hs)
	}
	if got := syncHooks(settings, "recarto"); got == "already present" {
		t.Fatalf("syncHooks reported %q; expected a re-sync to add the shell", got)
	}
	b, err := json.Marshal(settings)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(b), `"shell":"powershell"`); n != len(hookSpecs("recarto")) {
		t.Errorf("got %d powershell shells after re-sync, want %d:\n%s", n, len(hookSpecs("recarto")), b)
	}
	// And it is now idempotent.
	if got := syncHooks(settings, "recarto"); got != "already present" {
		t.Errorf("second syncHooks = %q, want already present", got)
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

// shellGroups returns the PostToolUse groups matching Bash in a settings file.
func shellGroups(t *testing.T, path string) []map[string]any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Hooks map[string][]map[string]any `json:"hooks"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	for _, g := range doc.Hooks["PostToolUse"] {
		if g["matcher"] == "Bash" {
			out = append(out, g)
		}
	}
	return out
}

// A Stage 1 install (no shell hooks) next to a foreign Bash hook upgrades in
// place at both scopes: one recarto Bash group with a handler per shell
// command, each filtered by its `if`, no duplicates on a re-run, and the
// foreign hook kept.
func TestInstallAddsPerCommandShellHooks(t *testing.T) {
	for _, scope := range []agents.Scope{agents.ScopeRepo, agents.ScopeUser} {
		p := agents.Paths{Scope: scope, Dir: t.TempDir(), Home: t.TempDir()}
		settingsPath := filepath.Join(p.Dir, ".claude", "settings.json")
		if scope == agents.ScopeUser {
			settingsPath = filepath.Join(p.Home, ".claude", "settings.json")
		}
		if err := os.MkdirAll(filepath.Dir(settingsPath), 0o755); err != nil {
			t.Fatal(err)
		}
		stage1 := `{"hooks":{"PostToolUse":[` +
			`{"matcher":"Bash","hooks":[{"type":"command","command":"./audit.sh"}]},` +
			`{"matcher":"Grep|Glob","hooks":[{"type":"command","command":"/old/recarto hook augment --agent claude"}]}]}}`
		if err := os.WriteFile(settingsPath, []byte(stage1), 0o644); err != nil {
			t.Fatal(err)
		}
		res, err := Install(p, "/bin/recarto")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(res.Hooks, "installed") {
			t.Errorf("%v: Hooks = %q, want installed", scope, res.Hooks)
		}
		first, _ := os.ReadFile(settingsPath)
		if res, err := Install(p, "/bin/recarto"); err != nil || res.Hooks != "already present" {
			t.Errorf("%v: re-run = %+v, %v; want already present", scope, res, err)
		}
		if again, _ := os.ReadFile(settingsPath); string(again) != string(first) {
			t.Errorf("%v: re-run changed settings:\n%s", scope, again)
		}

		groups := shellGroups(t, settingsPath)
		if len(groups) != 2 {
			t.Fatalf("%v: %d Bash groups, want the foreign one and recarto's:\n%s", scope, len(groups), first)
		}
		if h := groups[0]["hooks"].([]any); len(h) != 1 || h[0].(map[string]any)["command"] != "./audit.sh" {
			t.Errorf("%v: foreign Bash hook changed: %v", scope, h)
		}
		got := map[string]string{} // if -> command
		for _, h := range groups[1]["hooks"].([]any) {
			hm := h.(map[string]any)
			got[hm["if"].(string)] = hm["command"].(string)
		}
		if len(got) != len(agents.ShellCommands) {
			t.Errorf("%v: %d shell hooks, want %d: %v", scope, len(got), len(agents.ShellCommands), got)
		}
		for _, c := range agents.ShellCommands {
			rule := "Bash(" + c + " *)"
			if c == "git" {
				rule = "Bash(git grep *)"
			}
			if want := "/bin/recarto hook shell --agent claude --cmd " + c; got[rule] != want {
				t.Errorf("%v: if %q runs %q, want %q", scope, rule, got[rule], want)
			}
		}
		if strings.Contains(string(first), "/old/recarto") || strings.Count(string(first), "hook augment") != 1 {
			t.Errorf("%v: stale or duplicate hooks:\n%s", scope, first)
		}
	}
}

// An install whose shell hooks carry another `if` re-syncs rather than
// reporting them present.
func TestSyncHooksRefreshesChangedIf(t *testing.T) {
	settings := map[string]any{}
	for _, hs := range hookSpecs("recarto") {
		if hs.ifRule != "" {
			hs.ifRule = "Bash(*)"
		}
		appendHook(settings, hs)
	}
	if got := syncHooks(settings, "recarto"); got == "already present" {
		t.Fatalf("syncHooks = %q with stale if filters", got)
	}
	if got := syncHooks(settings, "recarto"); got != "already present" {
		t.Errorf("second syncHooks = %q, want already present", got)
	}
}
