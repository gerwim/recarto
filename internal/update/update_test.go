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
	// Flip a byte in a copy so the tamper unambiguously lands (not reliant on
	// slice capacity), invalidating the signature.
	man := u.Fetcher.(*fakeFetcher).man
	tampered := make([]byte, len(man))
	copy(tampered, man)
	tampered[0] = '}'
	u.Fetcher.(*fakeFetcher).man = tampered
	if err := u.InstallVersion(context.Background(), "1.4.0"); err == nil {
		t.Fatal("tampered manifest must be rejected")
	}
}

func TestCheckDevBuildAheadIsNotUpToDate(t *testing.T) {
	// A dev build numerically ahead of the latest release must still be offered
	// the release: the IsReleaseBuild guard (not version compare) decides this.
	u, _ := newTestUpdater(t, "1.5.0-dev", "NEWBIN") // latest in manifest is 1.4.0
	st, err := u.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.UpToDate {
		t.Fatal("dev build ahead of latest must not report UpToDate")
	}
}

func TestInstallVersionUnsupportedPlatform(t *testing.T) {
	u, _ := newTestUpdater(t, "1.3.0", "NEWBIN")
	u.Platform = "linux-arm64"
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
