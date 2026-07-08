package memory

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"git.gerwim.nl/gerwim/recarto/internal/clock"
	"git.gerwim.nl/gerwim/recarto/internal/store"
	"git.gerwim.nl/gerwim/recarto/internal/syncwire"
)

func TestSyncApplyInsertsNewRecord(t *testing.T) {
	e := newTestEngine(t, clock.NewFake(time.UnixMilli(1_000_000)))
	ctx := context.Background()

	rec := syncwire.Record{
		ID: "r1", Kind: string(KindLearning), Title: "t", Body: "a durable learning about retries",
		Confidence: 0.8, ReinforcementCount: 2, CreatedAt: 500, UpdatedAt: 700,
		Refs: []syncwire.Ref{{Type: "file", Value: "a.go"}},
	}
	out, id, err := e.SyncApply(ctx, rec)
	if err != nil {
		t.Fatal(err)
	}
	if out != SyncInserted || id != "r1" {
		t.Fatalf("want inserted r1, got %v %s", out, id)
	}
	got, _ := e.GetByID(ctx, "r1")
	if got == nil {
		t.Fatal("record not found")
	}
	if got.CreatedAt != 500 || got.UpdatedAt != 700 {
		t.Fatalf("timestamps not preserved: %+v", got)
	}
	if got.Confidence != 0.8 || got.ReinforcementCount != 2 {
		t.Fatalf("metadata not preserved: %+v", got)
	}
	if len(got.Refs) != 1 || got.Refs[0].Value != "a.go" {
		t.Fatalf("refs not persisted: %+v", got.Refs)
	}

	// An inserted-via-sync row must NOT be queued for push (it came from
	// elsewhere — pushing it back would echo it straight to the server).
	pending, err := e.PendingPush(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("SyncApply insert must not dirty the outbox, got %v", pending)
	}
}

func TestSyncApplyReinforcesNearDupAndReturnsCanonicalID(t *testing.T) {
	e := newTestEngine(t, clock.NewFake(time.UnixMilli(1_000_000)))
	ctx := context.Background()

	local, _, _, err := e.Record(ctx, NewMemory{
		Kind: KindLearning, Title: "t", Body: "a durable learning about retries and backoff",
	})
	if err != nil {
		t.Fatal(err)
	}
	// Clear the outbox entry Record() created, so the test cleanly observes only
	// what SyncApply itself does to the outbox below.
	if err := e.ClearPushed(ctx, []string{local}); err != nil {
		t.Fatal(err)
	}

	rec := syncwire.Record{
		ID: "remote-1", Kind: string(KindLearning), Title: "t",
		Body: "a durable learning about retries and backoff!", Confidence: 0.95, Pinned: true,
		CreatedAt: 2000, UpdatedAt: 2000,
	}
	out, id, err := e.SyncApply(ctx, rec)
	if err != nil {
		t.Fatal(err)
	}
	if out != SyncReinforced {
		t.Fatalf("want reinforced, got %v", out)
	}
	if id != local {
		t.Fatalf("canonical id must be the existing local row %q, got %q", local, id)
	}
	got, _ := e.GetByID(ctx, local)
	if !got.Pinned || got.Confidence < 0.95 {
		t.Fatalf("reinforce must MAX pinned/confidence: %+v", got)
	}
	// remote-1 never exists as its own row.
	if r, _ := e.GetByID(ctx, "remote-1"); r != nil {
		t.Fatalf("a reinforced near-dup must not create a row at the incoming id")
	}
	pending, err := e.PendingPush(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("SyncApply reinforce must not dirty the outbox, got %v", pending)
	}
}

// TestSyncApplyReinforceReplayIsIdempotent proves that re-applying the exact
// same wire record that previously reinforced a near-duplicate (a client
// retry after a push whose response was lost, or a replayed pull page) is a
// Noop, not a second reinforcement. A reinforce-merge never creates a row at
// the incoming id, so without memory_sync_merges remembering "this id, at
// this updated_at, already merged into canonical X", every replay looks
// identical to a fresh independent-authoring convergence and would reinforce
// the target again — inflating reinforcement_count and, on the server,
// bumping sync_index.seq again on every retry.
func TestSyncApplyReinforceReplayIsIdempotent(t *testing.T) {
	e := newTestEngine(t, clock.NewFake(time.UnixMilli(1_000_000)))
	ctx := context.Background()

	local, _, _, err := e.Record(ctx, NewMemory{
		Kind: KindLearning, Title: "t", Body: "always retry idempotent operations on timeout",
	})
	if err != nil {
		t.Fatal(err)
	}
	before, _ := e.GetByID(ctx, local)

	rec := syncwire.Record{
		ID: "remote-1", Kind: string(KindLearning), Title: "t",
		Body: "always retry idempotent operations on timeout!", CreatedAt: 2000, UpdatedAt: 2000,
	}
	out, id, err := e.SyncApply(ctx, rec)
	if err != nil {
		t.Fatal(err)
	}
	if out != SyncReinforced || id != local {
		t.Fatalf("setup: want reinforced %s, got %v %s", local, out, id)
	}
	afterFirst, _ := e.GetByID(ctx, local)
	if afterFirst.ReinforcementCount != before.ReinforcementCount+1 {
		t.Fatalf("setup: reinforce must bump the count once, got %+v", afterFirst)
	}

	// Replay the IDENTICAL record (same id, same updated_at) — simulates a
	// retried push/pull of a change the peer already applied.
	out, id, err = e.SyncApply(ctx, rec)
	if err != nil {
		t.Fatal(err)
	}
	if out != SyncNoop {
		t.Fatalf("a replayed reinforce-merge must be a Noop, got %v", out)
	}
	if id != local {
		t.Fatalf("a Noop replay must still report the canonical id %s, got %s", local, id)
	}
	afterReplay, _ := e.GetByID(ctx, local)
	if afterReplay.ReinforcementCount != afterFirst.ReinforcementCount {
		t.Fatalf("a replayed reinforce-merge must not bump reinforcement_count again: before=%d after=%d",
			afterFirst.ReinforcementCount, afterReplay.ReinforcementCount)
	}

	// A genuinely NEWER edit of the same origin row (later updated_at) must
	// still reinforce again — only an exact-or-older replay is a Noop.
	newer := rec
	newer.UpdatedAt = 3000
	out, id, err = e.SyncApply(ctx, newer)
	if err != nil {
		t.Fatal(err)
	}
	if out != SyncReinforced || id != local {
		t.Fatalf("a newer edit of an already-merged id must reinforce again, got %v %s", out, id)
	}
	afterNewer, _ := e.GetByID(ctx, local)
	if afterNewer.ReinforcementCount != afterFirst.ReinforcementCount+1 {
		t.Fatalf("a newer edit must bump reinforcement_count again: got %+v", afterNewer)
	}
}

