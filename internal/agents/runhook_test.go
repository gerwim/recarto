package agents

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"git.gerwim.nl/gerwim/recarto/internal/app"
	"git.gerwim.nl/gerwim/recarto/internal/clock"
	"git.gerwim.nl/gerwim/recarto/internal/memory"
)

// fixedAdapter decodes every payload to ev and encodes results verbatim.
type fixedAdapter struct{ ev Event }

func (f fixedAdapter) Decode(kind EventKind, _ io.Reader) (Event, error) {
	ev := f.ev
	ev.Kind = kind
	return ev, nil
}

func (fixedAdapter) Encode(_ EventKind, out io.Writer, res Result) error {
	_, err := fmt.Fprintf(out, "%+v", res)
	return err
}

var idleEvents = []Event{
	{Kind: KindPostRead, SessionID: "s"},
	{Kind: KindPostEdit, SessionID: "s"},
	{Kind: KindPostEdit, SessionID: "s", FilePath: "relative/x.go"},
	{Kind: KindAugment, SessionID: "s"},
	{Kind: KindAugment, SessionID: "s", Pattern: "*.cs"},
	{Kind: KindAugment, SessionID: "s", Pattern: "src/**/*.go"},
	{Kind: KindStop},
	{Kind: KindShell, SessionID: "s", Command: "go test ./... | grep FAIL"},
	{Kind: KindShell, SessionID: "s", Command: "ps aux | grep foo"},
	{Kind: KindShell, SessionID: "s", Command: "git log --grep fix && go build ./..."},
	{Kind: KindShell, SessionID: "s", Command: "cat > /tmp/f.go <<'EOF'\npackage f\nEOF"},
	{Kind: KindShell, SessionID: "s", Command: "rg Foo | head", Trigger: "head"}, // the rg hook answers it
	{Kind: KindShell, SessionID: "s", Command: "echo 'unterminated"},
}

func TestRunHookIdleEventNeverOpensApp(t *testing.T) {
	refuse := func(Event) (*app.App, error) {
		t.Fatal("an idle hook must not open the App")
		return nil, nil
	}
	for _, ev := range idleEvents {
		var out bytes.Buffer
		if err := RunHook(context.Background(), refuse, fixedAdapter{ev}, ev.Kind, strings.NewReader(""), &out); err != nil {
			t.Fatalf("%+v: %v", ev, err)
		}
		if want := fmt.Sprintf("%+v", Result{}); out.String() != want {
			t.Errorf("%+v: output %q, want the empty result %q", ev, out.String(), want)
		}
	}
}

// TestIdleEventsAreHandlerNoOps pins idle to the handlers' own early returns:
// skipping Handle for an idle event must change neither output nor any table.
func TestIdleEventsAreHandlerNoOps(t *testing.T) {
	a := newTestApp(t, "main")
	ctx := context.Background()
	snapshot := func() string {
		var counters, sessions int
		_ = a.Store.DB.QueryRowContext(ctx, `SELECT COUNT(*) + COALESCE(SUM(value), 0) FROM counters`).Scan(&counters)
		_ = a.Store.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM sessions`).Scan(&sessions)
		return fmt.Sprint(counters, sessions)
	}
	before := snapshot()
	for _, ev := range idleEvents {
		for range 2 { // twice: a repeat must stay quiet too
			if res, err := Handle(ctx, a, ev); err != nil || res != (Result{}) {
				t.Errorf("%+v: Handle = %+v, %v; want the empty result", ev, res, err)
			}
		}
	}
	if after := snapshot(); after != before {
		t.Errorf("idle events wrote state: %s -> %s", before, after)
	}
}

func TestRunHookOpenFailureWritesNothing(t *testing.T) {
	clearRecartoEnv(t) // the user's config could switch these hooks off before they open
	boom := errors.New("store cannot open")
	file := filepath.Join(t.TempDir(), "a.go") // absolute on every OS
	for _, ev := range []Event{
		{Kind: KindSessionStart, SessionID: "s"},
		{Kind: KindPostRead, SessionID: "s", FilePath: file},
		{Kind: KindPostEdit, SessionID: "s", FilePath: file},
		{Kind: KindAugment, SessionID: "s", Pattern: "Foo"},
		{Kind: KindStop, SessionID: "s"},
	} {
		var out bytes.Buffer
		err := RunHook(context.Background(), func(Event) (*app.App, error) { return nil, boom }, fixedAdapter{ev}, ev.Kind, strings.NewReader(""), &out)
		if !errors.Is(err, boom) || out.Len() != 0 {
			t.Errorf("%+v: err %v, output %q; want the open error and no output", ev, err, out.String())
		}
	}
}

// TestRunHookHandlerErrorWritesNothing: a handler that fails after the store
// opened — even once its advisory context is built — writes nothing.
func TestRunHookHandlerErrorWritesNothing(t *testing.T) {
	clearRecartoEnv(t)
	t.Setenv("RECARTO_CACHE_DIR", t.TempDir())
	root := t.TempDir() // absolute on every OS
	a, err := app.Open(app.Config{CWD: root, Runner: fakeGit{branch: "main", root: root}, Clock: clock.NewFake(time.UnixMilli(1_700_000_000_000))})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	ctx := context.Background()
	file := filepath.Join(root, "a.go")
	if err := os.WriteFile(file, []byte("package a\n\nfunc A() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := a.Engine.Record(ctx, memory.NewMemory{
		Kind: memory.KindTodo, Title: "fix a.go", Body: "fix a.go",
		Refs: []memory.Ref{{Type: memory.RefFile, Value: "a.go"}}, Branch: a.Git.Branch, Source: "cli",
	}); err != nil {
		t.Fatal(err)
	}
	// Break the graph so post-edit's reindex fails after the todo is surfaced.
	if _, err := a.Store.DB.ExecContext(ctx, "DROP TABLE nodes"); err != nil {
		t.Fatal(err)
	}
	if res, err := Handle(ctx, a, Event{Kind: KindPostEdit, SessionID: "probe", FilePath: file}); err == nil || !strings.Contains(res.Context, "fix a.go") {
		t.Fatalf("precondition: Handle = %q, %v; want the todo context and a reindex error", res.Context, err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	for _, c := range []struct {
		ctx context.Context
		ev  Event
	}{
		{ctx, Event{Kind: KindPostEdit, SessionID: "s", FilePath: file}},
		{cancelled, Event{Kind: KindPostRead, SessionID: "s", FilePath: file}},
	} {
		var out bytes.Buffer
		if err := RunHook(c.ctx, Opened(a), fixedAdapter{c.ev}, c.ev.Kind, strings.NewReader(""), &out); err == nil || out.Len() != 0 {
			t.Errorf("%+v: err %v, output %q; want the handler error and no output", c.ev, err, out.String())
		}
	}
}
