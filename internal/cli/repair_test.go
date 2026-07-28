package cli

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"git.gerwim.nl/gerwim/recarto/internal/app"
	"git.gerwim.nl/gerwim/recarto/internal/clock"
	"git.gerwim.nl/gerwim/recarto/internal/store"
)

// openAppIn builds an App against an explicit cache dir so a test can close it,
// damage the store on disk, and open it again.
func openAppIn(t *testing.T, cacheDir string) *app.App {
	t.Helper()
	t.Setenv("RECARTO_CACHE_DIR", cacheDir)
	a, err := app.Open(app.Config{
		CWD:    "/repo",
		Runner: fakeGit{branch: "main"},
		Clock:  clock.NewFake(time.UnixMilli(1_700_000_000_000)),
	})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// smashHeader destroys the SQLite magic string, which makes the file unopenable
// (SQLITE_NOTADB) — the state in which `recarto repair` is the only way back,
// since every App-opening command fails before it can run.
func smashHeader(t *testing.T, path string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteAt([]byte("NOT A DATABASE\x00\x00"), 0); err != nil {
		t.Fatal(err)
	}
}

// smashPagesPast overwrites every complete page at or after off with a byte that
// is not a legal b-tree page type, damaging only structures written after that
// point. See internal/store/repair_test.go for the full rationale.
func smashPagesPast(t *testing.T, path string, off int64) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	hdr := make([]byte, 100)
	if _, err := f.ReadAt(hdr, 0); err != nil {
		t.Fatal(err)
	}
	ps := int64(hdr[16])<<8 | int64(hdr[17])
	if ps == 1 {
		ps = 65536
	}
	fi, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	junk := make([]byte, ps)
	for i := range junk {
		junk[i] = 0xA5
	}
	n := 0
	for p := ((off + ps - 1) / ps) * ps; p+ps <= fi.Size(); p += ps {
		if _, err := f.WriteAt(junk, p); err != nil {
			t.Fatal(err)
		}
		n++
	}
	if n == 0 {
		t.Fatalf("no pages to damage (offset %d, size %d)", off, fi.Size())
	}
}

