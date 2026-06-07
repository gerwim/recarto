# `recarto update` Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a `recarto update` command that performs a signed, verified in-place upgrade or downgrade of the running binary, pulling releases from a public Cloudflare R2 bucket (`releases.gerwim.com/recarto/`).

**Architecture:** A new `internal/update` package holds pure, interface-isolated units — manifest parse/select, sha256 + minisign verification, an HTTP fetcher, and a `minio/selfupdate` apply wrapper — composed by an `Updater`. A thin `internal/cli/update.go` adds the command (flags, TTY version picker, exit codes), dispatched from `main.go` before the store opens. A `tools/release` program (used by a new `.gitlab-ci.yml` publish stage) computes hashes and merges the manifest; CI signs it with `minisign` and uploads to R2.

**Tech Stack:** Go 1.26, stdlib `flag`/`net/http`, `aead.dev/minisign` (verify + test-side sign), `github.com/minio/selfupdate` (in-place swap), `golang.org/x/mod/semver` (version compare). All pure-Go, no new cgo.

---

## File structure

| File | Responsibility |
|------|----------------|
| `internal/update/manifest.go` | Manifest/Release/Artifact types, `Parse`, `Marshal`, platform key, version/artifact selection, semver compare, `IsReleaseBuild` |
| `internal/update/merge.go` | `MergeManifest` — insert/replace a version, keep sorted newest-first, set `latest` (used by CI) |
| `internal/update/verify.go` | `VerifySHA256`, `VerifyManifestSignature` (minisign) |
| `internal/update/pubkey.go` | Embedded production minisign public key constant |
| `internal/update/fetch.go` | `Fetcher` interface, `HTTPFetcher`, base-URL resolution (`RECARTO_UPDATE_BASE_URL` override) |
| `internal/update/apply.go` | `Applier` interface + `SelfUpdateApplier` wrapping `minio/selfupdate` |
| `internal/update/update.go` | `Updater` — `Load`, `Check`, `InstallVersion`; ties fetch+verify+select+apply together |
| `internal/cli/update.go` | `Update` command: flag parsing, version picker, `--check`/`--list`/`--version`/`--yes`, `ErrUpdateAvailable` |
| `tools/release/main.go` | CI helper: hash built binaries, merge into index.json, write it |
| `.gitlab-ci.yml` | Publish stage (build → hash → merge → minisign → upload to R2) |
| `cmd/recarto/main.go` | Dispatch `update` with custom exit codes |
| `internal/cli/cli.go` | Add `update` to usage text |
| `README.md` | Document the command + release process |

Each `*.go` gets a sibling `*_test.go`. Tests follow the existing `internal/cli/*_test.go` style (`bytes.Buffer`, table tests).

---

## Task 1: Scaffold `internal/update` + manifest types and Parse/Marshal

**Files:**
- Create: `internal/update/manifest.go`
- Test: `internal/update/manifest_test.go`
- Modify: `go.mod`, `go.sum` (via `go get`)

- [ ] **Step 1: Add dependencies**

Run:
```bash
cd /home/claude/Projects/recarto-feature-auto-updater
go get aead.dev/minisign@latest github.com/minio/selfupdate@latest golang.org/x/mod@latest
```
Expected: `go.mod` gains the three modules (minisign/selfupdate as direct; x/mod promoted to direct).

- [ ] **Step 2: Write the failing test**

Create `internal/update/manifest_test.go`:
```go
package update

import "testing"

const goldenManifest = `{
  "schema": 1,
  "package": "recarto",
  "latest": "1.4.0",
  "versions": [
    {"version":"1.4.0","released":"2026-06-07T12:00:00Z","artifacts":{
      "linux-amd64":{"path":"recarto/1.4.0/recarto-linux-amd64","sha256":"aa","size":10},
      "darwin-arm64":{"path":"recarto/1.4.0/recarto-darwin-arm64","sha256":"bb","size":11},
      "windows-amd64":{"path":"recarto/1.4.0/recarto-windows-amd64.exe","sha256":"cc","size":12}}},
    {"version":"1.3.0","released":"2026-06-01T12:00:00Z","artifacts":{
      "linux-amd64":{"path":"recarto/1.3.0/recarto-linux-amd64","sha256":"dd","size":9}}}
  ]
}`

func TestParseGolden(t *testing.T) {
	m, err := Parse([]byte(goldenManifest))
	if err != nil {
		t.Fatal(err)
	}
	if m.Package != "recarto" || m.Latest != "1.4.0" {
		t.Fatalf("bad header: %+v", m)
	}
	if len(m.Versions) != 2 {
		t.Fatalf("want 2 versions, got %d", len(m.Versions))
	}
	a, ok := m.Versions[0].Artifacts["linux-amd64"]
	if !ok || a.SHA256 != "aa" || a.Size != 10 {
		t.Fatalf("bad artifact: %+v", a)
	}
}

func TestParseRejectsUnknownSchema(t *testing.T) {
	if _, err := Parse([]byte(`{"schema":2,"package":"recarto","latest":"1.0.0","versions":[{"version":"1.0.0"}]}`)); err == nil {
		t.Fatal("expected error for unknown schema")
	}
}

func TestParseRejectsEmpty(t *testing.T) {
	if _, err := Parse([]byte(`{"schema":1,"package":"recarto","latest":"","versions":[]}`)); err == nil {
		t.Fatal("expected error for empty manifest")
	}
}

func TestMarshalRoundTrip(t *testing.T) {
	m, err := Parse([]byte(goldenManifest))
	if err != nil {
		t.Fatal(err)
	}
	b, err := m.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	m2, err := Parse(b)
	if err != nil {
		t.Fatalf("re-parse failed: %v", err)
	}
	if m2.Latest != m.Latest || len(m2.Versions) != len(m.Versions) {
		t.Fatal("round-trip mismatch")
	}
}
```

- [ ] **Step 3: Run test to verify it fails**

Run: `go test ./internal/update/ -run TestParse -v`
Expected: FAIL — `undefined: Parse`.

- [ ] **Step 4: Write minimal implementation**

Create `internal/update/manifest.go`:
```go
// Package update implements recarto's signed in-place self-updater: it fetches a
// minisign-signed release manifest from the public distribution endpoint, verifies
// it, and swaps the running binary for a chosen version.
package update

import (
	"encoding/json"
	"fmt"
)

// SchemaVersion is the manifest schema this build understands. A manifest with a
// different schema is rejected so old binaries fail closed against future formats.
const SchemaVersion = 1

// Artifact is one platform's binary within a release.
type Artifact struct {
	Path   string `json:"path"`   // R2 path relative to the base URL, e.g. recarto/1.4.0/recarto-linux-amd64
	SHA256 string `json:"sha256"` // lowercase hex sha256 of the binary
	Size   int64  `json:"size"`
}

// Release is a single published version and its per-platform artifacts, keyed by
// "<goos>-<goarch>" (e.g. "linux-amd64").
type Release struct {
	Version   string              `json:"version"`
	Released  string              `json:"released"`
	Artifacts map[string]Artifact `json:"artifacts"`
}

// Manifest is the signed index of all published releases.
type Manifest struct {
	Schema   int       `json:"schema"`
	Package  string    `json:"package"`
	Latest   string    `json:"latest"`
	Versions []Release `json:"versions"`
}

// Parse decodes and validates a manifest. It rejects an unknown schema, a missing
// package/latest, or an empty version list — all fail-closed.
func Parse(data []byte) (*Manifest, error) {
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse manifest: %w", err)
	}
	if m.Schema != SchemaVersion {
		return nil, fmt.Errorf("unsupported manifest schema %d (this build understands %d; reinstall recarto)", m.Schema, SchemaVersion)
	}
	if m.Package == "" || m.Latest == "" {
		return nil, fmt.Errorf("manifest missing package or latest")
	}
	if len(m.Versions) == 0 {
		return nil, fmt.Errorf("manifest has no versions")
	}
	return &m, nil
}

// Marshal renders the manifest as indented JSON (the exact bytes CI signs/uploads).
func (m *Manifest) Marshal() ([]byte, error) {
	return json.MarshalIndent(m, "", "  ")
}
```

