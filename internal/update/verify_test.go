package update

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"aead.dev/minisign"
)

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
	if err := VerifyManifestSignature(pub.String(), []byte("tampered"), sig); err == nil {
		t.Fatal("tampered manifest must fail")
	}
	otherPub, _, _ := minisign.GenerateKey(rand.Reader)
	if err := VerifyManifestSignature(otherPub.String(), msg, sig); err == nil {
		t.Fatal("wrong key must fail")
	}
	if err := VerifyManifestSignature("not-a-key", msg, sig); err == nil {
		t.Fatal("malformed key must fail")
	}
}

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
