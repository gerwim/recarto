package graph

import (
	"context"
	"os"
	"path/filepath"
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

func TestArchitectureDependencyEdges(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	mustIndex := func(path, src string) {
		if err := e.IndexFile(ctx, "/wt", path, []byte(src), 1, int64(len(src))); err != nil {
			t.Fatal(err)
		}
	}
	// api/handler.go calls Login defined in auth/login.go -> dep api->auth.
	mustIndex("auth/login.go", "package auth\nfunc Login(){}")
	mustIndex("api/handler.go", "package api\nfunc Handle(){ Login() }")
	if err := e.Resolve(ctx, "/wt"); err != nil {
		t.Fatal(err)
	}
	arch, err := e.Architecture(ctx, "/wt", "")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, d := range arch.Deps {
		if d.From == "api" && d.To == "auth" && d.Count >= 1 {
			found = true
		}
	}
	if !found {
		t.Errorf("Deps = %+v, want an api->auth edge", arch.Deps)
	}
	// No self-edges (a module depending on itself is not reported).
	for _, d := range arch.Deps {
		if d.From == d.To {
			t.Errorf("unexpected self-dependency edge %+v", d)
		}
	}
}

func TestArchitectureProjectAwareModules(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	wt := t.TempDir()
	mkProj := func(dir, marker, srcfile, src string) {
		full := filepath.Join(wt, dir)
		if err := os.MkdirAll(full, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(full, marker), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		rel := filepath.ToSlash(filepath.Join(dir, srcfile))
		if err := e.IndexFile(ctx, wt, rel, []byte(src), 1, int64(len(src))); err != nil {
			t.Fatal(err)
		}
	}
	// Two nested projects: a C# project deep under backend/src, and the frontend.
	mkProj("backend/src/Ordia.Domain", "Ordia.Domain.csproj", "Appointment.cs", "namespace Ordia.Domain;\nclass Appointment {}")
	mkProj("frontend", "package.json", "app.ts", "export function App(){}")

	arch, err := e.Architecture(ctx, wt, "")
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, m := range arch.Modules {
		names[m.Name] = true
	}
	if !names["Ordia.Domain"] {
		t.Errorf("modules %+v, want one named Ordia.Domain (nearest .csproj dir)", arch.Modules)
	}
	if !names["frontend"] {
		t.Errorf("modules %+v, want one named frontend (nearest package.json dir)", arch.Modules)
	}
}

func TestArchitectureDisambiguatesCollidingModuleNames(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	wt := t.TempDir()
	mk := func(dir, marker, srcfile, src string) {
		full := filepath.Join(wt, dir)
		if err := os.MkdirAll(full, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(full, marker), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		rel := filepath.ToSlash(filepath.Join(dir, srcfile))
		if err := e.IndexFile(ctx, wt, rel, []byte(src), 1, int64(len(src))); err != nil {
			t.Fatal(err)
		}
	}
	// Two distinct projects whose directories share the basename "App".
	mk("backend/App", "App.csproj", "a.cs", "namespace A;\nclass X {}")
	mk("frontend/App", "package.json", "b.ts", "export function Y(){}")
	arch, err := e.Architecture(ctx, wt, "")
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, m := range arch.Modules {
		names[m.Name] = true
	}
	if names["App"] {
		t.Errorf("colliding basename 'App' must be disambiguated, got modules %+v", arch.Modules)
	}
	if !names["backend/App"] || !names["frontend/App"] {
		t.Errorf("want distinct modules backend/App and frontend/App, got %+v", arch.Modules)
	}
}
