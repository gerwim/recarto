// Package clock provides an injectable time source for deterministic tests.
package clock

import "time"

// Clock is a source of the current time.
type Clock interface {
	Now() time.Time
}

// Real is the production clock backed by time.Now.
type Real struct{}

// Now returns the current wall-clock time.
func (Real) Now() time.Time { return time.Now() }

// Fake is a controllable clock for tests.
type Fake struct{ t time.Time }

// NewFake returns a Fake clock fixed at t.
func NewFake(t time.Time) *Fake { return &Fake{t: t} }

// Now returns the Fake clock's current time.
func (f *Fake) Now() time.Time { return f.t }

// Advance moves the Fake clock forward by d.
func (f *Fake) Advance(d time.Duration) { f.t = f.t.Add(d) }
