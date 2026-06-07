package update

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"aead.dev/minisign"
)

// VerifyManifestSignature verifies a detached minisign signature over the manifest
// bytes using the given armored public key. Any failure (bad key, bad signature,
// tampered bytes) returns an error so the caller fails closed.
func VerifyManifestSignature(publicKey string, manifest, signature []byte) error {
	var pub minisign.PublicKey
	if err := pub.UnmarshalText([]byte(publicKey)); err != nil {
		return fmt.Errorf("decode public key: %w", err)
	}
	if !minisign.Verify(pub, manifest, signature) {
		return fmt.Errorf("manifest signature verification failed")
	}
	return nil
}

// VerifySHA256 checks data against a lowercase-hex sha256 digest, failing closed.
func VerifySHA256(data []byte, wantHex string) error {
	sum := sha256.Sum256(data)
	got := hex.EncodeToString(sum[:])
	if !strings.EqualFold(got, wantHex) {
		return fmt.Errorf("sha256 mismatch: manifest=%s actual=%s", wantHex, got)
	}
	return nil
}
