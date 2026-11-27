//go:build !windows

package mcp

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gerwim/recarto/internal/gitwrap"
	"github.com/gerwim/recarto/internal/graph"
)

func TestServedSnippetResolvesFileAfterFreshness(t *testing.T) {
	ctx := context.Background()
	folder, recarto, second := routeRepos(t)
	r := gitwrap.NewRecipe(t, second, graph.AttemptEnv)
	t.Setenv("PATH", r.PATH())
	t.Setenv(graph.AttemptEnv, "")
	prevWait := freshWait
	freshWait = time.Minute
	t.Cleanup(func() { freshWait = prevWait })
	a := codelessApp(t, folder, nil, recarto, second)
	indexRepo(t, repoAt(a, recarto))
	r.Hold(t, true)

	type result struct {
		out GetCodeSnippetOutput
		err error
	}
	done := make(chan result, 1)
	go func() {
		out, err := getCodeSnippet(ctx, a, GetCodeSnippetInput{Symbol: "LateFunc", File: "store/late.go"})
		done <- result{out: out, err: err}
	}()
	finished := false
	t.Cleanup(func() {
		if !finished {
			r.Hold(t, false)
			if got := <-done; got.err != nil {
				t.Error(got.err)
			}
		}
	})

	held := func(c gitwrap.Call) bool { return c.Marker != "" && strings.Contains(c.Args, "ls-files") }
	deadline := time.Now().Add(time.Minute)
	for !slices.ContainsFunc(r.Calls(t), held) {
		if time.Now().After(deadline) {
			t.Fatal("the first index never reached its listing")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := os.WriteFile(filepath.Join(second, "store", "late.go"), []byte("package store\nfunc LateFunc() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r.Hold(t, false)
	got := <-done
	finished = true
	if got.err != nil || got.out.Repo != "second" || strings.Contains(got.out.Hint, "No served repo has the file") {
		t.Fatalf("snippet after first index = %+v, err %v", got.out, got.err)
	}
}
