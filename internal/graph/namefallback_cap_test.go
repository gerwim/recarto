package graph

import (
	"context"
	"fmt"
	"testing"
)

// TestNameFallbackSkipsHyperCommonNames locks the S5 fan-out cap: a name matching
// more than 50 callables across the repo is too ambiguous for the name-only
// fallback (it would emit 10 arbitrary 0.2-confidence targets and is what makes S5
// blow up at scale), so it yields no name candidate; a rare name still does.
func TestNameFallbackSkipsHyperCommonNames(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	wt := t.TempDir()
	for i := 0; i < 51; i++ { // 51 > 50 -> hyper-common
		writeFile(t, wt, fmt.Sprintf("u%02d.go", i), "package p\nfunc Ubiquitous(){}")
	}
	writeFile(t, wt, "caller.go", "package p\nfunc Rare(){}\nfunc Caller(){ Ubiquitous(); Rare() }")
	if err := e.Reconcile(ctx, wt, FSSource{Root: wt}); err != nil {
		t.Fatal(err)
	}

	nameCands := func(dstName string) int {
		var n int
		if err := e.st.DB.QueryRow(`SELECT COUNT(*) FROM edge_candidates ec
			JOIN edges ed ON ed.id = ec.edge_id
			JOIN files f  ON f.id = ed.file_id
			WHERE f.worktree = ? AND ed.kind = 'calls'
			  AND ed.dst_name = ? AND ec.method = 'name'`, wt, dstName).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if got := nameCands("Ubiquitous"); got != 0 {
		t.Errorf("Ubiquitous (51 defs) got %d name candidates, want 0 (hyper-common cap)", got)
	}
	if got := nameCands("Rare"); got < 1 {
		t.Errorf("Rare (1 def) got %d name candidates, want >=1", got)
	}
}