- [ ] **Step 5: Run test to verify it passes**

Run: `go test ./internal/update/ -run 'TestParse|TestMarshal' -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add go.mod go.sum internal/update/manifest.go internal/update/manifest_test.go
git commit -m "feat(update): manifest types with parse/marshal and schema validation"
```

---

## Task 2: Platform key, version/artifact selection, semver compare, release-build detection

**Files:**
- Modify: `internal/update/manifest.go`
- Test: `internal/update/manifest_test.go`

- [ ] **Step 1: Write the failing test**

Append to `internal/update/manifest_test.go`:
```go
func TestPlatformKey(t *testing.T) {
	if got := platformKey("linux", "amd64"); got != "linux-amd64" {
		t.Fatalf("got %q", got)
	}
	if got := platformKey("windows", "amd64"); got != "windows-amd64" {
		t.Fatalf("got %q", got)
	}
}

func TestFindAndLatest(t *testing.T) {
	m, _ := Parse([]byte(goldenManifest))
	if _, ok := m.Find("1.3.0"); !ok {
		t.Fatal("1.3.0 should be found")
	}
	if _, ok := m.Find("9.9.9"); ok {
		t.Fatal("9.9.9 should not be found")
	}
	r, ok := m.LatestRelease()
	if !ok || r.Version != "1.4.0" {
		t.Fatalf("latest release wrong: %+v", r)
	}
}

func TestArtifactForUnsupportedPlatform(t *testing.T) {
	m, _ := Parse([]byte(goldenManifest))
	r, _ := m.Find("1.3.0") // only has linux-amd64
	if _, ok := r.ArtifactFor("darwin-arm64"); ok {
		t.Fatal("darwin-arm64 should be missing in 1.3.0")
	}
	if _, ok := r.ArtifactFor("linux-amd64"); !ok {
		t.Fatal("linux-amd64 should be present in 1.3.0")
	}
}

func TestCompareVersions(t *testing.T) {
	if CompareVersions("1.4.0", "1.3.0") <= 0 {
		t.Fatal("1.4.0 > 1.3.0")
	}
	if CompareVersions("1.3.0", "1.3.0") != 0 {
		t.Fatal("equal")
	}
	// leading-v and prerelease tolerated
	if CompareVersions("v1.4.0", "1.4.0-dev") <= 0 {
		t.Fatal("release > prerelease")
	}
}

func TestSortedVersionsNewestFirst(t *testing.T) {
	m, _ := Parse([]byte(goldenManifest))
	m.Versions = []Release{{Version: "1.3.0"}, {Version: "1.4.0"}} // deliberately out of order
	got := m.SortedVersions()
	if got[0].Version != "1.4.0" || got[1].Version != "1.3.0" {
		t.Fatalf("not sorted newest-first: %+v", got)
	}
}

func TestIsReleaseBuild(t *testing.T) {
	cases := map[string]bool{"1.4.0": true, "v1.4.0": true, "0.1.0-dev": false, "1.4.0-dirty": false, "garbage": false}
	for v, want := range cases {
		if IsReleaseBuild(v) != want {
			t.Fatalf("IsReleaseBuild(%q)=%v want %v", v, IsReleaseBuild(v), want)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/update/ -run 'TestPlatform|TestFind|TestArtifact|TestCompare|TestSorted|TestIsRelease' -v`
Expected: FAIL — `undefined: platformKey` etc.

- [ ] **Step 3: Write minimal implementation**

Append to `internal/update/manifest.go`:
```go
import (
	"runtime"
	"sort"
	"strings"

	"golang.org/x/mod/semver"
)
// (merge these into the existing import block; keep encoding/json and fmt.)

// CurrentPlatform is the "<goos>-<goarch>" key for the running binary.
func CurrentPlatform() string { return platformKey(runtime.GOOS, runtime.GOARCH) }

func platformKey(goos, goarch string) string { return goos + "-" + goarch }

// Find returns the release with the exact version (with or without a leading "v").
func (m *Manifest) Find(version string) (*Release, bool) {
	want := normalizeSemver(version)
	for i := range m.Versions {
		if normalizeSemver(m.Versions[i].Version) == want {
			return &m.Versions[i], true
		}
	}
	return nil, false
}

// LatestRelease returns the release named by the manifest's Latest field.
func (m *Manifest) LatestRelease() (*Release, bool) { return m.Find(m.Latest) }

// ArtifactFor returns the artifact for a "<goos>-<goarch>" platform key.
func (r *Release) ArtifactFor(platform string) (Artifact, bool) {
	a, ok := r.Artifacts[platform]
	return a, ok
}

// normalizeSemver ensures a leading "v" so golang.org/x/mod/semver accepts it.
func normalizeSemver(v string) string {
	if v == "" {
		return v
	}
	if !strings.HasPrefix(v, "v") {
		return "v" + v
	}
	return v
}

// CompareVersions orders two versions by semver (-1, 0, +1). Invalid versions
// sort below valid ones, then lexically, so a "dev"/"garbage" current build is
// always considered older than any real release.
func CompareVersions(a, b string) int {
	na, nb := normalizeSemver(a), normalizeSemver(b)
	va, vb := semver.IsValid(na), semver.IsValid(nb)
	switch {
	case va && vb:
		return semver.Compare(na, nb)
	case va:
		return 1
	case vb:
		return -1
	default:
		return strings.Compare(a, b)
	}
}

// SortedVersions returns releases newest-first without mutating the manifest.
func (m *Manifest) SortedVersions() []Release {
	out := make([]Release, len(m.Versions))
	copy(out, m.Versions)
	sort.SliceStable(out, func(i, j int) bool {
		return CompareVersions(out[i].Version, out[j].Version) > 0
	})
	return out
}

// IsReleaseBuild reports whether v is a clean release version (MAJOR.MINOR.PATCH,
// no prerelease or build metadata). Dev/dirty/unparseable builds return false.
func IsReleaseBuild(v string) bool {
	n := normalizeSemver(v)
	return semver.IsValid(n) && semver.Prerelease(n) == "" && semver.Build(n) == "" && semver.Canonical(n) == n
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/update/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/update/manifest.go internal/update/manifest_test.go
git commit -m "feat(update): platform key, version selection, semver compare, release-build detection"
```

---

## Task 3: sha256 verification

**Files:**
- Create: `internal/update/verify.go`
- Test: `internal/update/verify_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/update/verify_test.go`:
```go
package update

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

func TestVerifySHA256(t *testing.T) {
	data := []byte("hello recarto")
	sum := sha256.Sum256(data)
	hexsum := hex.EncodeToString(sum[:])

	if err := VerifySHA256(data, hexsum); err != nil {
		t.Fatalf("matching hash should verify: %v", err)
	}
	if err := VerifySHA256(data, "deadbeef"); err == nil {
		t.Fatal("mismatched hash must fail")
	}
	if err := VerifySHA256([]byte("tampered"), hexsum); err == nil {
		t.Fatal("tampered data must fail")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/update/ -run TestVerifySHA256 -v`
Expected: FAIL — `undefined: VerifySHA256`.

- [ ] **Step 3: Write minimal implementation**

Create `internal/update/verify.go`:
```go
package update

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// VerifySHA256 checks data against a lowercase-hex sha256 digest, failing closed.
func VerifySHA256(data []byte, wantHex string) error {
	sum := sha256.Sum256(data)
	got := hex.EncodeToString(sum[:])
	if !strings.EqualFold(got, wantHex) {
		return fmt.Errorf("sha256 mismatch: manifest=%s actual=%s", wantHex, got)
	}
	return nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/update/ -run TestVerifySHA256 -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/update/verify.go internal/update/verify_test.go
git commit -m "feat(update): sha256 binary verification"
```

---

## Task 4: minisign manifest-signature verification

**Files:**
- Modify: `internal/update/verify.go`
- Test: `internal/update/verify_test.go`

