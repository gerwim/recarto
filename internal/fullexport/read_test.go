package fullexport

import (
	"strings"
	"testing"

	"git.gerwim.nl/gerwim/recarto/internal/syncwire"
)

func TestReadSkipsHeaderLine(t *testing.T) {
	archive := `{"format":"recarto-export/v1","created_at":1,"recarto_version":"0.1.0","repos":[{"key":"k1"}]}
{"repo_key":"k1","record":{"id":"m1","kind":"learning","title":"t","body":"b","created_at":1,"updated_at":1}}
`
	var got []syncwire.ExportEntry
	stats, err := Read(strings.NewReader(archive), func(e syncwire.ExportEntry) error {
		got = append(got, e)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].RepoKey != "k1" || got[0].Record.ID != "m1" {
		t.Fatalf("entries = %+v, want one entry for m1 under k1", got)
	}
	if stats.Records != 1 || stats.Tombstones != 0 {
		t.Fatalf("stats = %+v, want 1 record, 0 tombstones", stats)
	}
}

func TestReadCountsTombstonesSeparately(t *testing.T) {
	archive := `{"format":"recarto-export/v1","created_at":1,"recarto_version":"0.1.0"}
{"repo_key":"k1","record":{"id":"m1","kind":"learning","title":"t","body":"b","created_at":1,"updated_at":1}}
{"repo_key":"k1","record":{"id":"m2","deleted":true,"updated_at":2}}
`
	stats, err := Read(strings.NewReader(archive), func(syncwire.ExportEntry) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if stats.Records != 1 || stats.Tombstones != 1 {
		t.Fatalf("stats = %+v, want 1 record and 1 tombstone", stats)
	}
}

func TestReadPropagatesCallbackError(t *testing.T) {
	archive := `{"format":"recarto-export/v1"}
{"repo_key":"k1","record":{"id":"m1","kind":"learning","created_at":1,"updated_at":1}}
`
	wantErr := errBoom
	_, err := Read(strings.NewReader(archive), func(syncwire.ExportEntry) error { return wantErr })
	if err != wantErr {
		t.Fatalf("err = %v, want the callback's own error propagated", err)
	}
}

func TestReadRejectsGarbledLine(t *testing.T) {
	archive := `{"format":"recarto-export/v1"}
not valid json at all
`
	_, err := Read(strings.NewReader(archive), func(syncwire.ExportEntry) error { return nil })
	if err == nil {
		t.Fatal("expected an error decoding a garbled line")
	}
}

// TestExportThenReadRoundTrips exercises Read against ExportAll's own output
// (already covered end-to-end in fullexport_test.go via readAllEntries), plus
// the edge case of an archive with no entries at all (header only).
func TestReadEmptyArchiveAfterHeader(t *testing.T) {
	archive := `{"format":"recarto-export/v1","repos":[]}
`
	var calls int
	stats, err := Read(strings.NewReader(archive), func(syncwire.ExportEntry) error {
		calls++
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 0 || stats.Records != 0 || stats.Tombstones != 0 {
		t.Fatalf("expected no entries for a header-only archive, got calls=%d stats=%+v", calls, stats)
	}
}

var errBoom = &boomErr{}

type boomErr struct{}

func (*boomErr) Error() string { return "boom" }
