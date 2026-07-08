package syncserver

import (
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"strings"
)

const bearerPrefix = "Bearer "

// authEqual reports whether a and b are equal, in constant time regardless of
// length (hashing both to a fixed 32 bytes first, so a length mismatch alone
// leaks nothing beyond what comparing the hashes already does).
func authEqual(a, b string) bool {
	ah := sha256.Sum256([]byte(a))
	bh := sha256.Sum256([]byte(b))
	return subtle.ConstantTimeCompare(ah[:], bh[:]) == 1
}

// requireKey wraps next with static-bearer-token auth. A missing/incorrect
// token gets 401 before next ever runs (in particular, before any per-repo
// store is opened).
func (s *Server) requireKey(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		h := r.Header.Get("Authorization")
		token, ok := strings.CutPrefix(h, bearerPrefix)
		if !ok || !authEqual(token, s.key) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}
