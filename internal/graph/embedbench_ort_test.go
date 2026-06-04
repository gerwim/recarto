//go:build ORT

package graph

import (
	"context"
	"os"
	"testing"
)

// benchEmbedder builds a hugot ONNX embedder for the hybrid search-quality
// benchmark from environment variables (so the model and native-library paths
// are not hard-coded):
//
//	RECARTO_BENCH_EMBED_MODEL  HuggingFace id (e.g. BAAI/bge-small-en-v1.5); unset => lexical-only
//	RECARTO_BENCH_ONNX         .onnx within the repo (default onnx/model.onnx)
//	RECARTO_BENCH_ORT_LIB      dir containing libonnxruntime.so
//	RECARTO_BENCH_CACHE        model download/cache dir (default ./bench-models)
func benchEmbedder(t *testing.T) Embedder {
	model := os.Getenv("RECARTO_BENCH_EMBED_MODEL")
	if model == "" {
		return nil
	}
	onnx := os.Getenv("RECARTO_BENCH_ONNX")
	if onnx == "" {
		onnx = "onnx/model.onnx"
	}
	cache := os.Getenv("RECARTO_BENCH_CACHE")
	if cache == "" {
		cache = "./bench-models"
	}
	emb, err := NewHugotEmbedder(context.Background(), HugotConfig{
		ModelID:      model,
		CacheDir:     cache,
		OnnxFile:     onnx,
		ExternalData: os.Getenv("RECARTO_BENCH_EXTDATA"),
		LibDir:       os.Getenv("RECARTO_BENCH_ORT_LIB"),
		DocPrefix:    os.Getenv("RECARTO_BENCH_DOC_PREFIX"),
		QueryPrefix:  os.Getenv("RECARTO_BENCH_QUERY_PREFIX"),
	})
	if err != nil {
		t.Fatalf("build embedder %s: %v", model, err)
	}
	t.Cleanup(func() { _ = emb.Close() })
	t.Logf("embedder: %s (%s), dim=%d", model, onnx, emb.Dim())
	return emb
}
