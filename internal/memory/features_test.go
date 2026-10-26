package memory

import (
	"path/filepath"
	"testing"
	"time"

	"git.gerwim.nl/gerwim/recarto/internal/clock"
	"git.gerwim.nl/gerwim/recarto/internal/features"
)

func TestSemanticRecallFollowsConfig(t *testing.T) {
	t.Setenv("RECARTO_SEMANTIC", "")
	p := filepath.Join(t.TempDir(), "config.toml")
	t.Setenv("RECARTO_CONFIG", p)
	clk := clock.NewFake(time.UnixMilli(1_700_000_000_000))
	if e := newTestEngine(t, clk); e.emb == nil {
		t.Fatal("semantic-recall on by default: engine has no embedder")
	}
	if err := features.Save(p, features.Set{features.SemanticRecall: false}); err != nil {
		t.Fatal(err)
	}
	if e := newTestEngine(t, clk); e.emb != nil {
		t.Fatalf("semantic-recall off: engine embedder = %T, want nil", e.emb)
	}
}
