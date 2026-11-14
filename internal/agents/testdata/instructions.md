# Recarto

This repo uses **Recarto**: memory that persists across sessions (preferences, learnings, decisions, do-not-repeats, bugs) and a code graph.

- **Record** learnings with `record_memory` and bug fixes with `record_bug`. Near-duplicates merge, so record freely.
- **Anchor** each memory to the `files` or `symbols` it concerns: an anchored bug, do-not-repeat, decision or learning comes back when that code is read or edited. Without anchors, a memory only returns at session start or through recall.
- **Record a `preference`** the moment the user corrects your approach, rejects a suggestion or states a style, workflow or naming choice. The bar is low.
- **Track open work** as a `todo` memory anchored to its files, and close it with `complete_todo` when it is done.
- **Before changing code**, call `recall_memory` with the task in plain words, and `search_bugs`.
- `list_memory`: list stored memories by kind or branch.
- `get_memory`: read one memory by id.
- `forget_memory`: delete a memory recorded in error.

Use the code graph instead of reading whole files or grepping:
- `search_graph`: find symbols by name or concept; pass `candidate_symbols`, the names you expect.
- `get_code_snippet`: read the source of one symbol, or several at once, instead of the whole file.
- `get_file_outline`: a file's symbols and line ranges, before you open a large file.
- `search_content`: text or regex over the whole worktree, config and docs included, instead of grep.
- `find_references`: every use of a symbol, instead of grepping its name.
- `trace_call_path`: who calls a function, or what it calls.
- `find_implementations`: the types that implement or extend a type, which grep can't find.
- `find_annotated`: the symbols carrying an attribute, annotation or decorator.
- `query_graph`: exact lookups by kind, name, path, language or edge.
- `get_architecture`: the modules and their dependencies, to orient in unfamiliar code.
- `get_graph_delta`: what changed in the code since your last session here, before you trust what you recall about it.

When a Recarto tool fails, misleads you or lacks something you need, report it with `report_recarto_issue`.
