package agents

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/gerwim/recarto/internal/app"
)

// NumberedLines returns the line numbers of the first run of consecutively
// numbered lines in text, each starting with its number and sep (OpenCode
// "12: ", Copilot "12. "). It returns 0, 0 when text has none.
func NumberedLines(text, sep string) (first, last int) {
	for line := range strings.Lines(text) {
		n := lineNumber(strings.TrimRight(line, "\r\n"), sep)
		switch {
		case first == 0 && n >= 1:
			first, last = n, n
		case first == 0:
			// a header before the numbered lines
		case n == last+1:
			last = n
		default:
			return first, last
		}
	}
	return first, last
}

// lineNumber returns the number line starts with when sep follows it, else 0.
func lineNumber(line, sep string) int {
	i := 0
	for i < len(line) && line[i] >= '0' && line[i] <= '9' {
		i++
	}
	if i == 0 || !strings.HasPrefix(line[i:], sep) {
		return 0
	}
	n, err := strconv.Atoi(line[:i])
	if err != nil {
		return 0 // too many digits to be a line number
	}
	return n
}

// readBefore reports whether earlier reads in scope returned every line from
// first to last of file as it is now, then records this read.
func readBefore(ctx context.Context, a *app.App, scope, file string, first, last int) (bool, error) {
	info, err := os.Stat(file)
	if err != nil {
		// Without a stamp the file can't be shown unchanged, so the advisory
		// notice stays silent.
		return false, nil
	}
	// shortcut: size+mtime misses a same-size rewrite within one mtime tick on a
	// coarse-mtime filesystem; switch to a content hash if that ever matters.
	prefix := fmt.Sprintf("%s%s@%d.%d:", readMetric, file, info.Size(), info.ModTime().UnixNano())
	earlier, err := a.Ledger.Metrics(ctx, scope, prefix)
	if err != nil {
		return false, err
	}
	if err := a.Ledger.Incr(ctx, scope, fmt.Sprintf("%s%d-%d", prefix, first, last), 1); err != nil {
		return false, err
	}
	return covered(earlier, prefix, first, last), nil
}

// covered reports whether the ranges recorded as metrics prefix+"<first>-<last>"
// together contain every line from first to last.
func covered(metrics []string, prefix string, first, last int) bool {
	type span struct{ start, end int }
	var spans []span
	for _, m := range metrics {
		a, b, ok := strings.Cut(strings.TrimPrefix(m, prefix), "-")
		s, errA := strconv.Atoi(a)
		e, errB := strconv.Atoi(b)
		if !ok || errA != nil || errB != nil {
			continue // only a file whose name continues our prefix makes another suffix
		}
		spans = append(spans, span{s, e})
	}
	slices.SortFunc(spans, func(x, y span) int { return x.start - y.start })
	next := first
	for _, r := range spans {
		if r.start > next {
			break
		}
		if r.end >= next {
			next = r.end + 1
		}
	}
	return next > last
}

// coversFile reports whether lines first..last are all of file. A file that
// can't be read counts as not covered, so the read is treated as partial.
func coversFile(file string, first, last int) bool {
	if first != 1 {
		return false
	}
	n, err := fileLines(file)
	return err == nil && last >= n
}

// fileLines counts file's lines as an editor numbers them: a final newline
// does not start another line.
func fileLines(path string) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer func() { _ = f.Close() }() // opened only for reading: closing cannot lose data
	buf := make([]byte, 64<<10)
	n, lastByte := 0, byte('\n')
	for {
		k, err := f.Read(buf)
		if k > 0 {
			n += bytes.Count(buf[:k], []byte{'\n'})
			lastByte = buf[k-1]
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return 0, err
		}
	}
	if lastByte != '\n' {
		n++
	}
	return n, nil
}