> Note: `aead.dev/minisign` exposes `GenerateKey(rand)`, `Sign(priv, msg)`, `Verify(pub, msg, sig)`, and `DecodePublicKey(string)`. If a signature differs at compile time, run `go doc aead.dev/minisign` to confirm and adjust. The test generates its own throwaway keypair, so it never depends on the production key.

- [ ] **Step 1: Write the failing test**

Append to `internal/update/verify_test.go`:
```go
import (
	"crypto/rand"

	"aead.dev/minisign"
)
// (merge into the existing import block)

func TestVerifyManifestSignature(t *testing.T) {
	pub, priv, err := minisign.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	msg := []byte(goldenManifest)
	sig := minisign.Sign(priv, msg)

	if err := VerifyManifestSignature(pub.String(), msg, sig); err != nil {
		t.Fatalf("valid signature should verify: %v", err)
	}
	// tampered manifest
	if err := VerifyManifestSignature(pub.String(), []byte("tampered"), sig); err == nil {
		t.Fatal("tampered manifest must fail")
	}
	// wrong key
	otherPub, _, _ := minisign.GenerateKey(rand.Reader)
	if err := VerifyManifestSignature(otherPub.String(), msg, sig); err == nil {
		t.Fatal("wrong key must fail")
	}
	// malformed public key
	if err := VerifyManifestSignature("not-a-key", msg, sig); err == nil {
		t.Fatal("malformed key must fail")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/update/ -run TestVerifyManifestSignature -v`
Expected: FAIL — `undefined: VerifyManifestSignature`.

- [ ] **Step 3: Write minimal implementation**

Append to `internal/update/verify.go`:
```go
import "aead.dev/minisign"
// (merge into the existing import block)

// VerifyManifestSignature verifies a detached minisign signature over the manifest
// bytes using the given armored public key. Any failure (bad key, bad signature,
// tampered bytes) returns an error so the caller fails closed.
func VerifyManifestSignature(publicKey string, manifest, signature []byte) error {
	pub, err := minisign.DecodePublicKey(publicKey)
	if err != nil {
		return fmt.Errorf("decode public key: %w", err)
	}
	if !minisign.Verify(pub, manifest, signature) {
		return fmt.Errorf("manifest signature verification failed")
	}
	return nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/update/ -run TestVerifyManifestSignature -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/update/verify.go internal/update/verify_test.go
git commit -m "feat(update): minisign manifest-signature verification"
```

---

## Task 5: Embedded production public key

**Files:**
- Create: `internal/update/pubkey.go`
- Test: `internal/update/pubkey_test.go`

- [ ] **Step 1: Generate the production keypair (one-time, keep the secret offline)**

Run:
```bash
minisign -G -p recarto-minisign.pub -s recarto-minisign.key
cat recarto-minisign.pub
```
The public file's second line is the armored public key (starts with `RW`). Keep `recarto-minisign.key` **out of git** — it goes into a masked GitLab CI variable later (Task 11). Add the local key files to ignore:
```bash
printf 'recarto-minisign.key\nrecarto-minisign.pub\n' >> .gitignore
```

- [ ] **Step 2: Write the failing test**

Create `internal/update/pubkey_test.go`:
```go
package update

import (
	"strings"
	"testing"

	"aead.dev/minisign"
)

func TestEmbeddedPublicKeyDecodes(t *testing.T) {
	if !strings.HasPrefix(ManifestPublicKey, "RW") {
		t.Fatalf("ManifestPublicKey not set to a real minisign key: %q", ManifestPublicKey)
	}
	if _, err := minisign.DecodePublicKey(ManifestPublicKey); err != nil {
		t.Fatalf("embedded public key does not decode: %v", err)
	}
}
```

- [ ] **Step 3: Run test to verify it fails**

Run: `go test ./internal/update/ -run TestEmbeddedPublicKey -v`
Expected: FAIL — `undefined: ManifestPublicKey`.

- [ ] **Step 4: Write the implementation with the real key**

Create `internal/update/pubkey.go`, pasting the armored key line printed in Step 1 (the line beginning `RW`):
```go
package update

// ManifestPublicKey is the minisign public key that signs the release manifest.
// The matching secret key lives only in CI (masked variable RECARTO_MINISIGN_KEY)
// and offline — never in this repo. Rotating it requires shipping a new binary.
const ManifestPublicKey = "RWQ_PASTE_THE_REAL_KEY_LINE_FROM_STEP_1_HERE"
```
Replace the placeholder string with the actual `RW...` line. (This is the only value in the plan you must fill from a generated artifact; the test in Step 2 fails until it is a real, decodable key.)

- [ ] **Step 5: Run test to verify it passes**

Run: `go test ./internal/update/ -run TestEmbeddedPublicKey -v`
Expected: PASS.

- [ ] **Step 6: Commit (public key only — never the secret)**

```bash
git add internal/update/pubkey.go internal/update/pubkey_test.go .gitignore
git commit -m "feat(update): embed minisign manifest public key"
```

---

## Task 6: HTTP fetcher with base-URL override

**Files:**
- Create: `internal/update/fetch.go`
- Test: `internal/update/fetch_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/update/fetch_test.go`:
```go
package update

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHTTPFetcher(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/recarto/index.json":
			io.WriteString(w, goldenManifest)
		case "/recarto/index.json.minisig":
			io.WriteString(w, "SIGDATA")
		case "/recarto/1.4.0/recarto-linux-amd64":
			io.WriteString(w, "BINARY")
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	f := &HTTPFetcher{BaseURL: srv.URL + "/", Client: srv.Client()}
	ctx := context.Background()

	man, sig, err := f.Manifest(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if string(man) != goldenManifest || string(sig) != "SIGDATA" {
		t.Fatalf("manifest/sig wrong: %q %q", man, sig)
	}

	rc, err := f.Binary(ctx, "recarto/1.4.0/recarto-linux-amd64")
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	b, _ := io.ReadAll(rc)
	if string(b) != "BINARY" {
		t.Fatalf("binary body wrong: %q", b)
	}

	if _, err := f.Binary(ctx, "recarto/nope"); err == nil {
		t.Fatal("404 must error")
	}
}

func TestBaseURLOverride(t *testing.T) {
	t.Setenv("RECARTO_UPDATE_BASE_URL", "https://example.test/x/")
	if got := BaseURL(); got != "https://example.test/x/" {
		t.Fatalf("override ignored: %q", got)
	}
	t.Setenv("RECARTO_UPDATE_BASE_URL", "")
	if got := BaseURL(); got != DefaultBaseURL {
		t.Fatalf("default wrong: %q", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/update/ -run 'TestHTTPFetcher|TestBaseURLOverride' -v`
Expected: FAIL — `undefined: HTTPFetcher`.

- [ ] **Step 3: Write minimal implementation**

