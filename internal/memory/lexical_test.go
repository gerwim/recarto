package memory

import (
	"context"
	"path/filepath"
	"testing"

	"git.gerwim.nl/gerwim/recarto/internal/store"
)

func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestFTSLexicalIndexAndSearch(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	lex := NewFTSLexical(st)

	docs := map[string]string{
		"a": "auth token refresh leaks goroutine on error",
		"b": "markdown export renders to stdout",
		"c": "auth session config database connection",
	}
	for id, txt := range docs {
		if err := st.WriteTx(ctx, func(c store.Conn) error {
			return lex.Index(ctx, c, id, txt)
		}); err != nil {
			t.Fatal(err)
		}
	}

	res, err := lex.Search(ctx, "auth token", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) == 0 {
		t.Fatal("expected lexical matches for 'auth token'")
	}
	if res[0].ID != "a" {
		t.Errorf("top match = %q, want \"a\"", res[0].ID)
	}
	if res[0].Score < res[len(res)-1].Score {
		t.Errorf("scores not descending: %+v", res)
	}

	// Delete then re-search: "b" gone.
	if err := st.WriteTx(ctx, func(c store.Conn) error {
		return lex.Delete(ctx, c, "b")
	}); err != nil {
		t.Fatal(err)
	}
	res, _ = lex.Search(ctx, "markdown export", 10)
	for _, r := range res {
		if r.ID == "b" {
			t.Error("deleted doc b still returned")
		}
	}
}
