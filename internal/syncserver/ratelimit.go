package syncserver

import (
	"net"
	"net/http"
	"sync"
	"time"
)

// rateLimiter is a minimal per-IP token bucket. It exists to blunt naive
// brute-force guessing of the single global bearer key and basic DoS against
// the sync server, without pulling in a dependency for something this small.
// It is deliberately simple ("basic per-IP rate limit" per the design doc),
// not a full-featured limiter (no distributed state, no per-route tuning).
type rateLimiter struct {
	rate  float64 // tokens replenished per second
	burst float64 // bucket capacity (also the initial token count)

	mu      sync.Mutex
	buckets map[string]*tokenBucket
}

type tokenBucket struct {
	tokens float64
	last   time.Time
}

func newRateLimiter(rate, burst float64) *rateLimiter {
	return &rateLimiter{rate: rate, burst: burst, buckets: map[string]*tokenBucket{}}
}

// allow reports whether key may proceed now, consuming one token if so.
func (rl *rateLimiter) allow(key string) bool {
	now := time.Now()
	rl.mu.Lock()
	defer rl.mu.Unlock()
	b, ok := rl.buckets[key]
	if !ok {
		b = &tokenBucket{tokens: rl.burst, last: now}
		rl.buckets[key] = b
	} else if elapsed := now.Sub(b.last).Seconds(); elapsed > 0 {
		b.tokens = min(rl.burst, b.tokens+elapsed*rl.rate)
		b.last = now
	}
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// clientIP extracts the request's remote IP (stripping the port), falling
// back to the raw RemoteAddr when it isn't a host:port pair (e.g. a unix
// socket in tests).
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// rateLimit wraps next so requests are throttled per client IP. It runs
// BEFORE auth (see Handler()), so it also throttles repeated wrong-key
// guesses against the static bearer token, not just authenticated traffic.
func (s *Server) rateLimit(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.limiter.allow(clientIP(r)) {
			http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
			return
		}
		next(w, r)
	}
}
