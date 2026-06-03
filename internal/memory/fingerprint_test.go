package memory

import (
	"math/bits"
	"testing"
)

func TestJaccard(t *testing.T) {
	a := map[string]struct{}{"x": {}, "y": {}, "z": {}}
	b := map[string]struct{}{"x": {}, "y": {}, "z": {}}
	if got := jaccard(a, b); got != 1 {
		t.Errorf("identical jaccard = %v, want 1", got)
	}
	c := map[string]struct{}{"p": {}, "q": {}}
	if got := jaccard(a, c); got != 0 {
		t.Errorf("disjoint jaccard = %v, want 0", got)
	}
}

func TestMinHashNearDupShareBand(t *testing.T) {
	near1 := shingleSet("the auth token refresh path leaks a goroutine on error")
	near2 := shingleSet("the auth token refresh path leaks a goroutine on failure")
	far := shingleSet("rendering the markdown export to stdout for the cli status")

	b1 := bandHashes(minHashSignature(near1))
	b2 := bandHashes(minHashSignature(near2))
	bf := bandHashes(minHashSignature(far))

	shares := func(x, y [lshBands]uint64) bool {
		for i := 0; i < lshBands; i++ {
			if x[i] == y[i] {
				return true
			}
		}
		return false
	}
	if !shares(b1, b2) {
		t.Error("near-duplicate texts should share at least one LSH band")
	}
	if shares(b1, bf) {
		t.Error("unrelated texts should not share an LSH band (probabilistic)")
	}
}

func TestSimHashCloserForNearDup(t *testing.T) {
	base := shingleSet("the auth token refresh path leaks a goroutine")
	near := shingleSet("the auth token refresh path leaks a goroutine on shutdown")
	far := shingleSet("rendering markdown export to stdout for the cli status report")

	hamming := func(a, b uint64) int { return bits.OnesCount64(a ^ b) }
	dNear := hamming(simHash64(base), simHash64(near))
	dFar := hamming(simHash64(base), simHash64(far))
	if dNear >= dFar {
		t.Errorf("near-duplicate SimHash should be closer than unrelated: near=%d far=%d",
			dNear, dFar)
	}
}
