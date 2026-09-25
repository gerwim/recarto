package embedding

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"math"
	"os"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
)

const (
	wantMatrixSHA = "e3f80724dc781e1289da97bb1ce95e7c3a55f34adc8deb8328a6d2d9c996e7f5"
	wantVocabSHA  = "1394523a67ddd404a825428018c0582a6998bcfa044ecbcbf1f4d71adb94c61c"
)

func TestEmbedConcurrent(t *testing.T) {
	m := Default()
	if m != Default() {
		t.Fatal("Default returned different model pointers")
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if got := m.Embed("concurrent WAL writers report SQLITE_BUSY"); len(got) != modelDim {
				t.Errorf("Embed returned %d dimensions, want %d", len(got), modelDim)
			}
		}()
	}
	wg.Wait()
}

func TestParityWithReference(t *testing.T) {
	var fixture struct {
		Cases []struct {
			Text string    `json:"text"`
			IDs  []int32   `json:"ids"`
			Vec  []float32 `json:"vec"`
		} `json:"cases"`
	}
	b, err := os.ReadFile("testdata/parity.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &fixture); err != nil {
		t.Fatal(err)
	}
	for i, c := range fixture.Cases {
		if got := tokenIDs(c.Text); !slices.Equal(got, c.IDs) {
			t.Errorf("case %d token IDs differ\n got: %v\nwant: %v", i, got, c.IDs)
		}
		got := Default().Embed(c.Text)
		if len(c.Vec) == 0 {
			if got != nil {
				t.Errorf("case %d Embed = %v, want nil", i, got)
			}
			continue
		}
		if cosine(got, c.Vec) < 0.999 {
			t.Errorf("case %d cosine = %.6f, want >= 0.999", i, cosine(got, c.Vec))
		}
	}
}

func TestAssetIntegrity(t *testing.T) {
	if string(matrixAsset[:8]) != "RCEMB1\x00\x00" || binary.LittleEndian.Uint32(matrixAsset[8:12]) != modelRows || binary.LittleEndian.Uint32(matrixAsset[12:16]) != modelDim {
		t.Fatalf("invalid asset header")
	}
	wantLen := 16 + modelRows*4 + modelRows*modelDim
	if len(matrixAsset) != wantLen {
		t.Fatalf("matrix length = %d, want %d", len(matrixAsset), wantLen)
	}
	if got := sha256Hex(matrixAsset); got != wantMatrixSHA {
		t.Fatalf("matrix sha256 = %s, want %s", got, wantMatrixSHA)
	}
	if got := sha256Hex(vocabAsset); got != wantVocabSHA {
		t.Fatalf("vocabulary sha256 = %s, want %s", got, wantVocabSHA)
	}
	count := 0
	for _, b := range vocabAsset {
		if b == '\n' {
			count++
		}
	}
	if count != modelRows {
		t.Fatalf("vocab line count = %d, want %d", count, modelRows)
	}
}

func TestEmbedNormalizedAndDeterministic(t *testing.T) {
	a := Default().Embed("Résumé SQLITE_BUSY concurrent writers")
	b := Default().Embed("Résumé SQLITE_BUSY concurrent writers")
	if !reflect.DeepEqual(a, b) {
		t.Fatal("same input produced different vectors")
	}
	var norm2 float64
	for _, x := range a {
		norm2 += float64(x) * float64(x)
	}
	if math.Abs(math.Sqrt(norm2)-1) > 1e-5 {
		t.Fatalf("vector norm = %.8f, want 1", math.Sqrt(norm2))
	}
}

func TestFold(t *testing.T) {
	if got := Fold("Résumé İ"); got != "resume i" {
		t.Fatalf("Fold = %q, want %q", got, "resume i")
	}
}

func BenchmarkEmbedQuery(b *testing.B) {
	m := Default()
	query := "Why do concurrent SQLite writers report SQLITE_BUSY?"
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m.Embed(query)
	}
}

func BenchmarkEmbedDoc(b *testing.B) {
	m := Default()
	doc := strings.Repeat("This memory covers semantic recall, SQLite writers, and lexical ranking. ", 22)
	b.SetBytes(int64(len(doc)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m.Embed(doc)
	}
}

func BenchmarkFirstUse(b *testing.B) {
	for i := 0; i < b.N; i++ {
		(&Model{}).Embed("concurrent SQLite writers report a database lock")
	}
}

func cosine(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, aa, bb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		aa += float64(a[i]) * float64(a[i])
		bb += float64(b[i]) * float64(b[i])
	}
	return dot / math.Sqrt(aa*bb)
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
