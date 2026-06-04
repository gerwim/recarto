# Ordia E2E Code-Graph Validation Campaign — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Validate Recarto's code graph is correct (symbols + edges) and LLM-queryable with precision against the real dual-language Ordia codebase, fix every bug found via TDD, distill findings into committed regression tests, and decide the embeddings question with evidence.

**Architecture:** A layered-oracle validation campaign. Three *independent* oracles establish ground truth — (1) a Roslyn syntactic symbol dump for C#, (2) a TypeScript-compiler-API symbol dump for TS/TSX/JS, (3) the serena LSP for sampled cross-file edge resolution — and `.csproj` `<ProjectReference>` parsing for module deps. Recarto's graph is read directly from its SQLite cache and diffed against the oracles. A cold, recarto-MCP-only subagent grades LLM-queryability with mandatory evidence-grounding (anti-contamination). Every discrepancy becomes a minimal committed Go fixture/test (+ a `schemaVersion` cache-wipe migration when the indexer changes). The embeddings decision is gated on a measured FTS-recall threshold.

**Tech Stack:** Go 1.26.4 (recarto, CGO/tree-sitter), Python 3.13 stdlib (`sqlite3`, comparison engine), .NET 10 + Roslyn (`Microsoft.CodeAnalysis.CSharp`) C# oracle, Node 24 + `typescript` compiler API (TS/JS oracle), serena MCP (LSP edge oracle), recarto MCP/CLI (LLM grading surface).

---

## Conventions used throughout this plan

Run every command from a shell where these are exported (set once in Task 0.1; re-export if your session resets):

```bash
export RECARTO=/home/claude/Projects/recarto
export ORDIA=/home/claude/Projects/ordia
export E2E=/home/claude/recarto-e2e            # scratch root — NOT committed
export RECARTO_CACHE_DIR=$E2E/cache            # isolates Ordia's graph to one DB
export RECARTO_BIN=$E2E/bin/recarto
```

- **Scratch vs committed.** Everything under `$E2E` is throwaway (oracle scripts, dumps, per-phase reports). The ONLY committed artifacts are: Go fixtures/tests/migrations in `$RECARTO`, and the final report `docs/validation/2026-06-04-ordia-e2e-report.md`.
- **Canonical file set.** Both recarto and every oracle must be constrained to the same files: `git ls-files` ∩ {`.cs`,`.ts`,`.tsx`,`.js`} minus generated/vendored paths (computed in Task 0.3). This is the apples-to-apples guard.
- **Bug logging.** Per OpenWolf, every discrepancy that is a real defect → append to `$RECARTO/.wolf/buglog.json` and (if a convention emerges) `$RECARTO/.wolf/cerebrum.md`.

---

## File Structure

**Scratch (under `$E2E`, not committed):**
- `bin/recarto` — the freshly built binary under test
- `fileset/{cs,ts,js}.txt` — canonical per-language file lists
- `oracle/cs/` — the Roslyn C# symbol-dump tool (`CsDump.csproj`, `Program.cs`) → `oracle/out/cs-symbols.json`
- `oracle/ts/dump.mjs` — the TS-compiler symbol dump → `oracle/out/ts-symbols.json`, `oracle/out/js-symbols.json`
- `compare/dump_recarto.py` — reads recarto's SQLite → `oracle/out/recarto-nodes.json`, `recarto-edges.json`
- `compare/completeness.py` — Phase 1 diff → `reports/phase1-completeness.md`, `reports/phase1-missing.json`
- `compare/accuracy.py` — Phase 2.1 symbol accuracy → `reports/phase2-accuracy.md`
- `edges/sample_edges.py` — Phase 2.2 edge sampler → `reports/phase2-edge-sample.json`
- `arch/csproj_deps.py` — Phase 2.3 `<ProjectReference>` graph → `reports/phase2-arch.md`
- `llm/questions.json` (Q, no answers), `llm/key.json` (K), `llm/answers.json`, `llm/grade.md`
- `embed/recall.py` — Phase 4 FTS recall@k; `embed/spike/` — gated vector spike
- `reports/` — all per-phase markdown/JSON

**Committed (under `$RECARTO`):**
- `internal/parse/testdata/<lang>/<case>.<ext>` — minimal fixtures reproducing each bug
- `internal/parse/parse_test.go` (or `zz_probe_test.go`) — symbol/edge regression tests
- `internal/parse/queries/*.scm` — grammar query fixes
- `internal/graph/*.go` + `*_test.go` — resolution/FTS fixes
- `internal/store/migrate.go` + `migrate_test.go` — cache-wipe migration bump (one per indexer change)
- `docs/validation/2026-06-04-ordia-e2e-report.md` — final committed report

---

## Phase 0 — Setup & baseline

### Task 0.1: Build recarto under test and prepare scratch

**Files:**
- Create: `$E2E/bin/recarto`

- [ ] **Step 1: Create scratch tree and export env**

```bash
export RECARTO=/home/claude/Projects/recarto ORDIA=/home/claude/Projects/ordia E2E=/home/claude/recarto-e2e
export RECARTO_CACHE_DIR=$E2E/cache RECARTO_BIN=$E2E/bin/recarto
mkdir -p $E2E/{bin,fileset,oracle/out,compare,edges,arch,llm,embed,reports} $RECARTO_CACHE_DIR
```

