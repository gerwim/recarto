// Package ids generates time-ordered unique identifiers.
package ids

import "github.com/google/uuid"

// New returns a time-ordered UUIDv7 as a 36-char string.
// UUIDv7 values sort lexicographically by creation time, which gives the
// SQLite primary-key index good locality and lets us order records by id.
func New() string {
	return uuid.Must(uuid.NewV7()).String()
}
