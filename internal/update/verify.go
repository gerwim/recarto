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
