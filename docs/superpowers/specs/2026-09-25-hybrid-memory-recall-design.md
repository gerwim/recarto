# Hybrid memory recall: BM25, static embeddings and relevance floor

## Decision

Recall combines the existing FTS5 BM25 rank with a static Model2Vec `potion-base-8M`
embedding rank using reciprocal-rank fusion (RRF, `k=60`), structural reference
overlap, and small recency/reinforcement/branch priors. A calibrated relevance floor
filters unrelated text candidates before pins or baseline seeds are added. Structural
matches always bypass the floor. Session-start and augment-hook paths do not load or
run the model.

The selected model is the int8-quantized potion-base-8M revision
`bf8b056651a2c21b8d2565580b8569da283cab23`, embedded in the binary and implemented in
pure Go. It adds about 8 MB, parses lazily in about 3 ms, embeds a query in about 6 µs,
an average 1.5 KB memory in about 0.17 ms, and scans 1,000 vectors in about 5 ms.
Vectors are derived, not synced or exported; a fresh store lazily re-embeds its active
memories. `Embedder.ID()` and the persisted model id allow a second vector space to be
added later without changing the storage contract.

## Measured retrieval quality

Measurements use 73 sanitized active memories from this repository, 90 labeled related
queries (50 keyword-style and 40 paraphrased), five long task descriptions, and 47
unrelated tasks, on a 2-CPU VM.

| system | keyword R@5 / Hit@5 / MRR | paraphrase R@5 / Hit@5 / MRR | long R@5 / Hit@5 | unrelated tasks returning nothing |
|---|---|---|---|---|
| master today | 0.815 / 0.880 / 0.810 | 0.642 / 0.800 / 0.634 | 0.950 / 1.00 | 0 / 47 |
| hybrid + balanced floor | **0.900 / 0.940 / 0.895** | **0.742 / 0.850 / 0.698** | 0.950 / 1.00 | **40 / 47** |
| hybrid, floor off | 0.900 / 0.940 / 0.898 | 0.785 / 0.925 / 0.732 | 0.950 / 1.00 | 0 / 47 |
| lexical only + floor | 0.800 / 0.860 / 0.792 | 0.604 / 0.750 / 0.581 | 0.750 / 0.80 | 40 / 47 |

The balanced thresholds are a deliberate recall/precision trade-off. Potion cosine
scores overlap: unrelated tasks can reach 0.44–0.61 while relevant memories may be
0.28–0.45. The floor therefore combines cosine with IDF-weighted lexical coverage.
The gate opens when any text candidate has cosine ≥ 0.55 or coverage ≥ 0.25; once open,
an item is kept at cosine ≥ 0.35 or coverage ≥ 0.15. Coverage uses FTS5 document
frequencies and `idf(t) = ln(1 + (N − df + 0.5)/(df + 0.5))`, so query terms absent
from the memory corpus contribute more mass than common terms.

Stricter floors were rejected: blocking 95% of unrelated tasks drops paraphrase R@5
to about 0.39. The owner selected the balanced point (83% of unrelated tasks blocked)
and accepted its vague-paraphrase misses. `RECARTO_RECALL_FLOOR=0` disables the floor;
structural refs continue to bypass it. `RECARTO_SEMANTIC=0` disables embeddings and
keeps lexical plus structural recall.

## Why static instead of a transformer

At the balanced floor, the measured paraphrase R@5 was 0.75 for static potion-base-8M,
0.79 for MiniLM-L6 ONNX, 0.79 for bge-base ONNX, and 0.83 for int8 EmbeddingGemma.
Keyword-style R@5 was 0.90, 0.88, 0.92 and 0.91 respectively. On two CPUs, indexing
1,000 memories took about 0.2 seconds for static, 2 minutes for MiniLM, 7 minutes for
bge-base and 12 minutes for EmbeddingGemma. Since vectors are derived and not synced,
each fresh client store would pay the full indexing cost. ONNX also needs platform
native libraries and a 90–310 MB model download. The modest retrieval gain does not
justify that recurring cost and deployment complexity yet.

The sync server could compute document vectors once, but every client must embed each
query in the same vector space. That would still require a client model or a network
round trip on every recall plus an offline fallback. Server-side embedding and vector
sync are therefore deferred.

## Storage and scoring notes

Migration v22 adds `memory_vectors(memory_id, model, source_at, vec)` and an
`fts5vocab` view over the memory FTS index. Vectors are lazily backfilled and updated
when `memories.updated_at` or the model id changes; empty vectors are persisted to
avoid retrying non-embeddable text. The table is derived state and is not synced,
exported or salvaged by repair.

Text arms fuse by reciprocal rank, not normalized BM25 score. When semantic ranking
runs, lexical and semantic each receive half of the 0.45 relevance weight; otherwise
lexical gets the full weight. Structural weight is 0.55. Recency, reinforcement and
branch priors retain their 3:2:1 ratio at one hundredth their former weight, avoiding
priors swamping relevance. Consequently the final score scale and `min_score` values
change. A text query rejected by the floor returns no memories, including pins;
structural evidence makes the recall related and restores the existing pin and
baseline behavior. Equal scores sort by memory id for deterministic output.

## Future model seam

A future model can implement the existing `Embedder` interface and write vectors under
a new model id. Model/tokenizer/pooling/quantization changes require a new id and
recalibrated floor thresholds. A server-side model additionally requires query
embedding by the same model; it must not make offline recall depend on the sync server.
