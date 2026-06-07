# `recarto update` — Self-Update Command Design

**Date:** 2026-06-07
**Branch:** feature/auto-updater
**Status:** Approved (design); implementation plan to follow

## Summary

Add a `recarto update` command that performs an in-place upgrade (or downgrade)
of the running `recarto` binary, pulling signed releases from a public
Cloudflare R2 bucket fronted by `releases.gerwim.com`. The private GitLab CI
remains the builder/publisher; the public R2 endpoint is the only thing clients
read, anonymously over HTTPS.

## Context

- Go CLI, module `git.gerwim.nl/gerwim/recarto`, self-hosted GitLab at
  `git.gerwim.nl` (private).
- Builds three static targets via `zig cc`: `linux/amd64` (musl),
  `darwin/arm64`, `windows/amd64`. Version is stamped at build time via
  `-ldflags "-X .../internal/version.Version=<v>"` (`internal/version/version.go`).
- Subcommands live in `internal/cli/*.go`, dispatched from
  `cmd/recarto/main.go` via `cli.Run`.

### Decisions locked during brainstorming

1. **Audience:** public distribution, but the GitLab instance stays private —
   so a public-facing channel separate from GitLab is required.
2. **Hosting:** Cloudflare R2 bound to `releases.gerwim.com`, with a per-package
   prefix (`/recarto/...`) so multiple tools can share the bucket later.
3. **Integrity:** minisign signature on the manifest **plus** per-binary sha256.
   Fail closed on any mismatch.
4. **Version selection:** the user can choose the latest release **or** an older
   version (downgrade). The manifest therefore lists full version history.
5. **CI tooling:** the GitLab CI image ships the `minisign` binary; signing runs
   in CI.

## Distribution layout (R2: `releases.gerwim.com/recarto/`)

Immutable versioned artifacts plus one small, signed, mutable manifest:

```
recarto/
  index.json              # manifest — overwritten each release, short cache TTL
  index.json.minisig      # detached minisign signature of index.json
  1.4.0/
    recarto-linux-amd64
    recarto-darwin-arm64
    recarto-windows-amd64.exe
  1.3.0/
    ...
```

- Binaries under `<version>/` are write-once and cache-forever.
- Only `index.json` and `index.json.minisig` are overwritten per release.
- The `/recarto/` prefix keeps the bucket multi-package.

## Manifest format & trust chain

`index.json` lists every released version, newest first, so the client can offer
downgrades without extra round-trips:

```json
{
  "schema": 1,
  "package": "recarto",
  "latest": "1.4.0",
  "versions": [
    {
      "version": "1.4.0",
      "released": "2026-06-07T12:00:00Z",
      "artifacts": {
        "linux-amd64":   { "path": "recarto/1.4.0/recarto-linux-amd64",     "sha256": "…", "size": 12345 },
        "darwin-arm64":  { "path": "recarto/1.4.0/recarto-darwin-arm64",    "sha256": "…", "size": 12345 },
        "windows-amd64": { "path": "recarto/1.4.0/recarto-windows-amd64.exe","sha256": "…", "size": 12345 }
      }
    },
    { "version": "1.3.0", "released": "…", "artifacts": { "…": "…" } }
  ]
}
```

- `schema` guards forward-compatibility; the client refuses unknown major
  schema versions with a clear "please update via reinstall" message.
- `artifacts` is keyed by `<os>-<arch>` matching `runtime.GOOS`/`runtime.GOARCH`.
- `path` is relative to the base URL (`https://releases.gerwim.com/`); the
  client joins base + path. Base URL is a build constant, overridable via
  `RECARTO_UPDATE_BASE_URL` for tests.

**Trust chain:** embedded minisign public key → verifies `index.json.minisig`
over `index.json` → manifest is authentic → manifest's per-artifact `sha256` →
verifies the downloaded binary. One signature (on the manifest) covers every
binary. A fully compromised R2/domain cannot forge an update without the offline
minisign secret key.

## Client — new `internal/update` package

Pure, narrowly-interfaced units, each independently testable:

- **`Fetcher`** — GETs `index.json` + `index.json.minisig` from the base URL.
  Behind an interface so tests use an in-memory fake; no network in unit tests.
