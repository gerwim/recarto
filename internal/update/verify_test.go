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
