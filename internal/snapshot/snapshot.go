// Package snapshot serializes active memories to a committable JSONL file
// (.recarto/memory.jsonl) and imports them back through the memory engine's
// dedup-aware ImportMemory. Local activity signals (last_used_epoch, recall_count,
// archived_epoch) and derived indexes (FTS/MinHash) are intentionally excluded:
// the former leak usage and churn, the latter are recomputed on import.
package snapshot

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"sort"

	"git.gerwim.nl/gerwim/recarto/internal/memory"
)

// line is the portable wire shape of one memory.
type line struct {
	ID                 string         `json:"id"`
	Kind               string         `json:"kind"`
	Title              string         `json:"title"`
	Body               string         `json:"body"`
	Refs               []refLine      `json:"refs,omitempty"`
	Branch             string         `json:"branch,omitempty"`
	Author             string         `json:"author,omitempty"`
	Source             string         `json:"source,omitempty"`
	Confidence         float64        `json:"confidence"`
	ReinforcementCount int            `json:"reinforcement_count"`
	Pinned             bool           `json:"pinned,omitempty"`
	CreatedAt          int64          `json:"created_at"`
	Extra              map[string]any `json:"extra,omitempty"`
	// Worktree, last_used_epoch, recall_count, and archived_epoch are
	// intentionally absent: they are local runtime/activity context, not
	// portable knowledge. FTS/MinHash rows are recomputed on import.
}

type refLine struct {
	Type  string `json:"type"`
	Value string `json:"value"`
}

// Export writes all active memories (pinned included) as id-sorted JSONL and
// returns the count. Deterministic ordering keeps git diffs minimal.
func Export(ctx context.Context, eng *memory.Engine, w io.Writer) (int, error) {
	recs, err := eng.List(ctx, memory.ListFilter{Status: "active"})
	if err != nil {
		return 0, err
	}
	sort.Slice(recs, func(i, j int) bool { return recs[i].ID < recs[j].ID })
	bw := bufio.NewWriter(w)
	enc := json.NewEncoder(bw)
	for _, r := range recs {
		l := line{
			ID: r.ID, Kind: string(r.Kind), Title: r.Title, Body: r.Body,
			Branch: r.Branch, Author: r.Author, Source: r.Source,
			Confidence: r.Confidence, ReinforcementCount: r.ReinforcementCount,
			Pinned: r.Pinned, CreatedAt: r.CreatedAt, Extra: r.Extra,
		}
		for _, ref := range r.Refs {
			l.Refs = append(l.Refs, refLine{Type: string(ref.Type), Value: ref.Value})
		}
		if err := enc.Encode(&l); err != nil {
			return 0, err
		}
	}
	if err := bw.Flush(); err != nil {
		return 0, err
	}
	return len(recs), nil
}

// Import reads JSONL and merges each record via ImportMemory, returning the
// per-outcome tallies.
func Import(ctx context.Context, eng *memory.Engine, r io.Reader) (inserted, reinforced, skipped int, err error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024) // allow large memory bodies
	for sc.Scan() {
		raw := sc.Bytes()
		if len(raw) == 0 {
			continue
		}
		var l line
		if uerr := json.Unmarshal(raw, &l); uerr != nil {
			return inserted, reinforced, skipped, uerr
		}
		im := memory.ImportMemory{
			ID: l.ID, Kind: memory.Kind(l.Kind), Title: l.Title, Body: l.Body,
			Branch: l.Branch, Author: l.Author, Source: l.Source,
			Confidence: l.Confidence, ReinforcementCount: l.ReinforcementCount,
			Pinned: l.Pinned, CreatedAt: l.CreatedAt, Extra: l.Extra,
		}
		for _, ref := range l.Refs {
			im.Refs = append(im.Refs, memory.Ref{Type: memory.RefType(ref.Type), Value: ref.Value})
		}
		out, _, ierr := eng.ImportMemory(ctx, im)
		if ierr != nil {
			return inserted, reinforced, skipped, ierr
		}
		switch out {
		case memory.ImportInserted:
			inserted++
		case memory.ImportReinforced:
			reinforced++
		case memory.ImportSkipped:
			skipped++
		}
	}
	return inserted, reinforced, skipped, sc.Err()
}
