// Package embedding embeds the pinned, quantized potion-base-8M model.
package embedding

import (
	_ "embed"
	"encoding/binary"
	"math"
	"sync"
)

const (
	// ModelID identifies model + revision + quantization + tokenizer recipe. Bump
	// the suffix whenever tokenization, pooling or quantization changes.
	ModelID   = "potion-base-8M@bf8b056/int8/v1"
	modelRows = 29528
	modelDim  = 256
)

//go:embed assets/potion-base-8M.i8
var matrixAsset []byte

//go:embed assets/potion-base-8M.vocab
var vocabAsset []byte

// Model is a static Model2Vec embedder. The embedded matrix is read in place;
// parsed maps and scales are initialized once and then immutable.
type Model struct {
	once         sync.Once
	failed       bool
	initial      map[string]int32
	continuation map[string]int32
	scales       []float32
	data         []byte
}

var defaultModel = &Model{}

// Default returns the process-wide model over the binary's embedded assets.
func Default() *Model { return defaultModel }

func (m *Model) ID() string { return ModelID }
func (m *Model) Dim() int   { return modelDim }

func (m *Model) load() {
	m.once.Do(func() {
		if len(matrixAsset) < 16 || string(matrixAsset[:8]) != "RCEMB1\x00\x00" ||
			binary.LittleEndian.Uint32(matrixAsset[8:12]) != modelRows ||
			binary.LittleEndian.Uint32(matrixAsset[12:16]) != modelDim {
			m.failed = true
			return
		}
		want := 16 + modelRows*4 + modelRows*modelDim
		if len(matrixAsset) != want {
			m.failed = true
			return
		}
		m.scales = make([]float32, modelRows)
		for i := range m.scales {
			m.scales[i] = math.Float32frombits(binary.LittleEndian.Uint32(matrixAsset[16+i*4:]))
		}
		m.data = matrixAsset[16+modelRows*4:]
		m.initial = make(map[string]int32, modelRows)
		m.continuation = make(map[string]int32, modelRows/2)
		start, id := 0, int32(0)
		for i, b := range vocabAsset {
			if b != '\n' {
				continue
			}
			end := i
			if end > start && vocabAsset[end-1] == '\r' {
				end--
			}
			if end == start || id >= modelRows {
				m.failed = true
				return
			}
			token := vocabAsset[start:end]
			if len(token) > 2 && token[0] == '#' && token[1] == '#' {
				m.continuation[string(token[2:])] = id
			} else {
				m.initial[string(token)] = id
			}
			start = i + 1
			id++
		}
		if start != len(vocabAsset) || id != modelRows {
			m.failed = true
		}
	})
}

// Embed returns an L2-normalized embedding, or nil when no tokens are known or
// an embedded asset is invalid. At most 2048 WordPiece tokens are pooled.
func (m *Model) Embed(text string) []float32 {
	if m == nil {
		return nil
	}
	m.load()
	if m.failed {
		return nil
	}
	ids := m.tokenIDs(text)
	if len(ids) == 0 {
		return nil
	}
	if len(ids) > 2048 {
		ids = ids[:2048]
	}
	vec := make([]float32, modelDim)
	for _, id := range ids {
		row := int(id) * modelDim
		scale := m.scales[id]
		for col := range vec {
			vec[col] += scale * float32(int8(m.data[row+col]))
		}
	}
	var norm2 float64
	for _, x := range vec {
		norm2 += float64(x) * float64(x)
	}
	if norm2 == 0 {
		return nil
	}
	inv := float32(1 / math.Sqrt(norm2))
	for i := range vec {
		vec[i] *= inv
	}
	return vec
}

func tokenIDs(text string) []int32 { return Default().tokenIDs(text) }
