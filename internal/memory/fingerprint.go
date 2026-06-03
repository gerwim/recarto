package memory

import (
	"hash/fnv"
	"math"
	"strings"
)

const (
	shingleSize = 3                    // words per shingle
	numHashes   = 128                  // MinHash signature length
	lshBands    = 16                   // LSH band count
	lshRows     = numHashes / lshBands // rows per band (8)
)

// shingleSet returns the set of word-level k-shingles of text. Texts with fewer
// than shingleSize distinct tokens (tokenize deduplicates) fall back to the set
// of individual words.
func shingleSet(text string) map[string]struct{} {
	words := tokenize(text)
	set := map[string]struct{}{}
	if len(words) < shingleSize {
		for _, w := range words {
			set[w] = struct{}{}
		}
		return set
	}
	for i := 0; i+shingleSize <= len(words); i++ {
		set[strings.Join(words[i:i+shingleSize], " ")] = struct{}{}
	}
	return set
}

// jaccard returns |a∩b| / |a∪b|. Two empty sets are defined as identical (1).
func jaccard(a, b map[string]struct{}) float64 {
	if len(a) == 0 && len(b) == 0 {
		return 1
	}
	inter := 0
	for k := range a {
		if _, ok := b[k]; ok {
			inter++
		}
	}
	union := len(a) + len(b) - inter
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}

// hash64 hashes s with a per-permutation seed (FNV-1a over seed bytes + s).
func hash64(s string, seed uint64) uint64 {
	h := fnv.New64a()
	var sb [8]byte
	for i := 0; i < 8; i++ {
		sb[i] = byte(seed >> (8 * i))
	}
	_, _ = h.Write(sb[:])
	_, _ = h.Write([]byte(s))
	return h.Sum64()
}

// minHashSignature computes the numHashes-length MinHash signature of set.
func minHashSignature(set map[string]struct{}) [numHashes]uint64 {
	var sig [numHashes]uint64
	for i := range sig {
		sig[i] = math.MaxUint64
	}
	for sh := range set {
		for i := 0; i < numHashes; i++ {
			if h := hash64(sh, uint64(i)); h < sig[i] {
				sig[i] = h
			}
		}
	}
	return sig
}

// bandHashes condenses a signature into one hash per LSH band; two sets that
// agree on any band are near-duplicate candidates.
func bandHashes(sig [numHashes]uint64) [lshBands]uint64 {
	var bands [lshBands]uint64
	for b := 0; b < lshBands; b++ {
		h := fnv.New64a()
		var buf [8]byte
		for r := 0; r < lshRows; r++ {
			v := sig[b*lshRows+r]
			for i := 0; i < 8; i++ {
				buf[i] = byte(v >> (8 * i))
			}
			_, _ = h.Write(buf[:])
		}
		bands[b] = h.Sum64()
	}
	return bands
}

// simHash64 is a 64-bit SimHash fingerprint over the set's shingles. Stored on
// each memory for future compaction (Phase 3); dedup uses MinHash+Jaccard.
func simHash64(set map[string]struct{}) uint64 {
	var v [64]int
	for sh := range set {
		h := hash64(sh, 0)
		for i := 0; i < 64; i++ {
			if h&(1<<uint(i)) != 0 {
				v[i]++
			} else {
				v[i]--
			}
		}
	}
	var out uint64
	for i := 0; i < 64; i++ {
		if v[i] > 0 {
			out |= 1 << uint(i)
		}
	}
	return out
}
