// Package syncwire defines the portable JSON wire shapes exchanged between a
// recarto client (internal/remotesync) and the recarto sync server
// (internal/syncserver) over the HTTP memory-sync API. It carries no logic —
// merge semantics live in memory.SyncApply, which both sides call with these
// same types (the server is, structurally, a headless recarto store).
package syncwire

// Ref is a structural reference attached to a memory (mirrors memory.Ref).
type Ref struct {
	Type  string `json:"type"`
	Value string `json:"value"`
}

// Record is one memory row on the wire. It is a superset of the fields
// memory.SyncApply needs to insert, LWW-update, reinforce, or tombstone a row.
//
// Deliberately excluded (local runtime/activity state, never portable):
// worktree, last_used_epoch, recall_count, archived_epoch, and the derived
// FTS/MinHash rows (recomputed on apply). Only active memories and tombstones
// are ever placed on the wire — the code graph never is (internal/graph is
// per-worktree, derived, and rebuilt locally in seconds).
type Record struct {
	ID                 string         `json:"id"`
	Kind               string         `json:"kind,omitempty"`
	Title              string         `json:"title,omitempty"`
	Body               string         `json:"body,omitempty"`
	Refs               []Ref          `json:"refs,omitempty"`
	Branch             string         `json:"branch,omitempty"`
	Author             string         `json:"author,omitempty"`
	Source             string         `json:"source,omitempty"`
	Confidence         float64        `json:"confidence,omitempty"`
	ReinforcementCount int            `json:"reinforcement_count,omitempty"`
	Pinned             bool           `json:"pinned,omitempty"`
	Status             string         `json:"status,omitempty"`
	SupersededBy       string         `json:"superseded_by,omitempty"`
	Extra              map[string]any `json:"extra,omitempty"`
	CreatedAt          int64          `json:"created_at"`
	UpdatedAt          int64          `json:"updated_at"`
	// Deleted marks a tombstone: id + updated_at (used as the delete's logical
	// clock) are the only fields that matter. All other fields are empty.
	Deleted bool `json:"deleted,omitempty"`
}

// ChangesResponse answers GET /v1/repos/{key}/changes.
type ChangesResponse struct {
	Records []Record `json:"records"`
	Cursor  int64    `json:"cursor"` // max seq applied; pass back as `since` next call
	More    bool     `json:"more"`   // true if another page is immediately available
}

// PushRequest is the body of POST /v1/repos/{key}/changes: locally-authored
// records (including tombstones) not yet known to the server.
type PushRequest struct {
	Records []Record `json:"records"`
}

// PushResponse answers POST /v1/repos/{key}/changes.
type PushResponse struct {
	Cursor   int64 `json:"cursor"`   // the repo's current max seq, for diagnostics
	Accepted int   `json:"accepted"` // records that produced a genuine change
}

// ExportFormat identifies a recarto-export/v1 archive (see ExportHeader).
const ExportFormat = "recarto-export/v1"

// ExportRepoMeta describes one project in an export archive's header.
type ExportRepoMeta struct {
	Key        string `json:"key"`
	Name       string `json:"name,omitempty"`
	RootCommit string `json:"root_commit,omitempty"`
}

// ExportHeader is the first line of a recarto-export/v1 archive (see
// internal/fullexport): informational metadata about every project the
// archive covers. It carries no records of its own — the importer detects it
// by Format and skips it.
type ExportHeader struct {
	Format         string           `json:"format"`
	CreatedAt      int64            `json:"created_at"`
	RecartoVersion string           `json:"recarto_version"`
	Repos          []ExportRepoMeta `json:"repos"`
}

// ExportEntry is one non-header line of an export archive: a single memory
// record (or tombstone), tagged with the server repo key it belongs to. Every
// entry is self-describing, so the server applies it without buffering the
// whole archive.
type ExportEntry struct {
	RepoKey string `json:"repo_key"`
	Record  Record `json:"record"`
}

// ImportResponse answers POST /v1/import.
type ImportResponse struct {
	Repos         map[string]RepoImportStat `json:"repos"`
	TotalEntries  int                       `json:"total_entries"`
	TotalAccepted int                       `json:"total_accepted"`
}

// RepoImportStat tallies one repo key's share of an import.
type RepoImportStat struct {
	Entries  int `json:"entries"`
	Accepted int `json:"accepted"`
}
