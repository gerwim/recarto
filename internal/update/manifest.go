// Package update implements recarto's signed in-place self-updater: it fetches a
// minisign-signed release manifest from the public distribution endpoint, verifies
// it, and swaps the running binary for a chosen version.
package update

import (
	"encoding/json"
	"fmt"
	"runtime"
	"sort"
	"strings"

	"golang.org/x/mod/semver"
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
