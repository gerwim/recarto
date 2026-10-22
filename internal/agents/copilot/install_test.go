package copilot

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"git.gerwim.nl/gerwim/recarto/internal/agents"
)

func TestInstallRepoScope(t *testing.T) {
	dir := t.TempDir()
	home := t.TempDir()
	res, err := Install(agents.Paths{Scope: agents.ScopeRepo, Dir: dir, Home: home}, "/bin/recarto")
	if err != nil {
		t.Fatal(err)
	}
	if res.MCP != "configured" {
		t.Errorf("MCP = %q", res.MCP)
	}
	mcp, _ := os.ReadFile(filepath.Join(dir, ".mcp.json"))
	if !strings.Contains(string(mcp), `"recarto"`) || !strings.Contains(string(mcp), `"serve"`) {
		t.Errorf(".mcp.json: %s", mcp)
	}
	hook, _ := os.ReadFile(filepath.Join(dir, ".github", "hooks", "recarto.json"))
	hs := string(hook)
	for _, want := range []string{`"sessionStart"`, `"postToolUse"`, `"agentStop"`, "hook session-start --agent copilot", "hook post-edit --agent copilot"} {
		if !strings.Contains(hs, want) {
			t.Errorf("hook file missing %q:\n%s", want, hs)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "AGENTS.md")); err != nil {
		t.Errorf("AGENTS.md missing: %v", err)
	}
	res2, _ := Install(agents.Paths{Scope: agents.ScopeRepo, Dir: dir, Home: home}, "/bin/recarto")
	if res2.MCP != "already present" {
		t.Errorf("not idempotent: %+v", res2)
	}
}

func TestEnsureMCPServerRefreshesOnBinaryDrift(t *testing.T) {
	doc := map[string]any{}
	ensureMCPServer(doc, "/old/recarto")
	if !ensureMCPServer(doc, "/new/recarto") {
		t.Fatal("ensureMCPServer should report a change when the binary path drifts")
	}
	rc := doc["mcpServers"].(map[string]any)["recarto"].(map[string]any)
	if rc["command"] != "/new/recarto" {
		t.Errorf("command = %v, want /new/recarto", rc["command"])
	}
	if ensureMCPServer(doc, "/new/recarto") {
		t.Error("ensureMCPServer should be a no-op when nothing drifts")
	}
}

func TestInstallUserScopeTargets(t *testing.T) {
	dir := t.TempDir()
	home := t.TempDir()
	if _, err := Install(agents.Paths{Scope: agents.ScopeUser, Dir: dir, Home: home}, "/bin/recarto"); err != nil {
		t.Fatal(err)
	}
	for _, rel := range [][]string{
		{".copilot", "mcp-config.json"},
		{".copilot", "hooks", "recarto.json"},
		{".copilot", "copilot-instructions.md"},
	} {
		if _, err := os.Stat(filepath.Join(append([]string{home}, rel...)...)); err != nil {
			t.Errorf("user target %v missing: %v", rel, err)
		}
	}
}

// TestHookFileRoutesEachToolOnBothShells pins the per-tool matchers and that
// every entry carries both shell keys (Copilot picks bash or powershell by OS).
func TestHookFileRoutesEachToolOnBothShells(t *testing.T) {
	var doc struct {
		Hooks map[string][]map[string]string `json:"hooks"`
	}
	if err := json.Unmarshal(hookFile(`C:\Program Files\it's\recarto.exe`), &doc); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{} // event/matcher -> recarto hook event
	for event, entries := range doc.Hooks {
		for _, h := range entries {
			bash, ps := h["bash"], h["powershell"]
			if !strings.HasPrefix(bash, `'C:\Program Files\it'\''s\recarto.exe' hook `) ||
				!strings.HasPrefix(ps, `& 'C:\Program Files\it''s\recarto.exe' hook `) {
				t.Errorf("%s/%s: bash %q powershell %q", event, h["matcher"], bash, ps)
			}
			got[event+"/"+h["matcher"]] = strings.Fields(strings.SplitN(bash, " hook ", 2)[1])[0]
		}
	}
	want := map[string]string{
		"sessionStart/":    "session-start",
		"agentStop/":       "stop",
		"postToolUse/view": "post-read", "postToolUse/edit": "post-edit", "postToolUse/create": "post-edit",
		"postToolUse/grep": "augment", "postToolUse/glob": "augment",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("hook routing = %v, want %v", got, want)
	}
}

// TestInstallUpgradesOldFanOutInPlace re-runs init over the pre-matcher hook
// file (every postToolUse ran post-edit, post-read and augment), at both scopes.
func TestInstallUpgradesOldFanOutInPlace(t *testing.T) {
	for _, scope := range []agents.Scope{agents.ScopeRepo, agents.ScopeUser} {
		p := agents.Paths{Scope: scope, Dir: t.TempDir(), Home: t.TempDir()}
		hooksDir := filepath.Join(p.Dir, ".github", "hooks")
		if scope == agents.ScopeUser {
			hooksDir = filepath.Join(p.Home, ".copilot", "hooks")
		}
		if err := os.MkdirAll(hooksDir, 0o755); err != nil {
			t.Fatal(err)
		}
		old := `{"version":1,"hooks":{"postToolUse":[` +
			`{"type":"command","bash":"/old/recarto hook post-edit --agent copilot"},` +
			`{"type":"command","bash":"/old/recarto hook post-read --agent copilot"},` +
			`{"type":"command","bash":"/old/recarto hook augment --agent copilot"}]}}`
		foreign := `{"version":1,"hooks":{"postToolUse":[{"type":"command","bash":"./lint.sh"}]}}`
		if err := os.WriteFile(filepath.Join(hooksDir, "recarto.json"), []byte(old), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(hooksDir, "team.json"), []byte(foreign), 0o644); err != nil {
			t.Fatal(err)
		}
		for range 2 {
			if _, err := Install(p, "/bin/recarto"); err != nil {
				t.Fatal(err)
			}
		}
		got, _ := os.ReadFile(filepath.Join(hooksDir, "recarto.json"))
		if string(got) != string(hookFile("/bin/recarto")) {
			t.Errorf("%v: recarto.json not upgraded in place:\n%s", scope, got)
		}
		if strings.Contains(string(got), "/old/recarto") || strings.Count(string(got), "hook post-read") != 2 { // one entry, two shell keys
			t.Errorf("%v: stale or duplicate hooks:\n%s", scope, got)
		}
		if b, _ := os.ReadFile(filepath.Join(hooksDir, "team.json")); string(b) != foreign {
			t.Errorf("%v: foreign hook file changed: %s", scope, b)
		}
	}
}
