package update

import (
	"bytes"
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
		return nil, fmt.Errorf("verify manifest signature: %w", err)
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
	// Load re-fetches and re-verifies the manifest; callers cannot inject an
	// unverified one, so the signature gate cannot be bypassed.
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
	return u.Applier.Apply(bytes.NewReader(data))
}
