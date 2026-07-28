package store

import (
	"errors"
	"strings"

	sqlitedrv "modernc.org/sqlite"
)

// SQLite primary result codes we treat as "the file is damaged, retrying will
// not help — rebuild it". Extended codes carry the primary code in their low
// byte (e.g. 267 = SQLITE_CORRUPT_VTAB, the classic FTS5 write-path failure;
// 523 = SQLITE_CORRUPT_SEQUENCE; 779 = SQLITE_CORRUPT_INDEX), so classification
// masks with 0xff.
const (
	sqliteCorrupt = 11 // SQLITE_CORRUPT — "database disk image is malformed"
	sqliteNotADB  = 26 // SQLITE_NOTADB  — "file is not a database"
)

// corruptMessages are matched against an error's text only when the driver's
// structured result code is unavailable — a wrapped/flattened error, an error
// produced by a PRAGMA integrity check (see errCorruptCheck), or a foreign
// driver. Code matching is preferred; this is the fallback.
var corruptMessages = []string{
	"database disk image is malformed",
	"file is not a database",
	"file opened that is not a database file", // modernc's SQLITE_NOTADB text
	"malformed database schema",
}

// IsCorrupt reports whether err indicates SQLite-level database corruption —
// the class of error that repair (not retry) addresses. It is the corruption
// analogue of isBusy: isBusy says "wait and try again", IsCorrupt says "the
// bytes on disk are damaged; rebuild the file".
//
// Classification prefers the driver's structured result code (modernc's
// *sqlite.Error, reached through errors.As so a wrapped error still matches),
// masking extended codes down to their primary code. A driver error carrying a
// known, non-corrupt code is definitively not corruption, so message matching
// is consulted only when no driver code is available.
func IsCorrupt(err error) bool {
	if err == nil {
		return false
	}
	var se *sqlitedrv.Error
	if errors.As(err, &se) {
		switch se.Code() & 0xff {
		case sqliteCorrupt, sqliteNotADB:
			return true
		}
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, m := range corruptMessages {
		if strings.Contains(msg, m) {
			return true
		}
	}
	return false
}

// errCorruptCheck reports a failed PRAGMA quick_check / integrity_check. Such a
// check does not fail the statement — it returns problem descriptions as rows —
// so the store turns the findings into an error that IsCorrupt recognises (via
// the message fallback above) and the open path routes to repair.
type errCorruptCheck struct{ detail string }

func (e *errCorruptCheck) Error() string {
	return "database disk image is malformed: " + e.detail
}