Create `internal/update/fetch.go`:
```go
package update

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// DefaultBaseURL is the public distribution root. Artifact paths in the manifest
// are joined onto this. Overridable via RECARTO_UPDATE_BASE_URL (used by tests).
const DefaultBaseURL = "https://releases.gerwim.com/"

const manifestPath = "recarto/index.json"

// BaseURL returns the distribution root, honoring the RECARTO_UPDATE_BASE_URL
// override. The returned value always ends in "/".
func BaseURL() string {
	if v := os.Getenv("RECARTO_UPDATE_BASE_URL"); v != "" {
		if !strings.HasSuffix(v, "/") {
			v += "/"
		}
		return v
	}
	return DefaultBaseURL
}

// Fetcher retrieves the signed manifest and individual binaries.
type Fetcher interface {
	Manifest(ctx context.Context) (manifest, signature []byte, err error)
	Binary(ctx context.Context, path string) (io.ReadCloser, error)
}

// HTTPFetcher fetches over HTTPS from BaseURL.
type HTTPFetcher struct {
	BaseURL string
	Client  *http.Client
}

// NewHTTPFetcher builds a fetcher against the resolved BaseURL with a sane timeout.
func NewHTTPFetcher() *HTTPFetcher {
	return &HTTPFetcher{BaseURL: BaseURL(), Client: &http.Client{Timeout: 60 * time.Second}}
}

func (f *HTTPFetcher) get(ctx context.Context, path string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.BaseURL+path, nil)
	if err != nil {
		return nil, err
	}
	resp, err := f.Client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("GET %s: %s", path, resp.Status)
	}
	return resp, nil
}

// Manifest downloads index.json and its detached signature.
func (f *HTTPFetcher) Manifest(ctx context.Context) ([]byte, []byte, error) {
	mResp, err := f.get(ctx, manifestPath)
	if err != nil {
		return nil, nil, err
	}
	defer mResp.Body.Close()
	manifest, err := io.ReadAll(mResp.Body)
	if err != nil {
		return nil, nil, err
	}
	sResp, err := f.get(ctx, manifestPath+".minisig")
	if err != nil {
		return nil, nil, err
	}
	defer sResp.Body.Close()
	sig, err := io.ReadAll(sResp.Body)
	if err != nil {
		return nil, nil, err
	}
	return manifest, sig, nil
}

// Binary streams the binary at the given manifest-relative path. Caller closes it.
func (f *HTTPFetcher) Binary(ctx context.Context, path string) (io.ReadCloser, error) {
	resp, err := f.get(ctx, path)
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/update/ -run 'TestHTTPFetcher|TestBaseURLOverride' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/update/fetch.go internal/update/fetch_test.go
git commit -m "feat(update): http fetcher with base-url override"
```

---

## Task 7: Apply wrapper around `minio/selfupdate`

**Files:**
- Create: `internal/update/apply.go`
- Test: `internal/update/apply_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/update/apply_test.go`. This exercises the real swap by pointing `selfupdate` at a throwaway target file in a temp dir (not the test binary itself):
```go
package update

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSelfUpdateApplierReplacesTarget(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "recarto-fake")
	if err := os.WriteFile(target, []byte("OLD"), 0o755); err != nil {
		t.Fatal(err)
	}
	a := SelfUpdateApplier{TargetPath: target}
	if err := a.Apply(strings.NewReader("NEW")); err != nil {
		t.Fatalf("apply failed: %v", err)
	}
	got, _ := os.ReadFile(target)
	if string(got) != "NEW" {
		t.Fatalf("target not replaced: %q", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/update/ -run TestSelfUpdateApplier -v`
Expected: FAIL — `undefined: SelfUpdateApplier`.

- [ ] **Step 3: Write minimal implementation**

Create `internal/update/apply.go`:
```go
package update

import (
	"fmt"
	"io"

	"github.com/minio/selfupdate"
)

// Applier swaps the running (or target) binary for the bytes read from r.
type Applier interface {
	Apply(r io.Reader) error
}

// SelfUpdateApplier performs an atomic in-place replacement via minio/selfupdate,
// which handles same-filesystem rename, permission preservation, the Windows
// can't-replace-a-running-exe dance, and rollback on a failed apply. TargetPath
// is empty for the real command (defaults to the running executable); tests set
// it to a throwaway file.
type SelfUpdateApplier struct {
	TargetPath string
}

func (a SelfUpdateApplier) Apply(r io.Reader) error {
	err := selfupdate.Apply(r, selfupdate.Options{TargetPath: a.TargetPath})
	if err != nil {
		if rerr := selfupdate.RollbackError(err); rerr != nil {
			return fmt.Errorf("apply failed and rollback also failed: %v (rollback: %v)", err, rerr)
		}
		return fmt.Errorf("apply update: %w", err)
	}
	return nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/update/ -run TestSelfUpdateApplier -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/update/apply.go internal/update/apply_test.go
git commit -m "feat(update): minio/selfupdate apply wrapper"
```

---

## Task 8: `Updater` orchestration (Load, Check, InstallVersion)

**Files:**
- Create: `internal/update/update.go`
- Test: `internal/update/update_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/update/update_test.go`. It uses in-test fakes plus a real test keypair so the whole verify→apply chain runs without network:
```go
package update

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"testing"

	"aead.dev/minisign"
)

// signedManifest returns a manifest whose single linux-amd64 artifact has the
// correct sha256 for binaryBody, plus a valid signature and the armored pubkey.
func signedManifest(t *testing.T, binaryBody string) (man, sig []byte, pub string) {
	t.Helper()
	sum := sha256.Sum256([]byte(binaryBody))
	m := &Manifest{
		Schema: 1, Package: "recarto", Latest: "1.4.0",
		Versions: []Release{
			{Version: "1.4.0", Artifacts: map[string]Artifact{
				"linux-amd64": {Path: "recarto/1.4.0/recarto-linux-amd64", SHA256: hex.EncodeToString(sum[:]), Size: int64(len(binaryBody))},
			}},
			{Version: "1.3.0", Artifacts: map[string]Artifact{
				"linux-amd64": {Path: "recarto/1.3.0/recarto-linux-amd64", SHA256: "00", Size: 1},
			}},
		},
	}
	mb, err := m.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	pk, sk, err := minisign.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return mb, minisign.Sign(sk, mb), pk.String()
}

type fakeFetcher struct {
	man, sig []byte
	bodies   map[string]string
}

func (f *fakeFetcher) Manifest(context.Context) ([]byte, []byte, error) { return f.man, f.sig, nil }
func (f *fakeFetcher) Binary(_ context.Context, path string) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewBufferString(f.bodies[path])), nil
}

type fakeApplier struct{ got []byte }

func (a *fakeApplier) Apply(r io.Reader) error {
	b, _ := io.ReadAll(r)
	a.got = b
	return nil
}

func newTestUpdater(t *testing.T, current, body string) (*Updater, *fakeApplier) {
	man, sig, pub := signedManifest(t, body)
	app := &fakeApplier{}
	u := &Updater{
		Current:  current,
		Platform: "linux-amd64",
		PubKey:   pub,
		Fetcher:  &fakeFetcher{man: man, sig: sig, bodies: map[string]string{"recarto/1.4.0/recarto-linux-amd64": body}},
		Applier:  app,
	}
	return u, app
}

func TestCheckReportsUpdateAvailable(t *testing.T) {
	u, _ := newTestUpdater(t, "1.3.0", "NEWBIN")
	st, err := u.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.UpToDate || st.Latest != "1.4.0" {
		t.Fatalf("expected update available: %+v", st)
	}
}

func TestCheckUpToDate(t *testing.T) {
	u, _ := newTestUpdater(t, "1.4.0", "NEWBIN")
	st, err := u.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !st.UpToDate {
		t.Fatalf("expected up-to-date: %+v", st)
	}
}

func TestInstallVersionVerifiesAndApplies(t *testing.T) {
	u, app := newTestUpdater(t, "1.3.0", "NEWBIN")
	if err := u.InstallVersion(context.Background(), "1.4.0"); err != nil {
		t.Fatal(err)
	}
	if string(app.got) != "NEWBIN" {
		t.Fatalf("applier received wrong bytes: %q", app.got)
	}
}

func TestInstallVersionRejectsTamperedBinary(t *testing.T) {
	u, app := newTestUpdater(t, "1.3.0", "NEWBIN")
	// Point the fetcher at a body that does not match the manifest sha256.
	u.Fetcher.(*fakeFetcher).bodies["recarto/1.4.0/recarto-linux-amd64"] = "TAMPERED"
	if err := u.InstallVersion(context.Background(), "1.4.0"); err == nil {
		t.Fatal("tampered binary must be rejected")
	}
	if app.got != nil {
		t.Fatal("applier must not run on sha256 mismatch")
	}
}

func TestInstallVersionRejectsTamperedManifest(t *testing.T) {
	u, _ := newTestUpdater(t, "1.3.0", "NEWBIN")
	u.Fetcher.(*fakeFetcher).man = append(u.Fetcher.(*fakeFetcher).man, ' ') // break the signature
	if err := u.InstallVersion(context.Background(), "1.4.0"); err == nil {
		t.Fatal("tampered manifest must be rejected")
	}
}

func TestInstallVersionUnsupportedPlatform(t *testing.T) {
	u, _ := newTestUpdater(t, "1.3.0", "NEWBIN")
	u.Platform = "linux-arm64" // not in the manifest
	if err := u.InstallVersion(context.Background(), "1.4.0"); err == nil {
		t.Fatal("unsupported platform must error")
	}
}

func TestInstallVersionUnknownVersion(t *testing.T) {
	u, _ := newTestUpdater(t, "1.3.0", "NEWBIN")
	if err := u.InstallVersion(context.Background(), "9.9.9"); err == nil {
		t.Fatal("unknown version must error")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/update/ -run 'TestCheck|TestInstallVersion' -v`
