package store

import (
	"context"
	"database/sql"
	"fmt"
)

// schemaVersion is the number of applied migrations.
const schemaVersion = 3

// migrations are applied in order; index+1 becomes user_version.
var migrations = []string{
	// v1: core schema (no FTS yet — FTS5 is added in Phase 1B).
	`
CREATE TABLE meta (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);

CREATE TABLE memories (
  id                  TEXT PRIMARY KEY,
  kind                TEXT NOT NULL CHECK (kind IN
                        ('preference','learning','decision','do_not_repeat','bug','action')),
  title               TEXT NOT NULL DEFAULT '',
  body                TEXT NOT NULL DEFAULT '',
  search_text         TEXT NOT NULL DEFAULT '',
  created_at          INTEGER NOT NULL,
  updated_at          INTEGER NOT NULL,
  branch              TEXT,
  worktree            TEXT,
  author              TEXT NOT NULL DEFAULT '',
  source              TEXT NOT NULL DEFAULT '',
  confidence          REAL NOT NULL DEFAULT 1.0,
  reinforcement_count INTEGER NOT NULL DEFAULT 0,
  pinned              INTEGER NOT NULL DEFAULT 0,
  status              TEXT NOT NULL DEFAULT 'active' CHECK (status IN
                        ('active','superseded','archived')),
  superseded_by       TEXT,
  simhash             INTEGER NOT NULL DEFAULT 0,
  extra               TEXT NOT NULL DEFAULT '{}'
);
CREATE INDEX idx_memories_kind   ON memories(kind);
CREATE INDEX idx_memories_status ON memories(status);
CREATE INDEX idx_memories_branch ON memories(branch);

CREATE TABLE memory_refs (
  memory_id TEXT NOT NULL REFERENCES memories(id) ON DELETE CASCADE,
  ref_type  TEXT NOT NULL CHECK (ref_type IN ('file','symbol','tag')),
  ref_value TEXT NOT NULL,
  PRIMARY KEY (memory_id, ref_type, ref_value)
);
CREATE INDEX idx_memory_refs_lookup ON memory_refs(ref_type, ref_value);

CREATE TABLE memory_minhash (
  memory_id TEXT NOT NULL REFERENCES memories(id) ON DELETE CASCADE,
  band      INTEGER NOT NULL,
  hash      INTEGER NOT NULL,
  PRIMARY KEY (memory_id, band)
);
CREATE INDEX idx_memory_minhash_lookup ON memory_minhash(band, hash);

CREATE TABLE counters (
  scope  TEXT NOT NULL,
  metric TEXT NOT NULL,
  value  INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (scope, metric)
);

CREATE TABLE sessions (
  id         TEXT PRIMARY KEY,
  agent      TEXT NOT NULL DEFAULT '',
  branch     TEXT,
  worktree   TEXT,
  started_at INTEGER NOT NULL,
  ended_at   INTEGER
);
`,
	// v2: FTS5 full-text index over search_text (Phase 1B). memory_id is
	// UNINDEXED so it is stored and returned but not tokenized. Rows are synced
	// manually from the memory write path — no external-content linkage, because
	// memories.id is a TEXT UUID, not the INTEGER rowid FTS5 external content
	// requires.
	`
CREATE VIRTUAL TABLE memories_fts USING fts5(
  memory_id UNINDEXED,
  search_text
);
`,
	// v3 (Phase 2): worktree-aware code graph. files is the reconcile diff
	// target; nodes/edges inherit worktree via file_id. nodes_fts is manually
	// synced (FTS5 has no external-content link to TEXT-uuid rowids).
	`
CREATE TABLE files (
  id              TEXT PRIMARY KEY,
  worktree        TEXT NOT NULL,
  path            TEXT NOT NULL,
  language        TEXT NOT NULL,
  content_hash    TEXT NOT NULL,
  mtime           INTEGER NOT NULL,
  size            INTEGER NOT NULL,
  last_indexed_at INTEGER NOT NULL,
  UNIQUE (worktree, path)
);
CREATE INDEX idx_files_worktree ON files(worktree);

CREATE TABLE nodes (
  id             TEXT PRIMARY KEY,
  file_id        TEXT NOT NULL REFERENCES files(id) ON DELETE CASCADE,
  kind           TEXT NOT NULL,
  name           TEXT NOT NULL,
  qualified_name TEXT NOT NULL DEFAULT '',
  signature      TEXT NOT NULL DEFAULT '',
  start_byte     INTEGER NOT NULL,
  end_byte       INTEGER NOT NULL,
  start_line     INTEGER NOT NULL,
  end_line       INTEGER NOT NULL
);
CREATE INDEX idx_nodes_file ON nodes(file_id);
CREATE INDEX idx_nodes_name ON nodes(name);
CREATE INDEX idx_nodes_qname ON nodes(qualified_name);

-- src_node_id is intentionally nullable: a file-scope reference (an import, or
-- a call not nested inside any symbol) has no enclosing definition node.
CREATE TABLE edges (
  id          TEXT PRIMARY KEY,
  src_node_id TEXT REFERENCES nodes(id) ON DELETE CASCADE,
  dst_node_id TEXT REFERENCES nodes(id) ON DELETE SET NULL,
  dst_name    TEXT NOT NULL DEFAULT '',
  kind        TEXT NOT NULL,
  file_id     TEXT NOT NULL REFERENCES files(id) ON DELETE CASCADE,
  line        INTEGER NOT NULL
);
CREATE INDEX idx_edges_src ON edges(src_node_id, kind);
CREATE INDEX idx_edges_dst ON edges(dst_node_id);
CREATE INDEX idx_edges_dstname ON edges(dst_name);

CREATE VIRTUAL TABLE nodes_fts USING fts5(
  node_id UNINDEXED,
  name,
  qualified_name
);
`,
}

// Migrate applies pending migrations under an IMMEDIATE lock so that
// concurrent processes opening a fresh DB cannot double-apply.
func Migrate(db *sql.DB) error {
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	rollback := func(e error) error {
		_, _ = conn.ExecContext(ctx, "ROLLBACK")
		return e
	}

	var current int
	if err := conn.QueryRowContext(ctx, "PRAGMA user_version").Scan(&current); err != nil {
		return rollback(err)
	}
	for i := current; i < len(migrations); i++ {
		if _, err := conn.ExecContext(ctx, migrations[i]); err != nil {
			return rollback(fmt.Errorf("migration %d: %w", i+1, err))
		}
	}
	// PRAGMA user_version does not accept bound params; format the literal int.
	if _, err := conn.ExecContext(ctx,
		fmt.Sprintf("PRAGMA user_version = %d", len(migrations))); err != nil {
		return rollback(err)
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return rollback(err)
	}
	return nil
}
