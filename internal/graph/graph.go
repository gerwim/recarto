// Package graph builds and queries Recarto's worktree-aware code graph over the
// shared per-repo store. It mirrors internal/memory.Engine: write side (Index/
// Reconcile) and read side (Search/TraceCallPath/Architecture/Query/Snippet). It
// is git-agnostic — callers pass the worktree and a file Source.
package graph

import (
	"crypto/sha256"
	"encoding/hex"

	"git.gerwim.nl/gerwim/recarto/internal/clock"
	"git.gerwim.nl/gerwim/recarto/internal/ledger"
	"git.gerwim.nl/gerwim/recarto/internal/parse"
	"git.gerwim.nl/gerwim/recarto/internal/store"
)

// Engine indexes files into, and answers structural queries over, the store.
type Engine struct {
	st  *store.Store
	led *ledger.Ledger
	clk clock.Clock
	reg *parse.Registry
	emb Embedder // optional semantic tier; nil => lexical-only search
}

// New builds an Engine, compiling the parse registry (which links tree-sitter).
func New(st *store.Store, led *ledger.Ledger, clk clock.Clock) (*Engine, error) {
	reg, err := parse.NewRegistry()
	if err != nil {
		return nil, err
	}
	return &Engine{st: st, led: led, clk: clk, reg: reg}, nil
}

// Close releases the parse registry's compiled queries.
func (e *Engine) Close() { e.reg.Close() }

func sha256hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// span is a node id with its byte range, used to attribute an edge to the
// innermost enclosing definition.
type span struct {
	id    string
	start uint
	end   uint
}

// enclosing returns the id of the innermost span containing byte b, or "".
func enclosing(spans []span, b uint) string {
	best := ""
	var bestLen uint
	for _, s := range spans {
		if b >= s.start && b < s.end {
			if l := s.end - s.start; best == "" || l < bestLen {
				best, bestLen = s.id, l
			}
		}
	}
	return best
}

// containingParent returns the id of the innermost span (other than child
// itself) whose byte range fully contains child's range, or "". It gives child
// its single direct structural parent: a method's parent is its class, the
// class's parent its namespace, and so on — building a containment tree.
func containingParent(spans []span, child span) string {
	best := ""
	var bestLen uint
	for _, s := range spans {
		if s.id == child.id {
			continue
		}
		if s.start <= child.start && child.end <= s.end {
			if l := s.end - s.start; best == "" || l < bestLen {
				best, bestLen = s.id, l
			}
		}
	}
	return best
}

// nullStr maps "" -> SQL NULL.
func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}
