//go:build !windows

package detach

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// hang only catches a hang; the test never waits it out on success.
const hang = time.Minute

// TestDetachHelper is the caller and the child of TestStartOutlivesCallerAndHoldsNoOutput.
func TestDetachHelper(t *testing.T) {
	switch os.Getenv("RECARTO_DETACH_HELPER") {
	case "caller":
		if err := os.Setenv("RECARTO_DETACH_HELPER", "child"); err != nil {
			t.Fatal(err)
		}
		if err := Start(os.Args[0], "-test.run=^TestDetachHelper$"); err != nil {
			t.Fatal(err)
		}
		fmt.Println("started")
		os.Exit(0)
	case "child":
		// Keep the reader open through the write so the FIFO handshake cannot race.
		g, err := os.Open(os.Getenv("GATE"))
		if err != nil {
			os.Exit(1)
		}
		if _, err := io.CopyN(io.Discard, g, 1); err != nil {
			os.Exit(1)
		}
		if err := g.Close(); err != nil {
			os.Exit(1)
		}
		if err := os.WriteFile(os.Getenv("DONE"), nil, 0o644); err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	case "exit3":
		os.Exit(3)
	default:
		t.Skip("helper process")
	}
}

func TestStartOutlivesCallerAndHoldsNoOutput(t *testing.T) {
	dir := t.TempDir()
	gate, done := filepath.Join(dir, "gate"), filepath.Join(dir, "done")
	if err := syscall.Mkfifo(gate, 0o600); err != nil {
		t.Fatal(err)
	}
	caller := exec.Command(os.Args[0], "-test.run=^TestDetachHelper$")
	caller.Env = append(os.Environ(), "RECARTO_DETACH_HELPER=caller", "GATE="+gate, "DONE="+done)
	caller.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	type result struct {
		out []byte
		err error
	}
	got := make(chan result, 1)
	go func() {
		out, err := caller.Output()
		got <- result{out, err}
	}()
	select {
	case r := <-got:
		if r.err != nil || string(r.out) != "started\n" {
			t.Fatalf("caller = %q, %v; want started", r.out, r.err)
		}
	case <-time.After(hang):
		t.Fatal("the caller's output stayed open: the child holds it")
	}

	// A host ending its session kills the hook's process group.
	if err := syscall.Kill(-caller.Process.Pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		t.Fatal(err)
	}

	deadline := time.Now().Add(hang)
	for {
		w, err := os.OpenFile(gate, os.O_WRONLY|syscall.O_NONBLOCK, 0)
		if err == nil {
			n, werr := w.Write([]byte{1})
			cerr := w.Close()
			if n != 1 || werr != nil || cerr != nil {
				t.Fatalf("write gate: wrote %d bytes, write error %v, close error %v", n, werr, cerr)
			}
			break
		}
		if !errors.Is(err, syscall.ENXIO) {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatal("the child never opened the gate: it died with the caller")
		}
		time.Sleep(10 * time.Millisecond)
	}
	for {
		if _, err := os.Stat(done); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the child did not finish")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestStartWatchedReportsTheExit(t *testing.T) {
	t.Setenv("RECARTO_DETACH_HELPER", "exit3")
	done, err := StartWatched(os.Args[0], "-test.run=^TestDetachHelper$")
	if err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 3 {
			t.Fatalf("want exit status 3, got %v", err)
		}
	case <-time.After(hang):
		t.Fatal("the process's exit never arrived")
	}
}
