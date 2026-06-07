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
	// aead.dev/minisign v0.3.0 has no DecodePublicKey; decode via UnmarshalText.
	var pub minisign.PublicKey
	if err := pub.UnmarshalText([]byte(ManifestPublicKey)); err != nil {
		t.Fatalf("embedded public key does not decode: %v", err)
	}
}
