//go:build ORT

// Package graph's optional semantic tier is implemented here against hugot's
// ONNX Runtime backend. It is compiled only with `-tags ORT` so the default
// recarto build stays lexical-only and free of the ONNX/transformer dependency
// tree. Building with ORT additionally requires the native libonnxruntime shared
// library and libtokenizers.a (see the README's semantic-search section).
package graph

import (
	"context"
	"fmt"
	"math"
	"path/filepath"

	"github.com/knights-analytics/hugot"
	"github.com/knights-analytics/hugot/options"
	"github.com/knights-analytics/hugot/pipelines"
)

// HugotEmbedder is an Embedder backed by a local ONNX sentence-embedding model
// run on CPU via ONNX Runtime. Vectors are L2-normalized so the vec0 index's
// default L2 distance ranks identically to cosine similarity. Documents and
// queries are prefixed with model-specific prompt strings (see HugotConfig).
type HugotEmbedder struct {
	session     *hugot.Session
	pipe        *pipelines.FeatureExtractionPipeline
	dim         int
	docPrefix   string
	queryPrefix string
}

// HugotConfig configures a HugotEmbedder. DocPrefix/QueryPrefix are the prompt
// strings a retrieval-tuned model needs; for EmbeddingGemma:
//
//	DocPrefix:   "title: none | text: "
//	QueryPrefix: "task: search result | query: "
//
// (bge / all-MiniLM use empty prefixes.) ExternalData names the sibling weights
// file for external-data ONNX exports (EmbeddingGemma ships onnx/model.onnx_data).
type HugotConfig struct {
	ModelID      string // HuggingFace id, e.g. "onnx-community/embeddinggemma-300m-ONNX"
	CacheDir     string // where models are downloaded/cached
	OnnxFile     string // which .onnx within the repo, e.g. "onnx/model.onnx"
	ExternalData string // sibling external-data file, e.g. "onnx/model.onnx_data" (empty if none)
	LibDir       string // directory containing libonnxruntime.so (empty => default search)
	DocPrefix    string // prompt prepended to documents
	QueryPrefix  string // prompt prepended to queries
}

// NewHugotEmbedder downloads (if needed) and loads the model. The caller owns Close.
func NewHugotEmbedder(ctx context.Context, cfg HugotConfig) (*HugotEmbedder, error) {
	var opts []options.WithOption
	if cfg.LibDir != "" {
		opts = append(opts, options.WithOnnxLibraryPath(cfg.LibDir))
	}
	session, err := hugot.NewORTSession(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("ort session: %w", err)
	}
	dl := hugot.NewDownloadOptions()
	dl.OnnxFilePath = cfg.OnnxFile
	dl.ExternalDataPath = cfg.ExternalData
	modelPath, err := hugot.DownloadModel(ctx, cfg.ModelID, cfg.CacheDir, dl)
	if err != nil {
		_ = session.Destroy()
		return nil, fmt.Errorf("download model %s: %w", cfg.ModelID, err)
	}
	pipe, err := hugot.NewPipeline(session, hugot.FeatureExtractionConfig{
		ModelPath: modelPath,
		// hugot flattens a downloaded "onnx/model_x.onnx" to "model_x.onnx" in the
		// model dir, so load it by basename (the repo subpath is only for download).
		OnnxFilename: filepath.Base(cfg.OnnxFile),
		Name:         "recarto-embeddings",
	})
	if err != nil {
		_ = session.Destroy()
		return nil, fmt.Errorf("feature-extraction pipeline: %w", err)
	}
	h := &HugotEmbedder{session: session, pipe: pipe, docPrefix: cfg.DocPrefix, queryPrefix: cfg.QueryPrefix}
	// Probe the dimension from a trivial embed.
	probe, err := h.run([]string{cfg.QueryPrefix + "x"})
	if err != nil || len(probe) == 0 {
		_ = session.Destroy()
		return nil, fmt.Errorf("probe embedding: %w", err)
	}
	h.dim = len(probe[0])
	return h, nil
}

func (h *HugotEmbedder) Dim() int { return h.dim }

// EmbedDocs prefixes each text with the document prompt and embeds the batch.
func (h *HugotEmbedder) EmbedDocs(_ context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	in := make([]string, len(texts))
	for i, t := range texts {
		in[i] = h.docPrefix + t
	}
	return h.run(in)
}

// EmbedQuery prefixes the text with the query prompt and embeds it.
func (h *HugotEmbedder) EmbedQuery(_ context.Context, text string) ([]float32, error) {
	out, err := h.run([]string{h.queryPrefix + text})
	if err != nil || len(out) == 0 {
		return nil, err
	}
	return out[0], nil
}

// run executes the pipeline and L2-normalizes each output vector.
func (h *HugotEmbedder) run(texts []string) ([][]float32, error) {
	res, err := h.pipe.RunPipeline(context.Background(), texts)
	if err != nil {
		return nil, err
	}
	for i := range res.Embeddings {
		normalize(res.Embeddings[i])
	}
	return res.Embeddings, nil
}

// Close releases the ONNX session.
func (h *HugotEmbedder) Close() error { return h.session.Destroy() }

func normalize(v []float32) {
	var sum float64
	for _, f := range v {
		sum += float64(f) * float64(f)
	}
	if sum == 0 {
		return
	}
	inv := float32(1 / math.Sqrt(sum))
	for i := range v {
		v[i] *= inv
	}
}
