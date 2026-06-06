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

func TestMapArchitectureRanksByFilesDemotingTests(t *testing.T) {
	arch := Architecture{Modules: []Module{
		{Name: "Core", Files: 100, Symbols: 500},
		{Name: "Core.Tests", Files: 200, Symbols: 900},      // most files, but a test module
		{Name: "Generated.Client", Files: 5, Symbols: 9000}, // symbol-dense, few files (e.g. codegen)
		{Name: "Data", Files: 50, Symbols: 300},
		{Name: "Util", Files: 10, Symbols: 60},
	}}
	mods, _ := MapArchitecture(arch, 3)
	if len(mods) != 3 {
		t.Fatalf("want top-3 modules, got %d", len(mods))
	}
	got := map[string]bool{}
	for _, m := range mods {
		got[m.Name] = true
	}
	// Highest file-count product modules: Core(100), Data(50), Util(10). The
	// symbol-dense Generated.Client (5 files) and the Core.Tests test module are
	// demoted out of a tight orientation budget even though one has the most files
	// and the other the most symbols.
	for _, want := range []string{"Core", "Data", "Util"} {
		if !got[want] {
			t.Errorf("expected product module %q in top-3, got %v", want, got)
		}
	}
	if got["Core.Tests"] {
		t.Errorf("test module Core.Tests must be demoted out of top-3: %v", got)
	}
	if got["Generated.Client"] {
		t.Errorf("symbol-dense Generated.Client must not crowd top-3 by file count: %v", got)
	}
	// Display is restored to name order for stability.
	for i := 1; i < len(mods); i++ {
		if mods[i-1].Name > mods[i].Name {
			t.Errorf("kept modules not name-sorted for display: %+v", mods)
		}
	}
}

func TestMapArchitectureRanksDepsByCount(t *testing.T) {
	arch := Architecture{Deps: []ModuleEdge{
		{From: "A", To: "Z", Count: 1},
		{From: "B", To: "Y", Count: 50},
		{From: "C", To: "X", Count: 5},
	}}
	_, deps := MapArchitecture(arch, 2)
	if len(deps) != 2 {
		t.Fatalf("want top-2 deps, got %d", len(deps))
	}
	// Heaviest edges B→Y(50) and C→X(5) kept; A→Z(1) dropped; restored to name order.
	if deps[0].From != "B" || deps[1].From != "C" {
		t.Errorf("want heaviest edges B,C in name order, got %+v", deps)
	}
}

func TestMapArchitectureDemotesTestEndpointDeps(t *testing.T) {
	arch := Architecture{Deps: []ModuleEdge{
		{From: "Core.Tests", To: "Core", Count: 9000}, // a test depending on code: obvious, low-signal
		{From: "Api", To: "Core", Count: 10},          // genuine product→product coupling
		{From: "Web", To: "Api.Tests", Count: 8000},   // product→test endpoint: also low-signal
	}}
	_, deps := MapArchitecture(arch, 1)
	if len(deps) != 1 {
		t.Fatalf("want top-1 dep, got %+v", deps)
	}
	// Despite the test edges having far higher counts, the product→product edge wins.
	if deps[0].From != "Api" || deps[0].To != "Core" {
		t.Errorf("want product edge Api→Core to outrank test-endpoint edges, got %+v", deps[0])
	}
}

func TestIsTestModuleNameConservative(t *testing.T) {
	tests := map[string]bool{
		"ApolloFederation.Tests":               true,
		"src/HotChocolate/Core/test/Types.Test": true,
		"Mocha.Tests":                          true,
		"Core":                                 false,
		"Types.Analyzers":                      false,
		"CostAnalysis":                         false, // "analysis" must not match "test"-like
		"Testing":                              false, // segment "testing" != "test"
		"contest":                              false,
	}
	for name, want := range tests {
		if got := isTestModuleName(name); got != want {
			t.Errorf("isTestModuleName(%q) = %v, want %v", name, got, want)
		}
	}
}