func TestSyncApplyLWWUpdateNewerWins(t *testing.T) {
	e := newTestEngine(t, clock.NewFake(time.UnixMilli(1_000_000)))
	ctx := context.Background()

	id, _, _, err := e.Record(ctx, NewMemory{Kind: KindPreference, Title: "old", Body: "prefer tabs over spaces"})
	if err != nil {
		t.Fatal(err)
	}
	before, _ := e.GetByID(ctx, id)

	rec := syncwire.Record{
		ID: id, Kind: string(KindPreference), Title: "new", Body: "prefer spaces over tabs, decided later",
		Confidence: 1.0, ReinforcementCount: 5, CreatedAt: before.CreatedAt, UpdatedAt: before.UpdatedAt + 1000,
	}
	out, canon, err := e.SyncApply(ctx, rec)
	if err != nil {
		t.Fatal(err)
	}
	if out != SyncUpdated || canon != id {
		t.Fatalf("want updated %s, got %v %s", id, out, canon)
	}
	after, _ := e.GetByID(ctx, id)
	if after.Title != "new" || after.Body != rec.Body {
		t.Fatalf("newer incoming record must overwrite content: %+v", after)
	}
	if after.UpdatedAt != before.UpdatedAt+1000 {
		t.Fatalf("updated_at must adopt the incoming (newer) value: %+v", after)
	}
	if after.ReinforcementCount != 5 {
		t.Fatalf("reinforcement_count must take the incoming MAX: %+v", after)
	}
}