- **`Verifier`** — minisign-verifies the manifest with the embedded public key
  (`aead.dev/minisign`, pure-Go, no cgo), then sha256-verifies a binary against
  the manifest entry. Fails closed.
- **`Manifest` logic** — parse; select the artifact for the running
  `GOOS/GOARCH`; semver-sort and compare versions (`golang.org/x/mod/semver`);
  resolve "latest" vs. a requested version; detect unsupported platforms.
- **`Applier`** — wraps `github.com/minio/selfupdate` `Apply` for the atomic
  in-place swap (same-filesystem rename, permission preservation, the Windows
  can't-replace-a-running-exe rename dance, rollback on failed apply).

HTTP, filesystem, and selfupdate calls all sit behind interfaces so the core
logic is exercised with fakes and golden manifests.

## CLI — `recarto update`

New `internal/cli/update.go` subcommand, dispatched like the existing ones.

- `recarto update` — fetch manifest; on a TTY with no `--version`, show a short
  numbered list (latest is the default — press Enter) so the user can pick the
  latest **or** an older version to downgrade; confirm; apply. Non-TTY ⇒ latest
  automatically.
- `recarto update --version X.Y.Z` — install that exact version (downgrade or
  pin); confirm unless `--yes`.
- `recarto update --list` — print available versions, marking *current* and
  *latest*; make no changes.
- `recarto update --check` — report only; exit 0 if already current, nonzero if
  an update is available (script-friendly); make no changes.
- `recarto update --yes` / `-y` — skip prompts.

### Behaviors & edge cases

- **Dev builds** (`0.1.0-dev`, `-dirty`, or otherwise unparseable as semver):
  allowed, but the client notes you are leaving a non-release build and still
  confirms before swapping.
- **Unsupported platform** (an `<os>-<arch>` the project does not build, e.g.
  `linux-arm64`): clear error, no swap.
- **Unwritable install path** (e.g. `/usr/local/bin/recarto` without
  privileges): caught from the apply layer; actionable message (re-run with
  privileges / reinstall) rather than a partial swap.
- **Already on the selected version:** no-op with a friendly message (unless a
  different `--version` is explicitly requested, allowing reinstall).

## CI publish job (private GitLab, on tag)

A `publish` stage after the existing cross-compile, gated on tag pipelines:

1. Build the three binaries (existing `make`/zig steps).
2. Compute sha256 for each.
3. Fetch the current `index.json` from R2, merge the new version in (newest
   first), and render the updated manifest.
4. Sign the manifest with `minisign -S` (the CI image ships `minisign`).
5. Upload artifacts + `index.json` + `index.json.minisig` to R2 via the S3 API
   (`aws s3 cp --endpoint-url …` or `rclone`).

**Secrets** as masked CI variables: R2 access key/secret, minisign secret key
and its password. The minisign **public** key is committed to the repo and
embedded in the binary as a constant.

## Testing (test-first)

- Manifest parse/select/semver-sort — table tests including malformed JSON,
  missing platform entry, unknown schema, and downgrade selection.
- Signature verification — golden manifest plus a real `.minisig` signed by a
  test keypair; tampered manifest and wrong-key cases must fail closed.
- sha256 mismatch ⇒ refuse to apply.
- Apply layer — a fake applier asserts only verified bytes reach it; the real
  `minio/selfupdate` swap covered by a focused integration test in a temp dir.
- CLI — flag parsing, `--check` exit codes, `--list` output, non-TTY
  latest-by-default behavior — following the existing `internal/cli/*_test.go`
  patterns.

## New dependencies

- `aead.dev/minisign` — manifest signature verification (pure-Go, no cgo).
- `github.com/minio/selfupdate` — the in-place binary swap primitive.
- `golang.org/x/mod/semver` — version comparison/sorting.

All small and pure-Go; no new cgo surface.

## Out of scope (YAGNI)

- Release channels (beta/nightly) — single stable channel only.
- Background "update available" nudges at startup.
- Auto-update without explicit user invocation.
- Manifest history cap — keep full history for now; revisit if it ever grows
  unwieldy (binaries stay in R2 regardless).
