package fullexport

import (
	"bufio"
	"encoding/json"
	"io"

	"git.gerwim.nl/gerwim/recarto/internal/syncwire"
)

// scannerBufSize allows a single archive line (one memory's full body) to
// grow well past bufio.Scanner's default 64KB max token size, mirroring
// internal/snapshot's Import.
const scannerBufSize = 4 * 1024 * 1024

// Read streams a recarto-export/v1 archive line by line, invoking fn once per
// entry. The header line (detected by its "format" field) is skipped, not
// passed to fn. It stops at, and returns, the first error from fn or from
// decoding a line.
func Read(r io.Reader, fn func(syncwire.ExportEntry) error) (Stats, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), scannerBufSize)

	var stats Stats
	first := true
	for sc.Scan() {
		raw := sc.Bytes()
		if len(raw) == 0 {
			continue
		}
		if first {
			first = false
			var probe struct {
				Format string `json:"format"`
			}
			if err := json.Unmarshal(raw, &probe); err == nil && probe.Format == syncwire.ExportFormat {
				continue // the header carries no record of its own
			}
		}
		var e syncwire.ExportEntry
		if err := json.Unmarshal(raw, &e); err != nil {
			return stats, err
		}
		if err := fn(e); err != nil {
			return stats, err
		}
		if e.Record.Deleted {
			stats.Tombstones++
		} else {
			stats.Records++
		}
	}
	return stats, sc.Err()
}
