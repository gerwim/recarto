package remotesync

import "testing"

// A nil *Syncer means "sync unconfigured" by contract (app.App.Sync); asking
// it for a poll period must return the default, never dereference (the 1.2.0
// serve SIGSEGV hit exactly this).
func TestIntervalNilReceiver(t *testing.T) {
	var s *Syncer
	if got := s.Interval(); got != DefaultInterval {
		t.Fatalf("nil Syncer Interval = %v, want %v", got, DefaultInterval)
	}
}