// TestSyncApplyLWWUpdatePreservesUnionedRefsInSearchText proves a same-id
// newer-wins update reindexes search_text/FTS from the UNIONED ref set (local
// ∪ incoming), not merely the incoming record's refs. memory_refs itself is
// already unioned (INSERT OR IGNORE); before this fix, a newer remote update
// carrying fewer/different refs than the local row would still union the DB
// rows but silently drop the local-only ref from lexical recall, since
// search_text/FTS were rebuilt from rec.Refs alone.
func TestSyncApplyLWWUpdatePreservesUnionedRefsInSearchText(t *testing.T) {
	e := newTestEngine(t, clock.NewFake(time.UnixMilli(1_000_000)))
	ctx := context.Background()

	id, _, _, err := e.Record(ctx, NewMemory{
		Kind: KindLearning, Title: "t", Body: "retry storage writes on conflict",
		Refs: []Ref{{Type: RefType("file"), Value: "storage/local_only_ref.go"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	before, _ := e.GetByID(ctx, id)

	rec := syncwire.Record{
		ID: id, Kind: string(KindLearning), Title: "t", Body: "retry storage writes on conflict, revised",
		Refs:      []syncwire.Ref{{Type: "file", Value: "storage/remote_only_ref.go"}},
		CreatedAt: before.CreatedAt, UpdatedAt: before.UpdatedAt + 1000,
	}
	out, canon, err := e.SyncApply(ctx, rec)
	if err != nil {
		t.Fatal(err)
	}
	if out != SyncUpdated || canon != id {
		t.Fatalf("want updated %s, got %v %s", id, out, canon)
	}
	after, err := e.GetByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(after.SearchText, "local_only_ref") {
		t.Fatalf("search_text must still contain the pre-existing local ref after a newer sync update, got %q", after.SearchText)
	}
	if !strings.Contains(after.SearchText, "remote_only_ref") {
		t.Fatalf("search_text must contain the incoming ref, got %q", after.SearchText)
	}
	refValues := map[string]bool{}
	for _, r := range after.Refs {
		refValues[r.Value] = true
	}
	if !refValues["storage/local_only_ref.go"] || !refValues["storage/remote_only_ref.go"] {
		t.Fatalf("memory_refs must contain both refs (already unioned), got %+v", after.Refs)
	}
}

func TestSyncApplyLWWUpdatePersistsExtraMetadata(t *testing.T) {
	e := newTestEngine(t, clock.NewFake(time.UnixMilli(1_000_000)))
	ctx := context.Background()

	// A row that already exists locally (e.g. bootstrapped from an earlier
	// pull) with no extra metadata yet.
	id, _, _, err := e.Record(ctx, NewMemory{Kind: KindFeedback, Title: "t", Body: "recarto ranked the wrong result first"})
	if err != nil {
		t.Fatal(err)
	}
	before, _ := e.GetByID(ctx, id)

	// A newer incoming record — e.g. a resolved feedback report pushed from
	// another machine — carries kind-specific extra metadata (mirrors what
	// ResolveFeedback stores under extra.resolution_note).
	rec := syncwire.Record{
		ID: id, Kind: string(KindFeedback), Title: "t", Body: "recarto ranked the wrong result first",
		Status: "resolved", Extra: map[string]any{"resolution_note": "fixed in v2"},
		CreatedAt: before.CreatedAt, UpdatedAt: before.UpdatedAt + 1000,
	}
	out, canon, err := e.SyncApply(ctx, rec)
	if err != nil {
		t.Fatal(err)
	}
	if out != SyncUpdated || canon != id {
		t.Fatalf("want updated %s, got %v %s", id, out, canon)
	}
	after, err := e.GetByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if note, _ := after.Extra["resolution_note"].(string); note != "fixed in v2" {
		t.Fatalf("a newer incoming record's extra metadata must be persisted, got %+v", after.Extra)
	}
}

func TestSyncApplyAbsentInsertPersistsExtraMetadata(t *testing.T) {
	e := newTestEngine(t, clock.NewFake(time.UnixMilli(1_000_000)))
	ctx := context.Background()

	// Bootstrap: this id has never existed locally, so SyncApply takes the
	// insert path — extra metadata must still survive (mirrors ImportMemory's
	// insert path, which already preserves it).
	rec := syncwire.Record{
		ID: "fb-1", Kind: string(KindFeedback), Title: "t", Body: "recarto ranked the wrong result first",
		Status: "resolved", Extra: map[string]any{"resolution_note": "fixed in v2"},
		CreatedAt: 500, UpdatedAt: 700,
	}
	out, id, err := e.SyncApply(ctx, rec)
	if err != nil {
		t.Fatal(err)
	}
	if out != SyncInserted || id != "fb-1" {
		t.Fatalf("want inserted fb-1, got %v %s", out, id)
	}
	got, err := e.GetByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if note, _ := got.Extra["resolution_note"].(string); note != "fixed in v2" {
		t.Fatalf("a bootstrap insert must preserve extra metadata, got %+v", got.Extra)
	}
}

func TestSyncApplyExistingOlderKeepsLocalButMaxesSoftSignals(t *testing.T) {
	e := newTestEngine(t, clock.NewFake(time.UnixMilli(5_000_000)))
	ctx := context.Background()

	id, _, _, err := e.Record(ctx, NewMemory{Kind: KindPreference, Title: "keep me", Body: "prefer tabs over spaces"})
	if err != nil {
		t.Fatal(err)
	}
	before, _ := e.GetByID(ctx, id)

	// Incoming record is OLDER (updated_at in the past) but claims higher
	// confidence/reinforcement/pinned — those must still ratchet up even though
	// content is not overwritten.
	rec := syncwire.Record{
		ID: id, Kind: string(KindPreference), Title: "should not appear", Body: "should not appear either",
		Confidence: 1.0, ReinforcementCount: before.ReinforcementCount + 10, Pinned: true,
		CreatedAt: before.CreatedAt, UpdatedAt: before.UpdatedAt - 1,
	}
	out, canon, err := e.SyncApply(ctx, rec)
	if err != nil {
		t.Fatal(err)
	}
	if out != SyncUpdated || canon != id {
		t.Fatalf("want updated (soft-signal merge) %s, got %v %s", id, out, canon)
	}
	after, _ := e.GetByID(ctx, id)
	if after.Title != before.Title || after.Body != before.Body {
		t.Fatalf("an older incoming record must not overwrite content: %+v", after)
	}
	if !after.Pinned {
		t.Fatalf("pinned must ratchet up even from a stale record: %+v", after)
	}
	if after.ReinforcementCount != before.ReinforcementCount+10 {
		t.Fatalf("reinforcement_count must still take MAX: %+v", after)
	}
	if after.UpdatedAt != before.UpdatedAt {
		t.Fatalf("updated_at must NOT change when content is not overwritten: %+v", after)
	}
}

// ftsCount reports how many memories_fts rows exist for id (0 or 1 — Index/
// Delete never leave duplicates).
func ftsCount(t *testing.T, e *Engine, id string) int {
	t.Helper()
	var n int
	if err := e.st.DB.QueryRow(`SELECT COUNT(*) FROM memories_fts WHERE memory_id=?`, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestSyncApplyLWWUpdateWithTerminalStatusDropsFromFTS proves a newer-wins
// same-id update that carries a terminal incoming status (done/resolved/
// superseded — e.g. a todo completed on another machine, pulled in here)
// removes the row from the lexical FTS index, exactly like the local
// completion paths (CompleteTodo/ResolveFeedback/an explicit supersede)
// already do. Before this fix, syncApplyExisting's newer-wins branch always
// re-indexed FTS regardless of status, so a remote completion pulled in here
// would keep crowding active memories out of lexical recall's ranked
// candidates even though recall's status filter would eventually exclude it.
func TestSyncApplyLWWUpdateWithTerminalStatusDropsFromFTS(t *testing.T) {
	e := newTestEngine(t, clock.NewFake(time.UnixMilli(1_000_000)))
	ctx := context.Background()

	id, _, _, err := e.Record(ctx, NewMemory{Kind: KindTodo, Title: "t", Body: "ship the feature"})
	if err != nil {
		t.Fatal(err)
	}
	before, _ := e.GetByID(ctx, id)
	if ftsCount(t, e, id) != 1 {
		t.Fatal("setup: an active todo must be FTS-indexed")
	}

	rec := syncwire.Record{
		ID: id, Kind: string(KindTodo), Title: "t", Body: "ship the feature",
		Status: "done", CreatedAt: before.CreatedAt, UpdatedAt: before.UpdatedAt + 1000,
	}
	out, canon, err := e.SyncApply(ctx, rec)
	if err != nil {
		t.Fatal(err)
	}
	if out != SyncUpdated || canon != id {
		t.Fatalf("want updated %s, got %v %s", id, out, canon)
	}
	after, _ := e.GetByID(ctx, id)
	if after.Status != "done" {
		t.Fatalf("status must adopt the incoming terminal status: %+v", after)
	}
	if ftsCount(t, e, id) != 0 {
		t.Fatal("a same-id sync update landing a terminal status must drop the row from memories_fts")
	}
}

// TestSyncApplyStaleRecordAddingRefRebuildsSearchTextAndFTS proves the
// keep-local-content branch (incoming record not newer than local) still
// rebuilds search_text/FTS when the incoming record contributes a NEW ref —
// memory_refs is unioned regardless of which side is newer, and before this
// fix only the newer-wins branch kept search_text/FTS in step with that
// union, leaving the index silently behind memory_refs whenever a stale/
// equal record was the one carrying a new ref.
func TestSyncApplyStaleRecordAddingRefRebuildsSearchTextAndFTS(t *testing.T) {
	e := newTestEngine(t, clock.NewFake(time.UnixMilli(5_000_000)))
	ctx := context.Background()

	id, _, _, err := e.Record(ctx, NewMemory{
		Kind: KindLearning, Title: "t", Body: "retry storage writes on conflict",
		Refs: []Ref{{Type: RefType("file"), Value: "storage/local_only_ref.go"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	before, _ := e.GetByID(ctx, id)

	// Older (or equal) incoming record — content must NOT change — but it
	// carries a ref the local row doesn't have yet.
	rec := syncwire.Record{
		ID: id, Kind: string(KindLearning), Title: "should not appear", Body: "should not appear either",
		Refs:      []syncwire.Ref{{Type: "file", Value: "storage/stale_side_ref.go"}},
		CreatedAt: before.CreatedAt, UpdatedAt: before.UpdatedAt - 1,
	}
	out, canon, err := e.SyncApply(ctx, rec)
	if err != nil {
		t.Fatal(err)
	}
	if out != SyncUpdated || canon != id {
		t.Fatalf("want updated (ref-only merge) %s, got %v %s", id, out, canon)
	}
	after, err := e.GetByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if after.Title != before.Title || after.Body != before.Body {
		t.Fatalf("a stale incoming record must not overwrite content: %+v", after)
	}
	if !strings.Contains(after.SearchText, "local_only_ref") || !strings.Contains(after.SearchText, "stale_side_ref") {
		t.Fatalf("search_text must be rebuilt from the unioned refs even on the stale-record path, got %q", after.SearchText)
	}
	if ftsCount(t, e, id) != 1 {
		t.Fatal("the row is still active, so it must remain FTS-indexed")
	}
	var ftsText string
	if err := e.st.DB.QueryRow(`SELECT search_text FROM memories_fts WHERE memory_id=?`, id).Scan(&ftsText); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ftsText, "stale_side_ref") {
		t.Fatalf("FTS entry must reflect the newly unioned ref, got %q", ftsText)
	}
}

func TestSyncApplyIdempotentReapplyIsNoop(t *testing.T) {
	e := newTestEngine(t, clock.NewFake(time.UnixMilli(1_000_000)))
	ctx := context.Background()

	rec := syncwire.Record{
		ID: "r1", Kind: string(KindLearning), Title: "t", Body: "a durable learning about retries",
		Confidence: 0.8, ReinforcementCount: 2, CreatedAt: 500, UpdatedAt: 700,
	}
	if out, _, err := e.SyncApply(ctx, rec); err != nil || out != SyncInserted {
		t.Fatalf("setup insert: out=%v err=%v", out, err)
	}
	// Re-applying the exact same record (as a naive re-push/re-pull would) must
	// be a genuine no-op: no content changed, so the server must not bump seq
	// for it and the client outbox must stay empty.
	out, canon, err := e.SyncApply(ctx, rec)
	if err != nil {
		t.Fatal(err)
	}
	if out != SyncNoop {
		t.Fatalf("idempotent re-apply must be a Noop, got %v (canon=%s)", out, canon)
	}
}

func TestSyncApplyTombstoneDeletesLocalRow(t *testing.T) {
	e := newTestEngine(t, clock.NewFake(time.UnixMilli(1_000_000)))
	ctx := context.Background()

	id, _, _, err := e.Record(ctx, NewMemory{Kind: KindLearning, Title: "t", Body: "ephemeral note"})
	if err != nil {
		t.Fatal(err)
	}
	before, _ := e.GetByID(ctx, id)

	out, canon, err := e.SyncApply(ctx, syncwire.Record{ID: id, Deleted: true, UpdatedAt: before.UpdatedAt + 1000})
	if err != nil {
		t.Fatal(err)
	}
	if out != SyncTombstoned || canon != id {
		t.Fatalf("want tombstoned %s, got %v %s", id, out, canon)
	}
	if rec, _ := e.GetByID(ctx, id); rec != nil {
		t.Fatalf("tombstone must delete the local row: %+v", rec)
	}
}

func TestSyncApplyTombstoneGuardsAgainstResurrection(t *testing.T) {
	e := newTestEngine(t, clock.NewFake(time.UnixMilli(1_000_000)))
	ctx := context.Background()

	id, _, _, err := e.Record(ctx, NewMemory{Kind: KindLearning, Title: "t", Body: "ephemeral note about caches"})
	if err != nil {
		t.Fatal(err)
	}
	before, _ := e.GetByID(ctx, id)
	deleteAt := before.UpdatedAt + 1000

	if out, _, err := e.SyncApply(ctx, syncwire.Record{ID: id, Deleted: true, UpdatedAt: deleteAt}); err != nil || out != SyncTombstoned {
		t.Fatalf("setup tombstone: out=%v err=%v", out, err)
	}

	// An older non-deleted record for the same id arrives after the tombstone
	// (e.g. a stale, out-of-order pull page) — it must NOT resurrect the row.
	stale := syncwire.Record{
		ID: id, Kind: string(KindLearning), Title: "t", Body: "ephemeral note about caches",
		CreatedAt: before.CreatedAt, UpdatedAt: deleteAt - 1,
	}
	out, _, err := e.SyncApply(ctx, stale)
	if err != nil {
		t.Fatal(err)
	}
	if out != SyncNoop {
		t.Fatalf("a record older than a recorded tombstone must be dropped as Noop, got %v", out)
	}
	if rec, _ := e.GetByID(ctx, id); rec != nil {
		t.Fatalf("resurrection guard failed: row reappeared: %+v", rec)
	}
}

func TestSyncApplyTombstoneOlderThanLocalEditKeepsLocal(t *testing.T) {
	e := newTestEngine(t, clock.NewFake(time.UnixMilli(10_000_000)))
	ctx := context.Background()

	id, _, _, err := e.Record(ctx, NewMemory{Kind: KindLearning, Title: "t", Body: "still relevant note"})
	if err != nil {
		t.Fatal(err)
	}
	before, _ := e.GetByID(ctx, id)

	// A tombstone that predates the local row's last edit must lose: the local
	// edit happened after the remote decided to delete it.
	out, _, err := e.SyncApply(ctx, syncwire.Record{ID: id, Deleted: true, UpdatedAt: before.UpdatedAt - 1})
	if err != nil {
		t.Fatal(err)
	}
	if out != SyncNoop {
		t.Fatalf("a stale tombstone must not win over a newer local edit, got %v", out)
	}
	if rec, _ := e.GetByID(ctx, id); rec == nil {
		t.Fatal("local row must survive a stale tombstone")
	}
}

// TestSyncApplyTombstoneResolvesNearDupMergeAlias covers the scenario a
// review pass flagged: a tombstone arriving for an id that was previously
// merged into a different canonical row (via syncApplyAbsent's near-dup
// reinforce path, which never creates a row at the incoming id) must delete
// and tombstone the CANONICAL row, not silently no-op against an id that was
// never a row on this store. This is reachable in practice whenever a peer
// (including the sync server itself) receives two independently-authored
// near-duplicates under different ids and merges the later-seen one into the
// earlier — a subsequent Forget of the original id, authored on the machine
// that owns it, must still take effect here.
func TestSyncApplyTombstoneResolvesNearDupMergeAlias(t *testing.T) {
	e := newTestEngine(t, clock.NewFake(time.UnixMilli(1_000_000)))
	ctx := context.Background()

	// This store's own canonical row (e.g. authored locally, or pulled first).
	canonID, _, _, err := e.Record(ctx, NewMemory{
		Kind: KindLearning, Title: "t", Body: "always jitter retry backoff to avoid thundering herds",
	})
	if err != nil {
		t.Fatal(err)
	}

	// A near-duplicate arrives under a DIFFERENT id ("origin-1" — this is the
	// id the AUTHORING machine considers its own canonical row for this
	// memory). It has no local match by id, but content-matches canonID, so it
	// reinforces canonID and is recorded as an alias in memory_sync_merges
	// rather than ever becoming its own row.
	dup := syncwire.Record{
		ID: "origin-1", Kind: string(KindLearning), Title: "t",
		Body: "always jitter retry backoff to avoid thundering herds!", CreatedAt: 500, UpdatedAt: 500,
	}
	out, mergedInto, err := e.SyncApply(ctx, dup)
	if err != nil {
		t.Fatal(err)
	}
	if out != SyncReinforced || mergedInto != canonID {
		t.Fatalf("setup: want reinforced into %s, got %v %s", canonID, out, mergedInto)
	}
	if got, _ := e.GetByID(ctx, "origin-1"); got != nil {
		t.Fatalf("setup: origin-1 must never become its own row: %+v", got)
	}

	canonAfterMerge, _ := e.GetByID(ctx, canonID)

	// The authoring machine later Forgets its own row (id "origin-1") and that
	// tombstone reaches this store via sync (pull, or the server applying a
	// push) — it must resolve through the alias and delete the CANONICAL row.
	// The delete's clock must be newer than the reinforce's own updated_at
	// bump above, or it is (correctly) treated as a stale tombstone losing to
	// a later local edit — not the scenario under test here.
	deleteAt := canonAfterMerge.UpdatedAt + 1000
	out, target, err := e.SyncApply(ctx, syncwire.Record{ID: "origin-1", Deleted: true, UpdatedAt: deleteAt})
	if err != nil {
		t.Fatal(err)
	}
	if out != SyncTombstoned {
		t.Fatalf("want tombstoned, got %v", out)
	}
	if target != canonID {
		t.Fatalf("tombstone must resolve to the canonical id %s, got %s", canonID, target)
	}
	if got, _ := e.GetByID(ctx, canonID); got != nil {
		t.Fatalf("the canonical row must be deleted, but survived: %+v", got)
	}

	tombAt, hasTomb, err := e.getTombstone(ctx, e.st.DB, canonID)
	if err != nil {
		t.Fatal(err)
	}
	if !hasTomb || tombAt != deleteAt {
		t.Fatalf("the tombstone must be recorded at the CANONICAL id, got hasTomb=%v at=%d", hasTomb, tombAt)
	}

	// A later independent push of the same near-duplicate content, still under
	// the original id, must not resurrect the canonical row: the replay guard
	// in syncApplyAbsent (memory_sync_merges, checked before any tombstone
	// lookup) already Noops an at-or-before-merge-time replay of "origin-1"
	// regardless of the delete.
	out, _, err = e.SyncApply(ctx, dup)
	if err != nil {
		t.Fatal(err)
	}
	if out != SyncNoop {
		t.Fatalf("a replay of the original near-dup push must not resurrect the deleted canonical row, got %v", out)
	}
	if got, _ := e.GetByID(ctx, canonID); got != nil {
		t.Fatalf("canonical row must stay deleted after a replayed near-dup push: %+v", got)
	}
}

// TestSyncApplyNonDeletedAliasRecordDoesNotResurrectAfterCanonicalTombstone
// covers a gap a review pass found in TestSyncApplyTombstoneResolvesNearDupMergeAlias's
// fix: that test only replayed the EXACT same (id, updated_at) near-duplicate
// record after the delete, which syncApplyAbsent's own memory_sync_merges
// replay guard already Noops regardless of any tombstone (it compares
// against the merge time, not the delete time). The gap is a DIFFERENT
// non-deleted record for the same alias id, with an updated_at strictly
// BETWEEN the merge time and the delete time: the replay guard lets it
// through (rec.UpdatedAt > mergedAt), but until the alias is resolved to the
// canonical id BEFORE the tombstone check, the tombstone lookup on the raw
// alias id finds nothing (the alias never has its own tombstone — the
// canonical id does), so the record falls through to syncApplyAbsent, finds
// no surviving near-duplicate (the canonical row is gone), and inserts a
// fresh row — resurrecting the memory under the alias id.
func TestSyncApplyNonDeletedAliasRecordDoesNotResurrectAfterCanonicalTombstone(t *testing.T) {
	e := newTestEngine(t, clock.NewFake(time.UnixMilli(1_000_000)))
	ctx := context.Background()

	canonID, _, _, err := e.Record(ctx, NewMemory{
		Kind: KindLearning, Title: "t", Body: "always cap goroutine pool size under load",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Merge alias "origin-2" -> canonID at updated_at=500.
	dup := syncwire.Record{
		ID: "origin-2", Kind: string(KindLearning), Title: "t",
		Body: "always cap goroutine pool size under load!", CreatedAt: 500, UpdatedAt: 500,
	}
	out, mergedInto, err := e.SyncApply(ctx, dup)
	if err != nil {
		t.Fatal(err)
	}
	if out != SyncReinforced || mergedInto != canonID {
		t.Fatalf("setup: want reinforced into %s, got %v %s", canonID, out, mergedInto)
	}

	canonAfterMerge, _ := e.GetByID(ctx, canonID)
	deleteAt := canonAfterMerge.UpdatedAt + 1000
	out, target, err := e.SyncApply(ctx, syncwire.Record{ID: "origin-2", Deleted: true, UpdatedAt: deleteAt})
	if err != nil {
		t.Fatal(err)
	}
	if out != SyncTombstoned || target != canonID {
		t.Fatalf("setup: want tombstoned %s, got %v %s", canonID, out, target)
	}
	if got, _ := e.GetByID(ctx, canonID); got != nil {
		t.Fatalf("setup: canonical row must be deleted: %+v", got)
	}

	// A delayed, non-deleted record for the alias id "origin-2" arrives with an
	// updated_at strictly between the merge (500) and the delete (deleteAt) —
	// newer than the merge, so the replay guard alone does not stop it; older
	// than the delete, so it must be dropped as a resurrection attempt via the
	// canonical tombstone, exactly like an ordinary same-id stale record would be.
	delayed := syncwire.Record{
		ID: "origin-2", Kind: string(KindLearning), Title: "t",
		Body: "always cap goroutine pool size under load!", CreatedAt: 500, UpdatedAt: deleteAt - 1,
	}
	out, _, err = e.SyncApply(ctx, delayed)
	if err != nil {
		t.Fatal(err)
	}
	if out != SyncNoop {
		t.Fatalf("a non-deleted alias record older than the canonical tombstone must be dropped as Noop, got %v", out)
	}
	if got, _ := e.GetByID(ctx, canonID); got != nil {
		t.Fatalf("canonical row must stay deleted, but a resurrection occurred: %+v", got)
	}
	if got, _ := e.GetByID(ctx, "origin-2"); got != nil {
		t.Fatalf("the alias id must not become its own resurrected row either: %+v", got)
	}

	// A genuinely newer record (after the delete) legitimately resurrects, same
	// as the ordinary same-id tombstone-vs-newer-record rule.
	newer := delayed
	newer.UpdatedAt = deleteAt + 1000
	out, id, err := e.SyncApply(ctx, newer)
	if err != nil {
		t.Fatal(err)
	}
	if out != SyncInserted || id != "origin-2" {
		t.Fatalf("a record newer than the tombstone must legitimately re-insert, got %v %s", out, id)
	}
}

// TestSyncApplyStaleMergeAliasDoesNotMisdirectTombstoneAfterResurrection
// covers a gap a review pass found in the legitimate-reinsertion path
// TestSyncApplyNonDeletedAliasRecordDoesNotResurrectAfterCanonicalTombstone
// exercises only up to the reinsert itself: once alias "origin-3" legitimately
// reinserts as its OWN row after its old canonical merge target was deleted,
// the STALE memory_sync_merges("origin-3" -> old canonical) row must not
// survive that reinsertion. Before this fix it did, so a LATER tombstone for
// "origin-3" (deleting the newly resurrected row) resolved through the stale
// alias to the old, already-deleted canonical id instead — re-tombstoning a
// row that no longer exists while leaving the actually-live resurrected row
// under "origin-3" undeletable forever, and (on the sync server) indexing the
// delete under the wrong id entirely.
func TestSyncApplyStaleMergeAliasDoesNotMisdirectTombstoneAfterResurrection(t *testing.T) {
	e := newTestEngine(t, clock.NewFake(time.UnixMilli(1_000_000)))
	ctx := context.Background()

	canonID, _, _, err := e.Record(ctx, NewMemory{
		Kind: KindLearning, Title: "t", Body: "always cap goroutine pool size under load",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Merge alias "origin-3" -> canonID at updated_at=500.
	dup := syncwire.Record{
		ID: "origin-3", Kind: string(KindLearning), Title: "t",
		Body: "always cap goroutine pool size under load!", CreatedAt: 500, UpdatedAt: 500,
	}
	out, mergedInto, err := e.SyncApply(ctx, dup)
	if err != nil {
		t.Fatal(err)
	}
	if out != SyncReinforced || mergedInto != canonID {
		t.Fatalf("setup: want reinforced into %s, got %v %s", canonID, out, mergedInto)
	}

	// Delete the canonical row via a tombstone for the alias (resolves through
	// memory_sync_merges, as TestSyncApplyTombstoneResolvesNearDupMergeAlias
	// already proves).
	canonAfterMerge, _ := e.GetByID(ctx, canonID)
	deleteAt := canonAfterMerge.UpdatedAt + 1000
	out, target, err := e.SyncApply(ctx, syncwire.Record{ID: "origin-3", Deleted: true, UpdatedAt: deleteAt})
	if err != nil {
		t.Fatal(err)
	}
	if out != SyncTombstoned || target != canonID {
		t.Fatalf("setup: want tombstoned %s, got %v %s", canonID, out, target)
	}

	// A genuinely newer, non-deleted record for "origin-3" legitimately
	// resurrects it as its OWN row (the canonical row is gone, so there is no
	// surviving near-duplicate to reinforce).
	resurrectAt := deleteAt + 1000
	resurrected := syncwire.Record{
		ID: "origin-3", Kind: string(KindLearning), Title: "t",
		Body: "always cap goroutine pool size under load, reborn", CreatedAt: resurrectAt, UpdatedAt: resurrectAt,
	}
	out, id, err := e.SyncApply(ctx, resurrected)
	if err != nil {
		t.Fatal(err)
	}
	if out != SyncInserted || id != "origin-3" {
		t.Fatalf("setup: want a legitimate reinsert at origin-3, got %v %s", out, id)
	}
	if got, _ := e.GetByID(ctx, "origin-3"); got == nil {
		t.Fatal("setup: the resurrected row must exist at origin-3")
	}

	// A later forget of the resurrected row arrives as a tombstone for
	// "origin-3" — it must delete THAT row, not silently redirect through the
	// stale (pre-resurrection) alias to the long-gone canonical id.
	forgetAt := resurrectAt + 1000
	out, target, err = e.SyncApply(ctx, syncwire.Record{ID: "origin-3", Deleted: true, UpdatedAt: forgetAt})
	if err != nil {
		t.Fatal(err)
	}
	if out != SyncTombstoned {
		t.Fatalf("want tombstoned, got %v", out)
	}
	if target != "origin-3" {
		t.Fatalf("a stale alias must not misdirect the tombstone: want target=origin-3, got %s", target)
	}
	if got, _ := e.GetByID(ctx, "origin-3"); got != nil {
		t.Fatalf("the resurrected row must be deleted, but survived: %+v", got)
	}
}

func TestSyncApplyTombstoneNotPushedBack(t *testing.T) {
	e := newTestEngine(t, clock.NewFake(time.UnixMilli(1_000_000)))
	ctx := context.Background()

	id, _, _, err := e.Record(ctx, NewMemory{Kind: KindLearning, Title: "t", Body: "ephemeral note"})
	if err != nil {
		t.Fatal(err)
	}
	before, _ := e.GetByID(ctx, id)

	// Simulate learning about this delete via a PULL from the server (not a
	// local Forget): the resulting local tombstone must be marked as already
	// known remotely, so it never gets queued for an outbound push.
	if out, _, err := e.SyncApply(ctx, syncwire.Record{ID: id, Deleted: true, UpdatedAt: before.UpdatedAt + 1}); err != nil || out != SyncTombstoned {
		t.Fatalf("setup: out=%v err=%v", out, err)
	}
	pending, err := e.PendingTombstones(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range pending {
		if p.ID == id {
			t.Fatalf("a remote-learned tombstone must not be queued for push: %+v", p)
		}
	}
}

func TestForgetTombstoneIsQueuedForPush(t *testing.T) {
	e := newTestEngine(t, clock.NewFake(time.UnixMilli(1_000_000)))
	ctx := context.Background()

	id, _, _, err := e.Record(ctx, NewMemory{Kind: KindLearning, Title: "t", Body: "forget me"})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.ClearPushed(ctx, []string{id}); err != nil {
		t.Fatal(err)
	}
	if ok, err := e.Forget(ctx, id, "test"); err != nil || !ok {
		t.Fatalf("Forget: ok=%v err=%v", ok, err)
	}
	pending, err := e.PendingTombstones(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, p := range pending {
		if p.ID == id {
			found = true
		}
	}
	if !found {
		t.Fatalf("an explicit Forget must queue a tombstone for push, got %+v", pending)
	}

	if err := e.MarkTombstonesPushed(ctx, []string{id}); err != nil {
		t.Fatal(err)
	}
	pending, err = e.PendingTombstones(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range pending {
		if p.ID == id {
			t.Fatalf("MarkTombstonesPushed must clear the pending queue: %+v", pending)
		}
	}
}

func TestCompactionNeverWritesTombstones(t *testing.T) {
	clk := clock.NewFake(time.UnixMilli(1_000_000))
	e := newTestEngine(t, clk)
	ctx := context.Background()

	// Two near-duplicate active learnings: dedupSweep should merge (supersede)
	// one into the other without ever touching memory_tombstones.
	if _, _, _, err := e.Record(ctx, NewMemory{Kind: KindLearning, Title: "t", Body: "always vendor go modules for reproducible builds"}); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := e.Record(ctx, NewMemory{Kind: KindLearning, Title: "t", Body: "always vendor go modules for reproducible builds!"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.dedupSweep(ctx, false, defaultDedupBuckets); err != nil {
		t.Fatal(err)
	}
	// Also exercise archive/GC paths directly.
	if _, err := e.gcSuperseded(ctx, false); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := e.evictToBudget(ctx, 1, 0, false); err != nil {
		t.Fatal(err)
	}
	if _, err := e.gcArchived(ctx, 1000, false); err != nil {
		t.Fatal(err)
	}

	var n int
	if err := e.st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM memory_tombstones`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("compaction must never write tombstones (only explicit Forget does), found %d", n)
	}
}

func TestPendingPushClearedAfterPush(t *testing.T) {
	e := newTestEngine(t, clock.NewFake(time.UnixMilli(1_000_000)))
	ctx := context.Background()

	id, _, _, err := e.Record(ctx, NewMemory{Kind: KindLearning, Title: "t", Body: "locally authored knowledge"})
	if err != nil {
		t.Fatal(err)
	}
	pending, err := e.PendingPush(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].ID != id {
		t.Fatalf("locally-authored Record must be queued for push, got %+v", pending)
	}
	if err := e.ClearPushed(ctx, []string{id}); err != nil {
		t.Fatal(err)
	}
	pending, err = e.PendingPush(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("ClearPushed must empty the outbox, got %+v", pending)
	}
}

func TestCompleteTodoQueuesForPushWithDoneStatus(t *testing.T) {
	e := newTestEngine(t, clock.NewFake(time.UnixMilli(1_000_000)))
	ctx := context.Background()

	id, _, _, err := e.Record(ctx, NewMemory{Kind: KindTodo, Title: "t", Body: "ship the feature"})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.ClearPushed(ctx, []string{id}); err != nil {
		t.Fatal(err)
	}

	if ok, err := e.CompleteTodo(ctx, id); err != nil || !ok {
		t.Fatalf("CompleteTodo: ok=%v err=%v", ok, err)
	}

	pending, err := e.PendingPush(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].ID != id {
		t.Fatalf("completing a todo must queue it for push, got %+v", pending)
	}
	if pending[0].Status != "done" {
		t.Fatalf("pushed record must carry the done status, got %+v", pending[0])
	}
}

func TestResolveFeedbackQueuesForPush(t *testing.T) {
	e := newTestEngine(t, clock.NewFake(time.UnixMilli(1_000_000)))
	ctx := context.Background()

	id, _, _, err := e.Record(ctx, NewMemory{Kind: KindFeedback, Title: "t", Body: "recarto ranked the wrong result first"})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.ClearPushed(ctx, []string{id}); err != nil {
		t.Fatal(err)
	}

	if ok, err := e.ResolveFeedback(ctx, id, "fixed in v2"); err != nil || !ok {
		t.Fatalf("ResolveFeedback: ok=%v err=%v", ok, err)
	}

	pending, err := e.PendingPush(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].ID != id || pending[0].Status != "resolved" {
		t.Fatalf("resolving feedback must queue it for push as resolved, got %+v", pending)
	}
}

func TestSupersedeQueuesOldMemoryForPush(t *testing.T) {
	e := newTestEngine(t, clock.NewFake(time.UnixMilli(1_000_000)))
	ctx := context.Background()

	oldID, _, _, err := e.Record(ctx, NewMemory{Kind: KindDecision, Title: "t", Body: "use sqlite directly"})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.ClearPushed(ctx, []string{oldID}); err != nil {
		t.Fatal(err)
	}

	newID, _, _, err := e.Record(ctx, NewMemory{
		Kind: KindDecision, Title: "t2", Body: "use a query builder instead", SupersedesID: oldID,
	})
	if err != nil {
		t.Fatal(err)
	}

	pending, err := e.PendingPush(ctx)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]syncwire.Record{}
	for _, p := range pending {
		byID[p.ID] = p
	}
	if _, ok := byID[newID]; !ok {
		t.Fatalf("the new superseding record must be queued for push, got %+v", pending)
	}
	old, ok := byID[oldID]
	if !ok {
		t.Fatalf("an explicitly superseded record must also be queued for push (other machines must stop treating it as active), got %+v", pending)
	}
	if old.Status != "superseded" || old.SupersededBy != newID {
		t.Fatalf("superseded record's wire status must reflect the supersede, got %+v", old)
	}
}

func TestSyncApplyBootstrapInsertLandsNonActiveStatus(t *testing.T) {
	e := newTestEngine(t, clock.NewFake(time.UnixMilli(1_000_000)))
	ctx := context.Background()

	// Simulates a fresh machine bootstrapping (pulling for the very first time)
	// a todo that was already completed on the origin machine before this one
	// ever saw it: the id has never existed locally, so SyncApply takes the
	// insert path — it must not resurrect the todo as active.
	rec := syncwire.Record{
		ID: "todo-done-1", Kind: string(KindTodo), Title: "t", Body: "ship the feature",
		Status: "done", CreatedAt: 500, UpdatedAt: 700,
	}
	out, id, err := e.SyncApply(ctx, rec)
	if err != nil {
		t.Fatal(err)
	}
	if out != SyncInserted || id != "todo-done-1" {
		t.Fatalf("want inserted todo-done-1, got %v %s", out, id)
	}
	got, err := e.GetByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "done" {
		t.Fatalf("bootstrap insert must preserve the incoming non-active status, got %+v", got)
	}
	active, err := e.ActiveTodos(ctx, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range active {
		if a.ID == id {
			t.Fatalf("a bootstrapped done todo must not surface as an active todo: %+v", a)
		}
	}
}

func TestSyncApplyTxHookFailureRollsBackTheMerge(t *testing.T) {
	e := newTestEngine(t, clock.NewFake(time.UnixMilli(1_000_000)))
	ctx := context.Background()

	rec := syncwire.Record{
		ID: "r1", Kind: string(KindLearning), Title: "t", Body: "a durable learning about retries",
		CreatedAt: 500, UpdatedAt: 700,
	}
	hookErr := errors.New("simulated index-bump failure")
	_, _, err := e.SyncApplyTx(ctx, rec, func(c store.Conn, outcome SyncOutcome, canonID string) error {
		return hookErr
	})
	if !errors.Is(err, hookErr) {
		t.Fatalf("SyncApplyTx must surface the hook's error, got %v", err)
	}
	// The whole write transaction — merge AND hook — must roll back together:
	// this is the atomicity the hook exists to provide (the sync server's
	// seq/sync_index bump must never be separable from the memory it indexes).
	if got, _ := e.GetByID(ctx, "r1"); got != nil {
		t.Fatalf("a failed afterApply hook must roll back the merge too, but the row was committed: %+v", got)
	}
}
