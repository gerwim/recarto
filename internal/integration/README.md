# Language-fixture integration suite

Correctness of the extraction queries (`internal/parse/queries/*.scm`) rests on
three independent, machine-checked layers — provenance alone is none of them:

1. **Compiler-validated fixtures, LSP-semantics expectations** (this suite):
   the C# fixture builds with `dotnet build` and the Rust one with
   `cargo build`, and every `def`/`ref` marker encodes what a language server
   reports; results are asserted exactly, in both directions.
2. **Upstream cross-check** (`internal/parse/coverage_test.go`): every
   definition/reference construct in the grammar's own `tags.scm` (the query
   GitHub code navigation runs, version-pinned to the exact grammar we link,
   vendored under `internal/parse/testdata/upstream/`) must be covered by our
   query or allowlisted with a written justification. Note the comparison runs
   this direction because our queries are a superset: upstream's C# tags.scm
   has no records, structs, enums, delegates, properties, constructors,
   file-scoped namespaces, attributes, receivers, or type bindings.
3. **Grammar-version pinning**: queries compile at startup against the linked
   grammar (a malformed or drifted query fails `NewRegistry`), and the
   vendored tags.scm comes from the same module version — a community query
   file from another version may not even compile.
4. **LSP cross-check** (`lspcheck_test.go`): the fixture's language server —
   the same authority an IDE's find-usages uses (csharp-ls is Roslyn-based,
   like Rider) — is asked `textDocument/references` for every `def` marker,
   and the answers must agree with the markers line-by-line in BOTH
   directions: the LSP must never find a usage the markers miss, and every
   `def`/`ref`/`implicit` line must be LSP-confirmed. Since the main suite
   proves recarto == markers, this closes the loop: recarto == markers == LSP.
   Runs when the manifest's `lsp_command` is on PATH (skips otherwise, so
   plain `go test ./...` stays hermetic); known server gaps live in the
   manifest's `lsp_known_misses` with a reason each, and an entry the server
   starts reporting fails the test as stale.

`go test ./internal/integration` indexes a real, compilable project per
language with the actual CLI (`recarto index`) and verifies every graph query
surface against expectations embedded in the fixture sources:

| subtest              | verifies                                                                  |
| -------------------- | ------------------------------------------------------------------------- |
| `fixture-hygiene`    | the fixture's own marker accounting (failures here = testdata bug)        |
| `census`             | indexed file/symbol totals — catches missing AND phantom definitions      |
| `outline`            | `graph outline` per file: exact (kind, line) multiset                     |
| `query`              | `graph query --path=` per file: exact (kind, qualified name, line) multiset |
| `grep`               | `graph grep -r '\bName\b'` equals an independent line scan EXACTLY (both directions), plus enclosing-symbol attribution on declaration lines |
| `search`             | `graph search --symbol=` surfaces every definition of that name           |
| `snippet`            | `graph snippet <qn>` returns the declaration (overload sets and partial/multi-file ambiguity included) |
| `impls`              | `graph impls` both directions per declared subtype relation               |
| `calls`              | `graph callers`/`callees` contain each marked call hop                    |
| `annotated`          | `graph annotated <Attr>` lists each decorated symbol                      |
| `reindex-idempotent` | re-running `recarto index` changes nothing                                |

The suite is deterministic: a fixed fixture, a fake clock, a temp store
(`RECARTO_CACHE_DIR`), and a non-git temp worktree (exercising the documented
filesystem-walk fallback); all comparisons are set/multiset-based.

## Expectation markers

Expectations live INLINE in the fixture sources (the technique Roslyn's own
find-references tests use), so line numbers can never drift from the code.
After the language's line comment + `#` (C#: `//#`), semicolon-separated:

```
def <kind> <qualified.name>   a symbol the graph must index, declared on this line
ref <Name>                    this line is a usage of <Name> (LSP find-references semantics)
tok <Name>                    textual-only occurrence: grep finds it, an LSP may not count it
implicit <Name>               semantic-only reference: the LSP reports it but no token is on
                              the line (e.g. an implicit base-constructor call), so no textual
                              search can — a documented blind spot of the grep contract
impl <Sub> of <Super>         subtype/implementation relation declared here
call <Caller> -> <Callee>     call edge; both sides accept Leaf or Type.Method
annot <Attr> on <Target>      <Target> is decorated with attribute <Attr> here
```

Authoring rules (the `fixture-hygiene` subtest enforces them mechanically):

- Mark EVERY definition the indexer is expected to produce — `census`,
  `outline` and `query` assert exact equality, so an unmarked symbol fails as a
  phantom and a marked-but-missing one fails as a miss.
- In the marker-stripped source text, every whole-word occurrence of a tracked
  simple name must be its `def` line or carry a `ref` marker. Concretely: never
  mention a tracked identifier in a string literal or an ordinary comment.
  This makes textual matches coincide with semantic usages, which is what lets
  `grep` be asserted exactly in both directions.
- Marker text itself may freely name other symbols (e.g. `call Main -> Swap` on
  a line where `Main` doesn't appear): the grep oracle reads the same full
  bytes the engine does, and hygiene checks the stripped text.
- Dotted names (namespaces) are exempt from hygiene: `\bA\.B\b` also matches
  `A.B.C` — textual-search semantics, not a fixture defect.
- Keep the fixture COMPILABLE (`dotnet build` for C#, `cargo build` for Rust,
  in a scratch copy of `src/`): ground truth for `ref`
  markers is what the language's LSP would report, and that is only
  well-defined for valid code.

## Adding a language

1. Create `testdata/<lang>/manifest.json`:

   ```json
   {
     "language": "<recarto language id>",
     "comment": "//",
     "source_exts": [".ext"],
     "exclude_dirs": ["build-artifact-dirs"]
   }
   ```

2. Put a small but construct-complete project under `testdata/<lang>/src/`,
   annotated with the markers above. Cover everything the language has:
   every declaration kind the indexer supports, plus the call/usage shapes
   that have historically been missed (generic call sites, qualified base
   types, attribute/decorator forms, partial/nested/overloaded symbols).

3. `go test ./internal/integration` — no Go changes needed. Fix hygiene
   failures first (testdata bugs); remaining failures are engine findings.