- [ ] **Step 2: Build recarto from source (this branch's indexer)**

```bash
cd $RECARTO && go build -o $RECARTO_BIN ./cmd/recarto
```
Expected: exits 0, binary at `$E2E/bin/recarto`.

- [ ] **Step 3: Verify the binary**

```bash
$RECARTO_BIN version && $RECARTO_BIN --help 2>&1 | head -20
```
Expected: prints a version string and usage listing `index` and `graph` subcommands.

- [ ] **Step 4: Confirm schema baseline**

Run: `grep -n 'schemaVersion = ' $RECARTO/internal/store/migrate.go`
Expected: `schemaVersion = 6`. Record this — the first indexer fix in Phase 5 bumps it to 7.

### Task 0.2: Prepare Ordia oracle toolchains

**Files:** none created (environment prep)

- [ ] **Step 1: Confirm toolchains**

```bash
dotnet --version; node --version; ls $ORDIA/frontend/node_modules/typescript/package.json
```
Expected: `10.0.300`, `v24.x`, and the typescript package present (the TS oracle imports it).

- [ ] **Step 2: Restore/build the .NET solution (for the semantic C# edge oracle)**

```bash
cd $ORDIA/api-gateway && (ls *.sln >/dev/null 2>&1 && dotnet build -c Debug 2>&1 | tail -20 || dotnet build src/Ordia.Api/Ordia.Api.csproj 2>&1 | tail -20)
```
Expected: `Build succeeded`. **If restore/build fails** (restricted network): record it in `$E2E/reports/phase0-setup.md` and proceed — the C# *completeness/accuracy* oracle (Task 1.1) is syntactic and needs no build; only the *sampled C# edge-resolution* checks in Task 2.2 degrade to manual reasoning.

- [ ] **Step 3: Sanity-check serena can see Ordia**

This step is performed by the executing agent, not a shell command. Load the serena tools via ToolSearch (`select:mcp__plugin_serena_serena__activate_project,mcp__plugin_serena_serena__get_symbols_overview`), then `activate_project` with path `/home/claude/Projects/ordia`, and `get_symbols_overview` on one C# file (e.g. `api-gateway/src/Ordia.Domain/...`) and one `.tsx` file. Expected: serena returns symbols for both. Record success/failure in `$E2E/reports/phase0-setup.md`.

### Task 0.3: Index Ordia, compute the canonical file set, dump recarto's graph

**Files:**
- Create: `$E2E/fileset/{cs,ts,js}.txt`, `$E2E/compare/dump_recarto.py`, `$E2E/oracle/out/recarto-nodes.json`, `$E2E/oracle/out/recarto-edges.json`

- [ ] **Step 1: Index Ordia and time it**

```bash
cd $ORDIA && time $RECARTO_BIN index 2>&1 | tee $E2E/reports/phase0-index.txt
```
Expected: prints counts (files/nodes/edges); records wall time for the report.

- [ ] **Step 2: Compute the canonical file set (git-tracked, exclude generated/vendored)**

```bash
cd $ORDIA
git ls-files '*.cs'  | grep -vE '(^|/)(bin|obj)/' > $E2E/fileset/cs.txt
git ls-files '*.ts' '*.tsx' | grep -vE '(^|/)(node_modules|\.next|dist|coverage)/|\.d\.ts$|next-env\.d\.ts|/__generated__/|\.generated\.' > $E2E/fileset/ts.txt
git ls-files '*.js' '*.mjs' '*.cjs' | grep -vE '(^|/)(node_modules|\.next|dist|coverage|public|out)/|\.min\.js$' > $E2E/fileset/js.txt
wc -l $E2E/fileset/*.txt
```
Expected: cs ≈ 160, ts ≈ 98. **Investigate the `.js` count**: if `js.txt` is still large, sample 10 paths (`shuf -n10 $E2E/fileset/js.txt`) to confirm provenance; if they are generated/config (`*.config.js`, codegen), tighten the exclude regex and note the decision in `$E2E/reports/phase0-setup.md`. Hand-written app `.js` stay in scope.

- [ ] **Step 3: Write the recarto SQLite dumper**

Create `$E2E/compare/dump_recarto.py`:

```python
#!/usr/bin/env python3
"""Dump recarto's per-repo code graph to JSON via the SQLite cache (read-only)."""
import glob, json, os, sqlite3, sys

cache = os.environ["RECARTO_CACHE_DIR"]
dbs = glob.glob(os.path.join(cache, "**", "*.db"), recursive=True)
dbs = [d for d in dbs if not d.endswith(("-wal", "-shm"))]
if len(dbs) != 1:
    sys.exit(f"expected exactly one cache DB under {cache}, found: {dbs}")
con = sqlite3.connect(f"file:{dbs[0]}?mode=ro", uri=True)
con.row_factory = sqlite3.Row

nodes = [dict(r) for r in con.execute("""
  SELECT n.id, f.language, f.path, n.kind, n.name, n.qualified_name,
         n.start_line, n.end_line, n.start_byte, n.end_byte
  FROM nodes n JOIN files f ON f.id = n.file_id""")]
edges = [dict(r) for r in con.execute("""
  SELECT e.id, e.kind, e.dst_name, e.line,
         e.src_node_id, e.dst_node_id, f.path AS file_path
  FROM edges e JOIN files f ON f.id = e.file_id""")]

out = os.path.join(os.environ["E2E"], "oracle", "out")
os.makedirs(out, exist_ok=True)
json.dump(nodes, open(os.path.join(out, "recarto-nodes.json"), "w"), indent=2)
json.dump(edges, open(os.path.join(out, "recarto-edges.json"), "w"), indent=2)
print(f"db={dbs[0]}  nodes={len(nodes)}  edges={len(edges)}")
print("by language/kind:")
from collections import Counter
for (lang, kind), c in sorted(Counter((n["language"], n["kind"]) for n in nodes).items()):
    print(f"  {lang:12} {kind:14} {c}")
```

- [ ] **Step 4: Run the dumper**

```bash
python3 $E2E/compare/dump_recarto.py
```
Expected: prints `nodes=…` and a per-language/kind breakdown; writes `recarto-nodes.json` / `recarto-edges.json`. Sanity: C# should show `record`, `enum`, `namespace`, `property`, `constructor` rows (the bug-069 fixes); TS should show `type`, `enum`, plus `function` for arrow-consts (bug-073 fixes). If any of these kinds is missing entirely, that is already a Phase 1 finding.

---

## Phase 1 — Completeness validation

The construct kinds recarto *should* emit, by language (from the `.scm` queries — the comparison must map oracle constructs to exactly these):

- **C# (`csharp`):** class, interface, struct, record, enum, enum_member, delegate, method, constructor, property, namespace
- **TS/TSX (`typescript`/`tsx`):** function (incl. arrow/function-expression `const`), method, class (incl. abstract), interface, enum, type (type alias), namespace
- **JS (`javascript`):** function (incl. generator + arrow/function-expression `const`), method, class

### Task 1.1: Build the C# Roslyn symbol-dump oracle (syntactic, exact, no build needed)

**Files:**
- Create: `$E2E/oracle/cs/CsDump.csproj`, `$E2E/oracle/cs/Program.cs`

- [ ] **Step 1: Create the project file**

`$E2E/oracle/cs/CsDump.csproj`:
```xml
<Project Sdk="Microsoft.NET.Sdk">
  <PropertyGroup>
    <OutputType>Exe</OutputType>
    <TargetFramework>net10.0</TargetFramework>
    <Nullable>enable</Nullable>
    <ImplicitUsings>enable</ImplicitUsings>
  </PropertyGroup>
  <ItemGroup>
    <PackageReference Include="Microsoft.CodeAnalysis.CSharp" Version="4.*" />
  </ItemGroup>
</Project>
```

- [ ] **Step 2: Write the syntactic walker**

`$E2E/oracle/cs/Program.cs`:
```csharp
using System.Text.Json;
using Microsoft.CodeAnalysis;
using Microsoft.CodeAnalysis.CSharp;
using Microsoft.CodeAnalysis.CSharp.Syntax;

// Usage: dotnet run -- <repo-root> <filelist.txt> <out.json>
var root = args[0]; var list = args[1]; var outPath = args[2];
var rows = new List<object>();
foreach (var rel in File.ReadLines(list))
{
    if (string.IsNullOrWhiteSpace(rel)) continue;
    var abs = Path.Combine(root, rel);
    if (!File.Exists(abs)) continue;
    var text = File.ReadAllText(abs);
    var tree = CSharpSyntaxTree.ParseText(text);
    var rootNode = tree.GetRoot();
    void Add(string kind, string name, SyntaxNode n) =>
        rows.Add(new { path = rel.Replace('\\','/'), kind, name,
            line = tree.GetLineSpan(n.Span).StartLinePosition.Line + 1 });
    foreach (var n in rootNode.DescendantNodes())
    {
        switch (n)
        {
            case ClassDeclarationSyntax c:        Add("class", c.Identifier.Text, c); break;
            case InterfaceDeclarationSyntax i:    Add("interface", i.Identifier.Text, i); break;
            case StructDeclarationSyntax s:       Add("struct", s.Identifier.Text, s); break;
            case RecordDeclarationSyntax r:       Add("record", r.Identifier.Text, r); break;
            case EnumDeclarationSyntax e:         Add("enum", e.Identifier.Text, e); break;
            case EnumMemberDeclarationSyntax em:  Add("enum_member", em.Identifier.Text, em); break;
            case DelegateDeclarationSyntax d:     Add("delegate", d.Identifier.Text, d); break;
            case MethodDeclarationSyntax m:       Add("method", m.Identifier.Text, m); break;
            case ConstructorDeclarationSyntax k:  Add("constructor", k.Identifier.Text, k); break;
            case PropertyDeclarationSyntax p:     Add("property", p.Identifier.Text, p); break;
            case NamespaceDeclarationSyntax ns:   Add("namespace", ns.Name.ToString(), ns); break;
            case FileScopedNamespaceDeclarationSyntax fns: Add("namespace", fns.Name.ToString(), fns); break;
        }
    }
}
File.WriteAllText(outPath, JsonSerializer.Serialize(rows, new JsonSerializerOptions { WriteIndented = true }));
Console.WriteLine($"C# oracle: {rows.Count} declarations -> {outPath}");
```

- [ ] **Step 3: Run the C# oracle**

```bash
cd $E2E/oracle/cs && dotnet run -- $ORDIA $E2E/fileset/cs.txt $E2E/oracle/out/cs-symbols.json 2>&1 | tail -5
```
Expected: `C# oracle: N declarations -> …`. **If `dotnet restore` fails offline:** create the ripgrep fallback `$E2E/oracle/cs/fallback.sh` and note the downgrade —
```bash
# Fallback (approximate counts only): per-construct line counts.
cd $ORDIA
while read f; do cat "$f"; done < $E2E/fileset/cs.txt | grep -oE '\b(class|interface|struct|record|enum|delegate)\b[[:space:]]+[A-Za-z_]' \
  | grep -oE '^[a-z]+' | sort | uniq -c
```
Use these approximate counts and proceed; mark C# completeness "approximate (no Roslyn)" in the report.

### Task 1.2: Build the TS/JS symbol-dump oracle (TypeScript compiler API, exact)

**Files:**
- Create: `$E2E/oracle/ts/dump.mjs`

- [ ] **Step 1: Write the AST walker**

`$E2E/oracle/ts/dump.mjs`:
```javascript
// Usage: node dump.mjs <repo-root> <filelist.txt> <out.json>
import ts from "/home/claude/Projects/ordia/frontend/node_modules/typescript/lib/typescript.js";
import { readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";

const [root, list, out] = process.argv.slice(2);
const files = readFileSync(list, "utf8").split("\n").map(s => s.trim()).filter(Boolean);
const rows = [];

function lineOf(sf, node) { return sf.getLineAndCharacterOfPosition(node.getStart(sf)).line + 1; }
function isFnInit(init) {
  return init && (ts.isArrowFunction(init) || ts.isFunctionExpression(init));
}
for (const rel of files) {
  const abs = join(root, rel);
  let text; try { text = readFileSync(abs, "utf8"); } catch { continue; }
  const sf = ts.createSourceFile(rel, text, ts.ScriptTarget.Latest, true,
    rel.endsWith(".tsx") || rel.endsWith(".jsx") ? ts.ScriptKind.TSX : undefined);
  const add = (kind, name, node) => name && rows.push({ path: rel, kind, name, line: lineOf(sf, node) });
  const visit = (node) => {
    if (ts.isFunctionDeclaration(node) && node.name) add("function", node.name.text, node);
    else if (ts.isClassDeclaration(node) && node.name) add("class", node.name.text, node);
    else if (ts.isInterfaceDeclaration(node)) add("interface", node.name.text, node);
    else if (ts.isEnumDeclaration(node)) add("enum", node.name.text, node);
    else if (ts.isTypeAliasDeclaration(node)) add("type", node.name.text, node);
    else if (ts.isModuleDeclaration(node) && node.name && ts.isIdentifier(node.name)) add("namespace", node.name.text, node);
    else if (ts.isMethodDeclaration(node) && node.name && ts.isIdentifier(node.name)) add("method", node.name.text, node);
    else if (ts.isVariableDeclaration(node) && node.name && ts.isIdentifier(node.name) && isFnInit(node.initializer))
      add("function", node.name.text, node);
    ts.forEachChild(node, visit);
  };
  visit(sf);
}
writeFileSync(out, JSON.stringify(rows, null, 2));
console.log(`TS/JS oracle: ${rows.length} declarations -> ${out}`);
```

- [ ] **Step 2: Run the oracle for TS/TSX and JS separately**

```bash
node $E2E/oracle/ts/dump.mjs $ORDIA $E2E/fileset/ts.txt $E2E/oracle/out/ts-symbols.json
node $E2E/oracle/ts/dump.mjs $ORDIA $E2E/fileset/js.txt $E2E/oracle/out/js-symbols.json
```
Expected: each prints `TS/JS oracle: N declarations -> …`.

### Task 1.3: Compare and triage completeness gaps

**Files:**
- Create: `$E2E/compare/completeness.py`, `$E2E/reports/phase1-completeness.md`, `$E2E/reports/phase1-missing.json`

- [ ] **Step 1: Write the comparison script**

`$E2E/compare/completeness.py`:
```python
#!/usr/bin/env python3
"""Diff recarto nodes vs the language oracles. Counts per (lang,kind) and the
list of oracle symbols recarto missed, keyed on (path, kind, name)."""
import json, os
from collections import Counter, defaultdict

OUT = os.path.join(os.environ["E2E"], "oracle", "out")
REP = os.path.join(os.environ["E2E"], "reports")
load = lambda f: json.load(open(os.path.join(OUT, f)))

recarto = load("recarto-nodes.json")
oracle = []
for f, langs in [("cs-symbols.json", {"csharp"}), ("ts-symbols.json", {"typescript", "tsx"}), ("js-symbols.json", {"javascript"})]:
    p = os.path.join(OUT, f)
    if os.path.exists(p):
        for r in json.load(open(p)):
            r["_langs"] = langs
            oracle.append(r)

# recarto languages collapse for comparison: tsx+typescript compared against the
# TS oracle; csharp vs C#; javascript vs JS.
def lang_bucket(l): return "ts" if l in ("typescript", "tsx") else ("cs" if l == "csharp" else "js")

def key(path, kind, name): return (path.replace("\\", "/"), kind, name)

r_keys = defaultdict(set)   # bucket -> set(key)
r_counts = Counter()
for n in recarto:
    b = lang_bucket(n["language"])
    r_keys[b].add(key(n["path"], n["kind"], n["name"]))
    r_counts[(b, n["kind"])] += 1

o_keys = defaultdict(set)
o_counts = Counter()
o_by_key = {}
for r in oracle:
    b = "ts" if "typescript" in r["_langs"] else ("cs" if "csharp" in r["_langs"] else "js")
    k = key(r["path"], r["kind"], r["name"])
    o_keys[b].add(k)
    o_counts[(b, r["kind"])] += 1
    o_by_key[(b, k)] = r

lines = ["# Phase 1 — Completeness\n", "## Per (bucket, kind) counts: oracle vs recarto\n",
         "| bucket | kind | oracle | recarto | delta |", "|---|---|---:|---:|---:|"]
for bk in sorted(set(o_counts) | set(r_counts)):
    b, kind = bk
    o, rc = o_counts.get(bk, 0), r_counts.get(bk, 0)
    lines.append(f"| {b} | {kind} | {o} | {rc} | {rc - o:+d} |")

missing = []
for b in ("cs", "ts", "js"):
    miss = o_keys[b] - r_keys[b]
    for k in sorted(miss):
        missing.append({"bucket": b, "path": k[0], "kind": k[1], "name": k[2]})
lines += ["\n## Oracle symbols missing from recarto (candidate bugs)\n",
          f"total missing: {len(missing)}\n",
          "Top missing kinds:"]
for (b, kind), c in Counter((m["bucket"], m["kind"]) for m in missing).most_common(20):
    lines.append(f"- {b}/{kind}: {c}")

# Extra = recarto has it but oracle doesn't (phantoms / over-capture).
extra = []
for b in ("cs", "ts", "js"):
    for k in sorted(r_keys[b] - o_keys[b]):
        extra.append({"bucket": b, "path": k[0], "kind": k[1], "name": k[2]})
lines += [f"\n## recarto symbols NOT in oracle (phantom / over-capture): {len(extra)}",
          "Top extra kinds:"]
for (b, kind), c in Counter((e["bucket"], e["kind"]) for e in extra).most_common(20):
    lines.append(f"- {b}/{kind}: {c}")

open(os.path.join(REP, "phase1-completeness.md"), "w").write("\n".join(lines))
json.dump({"missing": missing, "extra": extra}, open(os.path.join(REP, "phase1-missing.json"), "w"), indent=2)
print(f"missing={len(missing)} extra={len(extra)} -> reports/phase1-completeness.md")
```

- [ ] **Step 2: Run it**

```bash
python3 $E2E/compare/completeness.py && cat $E2E/reports/phase1-completeness.md
```
Expected: a counts table + missing/extra lists.

- [ ] **Step 3: Triage every nonzero delta and the missing/extra lists**

For each row with `delta != 0` and each missing/extra cluster, classify in `phase1-completeness.md` under a new `## Triage` heading:
- **Bug** — recarto *should* capture this construct but doesn't (e.g., an entire kind shows recarto=0 while oracle>0), or captures phantoms. → log to `$RECARTO/.wolf/buglog.json`, add to the Phase 5 fix queue.
- **Out-of-scope (documented)** — a construct the design intentionally skips (local functions, lambdas, anonymous types, getters/setters-as-properties mismatches, enum members nested under enums). Record the rationale; not a bug.
- **Oracle artifact** — kind-mapping mismatch (e.g., oracle labels a record's primary-constructor parameter as a property). Fix the mapping in the script and re-run.

Expected real findings to look for specifically (from reading the `.scm`): C# `event`/`event_field_declaration` not captured; C# `operator`/indexer/finalizer not captured; TS object-literal method shorthand; default-exported anonymous functions (no name → dropped). Decide bug vs out-of-scope for each.

- [ ] **Step 4: Commit the triage decisions to the report (scratch)** — no git commit (scratch dir). Ensure `phase1-completeness.md` has the `## Triage` section filled before moving on.

---

## Phase 2 — Accuracy & edge validation

### Task 2.1: Symbol accuracy (kind / name / span / phantoms / duplicates)

**Files:**
- Create: `$E2E/compare/accuracy.py`, `$E2E/reports/phase2-accuracy.md`

- [ ] **Step 1: Write the accuracy checker**

`$E2E/compare/accuracy.py`:
```python
#!/usr/bin/env python3
"""For symbols present in BOTH recarto and the oracle (matched on path+kind+name),
check the start line agrees (±2 tolerance for attribute/decorator lines). Also
report duplicate nodes (same path+kind+name+line appearing >1x in recarto) and
qualified_name quality."""
import json, os
from collections import Counter, defaultdict

OUT = os.path.join(os.environ["E2E"], "oracle", "out")
REP = os.path.join(os.environ["E2E"], "reports")
recarto = json.load(open(os.path.join(OUT, "recarto-nodes.json")))
oracle = []
for f in ("cs-symbols.json", "ts-symbols.json", "js-symbols.json"):
    p = os.path.join(OUT, f)
    if os.path.exists(p): oracle += json.load(open(p))

def norm(p): return p.replace("\\", "/")
o_line = {}
for r in oracle:
    o_line.setdefault((norm(r["path"]), r["kind"], r["name"]), []).append(r["line"])

dupes = [k for k, c in Counter((norm(n["path"]), n["kind"], n["name"], n["start_line"]) for n in recarto).items() if c > 1]
line_off, checked, qn_equals_name = [], 0, 0
for n in recarto:
    k = (norm(n["path"]), n["kind"], n["name"])
    if n["qualified_name"] == n["name"]: qn_equals_name += 1
    if k in o_line:
        checked += 1
        if not any(abs(n["start_line"] - ol) <= 2 for ol in o_line[k]):
            line_off.append({**{x: n[x] for x in ("path", "kind", "name", "start_line")},
                             "oracle_lines": o_line[k]})

lines = ["# Phase 2.1 — Symbol accuracy\n",
         f"- matched symbols line-checked: {checked}",
         f"- line mismatches (> ±2): {len(line_off)}",
         f"- duplicate recarto nodes (same path+kind+name+line): {len(dupes)}",
         f"- nodes where qualified_name == name (no real qualification): {qn_equals_name}/{len(recarto)}",
         "\n## Line mismatches (first 30)"]
for m in line_off[:30]:
    lines.append(f"- {m['path']} {m['kind']} {m['name']} recarto L{m['start_line']} vs oracle {m['oracle_lines']}")
lines += ["\n## Duplicate nodes (first 30)"] + [f"- {d}" for d in dupes[:30]]
open(os.path.join(REP, "phase2-accuracy.md"), "w").write("\n".join(lines))
print(f"checked={checked} line_off={len(line_off)} dupes={len(dupes)} qn==name={qn_equals_name}")
```

- [ ] **Step 2: Run and triage**

```bash
python3 $E2E/compare/accuracy.py && cat $E2E/reports/phase2-accuracy.md
```
Expected: low line-mismatch and zero duplicates. Triage: any duplicate node = bug (double-capture). A large `qualified_name == name` count is a *known design limitation* (recarto sets `qualified_name = name`); record it as a candidate improvement (affects cross-file resolution precision in Task 2.2), decide with the user whether it's in scope to fix now.

### Task 2.2: Edge resolution (calls / extends / implements / contains) via serena LSP

**Files:**
- Create: `$E2E/edges/sample_edges.py`, `$E2E/reports/phase2-edge-sample.json`, `$E2E/reports/phase2-edges.md`

- [ ] **Step 1: Build a stratified edge sample**

`$E2E/edges/sample_edges.py`:
```python
#!/usr/bin/env python3
"""Sample edges per kind for manual LSP verification, and compute resolution
stats: what fraction of 'calls' edges got a dst_node_id (resolved), and how often
the resolved target is cross-file vs same-file."""
import json, os, random
from collections import Counter, defaultdict

OUT = os.path.join(os.environ["E2E"], "oracle", "out")
REP = os.path.join(os.environ["E2E"], "reports")
nodes = {n["id"]: n for n in json.load(open(os.path.join(OUT, "recarto-nodes.json")))}
edges = json.load(open(os.path.join(OUT, "recarto-edges.json")))
random.seed(1)

by_kind = defaultdict(list)
for e in edges: by_kind[e["kind"]].append(e)

stats = {}
for kind, es in by_kind.items():
    resolved = [e for e in es if e["dst_node_id"]]
    crossfile = sum(1 for e in resolved if e["dst_node_id"] in nodes
                    and nodes[e["dst_node_id"]]["path"] != e["file_path"])
    stats[kind] = {"total": len(es), "resolved": len(resolved),
                   "resolved_pct": round(100*len(resolved)/max(1,len(es)), 1),
                   "cross_file_of_resolved": crossfile}

sample = {}
for kind in ("calls", "extends", "implements", "contains"):
    pool = by_kind.get(kind, [])
    pick = random.sample(pool, min(20, len(pool)))
    sample[kind] = [{
        "src": (nodes.get(e["src_node_id"]) or {}).get("name"),
        "src_path": e["file_path"], "line": e["line"], "dst_name": e["dst_name"],
        "resolved_to": (nodes.get(e["dst_node_id"]) or {}).get("name") if e["dst_node_id"] else None,
        "resolved_path": (nodes.get(e["dst_node_id"]) or {}).get("path") if e["dst_node_id"] else None,
    } for e in pick]

json.dump({"stats": stats, "sample": sample}, open(os.path.join(REP, "phase2-edge-sample.json"), "w"), indent=2)
print(json.dumps(stats, indent=2))
```

- [ ] **Step 2: Run it**

```bash
python3 $E2E/edges/sample_edges.py
```
Expected: prints per-kind resolution stats and writes the sample.

- [ ] **Step 3: Verify the sample against serena (LSP ground truth)**

This is performed by the executing agent. Load serena tools (`select:mcp__plugin_serena_serena__find_symbol,mcp__plugin_serena_serena__find_referencing_symbols,mcp__plugin_serena_serena__find_implementations`). For each sampled edge in `phase2-edge-sample.json`:
- **calls**: open `src_path:line`, identify the real callee; use serena `find_symbol` on `dst_name` to find its true definition(s). Mark recarto's `resolved_to`/`resolved_path` **correct / wrong / unresolved**. Pay special attention to the **first-match ambiguity** risk: `Resolve()` matches `dst_name` against node name OR qualified_name with `LIMIT 1`, so a common method name (e.g. `Handle`, `ToString`, `MapAsync`) may resolve to the wrong overload/class. Count how many sampled `calls` are mis-resolved due to ambiguity.
- **extends / implements**: for C#, recall the `.scm` maps `base_list → extends` only (C# cannot syntactically separate a base class from an interface), so interface implementations are labeled `extends`. Confirm this is what you see; classify as **known limitation** (not a per-symbol bug) unless an actual base type is missed. For TS, verify `extends` vs `implements` are correctly separated.
- **contains**: verify the parent is the true innermost encloser (namespace→class→method).

Record results in `$E2E/reports/phase2-edges.md`: per-kind correct/wrong/unresolved tallies + the cross-file resolution rate + a list of concrete mis-resolutions with file:line. Each *systematic* wrong-resolution pattern (not one-off ambiguity) → log a bug.

- [ ] **Step 4: Decide scope for ambiguity mis-resolution**

If first-match ambiguity mis-resolves a material fraction of cross-file `calls`, write a finding: the fix is to qualify resolution (prefer same-namespace/same-project, or populate a real `qualified_name`). Flag for the user as a candidate Phase 5 fix vs. a documented limitation — do not silently expand scope.

### Task 2.3: Architecture / module-dependency validation against `.csproj`

**Files:**
- Create: `$E2E/arch/csproj_deps.py`, `$E2E/reports/phase2-arch.md`

- [ ] **Step 1: Extract the true project-reference graph**

`$E2E/arch/csproj_deps.py`:
```python
#!/usr/bin/env python3
"""Ground-truth .NET module deps from <ProjectReference>; compare to recarto arch."""
import os, re, subprocess, glob
ORDIA = os.environ["ORDIA"]
deps = []
for csproj in glob.glob(os.path.join(ORDIA, "**", "*.csproj"), recursive=True):
    if "/bin/" in csproj or "/obj/" in csproj: continue
    proj = os.path.splitext(os.path.basename(csproj))[0]
    for m in re.findall(r'<ProjectReference\s+Include="([^"]+)"', open(csproj).read()):
        ref = os.path.splitext(os.path.basename(m.replace("\\", "/")))[0]
        deps.append((proj, ref))
print("True <ProjectReference> edges:")
for a, b in sorted(deps): print(f"  {a} -> {b}")
print(f"\nTotal: {len(deps)} edges across {len(set(p for p,_ in deps) | set(r for _,r in deps))} projects")
```

- [ ] **Step 2: Run both sides**

```bash
python3 $E2E/arch/csproj_deps.py | tee $E2E/reports/phase2-arch.md
echo "--- recarto arch ---" >> $E2E/reports/phase2-arch.md
(cd $ORDIA && $RECARTO_BIN graph arch) | tee -a $E2E/reports/phase2-arch.md
```

- [ ] **Step 3: Compare module grouping and dep edges**

In `phase2-arch.md`, note: (a) does recarto's module list correspond to the projects/top-level dirs sensibly? (b) do recarto's inter-module dep edges align with the `<ProjectReference>` direction? recarto infers deps from *resolved cross-module code edges*, so it will be a subset/variant of `<ProjectReference>` (it only sees deps that are actually exercised by indexed calls/extends). A *contradicting* edge (recarto shows A→B where B→A is the only real reference, or a module that should be present is entirely missing) is a bug; a *missing* edge for an unused reference is expected. Record the classification.

---

## Phase 3 — LLM query grading + token efficiency (clean-room)

### Task 3.1: Author the question set Q and verified key K

**Files:**
- Create: `$E2E/llm/questions.json` (no answers), `$E2E/llm/key.json`

- [ ] **Step 1: Draft ~30 questions spanning all five MCP tools**

Using the oracle dumps + serena (full knowledge), write `$E2E/llm/questions.json` as a list of `{ "id", "q", "tool_hint" }` and `$E2E/llm/key.json` as `{ "id": { "answer", "evidence" } }`. Distribute across tool types:
- `search_graph` (≥8): "Where is the symbol that handles X defined?" (use real Ordia symbol names verified to exist).
- `get_code_snippet` (≥5): "Show the body of method M in file F."
- `trace_call_path` callers (≥5): "What calls function F?" / callees (≥4): "What does F call?"
- `get_architecture` (≥4): "Which modules depend on the Domain project?" / "List the modules."
- `query_graph` (≥4): "List all enums in the C# backend." / "Find all React components (arrow-function consts) in `frontend/src/components`."

Each `key` answer must be a concrete, oracle-verified fact (exact symbol name, file path, or the set membership). Questions must require repo-specific facts (no generic answers a model could guess).

- [ ] **Step 2: Self-check the key**

For 5 random questions, independently re-verify the key answer via serena `find_symbol` / source. Fix any wrong key entry. (A wrong key would unfairly fail the grader.)

### Task 3.2: Run the cold, recarto-only grader subagent

**Files:**
- Create: `$E2E/llm/answers.json`

- [ ] **Step 1: Expose recarto to the grader**

Preferred (real MCP): register recarto's MCP server pointed at Ordia so the grader can call `search_graph` etc. From `$ORDIA`, run `$RECARTO_BIN init --agent claude --scope repo` **in a throwaway copy or with care** — OR, simpler and side-effect-free, instruct the grader to use the CLI surface (`$RECARTO_BIN graph search|callers|callees|arch|query|snippet`, `$RECARTO_BIN recall`) which routes through the same `Engine`. Decide and record which surface was used (MCP exercises tool descriptions/schemas; CLI does not — note the difference in the report).

- [ ] **Step 2: Dispatch the clean-room grader (fresh subagent, cold context)**

Spawn a subagent (Agent tool, general-purpose) with a prompt that enforces isolation. The prompt MUST include:
- The working dir is `/home/claude/Projects/ordia`.
- "Your ONLY means of inspecting this codebase is the recarto tool surface: `$RECARTO_BIN graph <search|callers|callees|arch|query|snippet> …` and `$RECARTO_BIN recall …` (or the recarto MCP tools if registered). You are FORBIDDEN from using Read, Grep, Glob, serena, the web, or your own prior knowledge of the code. If recarto cannot answer, say so."
- "For EACH question return JSON: `{id, answer, recarto_calls:[exact commands run], evidence:[verbatim tool output you relied on]}`. An answer with empty `recarto_calls` is invalid."
- The contents of `$E2E/llm/questions.json` (questions only — NOT the key).

Save the subagent's returned JSON to `$E2E/llm/answers.json`.

- [ ] **Step 3: Compliance audit**

Verify every answer has non-empty `recarto_calls` and the `evidence` plausibly came from recarto output. Disqualify (mark `invalid`) any answer that appears to use outside knowledge. Re-dispatch those questions with a stricter reminder if needed.

### Task 3.3: Grade and measure token efficiency

**Files:**
- Create: `$E2E/llm/grade.md`

- [ ] **Step 1: Grade answers vs the key**

For each question, compare `answers.json[id].answer` to `key.json[id].answer`: mark **correct / partial / wrong / tool-failed / invalid**. Compute accuracy = correct / (total − invalid). Target ≥ **90%** on factual lookups.

- [ ] **Step 2: Measure tokens-to-answer vs file-read baseline**

For ~8 representative questions: (a) sum the characters of the grader's `recarto_calls` outputs (≈ tokens/4) = the graph cost; (b) measure the file-read baseline = bytes of the file(s) that contain the answer (`wc -c` on the relevant source files). Report the ratio (graph vs whole-file) — this quantifies the "save tokens" claim.

- [ ] **Step 3: Root-cause every miss**

Classify each wrong/partial: **graph bug** (data missing/incorrect → Phase 5), **tool-ergonomics gap** (the data exists but the tool's description/args/output shape made it unreachable — e.g., `graph search` needs a kind filter the agent didn't discover; `callers` needs an exact name the agent couldn't guess), or **recall gap** (lexical search couldn't surface the symbol from a natural-language description → feeds Phase 4). Write `$E2E/llm/grade.md` with the table, accuracy, token ratios, and the classified miss list. Log graph bugs to buglog.

---

## Phase 4 — Embeddings evaluation (+ gated spike)

### Task 4.1: Measure lexical FTS recall on semantic queries

**Files:**
- Create: `$E2E/embed/queries.json`, `$E2E/embed/recall.py`, `$E2E/reports/phase4-recall.md`

- [ ] **Step 1: Build a semantic query set (no lexical overlap with the target)**

`$E2E/embed/queries.json`: ~20 `{ "q", "relevant": ["NodeName", …] }` where `q` is a natural-language description that shares NO identifier token with the target symbol(s). Examples (replace with real Ordia symbols verified via oracle): `{"q": "charge the customer's card", "relevant": ["ProcessPayment", "SettleInvoice"]}`, `{"q": "turn a domain entity into its API response shape", "relevant": ["ToDto", "MapToResponse"]}`. Pick targets whose names you confirmed exist in `recarto-nodes.json`.

- [ ] **Step 2: Write the recall harness**

`$E2E/embed/recall.py`:
```python
#!/usr/bin/env python3
"""recall@k for recarto FTS search over the semantic query set."""
import json, os, subprocess
Q = json.load(open(os.path.join(os.environ["E2E"], "embed", "queries.json")))
BIN, ORDIA = os.environ["RECARTO_BIN"], os.environ["ORDIA"]

def search(q, k):
    out = subprocess.run([BIN, "graph", "search", q], cwd=ORDIA,
                         capture_output=True, text=True).stdout
    names = []
    for line in out.splitlines():
        # line format: "• [kind] QualifiedName  path:line"
        if "]" in line:
            seg = line.split("]", 1)[1].strip().split("  ")[0].strip()
            names.append(seg.split(".")[-1])
    return names[:k]

def recall_at(k):
    hits = 0
    for item in Q:
        got = set(search(item["q"], k))
        if got & set(item["relevant"]): hits += 1
    return round(100 * hits / len(Q), 1)

r5, r10 = recall_at(5), recall_at(10)
rep = f"# Phase 4 — FTS semantic recall\n\n- queries: {len(Q)}\n- recall@5: {r5}%\n- recall@10: {r10}%\n"
rep += f"\n- spike trigger (recall@10 < 65%): {'YES' if r10 < 65 else 'NO'}\n"
open(os.path.join(os.environ["E2E"], "reports", "phase4-recall.md"), "w").write(rep)
print(rep)
```

- [ ] **Step 3: Run and decide the gate**

```bash
python3 $E2E/embed/recall.py
```
Expected: prints recall@5/@10 and the trigger decision. If **recall@10 ≥ 65%**, write the recommendation "lexical FTS is sufficient for Ordia-scale semantic recall; embeddings not required now" with the numbers, and **skip Task 4.2**.

### Task 4.2 (GATED — only if recall@10 < 65%): offline lightweight-local vector spike

**Files:**
- Create: `$E2E/embed/spike/build.py`, `$E2E/embed/spike/recall.py`, `$E2E/reports/phase4-spike.md`

- [ ] **Step 1: Build a dependency-free local vector index over the node corpus**

`$E2E/embed/spike/build.py`: read `recarto-nodes.json`; for each node build a document = identifier-split name + qualified_name + kind (this is what recarto already has — no source needed). Vectorize with **char 3-gram hashing + TF-IDF** (pure Python, preserves the zero-dependency static-binary constraint — no model, no network). Persist vectors to `$E2E/embed/spike/index.json`. This mirrors what recarto *could* ship in Go (feature hashing) without a model.

- [ ] **Step 2: Re-measure recall@k against the same query set**

`$E2E/embed/spike/recall.py`: cosine-rank the spike vectors for each `queries.json` query; compute recall@5/@10 exactly as Task 4.1. 

- [ ] **Step 3: Head-to-head recommendation**

Write `$E2E/reports/phase4-spike.md`: FTS vs spike recall@k side by side. State explicitly which query class each solves: the char-ngram/TF-IDF spike improves **fuzzy/morphology/partial** recall but does NOT add true semantics (a synonym with zero shared substrings still misses). If the spike materially beats FTS *and* is cheap to port to Go, recommend shipping it as an optional local index; if only a real embedding model would close the gap, say so and present the architecture cost (external API breaks offline/zero-dep; bundled model is heavy). The recommendation is evidence-based; the actual productionization is a separate follow-up plan.

---

## Phase 5 — Fix loop (TDD) + regression distillation

> Instantiate the template below **once per bug** logged in Phases 1–3. The bugs are discovered at runtime, so this is a repeatable procedure with a fully worked example, not a fixed task list. Work one bug at a time; commit after each.

### Task 5.0: Fix-loop template (worked example: a missing C# construct)

Suppose Phase 1 found recarto misses C# `event` declarations (oracle has them, recarto count = 0). The procedure (adapt the language/construct/fixture to the actual bug):

**Files (per bug):**
- Create: `internal/parse/testdata/csharp/events.cs`
- Modify: `internal/parse/parse_test.go`
- Modify: `internal/parse/queries/csharp.scm`
- Modify: `internal/store/migrate.go` (only if a `.scm` or FTS change), `internal/store/migrate_test.go`

- [ ] **Step 1: Confirm the real tree-sitter node name (MANDATORY before editing any `.scm`)**

The established convention (bug-069) is to verify node names against the actual grammar with an s-expression probe before editing a query — guessing node names is how constructs get silently dropped. Add a temporary probe in `internal/parse/zz_probe_test.go`:

```go
func TestProbeEventSexp(t *testing.T) {
	src := []byte("class C { public event System.Action OnX; }")
	r, _ := NewRegistry(); defer r.Close()
	p := sitter.NewParser(); defer p.Close()
	_ = p.SetLanguage(r.langs["csharp"])
	tree := p.Parse(src, nil); defer tree.Close()
	t.Log(tree.RootNode().ToSexp())
}
```
Run: `cd $RECARTO && go test ./internal/parse -run TestProbeEventSexp -v`
Expected: log shows the node type (e.g. `event_field_declaration` with a `variable_declaration`/`variable_declarator name:`). Use the EXACT node + field names it prints in the `.scm`. Remove the probe after.

- [ ] **Step 2: Write the failing fixture + test**

`internal/parse/testdata/csharp/events.cs`:
```csharp
namespace Demo;
public class Bus
{
    public event System.Action? OnTick;
}
```

Add to `internal/parse/parse_test.go`:
```go
func TestParseCSharpCapturesEvent(t *testing.T) {
	r, err := NewRegistry()
	if err != nil { t.Fatal(err) }
	defer r.Close()
	src, err := os.ReadFile("testdata/csharp/events.cs")
	if err != nil { t.Fatal(err) }
	res, err := r.ParseFile("csharp", "events.cs", src)
	if err != nil { t.Fatal(err) }
	if !hasSymbol(res, "event", "OnTick") {
		t.Fatalf("expected event OnTick, got %+v", res.Symbols)
	}
}
```
(Reuse the existing `hasSymbol` helper in `parse_test.go`; if absent, add `func hasSymbol(r Result, kind, name string) bool { for _, s := range r.Symbols { if s.Kind==kind && s.Name==name { return true } }; return false }`.)

- [ ] **Step 3: Run to verify it fails**

Run: `cd $RECARTO && go test ./internal/parse -run TestParseCSharpCapturesEvent -v`
Expected: FAIL — `expected event OnTick`.

- [ ] **Step 4: Fix the query**

Add to `internal/parse/queries/csharp.scm` (using the node/field names from Step 1):
```
(event_field_declaration (variable_declaration (variable_declarator name: (identifier) @name))) @definition.event
```

- [ ] **Step 5: Run to verify it passes**

Run: `cd $RECARTO && go test ./internal/parse -run TestParseCSharpCapturesEvent -v`
Expected: PASS.

- [ ] **Step 6: Add the cache-wipe migration (REQUIRED because a `.scm` changed)**

The standing convention (bug-071): any parser-query or FTS-content change MUST pair with a graph-wipe migration so existing per-repo caches rebuild on upgrade — enforced by `migrate_test.go::TestLatestGraphMigrationRebuildsIndex`. In `internal/store/migrate.go`, bump `schemaVersion` 6→7 and append to `migrations`:
```go
	// v7 (Ordia validation): csharp query completed to capture event declarations
	// (and any sibling constructs from this campaign). The derived graph is
	// rebuildable, so wipe it and clear the freshness token. See the v5 comment.
	`
DELETE FROM nodes_fts;
DELETE FROM edges;
DELETE FROM nodes;
DELETE FROM files;
DELETE FROM meta WHERE key LIKE 'graph_token:%';
`,
```
Batch all of this campaign's `.scm`/FTS changes into **one** v7 migration if you fix several constructs together (don't add one migration per construct).

- [ ] **Step 7: Verify the migration invariant + full suite**

Run: `cd $RECARTO && go test ./internal/store -run TestLatestGraphMigrationRebuildsIndex -v && go test ./... && go vet ./... && gofmt -l .`
Expected: the invariant test PASSES, full suite PASSES, vet clean, `gofmt -l` prints nothing.

- [ ] **Step 8: Confirm the real-world discrepancy closed on Ordia**

```bash
go build -o $RECARTO_BIN $RECARTO/cmd/recarto && (cd $ORDIA && $RECARTO_BIN index) && python3 $E2E/compare/dump_recarto.py && python3 $E2E/compare/completeness.py
grep -A2 'csharp | event' $E2E/reports/phase1-completeness.md || true
```
Expected: the previously-missing kind now shows recarto count ≈ oracle count (the v7 migration wiped the stale cache, the rebuild used the fixed query).

- [ ] **Step 9: Log the bug and commit**

Append the bug to `$RECARTO/.wolf/buglog.json` (error/root_cause/fix/tags). Then:
```bash
cd $RECARTO && git add internal/parse/queries/csharp.scm internal/parse/parse_test.go internal/parse/testdata/csharp/events.cs internal/store/migrate.go internal/store/migrate_test.go
git commit -m "fix(graph): capture C# event declarations (Ordia e2e validation)

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

- [ ] **Step 10: Repeat Task 5.0 for each remaining bug**

For non-`.scm` bugs (e.g., duplicate-node insertion, edge mis-resolution), the same TDD shape applies but the fix is in `internal/graph/*.go` and the test in `internal/graph/*_test.go`; a migration bump is needed only if the *indexer output or FTS content* changes. If a fix only changes query/ranking logic (not stored data), no migration is needed.

---

## Phase 6 — Report, cerebrum, and wrap

### Task 6.1: Write the committed campaign report

**Files:**
- Create: `$RECARTO/docs/validation/2026-06-04-ordia-e2e-report.md`

- [ ] **Step 1: Aggregate all phase reports**

Write `docs/validation/2026-06-04-ordia-e2e-report.md` with sections: codebase profile (Ordia counts, canonical file set + exclusions); Phase 1 completeness table + triage; Phase 2 accuracy/edge/arch findings (incl. the cross-file resolution rate and the `qualified_name == name` limitation); Phase 3 LLM accuracy + token-savings ratio + ergonomics gaps; Phase 4 recall numbers + the **embeddings recommendation** (with spike results if triggered); a consolidated **bugs found/fixed** table (with buglog ids + commit hashes); index/query timing; residual known limitations; and (optional) the Rollout scale cross-check.

- [ ] **Step 2: (Optional) Rollout scale cross-check**

If validating the "very large codebase" claim, index `~/Projects/rollout-feature-lsp` (or the largest available Rollout worktree), capture `time recarto index` and `time recarto graph search …`, and add the numbers to the report's timing section. Not a gate.

- [ ] **Step 3: Commit the report**

```bash
cd $RECARTO && git add docs/validation/2026-06-04-ordia-e2e-report.md
git commit -m "docs: Ordia e2e code-graph validation report + embeddings recommendation

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

### Task 6.2: Update cerebrum + final verification

- [ ] **Step 1: Record durable learnings**

Append to `$RECARTO/.wolf/cerebrum.md`: any new construct-coverage conventions discovered, the cross-file resolution ambiguity finding, and the embeddings decision + rationale (Decision Log). Update `.wolf/buglog.json` if not already done per-fix.

- [ ] **Step 2: Final green check**

Run: `cd $RECARTO && go test ./... && go vet ./... && gofmt -l . && $RECARTO_BIN doctor`
Expected: suite PASSES, vet clean, `gofmt -l` empty, `doctor` all `[ok]`.

- [ ] **Step 3: Confirm deliverables**

Verify the committed set: new fixtures under `internal/parse/testdata/`, new tests, the single v7 migration (if any indexer change), and the report. The `$E2E` scratch tree is intentionally NOT committed.

---

## Self-Review (completed by plan author)

- **Spec coverage:** Completeness (Phase 1) ✓; Accuracy/edges/architecture (Phase 2) ✓; clean-room LLM grading with evidence-grounding + token efficiency (Phase 3, spec §4) ✓; embeddings eval + gated lightweight-local spike (Phase 4, spec §6) ✓; TDD fix loop + cache-wipe migration convention + committed fixtures (Phase 5, spec §5/§8) ✓; committed report (Phase 6, spec §8) ✓; Roslyn-built C# oracle (spec §3, "Build .NET" decision) ✓ with documented degrade path (spec §7) ✓; canonical `git ls-files` file set + `.js` exclusion (spec §7) ✓; optional Rollout scale cross-check (spec §7) ✓.
- **Placeholder scan:** No "TBD/TODO/handle edge cases". The per-bug fixes are a worked template (real fixture, test, query, migration) explicitly instantiated per discovered bug — content is shown, not deferred.
- **Type/identifier consistency:** Real schema columns used (`qualified_name`, `start_line`, `src_node_id`, `dst_node_id`, `dst_name`); real `.scm` kinds (class/interface/struct/record/enum/method/constructor/property/namespace/type/function); `schemaVersion 6→7`; `RECARTO_CACHE_DIR`/`RECARTO_BIN`/`E2E` consistent across all tasks; `migrate_test.go::TestLatestGraphMigrationRebuildsIndex` referenced as the enforcing invariant.
