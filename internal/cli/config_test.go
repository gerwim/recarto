package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"git.gerwim.nl/gerwim/recarto/internal/features"
)

// useConfig points RECARTO_CONFIG at a fresh path holding content ("" for no file).
func useConfig(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "recarto", "config.toml")
	t.Setenv("RECARTO_CONFIG", p)
	if content != "" {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return p
}

// TestConfigWithoutTerminalLists: off a terminal `recarto config` prints every
// feature, as `config list` does.
func TestConfigWithoutTerminalLists(t *testing.T) {
	p := useConfig(t, "todos = false\n")
	for _, args := range [][]string{nil, {"list"}} {
		var out bytes.Buffer
		if err := Run(append([]string{"config"}, args...), &out); err != nil {
			t.Fatal(err)
		}
		text := out.String()
		if !strings.HasPrefix(text, p+"\n") || !strings.Contains(text, "recarto config enable|disable <feature>") {
			t.Errorf("list lacks the path or the hint:\n%s", text)
		}
		for _, f := range features.All() {
			state := "on "
			if f == features.Todos {
				state = "off"
			}
			row := regexp.MustCompile(`(?m)^  ` + state + `  ` + regexp.QuoteMeta(f.Key) + ` +` + regexp.QuoteMeta(f.Desc) + `$`)
			if !row.MatchString(text) || !strings.Contains(text, "\n"+f.Group+"\n") {
				t.Errorf("%s missing or wrong:\n%s", f.Key, text)
			}
		}
	}
}

func TestConfigEnableDisable(t *testing.T) {
	p := useConfig(t, "")
	var out bytes.Buffer
	if err := Run([]string{"config", "disable", "stop-nudge", "todos"}, &out); err != nil {
		t.Fatal(err)
	}
	if out.String() != "todos: off\nstop-nudge: off\n" {
		t.Errorf("output %q", out.String())
	}
	out.Reset()
	if err := Run([]string{"config", "enable", "todos"}, &out); err != nil {
		t.Fatal(err)
	}
	if out.String() != "todos: on\n" {
		t.Errorf("output %q", out.String())
	}
	s, err := features.Read(p)
	if err != nil {
		t.Fatal(err)
	}
	if !s.On(features.Todos) || s.On(features.StopNudge) || !s.On(features.Sync) {
		t.Errorf("file holds %v", s)
	}
}

func TestConfigRejectsUnknownFeatures(t *testing.T) {
	p := useConfig(t, "")
	err := Run([]string{"config", "disable", "todo"}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), `unknown feature "todo" (did you mean todos?)`) {
		t.Errorf("err = %v", err)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Error("a failed disable wrote the file")
	}
	for _, args := range [][]string{{"config", "disable"}, {"config", "frob"}} {
		if err := Run(args, &bytes.Buffer{}); err == nil {
			t.Errorf("%v: want an error", args)
		}
	}
}

func TestConfigReportsABadFileAndLeavesIt(t *testing.T) {
	p := useConfig(t, "# mine\ntodo = false\n")
	var out bytes.Buffer
	err := Run([]string{"config", "list"}, &out)
	if err == nil || !strings.Contains(err.Error(), p+`:2: unknown feature "todo" (did you mean todos?)`) {
		t.Errorf("list err = %v", err)
	}
	if !strings.Contains(out.String(), "session-memories") {
		t.Errorf("list must still show the features:\n%s", out.String())
	}
	if err := Run([]string{"config", "disable", "sync"}, &out); err == nil {
		t.Error("disable rewrote a file it cannot read")
	}
	if b, _ := os.ReadFile(p); string(b) != "# mine\ntodo = false\n" {
		t.Errorf("file changed: %q", b)
	}
}

func TestConfigPath(t *testing.T) {
	p := useConfig(t, "")
	var out bytes.Buffer
	if err := Run([]string{"config", "path"}, &out); err != nil || out.String() != p+"\n" {
		t.Errorf("path = %q, %v", out.String(), err)
	}
}

func TestDoctorReportsConfig(t *testing.T) {
	t.Setenv("RECARTO_SYNC_URL", "") // doctor would ping a sync server set in the environment
	a := newTestApp(t, "main")
	ctx := context.Background()
	doctor := func(content string) (string, error) {
		useConfig(t, content)
		var out bytes.Buffer
		err := Doctor(ctx, a, &out)
		return out.String(), err
	}
	if out, err := doctor(""); err != nil || !strings.Contains(out, "[ok] config: all features on\n") {
		t.Errorf("no file: %v\n%s", err, out)
	}
	if out, err := doctor("todos = false\nsync = false\n"); err != nil || !strings.Contains(out, "[ok] config: 2 off: todos, sync\n") {
		t.Errorf("two off: %v\n%s", err, out)
	}
	if out, err := doctor("todo = false\n"); err == nil || !strings.Contains(out, `[FAIL] config: `) || !strings.Contains(out, `:1: unknown feature "todo"`) {
		t.Errorf("bad file: %v\n%s", err, out)
	}
}

func TestSyncSaysWhenSwitchedOff(t *testing.T) {
	t.Setenv("RECARTO_SYNC_URL", "")
	a := newTestApp(t, "main")
	a.Features = features.Set{features.Sync: false}
	var out bytes.Buffer
	if err := Sync(context.Background(), a, &out, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out.String(), "sync: off") {
		t.Errorf("got %q", out.String())
	}
}
