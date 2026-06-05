package cli

import (
	"fmt"
	"io"

	"github.com/schollz/progressbar/v3"
)

// indexProgress renders code-graph indexing progress. On an interactive terminal
// it shows an animated bar; otherwise (MCP server, hooks, piped output) it emits
// occasional plain "indexed N/total files" lines so a log stays readable. It
// implements graph.Progress.
type indexProgress struct {
	w           io.Writer
	isTTY       bool
	every       int // plain-line interval (non-TTY)
	total       int
	n           int
	lastPrinted int
	bar         *progressbar.ProgressBar
}

// newIndexProgress builds a reporter writing to w. isTTY selects the animated bar
// (true) or periodic plain lines (false).
func newIndexProgress(w io.Writer, isTTY bool) *indexProgress {
	return &indexProgress{w: w, isTTY: isTTY, every: 1000}
}

func (p *indexProgress) Start(total int) {
	p.total = total
	if p.isTTY {
		p.bar = progressbar.NewOptions(total,
			progressbar.OptionSetWriter(p.w),
			progressbar.OptionSetDescription("indexing"),
			progressbar.OptionShowCount(),
			progressbar.OptionSetPredictTime(true),
			progressbar.OptionClearOnFinish(),
		)
	}
}

func (p *indexProgress) Inc() {
	p.n++
	if p.isTTY {
		if p.bar != nil {
			_ = p.bar.Add(1)
		}
		return
	}
	if p.every > 0 && p.n%p.every == 0 {
		p.printLine()
	}
}

func (p *indexProgress) Phase(label string) {
	// Close the file bar (if any) and print the phase so a long tick-less stage
	// (resolving references) does not look frozen after the bar fills.
	if p.isTTY && p.bar != nil {
		_ = p.bar.Finish()
		p.bar = nil
	}
	fmt.Fprintf(p.w, "%s...\n", label)
}

func (p *indexProgress) Done() {
	if p.isTTY {
		if p.bar != nil {
			_ = p.bar.Finish()
		}
		return
	}
	// Final line, unless the last interval line already reported this count (so an
	// empty or exactly-on-boundary worklist prints nothing redundant).
	if p.n != p.lastPrinted {
		p.printLine()
	}
}

func (p *indexProgress) printLine() {
	fmt.Fprintf(p.w, "indexed %d/%d files\n", p.n, p.total)
	p.lastPrinted = p.n
}
