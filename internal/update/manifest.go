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
