package features

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

// TestEditHelperProcess is the editor TestEditRestoresTheTerminal runs on a
// pseudo-terminal; on its own it does nothing.
func TestEditHelperProcess(t *testing.T) {
	if os.Getenv("RECARTO_EDIT_HELPER") == "" {
		t.Skip("run by TestEditRestoresTheTerminal")
	}
	err := Edit(os.Getenv("RECARTO_CONFIG"), os.Stdin, os.Stdout)
	fmt.Fprintf(os.Stderr, "edit ended: %v\n", err)
	if err != nil {
		os.Exit(1)
	}
	os.Exit(0)
}

// TestEditRestoresTheTerminal ends a real editor process every way it can end
// — its quit key and the signals that ask a process to stop — and checks the
// terminal is back in normal mode, off the alternate screen, and told what
// changed.
func TestEditRestoresTheTerminal(t *testing.T) {
	for _, end := range []struct {
		name string
		do   func(p *os.Process, typing *os.File) error
		code int
		says string
	}{
		{"q", func(_ *os.Process, typing *os.File) error { _, err := typing.Write([]byte("q")); return err }, 0, "<nil>"},
		{"SIGTERM", func(p *os.Process, _ *os.File) error { return p.Signal(syscall.SIGTERM) }, 1, "terminated"},
		{"SIGHUP", func(p *os.Process, _ *os.File) error { return p.Signal(syscall.SIGHUP) }, 1, "hangup"},
		{"SIGINT", func(p *os.Process, _ *os.File) error { return p.Signal(syscall.SIGINT) }, 1, "interrupt"},
	} {
		t.Run(end.name, func(t *testing.T) {
			master, slave := openPTY(t)
			const cooked = unix.ICANON | unix.ECHO
			if modes(t, slave)&cooked != cooked {
				t.Fatal("a new terminal is not in normal mode")
			}
			cmd := exec.Command(os.Args[0], "-test.run=^TestEditHelperProcess$")
			cmd.Env = append(os.Environ(), "RECARTO_EDIT_HELPER=1", "RECARTO_CONFIG="+filepath.Join(t.TempDir(), "config.toml"))
			cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			exited := make(chan struct{})
			go func() { _ = cmd.Wait(); close(exited) }()
			screen := read(master)
			var seen []byte
			waitFor := func(text string) {
				t.Helper()
				for !bytes.Contains(seen, []byte(text)) {
					select {
					case b := <-screen:
						seen = append(seen, b...)
					case <-exited:
						t.Fatalf("the editor exited before writing %q:\n%q", text, seen)
					}
				}
			}

			waitFor(" Recarto features")
			if modes(t, slave)&cooked != 0 {
				t.Fatal("the editor did not switch the terminal to raw mode")
			}
			if _, err := master.Write([]byte(" ")); err != nil {
				t.Fatal(err)
			}
			waitFor("Saved session-memories = false")
			if err := end.do(cmd.Process, master); err != nil {
				t.Fatal(err)
			}
			<-exited
			if got := cmd.ProcessState.ExitCode(); got != end.code {
				t.Errorf("exit code %d (%v), want %d", got, cmd.ProcessState, end.code)
			}
			if modes(t, slave)&cooked != cooked {
				t.Error("the terminal was left in raw mode")
			}
			slave.Close() // with no writer left, the master reads the rest and stops
			for b := range screen {
				seen = append(seen, b...)
			}
			for _, want := range []string{"\x1b[?25h\x1b[?1049l", "session-memories: off", "edit ended: " + end.says} {
				if !bytes.Contains(seen, []byte(want)) {
					t.Errorf("output lacks %q:\n%q", want, seen)
				}
			}
		})
	}
}

// openPTY opens a pseudo-terminal, skipping the test where the system has
// none to give.
func openPTY(t *testing.T) (master, slave *os.File) {
	t.Helper()
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Skipf("no pseudo-terminal: %v", err)
	}
	t.Cleanup(func() { master.Close() })
	var n int
	control(t, master, func(fd int) (err error) {
		if err = unix.IoctlSetPointerInt(fd, unix.TIOCSPTLCK, 0); err == nil {
			n, err = unix.IoctlGetInt(fd, unix.TIOCGPTN)
		}
		return err
	})
	slave, err = os.OpenFile("/dev/pts/"+strconv.Itoa(n), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { slave.Close() })
	return master, slave
}

// modes returns the local modes (ICANON, ECHO, …) of terminal f.
func modes(t *testing.T, f *os.File) uint32 {
	t.Helper()
	var lflag uint32
	control(t, f, func(fd int) error {
		tio, err := unix.IoctlGetTermios(fd, unix.TCGETS)
		if err == nil {
			lflag = tio.Lflag
		}
		return err
	})
	return lflag
}

// control runs fn on f's descriptor without taking it out of the poller.
func control(t *testing.T, f *os.File, fn func(fd int) error) {
	t.Helper()
	rc, err := f.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var ferr error
	if err := rc.Control(func(fd uintptr) { ferr = fn(int(fd)) }); err != nil {
		t.Fatal(err)
	}
	if ferr != nil {
		t.Fatal(ferr)
	}
}

// read sends what master reads, chunk by chunk, until it fails.
func read(master *os.File) chan []byte {
	c := make(chan []byte)
	go func() {
		defer close(c)
		for {
			b := make([]byte, 4096)
			n, err := master.Read(b)
			if n > 0 {
				c <- b[:n]
			}
			if err != nil {
				return
			}
		}
	}()
	return c
}