Expected: FAIL — `undefined: Updater`.

- [ ] **Step 3: Write minimal implementation**

Create `internal/update/update.go`:
```go
package update

import (
	"context"
	"fmt"
	"io"
)

// Updater composes fetch, verify, select, and apply into the self-update flow.
type Updater struct {
	Current  string // running binary's version (from internal/version.Version)
	Platform string // "<goos>-<goarch>"; defaults to CurrentPlatform() when empty
	PubKey   string // armored minisign public key (ManifestPublicKey in production)
	Fetcher  Fetcher
	Applier  Applier
}

// Status summarizes the current state relative to the latest release.
type Status struct {
	Current  string
	Latest   string
	UpToDate bool
	Manifest *Manifest
}

func (u *Updater) platform() string {
	if u.Platform != "" {
		return u.Platform
	}
	return CurrentPlatform()
}

// Load fetches the manifest and its signature, verifies the signature against the
// embedded public key, and parses it. A failure here means we never trust the
// contents — fail closed.
func (u *Updater) Load(ctx context.Context) (*Manifest, error) {
	man, sig, err := u.Fetcher.Manifest(ctx)
	if err != nil {
		return nil, fmt.Errorf("fetch manifest: %w", err)
	}
	if err := VerifyManifestSignature(u.PubKey, man, sig); err != nil {
		return nil, err
	}
	return Parse(man)
}

// Check loads the manifest and compares the latest release to the current build.
func (u *Updater) Check(ctx context.Context) (*Status, error) {
	m, err := u.Load(ctx)
	if err != nil {
		return nil, err
	}
	st := &Status{Current: u.Current, Latest: m.Latest, Manifest: m}
	st.UpToDate = IsReleaseBuild(u.Current) && CompareVersions(u.Current, m.Latest) >= 0
	return st, nil
}

// InstallVersion downloads the given version for this platform, verifies its
// sha256 against the (already signature-verified) manifest, and applies it.
func (u *Updater) InstallVersion(ctx context.Context, version string) error {
	m, err := u.Load(ctx)
	if err != nil {
		return err
	}
	rel, ok := m.Find(version)
	if !ok {
		return fmt.Errorf("version %s is not available", version)
	}
	art, ok := rel.ArtifactFor(u.platform())
	if !ok {
		return fmt.Errorf("no %s build for version %s", u.platform(), version)
	}
	rc, err := u.Fetcher.Binary(ctx, art.Path)
	if err != nil {
		return fmt.Errorf("download binary: %w", err)
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		return fmt.Errorf("read binary: %w", err)
	}
	if err := VerifySHA256(data, art.SHA256); err != nil {
		return err
	}
	return u.Applier.Apply(bytesReader(data))
}

// bytesReader is a tiny indirection kept separate so the apply input is obvious.
func bytesReader(b []byte) io.Reader { return &byteReader{b: b} }

type byteReader struct {
	b []byte
	i int
}

func (r *byteReader) Read(p []byte) (int, error) {
	if r.i >= len(r.b) {
		return 0, io.EOF
	}
	n := copy(p, r.b[r.i:])
	r.i += n
	return n, nil
}
```

> Note: `bytesReader` could be `bytes.NewReader`; it is spelled out here only to avoid adding an import in this step. Replacing it with `bytes.NewReader(data)` (and importing `bytes`) is equivalent and preferable — do that if you are already importing `bytes`.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/update/ -run 'TestCheck|TestInstallVersion' -v`
Expected: PASS.

- [ ] **Step 5: Simplify the reader (optional cleanup) and re-run all package tests**

Replace `bytesReader(data)` with `bytes.NewReader(data)`, add `"bytes"` to imports, delete `bytesReader`/`byteReader`.
Run: `go test ./internal/update/ -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/update/update.go internal/update/update_test.go
git commit -m "feat(update): Updater orchestration (load, check, install with verification)"
```

---

## Task 9: `MergeManifest` (used by the publish tool)

**Files:**
- Create: `internal/update/merge.go`
- Test: `internal/update/merge_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/update/merge_test.go`:
```go
package update

import "testing"

func TestMergeManifestInsertsNewest(t *testing.T) {
	base := &Manifest{Schema: 1, Package: "recarto", Latest: "1.3.0", Versions: []Release{
		{Version: "1.3.0", Artifacts: map[string]Artifact{"linux-amd64": {Path: "p", SHA256: "x", Size: 1}}},
	}}
	rel := Release{Version: "1.4.0", Released: "2026-06-07T00:00:00Z",
		Artifacts: map[string]Artifact{"linux-amd64": {Path: "q", SHA256: "y", Size: 2}}}
	out := MergeManifest(base, rel)
	if out.Latest != "1.4.0" {
		t.Fatalf("latest not bumped: %s", out.Latest)
	}
	if out.Versions[0].Version != "1.4.0" {
		t.Fatalf("newest not first: %+v", out.Versions)
	}
	if len(out.Versions) != 2 {
		t.Fatalf("want 2 versions: %d", len(out.Versions))
	}
}

func TestMergeManifestReplacesSameVersion(t *testing.T) {
	base := &Manifest{Schema: 1, Package: "recarto", Latest: "1.4.0", Versions: []Release{
		{Version: "1.4.0", Artifacts: map[string]Artifact{"linux-amd64": {SHA256: "old"}}},
	}}
	rel := Release{Version: "1.4.0", Artifacts: map[string]Artifact{"linux-amd64": {SHA256: "new"}}}
	out := MergeManifest(base, rel)
	if len(out.Versions) != 1 || out.Versions[0].Artifacts["linux-amd64"].SHA256 != "new" {
		t.Fatalf("did not replace existing version: %+v", out.Versions)
	}
}

