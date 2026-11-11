package mcp

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gerwim/recarto/internal/app"
	"github.com/gerwim/recarto/internal/clock"
	"github.com/gerwim/recarto/internal/store"
)

// damageAfter overwrites every complete database page at or after off with a
// byte pattern that is not a legal b-tree page type. Pages are appended as the
// database grows, so writing table A, recording the size, then writing table B
// puts B's pages past the offset: this damages B and nothing else. See
// internal/store/repair_test.go for the full rationale.
func damageAfter(t *testing.T, path string, off int64) {
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

func openAppIn(t *testing.T, cacheDir string, opts ...bool) *app.App {
	t.Helper()
	t.Setenv("RECARTO_CACHE_DIR", cacheDir)
	quick := len(opts) > 0 && opts[0]
	a, err := app.Open(app.Config{
		CWD:        "/repo",
		Runner:     fakeGit{branch: "main"},
		Clock:      clock.NewFake(time.UnixMilli(1_700_000_000_000)),
		QuickCheck: quick,
	})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// TestRecordMemoryOnACorruptStore is the reported bug, end to end minus the
// transport: the store opens, reads serve fine all session, and then
// record_memory fails with "database disk image is malformed (11)".
//
// The damage is aimed at the structures record_memory WRITES through — the
// MinHash band index it consults for near-duplicate detection and the code
// graph — while the memories table itself, written earlier, stays intact and
// readable. That asymmetry is the whole reason the failure looked so strange in
// the field.
//
// Two things must now be true. The failing call reports something the agent can
// act on instead of a raw SQLite code — an in-session file swap under a live
// connection pool is not safe, so this call cannot be healed in place. And the
// next session (which `recarto serve` opens with the proactive check) starts on
// a repaired store, so record_memory succeeds and the earlier memories are still
// there.
func TestRecordMemoryOnACorruptStore(t *testing.T) {
	clearRecartoEnv(t)
	t.Setenv("RECARTO_AUTO_REPAIR", "")
	t.Setenv("RECARTO_QUICKCHECK_INTERVAL", "")
	cacheDir := t.TempDir()
	ctx := context.Background()

	a := openAppIn(t, cacheDir)
	path := a.Store.Path()

	// A session's worth of memories, written FIRST so their pages sit early in
	// the file and survive the damage below.
	if err := a.Store.WriteTx(ctx, func(c store.Conn) error {
		for i := 0; i < 120; i++ {
			id := "seed-" + string(rune('a'+i/26)) + string(rune('a'+i%26))
			text := id + " padding padding padding padding padding padding padding padding"
			if _, err := c.ExecContext(ctx,
				`INSERT INTO memories(id,kind,title,body,search_text,created_at,updated_at,status)
				 VALUES (?,?,?,?,?,?,?,'active')`,
				id, "learning", "seed "+id, text, text, 1, 1); err != nil {
				return err
			}
			if _, err := c.ExecContext(ctx,
				`INSERT INTO memories_fts(memory_id,search_text) VALUES (?,?)`, id, text); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	preID, err := recordMemory(ctx, a, RecordMemoryInput{
		Kind: "learning",
		Body: "recorded before the corruption; must survive the repair",
	})
	if err != nil {
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

	// Now grow the write-path structures — the MinHash band index and the code
	// graph — so their pages are appended past memoriesEnd.
	if err := a.Store.WriteTx(ctx, func(c store.Conn) error {
		for i := 0; i < 120; i++ {
			id := "seed-" + string(rune('a'+i/26)) + string(rune('a'+i%26))
			for band := 0; band < 16; band++ {
				if _, err := c.ExecContext(ctx,
					`INSERT INTO memory_minhash(memory_id,band,hash) VALUES (?,?,?)`,
					id, band, int64(i*100+band)); err != nil {
					return err
				}
			}
		}
		for i := 0; i < 200; i++ {
			fid := "f" + string(rune('a'+i/26)) + string(rune('a'+i%26))
			if _, err := c.ExecContext(ctx,
				`INSERT INTO files(id,worktree,path,language,content_hash,mtime,size,last_indexed_at)
				 VALUES (?,?,?,?,?,?,?,?)`,
				fid, "/repo", "pkg/"+fid+".go", "go", "hash", 0, 0, 0); err != nil {
				return err
			}
			for j := 0; j < 10; j++ {
				nid := fid + "_" + string(rune('a'+j))
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
	}); err != nil {
		t.Fatal(err)
	}
	if err := a.Store.Checkpoint(ctx); err != nil {
		t.Fatal(err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	damageAfter(t, path, memoriesEnd)

	// A session that started before the damage: it opened fine and its reads work.
	a = openAppIn(t, cacheDir)
	var mem int
	if err := a.Store.DB.QueryRow(`SELECT count(*) FROM memories`).Scan(&mem); err != nil {
		t.Fatalf("memory reads should still work — this is not the reported scenario: %v", err)
	}
	if mem != 121 {
		t.Fatalf("memories readable = %d, want 121", mem)
	}
	_, err = recordMemory(ctx, a, RecordMemoryInput{
		Kind: "learning",
		Body: "a completely different subject, recorded while the store is damaged",
	})
	if err == nil {
		t.Fatal("expected record_memory to fail on the corrupt store")
	}
	if !strings.Contains(err.Error(), "recarto repair") {
		t.Errorf("record_memory error is not actionable: %v", err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}

	// The next `recarto serve` session: the proactive check repairs at startup.
	a = openAppIn(t, cacheDir, true)
	defer a.Close()
	out, err := recordMemory(ctx, a, RecordMemoryInput{
		Kind: "learning",
		Body: "a completely different subject, recorded while the store is damaged",
	})
	if err != nil {
		t.Fatalf("record_memory still failing after the healing open: %v", err)
	}
	if out.ID == "" {
		t.Error("record_memory returned an empty id")
	}
	var title string
	if err := a.Store.DB.QueryRow(
		`SELECT title FROM memories WHERE id = ?`, preID.ID).Scan(&title); err != nil {
		t.Fatalf("the memory recorded before the corruption did not survive: %v", err)
	}
}
