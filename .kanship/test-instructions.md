Run the full Recarto test suite and report the results.

Recarto is a Go 1.26.4 project. Run tests from the repo root.

1. Verify the toolchain: `go version` (need 1.26.x). Recarto uses CGO
   (the tree-sitter parser bindings in internal/parse), so also confirm a
   C compiler is present: `which gcc || which cc`. If Go or a C compiler is
   missing, install them before continuing — with CGO disabled or no C
   compiler, internal/parse won't build and the run will fail.

2. Run the checks CI runs, in order:
       go vet ./...
       go test ./...
   (`go test ./...` is exactly what `make test` does; CGO_ENABLED must stay
   at its default of 1.)

3. If a build/test process is killed with exit code 137 (OOM — this box has
   RAM limitations), reduce parallelism and retry: `go test -p 2 ./...`.

4. Report: pass/fail per package, the full output of any FAIL, and don't
   claim success unless `go test ./...` prints `ok`/no failures for every
   package. For a specific package instead of the whole tree, use e.g.
   `go test ./internal/parse/...`.