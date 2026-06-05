package memory

import (
	"context"
	"database/sql"
	"errors"

	"git.gerwim.nl/gerwim/recarto/internal/store"
)

// ImportMemory is a portable memory record from a team snapshot.
type ImportMemory struct {
	ID                 string
	Kind               Kind
	Title              string
	Body               string
	Refs               []Ref
	Branch             string
	Author             string
	Source             string
	Confidence         float64
	ReinforcementCount int
	Pinned             bool
	CreatedAt          int64
	Extra              map[string]any
}

// ImportOutcome is the result of merging one snapshot record.
type ImportOutcome int

const (
	ImportSkipped    ImportOutcome = iota // id already present locally (idempotent re-import)
	ImportInserted                        // new memory, snapshot metadata preserved
	ImportReinforced                      // near-duplicate of an existing memory → reinforced
)

// ImportMemory merges one snapshot record into the store, reusing the same
// MinHash near-dup path as Record. Identity is the snapshot id (idempotent
// re-import); a different id whose body is a near-duplicate reinforces the
// existing memory so independently-authored copies converge.
func (e *Engine) ImportMemory(ctx context.Context, im ImportMemory) (ImportOutcome, string, error) {
	if !im.Kind.Valid() {
		return ImportSkipped, "", errInvalidKind
	}
	nm := NewMemory{
		Kind: im.Kind, Title: im.Title, Body: im.Body, Refs: im.Refs,
		Branch: im.Branch, Author: im.Author, Source: im.Source,
		Pinned: im.Pinned, Extra: im.Extra,
	}
	searchText := BuildSearchText(nm.Title, nm.Body, nm.Refs)
	set := shingleSet(nm.Body)
	bands := bandHashes(minHashSignature(set))
	sim := simHash64(set)
	now := e.clk.Now().UnixMilli()

	outcome := ImportSkipped
	retID := im.ID
	err := e.st.WriteTx(ctx, func(c store.Conn) error {
		var exists int
		if err := c.QueryRowContext(ctx,
			`SELECT 1 FROM memories WHERE id=? LIMIT 1`, im.ID).Scan(&exists); err == nil {
			outcome = ImportSkipped
			return nil
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}

		candID, found, ferr := e.findNearDup(ctx, c, im.Kind, bands, set)
		if ferr != nil {
			return ferr
		}
		if found {
			outcome, retID = ImportReinforced, candID
			return e.reinforce(ctx, c, candID, nm, now)
		}

		if err := e.insert(ctx, c, im.ID, nm, searchText, sim, bands, im.CreatedAt); err != nil {
			return err
		}
		if _, err := c.ExecContext(ctx,
			`UPDATE memories SET confidence=?, reinforcement_count=? WHERE id=?`,
			clampConfidence(im.Confidence), im.ReinforcementCount, im.ID); err != nil {
			return err
		}
		outcome = ImportInserted
		return nil
	})
	if err != nil {
		return ImportSkipped, "", err
	}
	if outcome != ImportSkipped {
		_ = e.led.Incr(ctx, "lifetime", "memories_recorded", 1)
	}
	return outcome, retID, nil
}

func clampConfidence(c float64) float64 {
	if c <= 0 {
		return 1.0 // missing/zero (or corrupt negative) → default full confidence
	}
	if c > 1.0 {
		return 1.0
	}
	return c
}