// seedGraphRows appends a code graph large enough to occupy many fresh pages.
func seedGraphRows(t *testing.T, a *app.App, files int) {
	t.Helper()
	ctx := context.Background()
	err := a.Store.WriteTx(ctx, func(c store.Conn) error {
		for i := 0; i < files; i++ {
			fid := "f" + strings.Repeat("0", 4-len(itoa(i))) + itoa(i)
			if _, err := c.ExecContext(ctx,
				`INSERT INTO files(id,worktree,path,language,content_hash,mtime,size,last_indexed_at)
				 VALUES (?,?,?,?,?,?,?,?)`,
				fid, "/repo", "pkg/"+fid+".go", "go", "hash", 0, 0, 0); err != nil {
				return err
			}
			for j := 0; j < 10; j++ {
				nid := fid + "_" + itoa(j)
				if _, err := c.ExecContext(ctx,
					`INSERT INTO nodes(id,file_id,kind,name,qualified_name,parent_name,signature,
					   start_byte,end_byte,start_line,end_line)
					 VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
					nid, fid, "function", nid, "pkg."+nid, "pkg", "func()", 0, 1, 1, 2); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

// TestRepairCommandRunsWhenTheAppCannotOpen is the escape hatch's whole point:
// `recarto repair` is dispatched before app.Open, so it still works on a store
// that no other command can open.
func TestRepairCommandRunsWhenTheAppCannotOpen(t *testing.T) {
	cacheDir := t.TempDir()
	t.Setenv("RECARTO_AUTO_REPAIR", "0") // prove the command itself does the work

	a := openAppIn(t, cacheDir)
	path := a.Store.Path()
	ctx := context.Background()
	if err := a.Store.WriteTx(ctx, func(c store.Conn) error {
		_, err := c.ExecContext(ctx,
			`INSERT INTO memories(id,kind,title,body,search_text,created_at,updated_at,status)
			 VALUES ('m1','learning','keep me','body','keep me body',1,1,'active')`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := a.Store.Checkpoint(ctx); err != nil {
		t.Fatal(err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	smashHeader(t, path)

	if _, err := app.Open(app.Config{CWD: "/repo", Runner: fakeGit{branch: "main"}}); err == nil {
		t.Fatal("expected app.Open to fail on the smashed store")
	}

	var buf bytes.Buffer
	if err := Repair(&buf, []string{"--path", path}); err != nil {
		t.Fatalf("repair: %v\n%s", err, buf.String())
	}
	out := buf.String()
	for _, want := range []string{
		"repairing",
		"could not be read at all",
		"total salvaged rows: 0",
		"quarantined old store",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("repair output missing %q:\n%s", want, out)
		}
	}

	a2 := openAppIn(t, cacheDir)
	defer a2.Close()
	var n int
	if err := a2.Store.DB.QueryRow(`SELECT count(*) FROM memories`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("memories = %d, want 0 (the file was unreadable, so nothing was salvageable)", n)
	}
}

// TestRepairCommandSalvagesMemories covers the common case — damage that spares
// the memory pages — and checks the per-table stats reach the user.
func TestRepairCommandSalvagesMemories(t *testing.T) {
	cacheDir := t.TempDir()
	t.Setenv("RECARTO_AUTO_REPAIR", "0")

	a := openAppIn(t, cacheDir)
	path := a.Store.Path()
	ctx := context.Background()
	if err := a.Store.WriteTx(ctx, func(c store.Conn) error {
		_, err := c.ExecContext(ctx,
			`INSERT INTO memories(id,kind,title,body,search_text,created_at,updated_at,status)
			 VALUES ('m1','learning','keep me','body','keep me body',1,1,'active')`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := a.Store.Checkpoint(ctx); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	memoriesEnd := fi.Size()
	seedGraphRows(t, a, 200)
	if err := a.Store.Checkpoint(ctx); err != nil {
		t.Fatal(err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	smashPagesPast(t, path, memoriesEnd)

	var buf bytes.Buffer
	if err := Repair(&buf, []string{"--path", path}); err != nil {
		t.Fatalf("repair: %v\n%s", err, buf.String())
	}
	if !strings.Contains(buf.String(), "memories             salvaged=1") {
		t.Errorf("expected the memory to be reported as salvaged:\n%s", buf.String())
	}

	a2 := openAppIn(t, cacheDir)
	defer a2.Close()
	var title string
	if err := a2.Store.DB.QueryRow(`SELECT title FROM memories WHERE id='m1'`).Scan(&title); err != nil {
		t.Fatalf("memory not salvaged: %v", err)
	}
	if title != "keep me" {
		t.Errorf("title = %q, want %q", title, "keep me")
	}
}

// TestDoctorReportsLastRepair: a store that has been silently rebuilt must say so.
func TestDoctorReportsLastRepair(t *testing.T) {
	a := newTestApp(t, "main")
	ctx := context.Background()
	if err := a.Store.WriteTx(ctx, func(c store.Conn) error {
		_, err := c.ExecContext(ctx,
			`INSERT INTO meta(key,value) VALUES
			   ('repair_last_ts','1700000000'),
			   ('repair_last_stats','{"tables":{"memories":{"salvaged":7}},"source_open":true}')`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := Doctor(ctx, a, &buf); err != nil {
		t.Fatalf("doctor: %v\n%s", err, buf.String())
	}
	out := buf.String()
	if !strings.Contains(out, "last repair") || !strings.Contains(out, `"salvaged":7`) {
		t.Errorf("doctor did not report the last repair:\n%s", out)
	}
}

// TestDoctorHintsAtRepairWhenIntegrityFails: the integrity check is the one
// place a user learns their store is damaged, so it must say what to do next.
func TestDoctorHintsAtRepairWhenIntegrityFails(t *testing.T) {
	cacheDir := t.TempDir()
	t.Setenv("RECARTO_AUTO_REPAIR", "0")

	a := openAppIn(t, cacheDir)
	path := a.Store.Path()
	ctx := context.Background()
	if err := a.Store.Checkpoint(ctx); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	base := fi.Size()
	seedGraphRows(t, a, 200)
	if err := a.Store.Checkpoint(ctx); err != nil {
		t.Fatal(err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	smashPagesPast(t, path, base)

	a2 := openAppIn(t, cacheDir) // opens fine: the damage is not on the open path
	defer a2.Close()

	var buf bytes.Buffer
	err = Doctor(ctx, a2, &buf)
	if err == nil {
		t.Fatalf("doctor passed on a corrupt store:\n%s", buf.String())
	}
	out := buf.String()
	if !strings.Contains(out, "[FAIL] integrity") {
		t.Errorf("expected a failed integrity check:\n%s", out)
	}
	if !strings.Contains(out, "recarto repair") {
		t.Errorf("expected a repair hint:\n%s", out)
	}
}
