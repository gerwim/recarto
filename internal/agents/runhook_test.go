package agents

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"git.gerwim.nl/gerwim/recarto/internal/app"
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
	boom := errors.New("store cannot open")
	for _, ev := range []Event{
		{Kind: KindSessionStart, SessionID: "s"},
		{Kind: KindPostRead, SessionID: "s", FilePath: "/repo/a.go"},
		{Kind: KindPostEdit, SessionID: "s", FilePath: "/repo/a.go"},
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