func TestMergeManifestNilBase(t *testing.T) {
	rel := Release{Version: "1.0.0", Artifacts: map[string]Artifact{"linux-amd64": {SHA256: "z"}}}
	out := MergeManifest(nil, rel)
	if out.Schema != SchemaVersion || out.Package != "recarto" || out.Latest != "1.0.0" || len(out.Versions) != 1 {
		t.Fatalf("nil-base merge wrong: %+v", out)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/update/ -run TestMergeManifest -v`
Expected: FAIL — `undefined: MergeManifest`.

- [ ] **Step 3: Write minimal implementation**

Create `internal/update/merge.go`:
```go
package update

// MergeManifest returns a manifest with rel inserted (or replacing an existing
// entry of the same version), versions sorted newest-first, and Latest set to the
// highest version. A nil base starts a fresh recarto manifest. The input is not
// mutated.
func MergeManifest(base *Manifest, rel Release) *Manifest {
	out := &Manifest{Schema: SchemaVersion, Package: "recarto"}
	if base != nil {
		out.Package = base.Package
		for _, v := range base.Versions {
			if normalizeSemver(v.Version) != normalizeSemver(rel.Version) {
				out.Versions = append(out.Versions, v)
			}
		}
	}
	out.Versions = append(out.Versions, rel)
	out.Versions = out.SortedVersions()
	if len(out.Versions) > 0 {
		out.Latest = out.Versions[0].Version
	}
	return out
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/update/ -run TestMergeManifest -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/update/merge.go internal/update/merge_test.go
git commit -m "feat(update): MergeManifest for the publish pipeline"
```

---

## Task 10: `recarto update` CLI command

**Files:**
- Create: `internal/cli/update.go`
- Test: `internal/cli/update_test.go`
- Modify: `internal/cli/cli.go` (usage text), `cmd/recarto/main.go` (dispatch + exit codes)

- [ ] **Step 1: Write the failing test**

Create `internal/cli/update_test.go`. The selection logic is the testable core; it is pure over a manifest + flags + TTY + stdin:
```go
package cli

import (
	"bytes"
	"strings"
	"testing"

	"git.gerwim.nl/gerwim/recarto/internal/update"
)

func testManifest() *update.Manifest {
	return &update.Manifest{Schema: 1, Package: "recarto", Latest: "1.4.0", Versions: []update.Release{
		{Version: "1.4.0", Artifacts: map[string]update.Artifact{"linux-amd64": {Path: "p140"}}},
		{Version: "1.3.0", Artifacts: map[string]update.Artifact{"linux-amd64": {Path: "p130"}}},
	}}
}

func TestSelectTargetExplicitVersion(t *testing.T) {
	var out bytes.Buffer
	v, proceed, err := selectTarget(testManifest(), "1.3.0", "1.3.0", false, strings.NewReader(""), &out)
	if err != nil || !proceed || v != "1.3.0" {
		t.Fatalf("explicit version: v=%q proceed=%v err=%v", v, proceed, err)
	}
}

func TestSelectTargetExplicitUnknownVersion(t *testing.T) {
	var out bytes.Buffer
	if _, _, err := selectTarget(testManifest(), "1.3.0", "9.9.9", false, strings.NewReader(""), &out); err == nil {
		t.Fatal("unknown explicit version must error")
	}
}

func TestSelectTargetNonTTYPicksLatest(t *testing.T) {
	var out bytes.Buffer
	v, proceed, err := selectTarget(testManifest(), "1.3.0", "", false, strings.NewReader(""), &out)
	if err != nil || !proceed || v != "1.4.0" {
		t.Fatalf("non-tty latest: v=%q proceed=%v err=%v", v, proceed, err)
	}
}

func TestSelectTargetTTYEnterPicksLatest(t *testing.T) {
	var out bytes.Buffer
	v, proceed, err := selectTarget(testManifest(), "1.3.0", "", true, strings.NewReader("\n"), &out)
	if err != nil || !proceed || v != "1.4.0" {
		t.Fatalf("tty enter: v=%q proceed=%v err=%v", v, proceed, err)
	}
}

func TestSelectTargetTTYPicksOlder(t *testing.T) {
	var out bytes.Buffer
	// menu is numbered newest-first: 1) 1.4.0  2) 1.3.0
	v, proceed, err := selectTarget(testManifest(), "1.4.0", "", true, strings.NewReader("2\n"), &out)
	if err != nil || !proceed || v != "1.3.0" {
		t.Fatalf("tty pick older: v=%q proceed=%v err=%v", v, proceed, err)
	}
}

func TestSelectTargetTTYQuitDoesNotProceed(t *testing.T) {
	var out bytes.Buffer
	_, proceed, err := selectTarget(testManifest(), "1.4.0", "", true, strings.NewReader("q\n"), &out)
	if err != nil || proceed {
		t.Fatalf("quit should not proceed: proceed=%v err=%v", proceed, err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/cli/ -run TestSelectTarget -v`
Expected: FAIL — `undefined: selectTarget`.

- [ ] **Step 3: Write the command and selection logic**

Create `internal/cli/update.go`:
```go
package cli

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/mattn/go-isatty"
	"os"

	"git.gerwim.nl/gerwim/recarto/internal/update"
	"git.gerwim.nl/gerwim/recarto/internal/version"
)

// ErrUpdateAvailable is returned by `recarto update --check` when a newer release
// exists. main.go maps it to a distinct exit code (10) so scripts can detect it.
var ErrUpdateAvailable = errors.New("update available")

// Update implements `recarto update`. stdin/isTTY are injected for testability;
// main.go passes os.Stdin and the real terminal check.
func Update(out io.Writer, stdin io.Reader, isTTY bool, args []string) error {
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	fs.SetOutput(out)
	wantVersion := fs.String("version", "", "install a specific version (downgrade/pin)")
	check := fs.Bool("check", false, "report whether an update is available; make no changes")
	list := fs.Bool("list", false, "list available versions; make no changes")
	yes := fs.Bool("yes", false, "skip confirmation prompts")
	fs.BoolVar(yes, "y", false, "skip confirmation prompts (shorthand)")
	if helped, err := parseFlags(fs, args); helped || err != nil {
		return err
	}

	u := &update.Updater{
		Current:  version.Version,
		Platform: update.CurrentPlatform(),
		PubKey:   update.ManifestPublicKey,
		Fetcher:  update.NewHTTPFetcher(),
		Applier:  update.SelfUpdateApplier{}, // empty TargetPath => running binary
	}
	ctx := context.Background()

	st, err := u.Check(ctx)
	if err != nil {
		return err
	}

	if *list {
		printVersions(out, st)
		return nil
	}
	if *check {
		if st.UpToDate {
			fmt.Fprintf(out, "recarto %s is up to date.\n", st.Current)
			return nil
		}
		fmt.Fprintf(out, "update available: %s -> %s\n", st.Current, st.Latest)
		return ErrUpdateAvailable
	}

	target, proceed, err := selectTarget(st.Manifest, st.Current, *wantVersion, isTTY && !*yes, stdin, out)
	if err != nil {
		return err
	}
	if !proceed {
		fmt.Fprintln(out, "no changes made.")
		return nil
	}
	if !update.IsReleaseBuild(st.Current) {
		fmt.Fprintf(out, "note: current build %q is not a release build.\n", st.Current)
	}
	fmt.Fprintf(out, "installing recarto %s ...\n", target)
	if err := u.InstallVersion(ctx, target); err != nil {
		return updateApplyHint(err)
	}
	fmt.Fprintf(out, "recarto is now %s.\n", target)
	return nil
}

// selectTarget decides which version to install. An explicit flagVersion wins (and
// must exist). Otherwise non-interactive picks Latest; interactive shows a numbered
// menu (newest-first, Enter = latest, q = cancel).
func selectTarget(m *update.Manifest, current, flagVersion string, interactive bool, stdin io.Reader, out io.Writer) (string, bool, error) {
	versions := m.SortedVersions()
	if flagVersion != "" {
		if _, ok := m.Find(flagVersion); !ok {
			return "", false, fmt.Errorf("version %s is not available (try: recarto update --list)", flagVersion)
		}
		return flagVersion, true, nil
	}
	if !interactive {
		return m.Latest, true, nil
	}
	fmt.Fprintf(out, "current: %s   latest: %s\n", current, m.Latest)
	fmt.Fprintln(out, "select a version to install (Enter = latest, q = cancel):")
	for i, r := range versions {
		marker := ""
		if r.Version == m.Latest {
			marker = "  (latest)"
		}
		if r.Version == current {
			marker += "  (current)"
		}
		fmt.Fprintf(out, "  %d) %s%s\n", i+1, r.Version, marker)
	}
	fmt.Fprint(out, "> ")
	line, _ := bufio.NewReader(stdin).ReadString('\n')
	line = strings.TrimSpace(line)
	switch {
	case line == "":
		return m.Latest, true, nil
	case line == "q" || line == "Q":
		return "", false, nil
	}
	n, err := strconv.Atoi(line)
	if err != nil || n < 1 || n > len(versions) {
		return "", false, fmt.Errorf("invalid selection %q", line)
	}
	return versions[n-1].Version, true, nil
}

func printVersions(out io.Writer, st *update.Status) {
	fmt.Fprintf(out, "current: %s   latest: %s\n", st.Current, st.Latest)
	for _, r := range st.Manifest.SortedVersions() {
		marker := ""
		if r.Version == st.Latest {
			marker = "  (latest)"
		}
		if r.Version == st.Current {
			marker += "  (current)"
		}
		fmt.Fprintf(out, "  %s%s\n", r.Version, marker)
	}
}

// updateApplyHint adds an actionable hint when the swap likely failed due to file
// permissions on the installed binary.
func updateApplyHint(err error) error {
	if errors.Is(err, os.ErrPermission) || strings.Contains(err.Error(), "permission denied") {
		return fmt.Errorf("%w\nhint: the recarto binary is not writable by this user; re-run with elevated privileges or reinstall", err)
	}
	return err
}
```

- [ ] **Step 4: Run the selection tests to verify they pass**

Run: `go test ./internal/cli/ -run TestSelectTarget -v`
Expected: PASS.

- [ ] **Step 5: Add `update` to the usage text**

In `internal/cli/cli.go`, inside `usage`, add a line after the `snapshot` line:
```go
	fmt.Fprintln(w, "  update            self-update to the latest (or a chosen) release (--version --list --check --yes)")
```

- [ ] **Step 6: Dispatch `update` from main.go with custom exit codes**

In `cmd/recarto/main.go`, add a case to the `switch os.Args[1]` block (alongside `version`/`init`), and import nothing new beyond what is present (`cli`, `isatty`, `os`, `errors`):
```go
	case "update":
		isTTY := isatty.IsTerminal(os.Stdin.Fd())
		err := cli.Update(os.Stdout, os.Stdin, isTTY, os.Args[2:])
		if errors.Is(err, cli.ErrUpdateAvailable) {
			os.Exit(10)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "recarto:", err)
			os.Exit(1)
		}
```
Add `"errors"` to the import block in `main.go` if not already present.

- [ ] **Step 7: Build and run the whole suite**

Run:
```bash
go build ./... && go vet ./... && go test ./...
```
Expected: build clean, vet clean, all tests PASS.

- [ ] **Step 8: Manual smoke test against a fake server (no real R2 needed)**

Run:
```bash
go run ./cmd/recarto update --check ; echo "exit=$?"
```
Expected: with no `RECARTO_UPDATE_BASE_URL` set and no network to releases.gerwim.com, it errors on fetch (exit 1) — that is fine here; the goal is that the command wires up and parses flags. (A full end-to-end check happens once R2 is populated in Task 11.)

- [ ] **Step 9: Commit**

```bash
git add internal/cli/update.go internal/cli/update_test.go internal/cli/cli.go cmd/recarto/main.go
git commit -m "feat(cli): recarto update command with version picker and --check/--list"
```

---

## Task 11: `tools/release` publish helper

**Files:**
- Create: `tools/release/main.go`
- Test: `tools/release/main_test.go`

The program reads the current `index.json` (or starts fresh), hashes each built binary, builds a `Release`, merges it, and writes the new `index.json`. CI then signs and uploads.

- [ ] **Step 1: Write the failing test**

Create `tools/release/main_test.go`:
```go
package main

import (
	"os"
	"path/filepath"
	"testing"

	"git.gerwim.nl/gerwim/recarto/internal/update"
)

func TestBuildReleaseHashesBinaries(t *testing.T) {
	dir := t.TempDir()
	lin := filepath.Join(dir, "linux")
	if err := os.WriteFile(lin, []byte("LINUXBIN"), 0o755); err != nil {
		t.Fatal(err)
	}
	rel, err := buildRelease("1.5.0", "2026-06-07T00:00:00Z", []artifactInput{
		{Platform: "linux-amd64", File: lin, RPath: "recarto/1.5.0/recarto-linux-amd64"},
	})
	if err != nil {
		t.Fatal(err)
	}
	a := rel.Artifacts["linux-amd64"]
	if a.Path != "recarto/1.5.0/recarto-linux-amd64" || a.Size != 8 {
		t.Fatalf("bad artifact: %+v", a)
	}
	if err := update.VerifySHA256([]byte("LINUXBIN"), a.SHA256); err != nil {
		t.Fatalf("hash wrong: %v", err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./tools/release/ -v`
Expected: FAIL — `undefined: buildRelease`.

- [ ] **Step 3: Write the implementation**

Create `tools/release/main.go`:
```go
// Command release builds/merges the recarto distribution manifest for CI. It
// hashes the freshly built binaries, inserts a new version into the existing
// index.json (fetched from R2 beforehand, or absent on first release), and writes
// the updated index.json. CI signs it with minisign and uploads it to R2.
//
// Usage:
//
//	release -version 1.5.0 -released 2026-06-07T12:00:00Z \
//	  -in index.json -out index.json \
//	  -artifact linux-amd64=releases/linux-amd64/recarto:recarto/1.5.0/recarto-linux-amd64 \
//	  -artifact darwin-arm64=releases/darwin-arm64/recarto:recarto/1.5.0/recarto-darwin-arm64 \
//	  -artifact windows-amd64=releases/windows-amd64/recarto.exe:recarto/1.5.0/recarto-windows-amd64.exe
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"strings"

	"git.gerwim.nl/gerwim/recarto/internal/update"
)

type artifactInput struct {
	Platform string // linux-amd64
	File     string // local path to the built binary
	RPath    string // path under the R2 base, recorded in the manifest
}

type artifactFlags []artifactInput

func (a *artifactFlags) String() string { return "" }
func (a *artifactFlags) Set(v string) error {
	// platform=localfile:rpath
	eq := strings.IndexByte(v, '=')
	colon := strings.LastIndexByte(v, ':')
	if eq < 0 || colon < 0 || colon < eq {
		return fmt.Errorf("artifact must be platform=localfile:rpath, got %q", v)
	}
	*a = append(*a, artifactInput{Platform: v[:eq], File: v[eq+1 : colon], RPath: v[colon+1:]})
	return nil
}

func buildRelease(version, released string, inputs []artifactInput) (update.Release, error) {
	rel := update.Release{Version: version, Released: released, Artifacts: map[string]update.Artifact{}}
	for _, in := range inputs {
		data, err := os.ReadFile(in.File)
		if err != nil {
			return update.Release{}, fmt.Errorf("read %s: %w", in.File, err)
		}
		sum := sha256.Sum256(data)
		rel.Artifacts[in.Platform] = update.Artifact{
			Path:   in.RPath,
			SHA256: hex.EncodeToString(sum[:]),
			Size:   int64(len(data)),
		}
	}
	return rel, nil
}

func loadManifest(path string) (*update.Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // first release
		}
		return nil, err
	}
	return update.Parse(data)
}

func main() {
	var (
		version  = flag.String("version", "", "release version, e.g. 1.5.0")
		released = flag.String("released", "", "RFC3339 release timestamp")
		in       = flag.String("in", "", "existing index.json (optional)")
		out      = flag.String("out", "index.json", "output index.json path")
		arts     artifactFlags
	)
	flag.Var(&arts, "artifact", "platform=localfile:rpath (repeatable)")
	flag.Parse()

	if *version == "" || *released == "" || len(arts) == 0 {
		fmt.Fprintln(os.Stderr, "release: -version, -released and at least one -artifact are required")
		os.Exit(2)
	}

	base, err := loadManifest(*in)
	if err != nil {
		fmt.Fprintln(os.Stderr, "release:", err)
		os.Exit(1)
	}
	rel, err := buildRelease(*version, *released, arts)
	if err != nil {
		fmt.Fprintln(os.Stderr, "release:", err)
		os.Exit(1)
	}
	merged := update.MergeManifest(base, rel)
	data, err := merged.Marshal()
	if err != nil {
		fmt.Fprintln(os.Stderr, "release:", err)
		os.Exit(1)
	}
	if err := os.WriteFile(*out, append(data, '\n'), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "release:", err)
		os.Exit(1)
	}
	fmt.Printf("wrote %s (latest=%s, %d versions)\n", *out, merged.Latest, len(merged.Versions))
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./tools/release/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add tools/release/main.go tools/release/main_test.go
git commit -m "feat(release): manifest publish helper that hashes binaries and merges index.json"
```

---

## Task 12: GitLab CI publish stage

**Files:**
- Create: `.gitlab-ci.yml`

> Prerequisites configured in the GitLab project (Settings → CI/CD → Variables), all **masked**: `RECARTO_MINISIGN_KEY` (contents of `recarto-minisign.key` from Task 5), `RECARTO_MINISIGN_PASSWORD` (its passphrase), `R2_ACCESS_KEY_ID`, `R2_SECRET_ACCESS_KEY`, `R2_ACCOUNT_ID`, `R2_BUCKET`. The CI image must provide `minisign`, `zig`, Go 1.26, and the AWS CLI (or `rclone`).

- [ ] **Step 1: Write the publish pipeline**

Create `.gitlab-ci.yml`:
```yaml
stages: [test, build, publish]

variables:
  R2_ENDPOINT: "https://${R2_ACCOUNT_ID}.r2.cloudflarestorage.com"

test:
  stage: test
  image: golang:1.26
  script:
    - go vet ./...
    - go test ./...

# Cross-compile the three release binaries (same toolchain as the Makefile).
build:
  stage: build
  image: golang:1.26
  rules:
    - if: '$CI_COMMIT_TAG'
  before_script:
    - apt-get update && apt-get install -y --no-install-recommends curl xz-utils
    - curl -sSL https://ziglang.org/download/0.13.0/zig-linux-x86_64-0.13.0.tar.xz | tar -xJ -C /opt
    - export PATH="/opt/zig-linux-x86_64-0.13.0:$PATH"
  script:
    - make all VERSION="$CI_COMMIT_TAG"
  artifacts:
    paths:
      - releases/
    expire_in: 1 hour

# Hash binaries, merge index.json, sign with minisign, upload everything to R2.
publish:
  stage: publish
  image: golang:1.26
  rules:
    - if: '$CI_COMMIT_TAG'
  needs: [build]
  before_script:
    - apt-get update && apt-get install -y --no-install-recommends minisign awscli ca-certificates
  script:
    - export VER="$CI_COMMIT_TAG"
    - export NOW="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
    # Pull the current manifest if it exists (ignore failure on first release).
    - aws s3 cp "s3://${R2_BUCKET}/recarto/index.json" index.json --endpoint-url "$R2_ENDPOINT" || true
    # Build the merged manifest from the freshly built binaries.
    - >
      go run ./tools/release
      -version "$VER" -released "$NOW"
      -in index.json -out index.json
      -artifact "linux-amd64=releases/linux-amd64/recarto:recarto/${VER}/recarto-linux-amd64"
      -artifact "darwin-arm64=releases/darwin-arm64/recarto:recarto/${VER}/recarto-darwin-arm64"
      -artifact "windows-amd64=releases/windows-amd64/recarto.exe:recarto/${VER}/recarto-windows-amd64.exe"
    # Sign the manifest (non-interactive passphrase).
    - echo "$RECARTO_MINISIGN_KEY" > minisign.key
    - minisign -S -s minisign.key -m index.json <<< "$RECARTO_MINISIGN_PASSWORD"
    - rm -f minisign.key
    # Upload immutable versioned binaries (long cache), then the manifest (short cache).
    - aws s3 cp releases/linux-amd64/recarto   "s3://${R2_BUCKET}/recarto/${VER}/recarto-linux-amd64"     --endpoint-url "$R2_ENDPOINT" --cache-control "public,max-age=31536000,immutable"
    - aws s3 cp releases/darwin-arm64/recarto  "s3://${R2_BUCKET}/recarto/${VER}/recarto-darwin-arm64"    --endpoint-url "$R2_ENDPOINT" --cache-control "public,max-age=31536000,immutable"
    - aws s3 cp releases/windows-amd64/recarto.exe "s3://${R2_BUCKET}/recarto/${VER}/recarto-windows-amd64.exe" --endpoint-url "$R2_ENDPOINT" --cache-control "public,max-age=31536000,immutable"
    - aws s3 cp index.json         "s3://${R2_BUCKET}/recarto/index.json"         --endpoint-url "$R2_ENDPOINT" --cache-control "public,max-age=60"
    - aws s3 cp index.json.minisig "s3://${R2_BUCKET}/recarto/index.json.minisig" --endpoint-url "$R2_ENDPOINT" --cache-control "public,max-age=60"
```

> The `minisign -S ... <<< "$PASSWORD"` form feeds the passphrase on stdin. If the CI image's minisign build does not read the passphrase from stdin, generate the key with `-W` (no passphrase) in Task 5 and drop `RECARTO_MINISIGN_PASSWORD` — the key still lives only in the masked CI variable.

- [ ] **Step 2: Validate the YAML locally**

Run:
```bash
python3 -c "import yaml,sys; yaml.safe_load(open('.gitlab-ci.yml')); print('yaml ok')"
```
Expected: `yaml ok`.

- [ ] **Step 3: Commit**

```bash
git add .gitlab-ci.yml
git commit -m "ci(release): publish signed manifest and binaries to R2 on tag"
```

- [ ] **Step 4: First real release (manual verification, after merge)**

After this branch is merged and the CI variables are set:
```bash
git tag 0.2.0 && git push origin 0.2.0
```
Then verify the artifacts are public and the client round-trips:
```bash
curl -fsS https://releases.gerwim.com/recarto/index.json | head
RECARTO_UPDATE_BASE_URL=https://releases.gerwim.com/ go run ./cmd/recarto update --check ; echo "exit=$?"
```
Expected: manifest downloads; `--check` reports up-to-date (exit 0) or available (exit 10) depending on the local build's version.

---

## Task 13: Documentation

**Files:**
- Modify: `README.md`

- [ ] **Step 1: Document the command and release flow**

Add a `## Updating` section to `README.md`:
```markdown
## Updating

`recarto update` upgrades (or downgrades) the binary in place from
`https://releases.gerwim.com/recarto/`. Releases are signed with minisign; the
public key is embedded in the binary, so a download is applied only if its
signature and sha256 verify.

- `recarto update` — pick the latest (default) or, interactively, an older version.
- `recarto update --version X.Y.Z` — install a specific version (downgrade/pin).
- `recarto update --list` — list available versions.
- `recarto update --check` — report whether an update exists (exit 0 = current, 10 = available); makes no changes.
- `recarto update --yes` — skip prompts (always installs the latest).

### Releasing

Tag a commit (`git tag X.Y.Z && git push origin X.Y.Z`). GitLab CI cross-compiles
the three targets, computes sha256s, merges `index.json`, signs it with minisign,
and uploads the binaries + manifest + signature to the R2 bucket behind
`releases.gerwim.com`. The minisign secret key and R2 credentials live only in
masked CI variables.
```

- [ ] **Step 2: Commit**

```bash
git add README.md
git commit -m "docs: document recarto update and the release process"
```

---

## Self-review notes

- **Spec coverage:** R2 layout (Tasks 11–12), signed manifest + trust chain (Tasks 1,4,5,8), `internal/update` units Fetcher/Verifier/Manifest/Applier (Tasks 1–8), full version history + downgrade (Tasks 2,10), CLI flags incl. `--check` exit code (Task 10), dev-build note / unsupported-platform / unwritable-path handling (Tasks 8,10), CI publish with minisign-in-image (Tasks 11–12), tests-first throughout, README (Task 13). All spec sections map to a task.
- **Placeholder scan:** the only fill-in value is the real minisign public key in Task 5 Step 4 — gated by a test that fails until it is a decodable key, and called out explicitly. No `TODO`/`TBD`/"add error handling" steps; every code step shows complete code.
- **Type consistency:** `Manifest`/`Release`/`Artifact`, `Fetcher`/`Applier`, `Updater{Current,Platform,PubKey,Fetcher,Applier}`, `Status{Current,Latest,UpToDate,Manifest}`, `CurrentPlatform`, `CompareVersions`, `IsReleaseBuild`, `MergeManifest`, `VerifySHA256`, `VerifyManifestSignature`, `ManifestPublicKey`, `BaseURL`/`DefaultBaseURL`, `selectTarget`, `ErrUpdateAvailable` are used identically across tasks.
