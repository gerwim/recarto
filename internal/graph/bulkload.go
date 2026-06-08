package graph

import (
	"context"

	"git.gerwim.nl/gerwim/recarto/internal/store"
)

// bulkLoadMinFiles is the worklist size above which a fresh index drops and
// rebuilds the secondary indexes rather than maintaining them inline. Below it the
// one-pass rebuild would not pay for itself.
const bulkLoadMinFiles = 2000

// graphSecondaryIndexes is the canonical set of non-PK, non-FTS indexes on the
// bulk-written graph tables. During a large first index the writer otherwise
// maintains all of them across ~1M row inserts; dropping them up front and
// rebuilding each once over the finished tables is far cheaper. They MUST be
// rebuilt before Resolve, which keys on idx_nodes_name / idx_type_bindings_file_name
// / etc.
//
// This list mirrors the indexes created in migrate.go and is the source of truth
// for the rebuild (createGraphSecondaryIndexes is also the self-heal path, so it
// cannot rely on definitions captured at drop time). Keep it in sync: adding an
// index here that migrate.go does not create, or vice versa, only costs the
// optimization — except REMOVING an index from the schema, which also requires
// removing it here so the rebuild does not resurrect it.
var graphSecondaryIndexes = []struct{ name, create string }{
	{"idx_nodes_file", `CREATE INDEX IF NOT EXISTS idx_nodes_file ON nodes(file_id)`},
	{"idx_nodes_name", `CREATE INDEX IF NOT EXISTS idx_nodes_name ON nodes(name)`},
	{"idx_nodes_qname", `CREATE INDEX IF NOT EXISTS idx_nodes_qname ON nodes(qualified_name)`},
	{"idx_nodes_name_parent", `CREATE INDEX IF NOT EXISTS idx_nodes_name_parent ON nodes(name, parent_name)`},
	{"idx_edges_src", `CREATE INDEX IF NOT EXISTS idx_edges_src ON edges(src_node_id, kind)`},
	{"idx_edges_dst", `CREATE INDEX IF NOT EXISTS idx_edges_dst ON edges(dst_node_id)`},
	{"idx_edges_dstname", `CREATE INDEX IF NOT EXISTS idx_edges_dstname ON edges(dst_name)`},
	{"idx_edges_file", `CREATE INDEX IF NOT EXISTS idx_edges_file ON edges(file_id)`},
	{"idx_type_bindings_file", `CREATE INDEX IF NOT EXISTS idx_type_bindings_file ON type_bindings(file_id)`},
	{"idx_type_bindings_name", `CREATE INDEX IF NOT EXISTS idx_type_bindings_name ON type_bindings(name)`},
	{"idx_type_bindings_file_name", `CREATE INDEX IF NOT EXISTS idx_type_bindings_file_name ON type_bindings(file_id, name)`},
}

// dropGraphSecondaryIndexes removes the secondary indexes so the bulk write does
// not pay per-row index maintenance. Safe only when no concurrent reader needs
// them; used for a fresh first index.
func (e *Engine) dropGraphSecondaryIndexes(ctx context.Context) error {
	return e.st.WriteTx(ctx, func(c store.Conn) error {
		for _, ix := range graphSecondaryIndexes {
			if _, err := c.ExecContext(ctx, `DROP INDEX IF EXISTS `+ix.name); err != nil {
				return err
			}
		}
		return nil
	})
}

// createGraphSecondaryIndexes rebuilds the secondary indexes in one bulk pass each
// over the finished tables. Must run after the bulk write and before Resolve.
func (e *Engine) createGraphSecondaryIndexes(ctx context.Context) error {
	return e.st.WriteTx(ctx, func(c store.Conn) error {
		for _, ix := range graphSecondaryIndexes {
			if _, err := c.ExecContext(ctx, ix.create); err != nil {
				return err
			}
		}
		return nil
	})
}
