package graph

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestSignaturePopulatedOnIndex(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	src := []byte("package p\nfunc Greet(name string) string { return name }")
	if err := e.IndexFile(ctx, "/wt", "sample.go", src, 1, int64(len(src))); err != nil {
		t.Fatal(err)
	}
	res, err := e.Search(ctx, "/wt", "Greet", "", "", 5)
	if err != nil {
		t.Fatal(err)
	}
	var sig string
	for _, r := range res {
		if r.Name == "Greet" {
			sig = r.Signature
		}
	}
	if !strings.Contains(sig, "func Greet(") {
		t.Fatalf("signature = %q, want it to contain the declaration line", sig)
	}
}

func countRows(t *testing.T, e *Engine, table, worktree string) int {
	t.Helper()
	var n int
	q := "SELECT COUNT(*) FROM " + table
	var err error
	switch table {
	case "files":
		err = e.st.DB.QueryRow(q+" WHERE worktree=?", worktree).Scan(&n)
	default:
		err = e.st.DB.QueryRow(q+
			" WHERE file_id IN (SELECT id FROM files WHERE worktree=?)", worktree).Scan(&n)
	}
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestIndexFileWritesNodesAndEdges(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	src := []byte("package p\nfunc A() { B() }\nfunc B() {}")
	if err := e.IndexFile(ctx, "/wt", "p/a.go", src, 111, int64(len(src))); err != nil {
		t.Fatal(err)
	}
	if got := countRows(t, e, "files", "/wt"); got != 1 {
		t.Errorf("files = %d, want 1", got)
	}
	if got := countRows(t, e, "nodes", "/wt"); got != 2 { // A, B
		t.Errorf("nodes = %d, want 2", got)
	}
	if got := countRows(t, e, "edges", "/wt"); got < 1 { // A calls B
		t.Errorf("edges = %d, want >=1", got)
	}
	// The call edge must be attributed to its enclosing function A.
	var srcName string
	err := e.st.DB.QueryRow(`
		SELECT n.name FROM edges ed JOIN nodes n ON n.id = ed.src_node_id
		WHERE ed.kind='calls' AND ed.dst_name='B'`).Scan(&srcName)
	if err != nil {
		t.Fatalf("call edge not attributed to enclosing node: %v", err)
	}
	if srcName != "A" {
		t.Errorf("call to B attributed to %q, want A", srcName)
	}
}

func TestIndexFileEmitsContainsEdges(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	// A TypeScript class with a method: the method is structurally nested inside
	// the class, so a `contains` edge (class -> method) must be emitted, linking
	// both nodes directly (intra-file, no name resolution needed).
	src := []byte("class Foo {\n  bar() {}\n}")
	if err := e.IndexFile(ctx, "/wt", "a.ts", src, 1, int64(len(src))); err != nil {
		t.Fatal(err)
	}
	var parent, child string
	err := e.st.DB.QueryRow(`
		SELECT p.name, ch.name
		  FROM edges ed
		  JOIN nodes p  ON p.id  = ed.src_node_id
		  JOIN nodes ch ON ch.id = ed.dst_node_id
		 WHERE ed.kind='contains'`).Scan(&parent, &child)
	if err != nil {
		t.Fatalf("expected a contains edge class->method: %v", err)
	}
	if parent != "Foo" || child != "bar" {
		t.Errorf("contains edge = %s -> %s, want Foo -> bar", parent, child)
	}
}

func TestIndexFileReplacesOnReindex(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	if err := e.IndexFile(ctx, "/wt", "p/a.go", []byte("package p\nfunc A(){}\nfunc B(){}"), 1, 30); err != nil {
		t.Fatal(err)
	}
	// Reindex same path with fewer symbols -> old rows replaced, not duplicated.
	if err := e.IndexFile(ctx, "/wt", "p/a.go", []byte("package p\nfunc A(){}"), 2, 20); err != nil {
		t.Fatal(err)
	}
	if got := countRows(t, e, "files", "/wt"); got != 1 {
		t.Errorf("files = %d, want 1 (upsert, not insert)", got)
	}
	if got := countRows(t, e, "nodes", "/wt"); got != 1 {
		t.Errorf("nodes = %d, want 1 after reindex", got)
	}
	for _, table := range []string{"nodes_fts", "nodes_fts_stem", "nodes_fts_tri"} {
		var got int
		if err := e.st.DB.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != 1 {
			t.Errorf("%s rows = %d, want 1 after reindex", table, got)
		}
	}
	var mapped int
	if err := e.st.DB.QueryRow(`SELECT COUNT(*) FROM node_fts_rows`).Scan(&mapped); err != nil {
		t.Fatal(err)
	}
	if mapped != 1 {
		t.Errorf("node_fts_rows = %d, want 1 after reindex", mapped)
	}
}

func TestReindexKeepsNodeIDsAndBindings(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	src := "package p\ntype Saga struct{}\nfunc (s *Saga) Handle() { s.step() }\nfunc (s *Saga) step() {}\n"
	ids := func() map[string]string {
		m := map[string]string{}
		rows, err := e.st.DB.Query(`SELECT qualified_name, id FROM nodes`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		for rows.Next() {
			var qn, id string
			if err := rows.Scan(&qn, &id); err != nil {
				t.Fatal(err)
			}
			m[qn] = id
		}
		return m
	}
	if err := e.IndexFile(ctx, "/wt", "saga.go", []byte(src), 1, int64(len(src))); err != nil {
		t.Fatal(err)
	}
	before := ids()
	bindings := countRows(t, e, "type_bindings", "/wt")
	edited := "// moved\n" + src
	if err := e.IndexFile(ctx, "/wt", "saga.go", []byte(edited), 2, int64(len(edited))); err != nil {
		t.Fatal(err)
	}
	after := ids()
	if len(after) != len(before) {
		t.Fatalf("nodes after = %v, before = %v", after, before)
	}
	for qn, id := range before {
		if after[qn] != id {
			t.Errorf("%s id changed %s -> %s", qn, id, after[qn])
		}
	}
	var start int
	if err := e.st.DB.QueryRow(`SELECT start_line FROM nodes WHERE qualified_name='Saga.Handle'`).Scan(&start); err != nil {
		t.Fatal(err)
	}
	if start != 4 {
		t.Errorf("Handle start_line = %d, want 4 (position refreshed)", start)
	}
	if got := countRows(t, e, "type_bindings", "/wt"); got != bindings {
		t.Errorf("type_bindings = %d after reindex, want %d", got, bindings)
	}
	var fts int
	if err := e.st.DB.QueryRow(`SELECT COUNT(*) FROM nodes_fts`).Scan(&fts); err != nil {
		t.Fatal(err)
	}
	if fts != len(after) {
		t.Errorf("nodes_fts = %d, want %d", fts, len(after))
	}
}

func TestIndexFileOverwritesOrphanFTSRowID(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	// Orphans from before node_fts_rows existed sit at the rowid the mapping hands out next.
	for _, q := range []string{
		`INSERT INTO nodes_fts(rowid,node_id,name,qualified_name) VALUES (1,'gone','X','X')`,
		`INSERT INTO nodes_fts_stem(rowid,node_id,name,qualified_name) VALUES (1,'gone','X','X')`,
		`INSERT INTO nodes_fts_tri(rowid,node_id,content) VALUES (1,'gone','X')`,
	} {
		if _, err := e.st.DB.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.IndexFile(ctx, "/wt", "p/a.go", []byte("package p\nfunc A(){}"), 1, 20); err != nil {
		t.Fatalf("index over orphan FTS rowid: %v", err)
	}
}

func TestFTSCleanupUsesRowIDLookup(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	src := []byte("package p\nfunc A(){}\nfunc B(){}")
	if err := e.IndexFile(ctx, "/wt", "p/a.go", src, 1, int64(len(src))); err != nil {
		t.Fatal(err)
	}

	for _, table := range []string{"nodes_fts", "nodes_fts_stem", "nodes_fts_tri"} {
		var fileID string
		if err := e.st.DB.QueryRow(`SELECT id FROM files WHERE worktree=? AND path=?`, "/wt", "p/a.go").Scan(&fileID); err != nil {
			t.Fatal(err)
		}
		rows, err := e.st.DB.Query(`EXPLAIN QUERY PLAN DELETE FROM `+table+` WHERE rowid IN (
			SELECT fts_rowid FROM node_fts_rows
			WHERE node_id IN (SELECT id FROM nodes WHERE file_id=?))`,
			fileID)
		if err != nil {
			t.Fatal(err)
		}
		foundRowIDSeek := false
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			if strings.Contains(detail, table) && strings.Contains(detail, "VIRTUAL TABLE INDEX 0:=") {
				foundRowIDSeek = true
			}
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
		if !foundRowIDSeek {
			t.Errorf("%s cleanup does not seek by rowid", table)
		}
	}
}

func TestIndexFileSkipsUnsupported(t *testing.T) {
	e := newTestEngine(t)
	if err := e.IndexFile(context.Background(), "/wt", "README.md", []byte("# hi"), 1, 4); err != nil {
		t.Fatalf("unsupported file should be skipped, got %v", err)
	}
	if got := countRows(t, e, "files", "/wt"); got != 0 {
		t.Errorf("files = %d, want 0 (unsupported extension skipped)", got)
	}
}

func TestIndexFileFileScopeEdgeHasNullSrc(t *testing.T) {
	e := newTestEngine(t)
	// The import is at file scope (outside any symbol), so its edge has no
	// enclosing node -> src_node_id must be NULL.
	src := []byte("package p\nimport \"fmt\"\nfunc A() { fmt.Println() }")
	if err := e.IndexFile(context.Background(), "/wt", "p.go", src, 1, int64(len(src))); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := e.st.DB.QueryRow(
		`SELECT COUNT(*) FROM edges WHERE kind='imports' AND src_node_id IS NULL`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("file-scope import edges with NULL src = %d, want 1", n)
	}
}

func TestIndexFilePopulatesQualifiedNameFileScopedNamespace(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	// C# file-scoped namespace: the namespace node does NOT span-contain the
	// following types, so qualified_name must prefix them via a preceding-namespace
	// rule, not pure containment.
	src := []byte("namespace Ordia.Domain;\nclass Appointment {\n  void Handle() {}\n}")
	if err := e.IndexFile(ctx, "/wt", "a.cs", src, 1, int64(len(src))); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"Appointment": "Ordia.Domain.Appointment",
		"Handle":      "Ordia.Domain.Appointment.Handle",
	}
	for name, wantQN := range want {
		var qn string
		if err := e.st.DB.QueryRow(`SELECT qualified_name FROM nodes WHERE name=?`, name).Scan(&qn); err != nil {
			t.Fatal(err)
		}
		if qn != wantQN {
			t.Errorf("qualified_name(%s)=%q, want %q", name, qn, wantQN)
		}
	}
}

func TestIndexFilePopulatesQualifiedNameBlockNamespace(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	src := []byte("namespace Ordia.Domain {\n class Appointment {\n  void Handle() {}\n }\n}")
	if err := e.IndexFile(ctx, "/wt", "b.cs", src, 1, int64(len(src))); err != nil {
		t.Fatal(err)
	}
	var qn string
	if err := e.st.DB.QueryRow(`SELECT qualified_name FROM nodes WHERE name='Handle'`).Scan(&qn); err != nil {
		t.Fatal(err)
	}
	if qn != "Ordia.Domain.Appointment.Handle" {
		t.Errorf("qualified_name(Handle)=%q, want Ordia.Domain.Appointment.Handle", qn)
	}
}

func TestSignatureOfRuneSafeCap(t *testing.T) {
	// A first line longer than max whose byte-cap boundary lands inside a
	// multibyte rune must still yield valid UTF-8 (cap on a rune boundary).
	// 14-byte ASCII prefix then 2-byte 'é' runes; cap=15 lands inside the first é.
	src := []byte("func f() { /* " + strings.Repeat("é", 200) + " */ }")
	got := signatureOf(src, 0, 15)
	if !utf8.ValidString(got) {
		t.Fatalf("signatureOf produced invalid UTF-8: %q", got)
	}
	if len(got) > 15 {
		t.Errorf("byte cap exceeded: %d bytes", len(got))
	}
	// Trimming + no-newline + past-EOF basics.
	if s := signatureOf([]byte("\tfunc Greet()\nbody"), 1, 200); s != "func Greet()" {
		t.Errorf("trim/newline: got %q", s)
	}
	if s := signatureOf([]byte("xy"), 9, 200); s != "" {
		t.Errorf("past EOF: got %q", s)
	}
}
