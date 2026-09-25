// Command embedgen converts the pinned potion-base-8M release to Recarto's
// compact, deterministic int8 asset format.
package main

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
)

const (
	modelSHA   = "f65d0f325faadc1e121c319e2faa41170d3fa07d8c89abd48ca5358d9a223de2"
	tokenSHA   = "e67e803f624fb4d67dea1c730d06e1067e1b14d830e2c2202569e3ef0f70bb50"
	rows       = 29528
	dim        = 256
	assetMagic = "RCEMB1\x00\x00"
)

type tensorInfo struct {
	DType       string `json:"dtype"`
	Shape       []int  `json:"shape"`
	DataOffsets []int  `json:"data_offsets"`
}

func main() {
	safetensors := flag.String("safetensors", "", "pinned model.safetensors path")
	tokenizer := flag.String("tokenizer", "", "pinned tokenizer.json path")
	out := flag.String("out", "internal/embedding/assets", "output directory")
	flag.Parse()
	if *safetensors == "" || *tokenizer == "" {
		fatalf("-safetensors and -tokenizer are required")
	}
	if err := generate(*safetensors, *tokenizer, *out); err != nil {
		fatalf("%v", err)
	}
}

func generate(modelPath, tokenizerPath, outDir string) error {
	model, err := os.ReadFile(modelPath)
	if err != nil {
		return err
	}
	if got := digest(model); got != modelSHA {
		return fmt.Errorf("model sha256 = %s, want %s", got, modelSHA)
	}
	tokenizer, err := os.ReadFile(tokenizerPath)
	if err != nil {
		return err
	}
	if got := digest(tokenizer); got != tokenSHA {
		return fmt.Errorf("tokenizer sha256 = %s, want %s", got, tokenSHA)
	}
	if len(model) < 8 {
		return fmt.Errorf("safetensors file is shorter than its header length")
	}
	headerLen := binary.LittleEndian.Uint64(model[:8])
	if headerLen > uint64(len(model)-8) {
		return fmt.Errorf("safetensors header length %d exceeds file", headerLen)
	}
	var header map[string]tensorInfo
	if err := json.Unmarshal(model[8:8+headerLen], &header); err != nil {
		return fmt.Errorf("decode safetensors header: %w", err)
	}
	tensor, ok := header["embeddings"]
	if !ok || tensor.DType != "F32" || len(tensor.Shape) != 2 || tensor.Shape[0] != rows || tensor.Shape[1] != dim || len(tensor.DataOffsets) != 2 {
		return fmt.Errorf("unexpected embeddings tensor metadata: %+v", tensor)
	}
	data := model[8+headerLen:]
	start, end := tensor.DataOffsets[0], tensor.DataOffsets[1]
	if start < 0 || end < start || end > len(data) || end-start != rows*dim*4 {
		return fmt.Errorf("invalid embeddings data offsets %v", tensor.DataOffsets)
	}
	data = data[start:end]

	var tok struct {
		Model struct {
			Vocab map[string]int `json:"vocab"`
		} `json:"model"`
	}
	if err := json.Unmarshal(tokenizer, &tok); err != nil {
		return fmt.Errorf("decode tokenizer: %w", err)
	}
	if len(tok.Model.Vocab) != rows {
		return fmt.Errorf("vocabulary has %d tokens, want %d", len(tok.Model.Vocab), rows)
	}
	vocab := make([]string, rows)
	for token, id := range tok.Model.Vocab {
		if id < 0 || id >= rows || vocab[id] != "" {
			return fmt.Errorf("vocabulary id %d is out of range or duplicated", id)
		}
		vocab[id] = token
	}
	for id, token := range vocab {
		if token == "" {
			return fmt.Errorf("vocabulary is missing id %d", id)
		}
	}

	asset := make([]byte, 16+rows*4+rows*dim)
	copy(asset[:8], assetMagic)
	binary.LittleEndian.PutUint32(asset[8:12], rows)
	binary.LittleEndian.PutUint32(asset[12:16], dim)
	quantized := asset[16+rows*4:]
	for row := 0; row < rows; row++ {
		maxAbs := float32(0)
		for col := 0; col < dim; col++ {
			x := math.Float32frombits(binary.LittleEndian.Uint32(data[(row*dim+col)*4:]))
			if abs := float32(math.Abs(float64(x))); abs > maxAbs {
				maxAbs = abs
			}
		}
		scale := float32(0)
		if maxAbs != 0 {
			scale = maxAbs / 127
		}
		binary.LittleEndian.PutUint32(asset[16+row*4:], math.Float32bits(scale))
		if scale == 0 {
			continue
		}
		for col := 0; col < dim; col++ {
			x := math.Float32frombits(binary.LittleEndian.Uint32(data[(row*dim+col)*4:]))
			q := math.Round(float64(x) / float64(scale))
			q = math.Max(-127, math.Min(127, q))
			quantized[row*dim+col] = byte(int8(q))
		}
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(outDir, "potion-base-8M.i8"), asset, 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(outDir, "potion-base-8M.vocab"), []byte(strings.Join(vocab, "\n")+"\n"), 0o644); err != nil {
		return err
	}
	fmt.Printf("wrote %d rows × %d dimensions; int8 sha256 %s\n", rows, dim, digest(asset))
	return nil
}

func digest(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
