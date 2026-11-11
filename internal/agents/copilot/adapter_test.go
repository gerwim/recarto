package copilot

import (
	"strings"
	"testing"

	"github.com/gerwim/recarto/internal/agents"
)

func TestDecodePostToolUseRoutesAndExtractsPath(t *testing.T) {
	in := strings.NewReader(`{"sessionId":"s","toolName":"edit_file","toolArgs":{"path":"/a/b.go"}}`)
	ev, err := Adapter{}.Decode(agents.KindPostEdit, in)
	if err != nil {
		t.Fatal(err)
	}
	if ev.FilePath != "/a/b.go" || ev.Agent != "copilot" {
		t.Errorf("ev = %+v", ev)
	}
}

func TestDecodeAugmentPattern(t *testing.T) {
	in := strings.NewReader(`{"sessionId":"s","toolName":"grep","toolArgs":{"pattern":"foo"}}`)
	ev, err := Adapter{}.Decode(agents.KindAugment, in)
	if err != nil {
		t.Fatal(err)
	}
	if ev.Pattern != "foo" {
		t.Errorf("pattern = %q", ev.Pattern)
	}
}

func TestEncodeAdditionalContext(t *testing.T) {
	var sb strings.Builder
	if err := (Adapter{}).Encode(agents.KindSessionStart, &sb, agents.Result{Context: "hi"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sb.String(), `"additionalContext"`) || !strings.Contains(sb.String(), "hi") {
		t.Errorf("out = %s", sb.String())
	}
}

func TestEncodeStopIsEmptyObject(t *testing.T) {
	var sb strings.Builder
	if err := (Adapter{}).Encode(agents.KindStop, &sb, agents.Result{SystemMessage: "x"}); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(sb.String()) != "{}" {
		t.Errorf("stop should emit empty object, got %s", sb.String())
	}
}

func TestDecodeRoutesBuiltinToolsToTheirOwnHook(t *testing.T) {
	payload := func(tool, args string) string {
		return `{"sessionId":"s","toolName":"` + tool + `","toolArgs":` + args + `}`
	}
	view := payload("view", `{"path":"/r/a.go"}`)
	edit := payload("edit", `{"path":"/r/a.go","old_str":"x","new_str":"y"}`)
	create := payload("create", `{"path":"/r/b.go","file_text":"package r"}`)
	grep := payload("grep", `{"pattern":"Foo","path":"/r"}`)
	glob := payload("glob", `{"pattern":"**/*Foo*.go","path":"/r"}`)
	bash := payload("bash", `{"command":"go test ./...","description":"run"}`)
	cases := []struct {
		kind          agents.EventKind
		in            string
		path, pattern string
	}{
		{agents.KindPostRead, view, "/r/a.go", ""},
		{agents.KindPostEdit, view, "", ""}, // a view never re-indexes or counts as an edit
		{agents.KindAugment, view, "", ""},
		{agents.KindPostEdit, edit, "/r/a.go", ""},
		{agents.KindPostEdit, create, "/r/b.go", ""},
		{agents.KindPostRead, edit, "", ""}, // edit/create never count as reads
		{agents.KindPostRead, create, "", ""},
		{agents.KindAugment, grep, "/r", "Foo"}, // augment reads only the pattern
		{agents.KindAugment, glob, "/r", "**/*Foo*.go"},
		{agents.KindPostRead, grep, "", ""}, // a search's dir is neither a read nor an edit
		{agents.KindPostEdit, glob, "", ""},
		{agents.KindPostRead, bash, "", ""},
		{agents.KindPostEdit, bash, "", ""},
		{agents.KindAugment, bash, "", ""},
	}
	for _, c := range cases {
		ev, err := Adapter{}.Decode(c.kind, strings.NewReader(c.in))
		if err != nil {
			t.Fatal(err)
		}
		if ev.FilePath != c.path || ev.Pattern != c.pattern || ev.SessionID != "s" {
			t.Errorf("kind %d, %s: path %q pattern %q, want %q %q", c.kind, c.in, ev.FilePath, ev.Pattern, c.path, c.pattern)
		}
	}
}

// The shell hook reads the bash tool's command, run in the payload's cwd, and
// no other tool's arguments — not even another tool's "command".
func TestDecodeShellReadsOnlyBash(t *testing.T) {
	for tool, want := range map[string]string{"bash": "rg Foo", "view": "", "powershell": "", "mcp_run": ""} {
		in := `{"sessionId":"s","cwd":"/r","toolName":"` + tool + `","toolArgs":"{\"command\":\"rg Foo\",\"path\":\"/r/a.go\"}"}`
		ev, err := Adapter{}.Decode(agents.KindShell, strings.NewReader(in))
		if err != nil {
			t.Fatal(err)
		}
		if ev.Command != want || (want != "" && ev.CWD != "/r") || ev.FilePath != "" {
			t.Errorf("%s: command %q cwd %q path %q, want command %q", tool, ev.Command, ev.CWD, ev.FilePath, want)
		}
	}
}

func TestDecodeAcceptsStringToolArgs(t *testing.T) {
	in := strings.NewReader(`{"sessionId":"s","toolName":"view","toolArgs":"{\"path\":\"/a/b.go\"}"}`)
	ev, err := Adapter{}.Decode(agents.KindPostRead, in)
	if err != nil {
		t.Fatal(err)
	}
	if ev.FilePath != "/a/b.go" {
		t.Errorf("path = %q from a JSON-string toolArgs", ev.FilePath)
	}
}
