package graph

import (
	"context"
	"testing"
)

func TestArchitectureGroupsByModule(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	mustIndex := func(path, src string) {
		if err := e.IndexFile(ctx, "/wt", path, []byte(src), 1, int64(len(src))); err != nil {
			t.Fatal(err)
		}
	}
	mustIndex("auth/login.go", "package auth\nfunc Login(){}\nfunc Logout(){}")
	mustIndex("auth/token.go", "package auth\nfunc Mint(){}")
	mustIndex("api/handler.go", "package api\nfunc Handle(){}")

	arch, err := e.Architecture(ctx, "/wt", "")
	if err != nil {
		t.Fatal(err)
	}
	mods := map[string]Module{}
	for _, m := range arch.Modules {
		mods[m.Name] = m
	}
	if mods["auth"].Files != 2 || mods["auth"].Symbols != 3 {
		t.Errorf("auth module = %+v, want 2 files / 3 symbols", mods["auth"])
	}
	if mods["api"].Files != 1 || mods["api"].Symbols != 1 {
		t.Errorf("api module = %+v, want 1 file / 1 symbol", mods["api"])
	}
}
