//go:build unix

package features

import (
	"os"
	"os/signal"
	"syscall"
)

// enableVT does nothing: unix terminals take escape sequences as they are.
func enableVT(*os.File) func() { return func() {} }

// notifyResize sends to c whenever the terminal is resized.
func notifyResize(c chan<- os.Signal) { signal.Notify(c, syscall.SIGWINCH) }
