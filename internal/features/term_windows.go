//go:build windows

package features

import (
	"os"

	"golang.org/x/sys/windows"
)

// enableVT turns on escape-sequence output for the console out, returning a
// func that restores its mode.
func enableVT(out *os.File) func() {
	h := windows.Handle(out.Fd())
	var mode uint32
	if windows.GetConsoleMode(h, &mode) != nil {
		return func() {}
	}
	_ = windows.SetConsoleMode(h, mode|windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING)
	return func() { _ = windows.SetConsoleMode(h, mode) }
}

// notifyResize does nothing: a Windows console resize is no signal, so the
// editor redraws for the new size on the next key.
func notifyResize(chan<- os.Signal) {}
