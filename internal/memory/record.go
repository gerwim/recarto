// Package memory is Recarto's write+dedup and recall engine over the store.
package memory

// Kind enumerates the unified memory kinds. The memories.kind CHECK is a
// permissive superset: it still lists the retired `action` kind so existing
// stores stay valid, but `action` is no longer a Kind and Valid() rejects it.
type Kind string

const (
	KindPreference  Kind = "preference"
	KindLearning    Kind = "learning"
	KindDecision    Kind = "decision"
	KindDoNotRepeat Kind = "do_not_repeat"
	KindBug         Kind = "bug"
	KindTodo        Kind = "todo"
)

// AllKinds is the canonical, ordered list of memory kinds. It is the single
// source of truth: validKinds is derived from it, and the MCP kind enum lists it.
var AllKinds = []Kind{
	KindPreference, KindLearning, KindDecision,
	KindDoNotRepeat, KindBug, KindTodo,
}

var validKinds = func() map[Kind]bool {
	m := make(map[Kind]bool, len(AllKinds))
	for _, k := range AllKinds {
		m[k] = true
	}
	return m
}()

// Valid reports whether k is a known memory kind.
func (k Kind) Valid() bool { return validKinds[k] }

// RefType enumerates structural reference types (mirrors memory_refs.ref_type).
type RefType string

const (
	RefFile   RefType = "file"
	RefSymbol RefType = "symbol"
	RefTag    RefType = "tag"
)

// Ref is a structural reference attached to a memory.
type Ref struct {
	Type  RefType
	Value string
}

// NewMemory is the input to Engine.Record.
type NewMemory struct {
	Kind         Kind
	Title        string
	Body         string
	Refs         []Ref
	Branch       string // "" → repo-global (stored as NULL)
	Worktree     string
	Author       string
	Source       string // "mcp" | "hook" | "cli"
	Pinned       bool
	SupersedesID string         // optional: a prior memory this one supersedes
	Extra        map[string]any // kind-specific fields (bug: error_message, ...)
}

// Record is a stored memory row plus its refs.
type Record struct {
	ID                 string
	Kind               Kind
	Title              string
	Body               string
	SearchText         string
	CreatedAt          int64  // unix ms
	UpdatedAt          int64  // unix ms
	Branch             string // "" if NULL
	Worktree           string
	Author             string
	Source             string
	Confidence         float64
	ReinforcementCount int
	Pinned             bool
	Status             string
	SupersededBy       string
	SimHash            uint64
	Extra              map[string]any
	Refs               []Ref
}
