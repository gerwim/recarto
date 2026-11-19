// Package detach starts a process that outlives the process that starts it.
package detach

import "os/exec"

// Start starts name with args in the current directory and environment, and
// does not wait for it. The process gets no standard input or output: a hook
// host reads the hook's output until every holder has closed it, so an
// inherited pipe would make the host wait for this process too. It also runs
// outside the caller's session (Unix) or console (Windows), so the host ending
// its session does not stop it.
func Start(name string, args ...string) error {
	_, err := StartWatched(name, args...)
	return err
}

// StartWatched is Start that also reports the process's exit: the returned
// channel gets its Wait error (nil on exit status 0) once it ends, while the
// caller still runs. The caller need not read it.
func StartWatched(name string, args ...string) (<-chan error, error) {
	cmd := exec.Command(name, args...)
	cmd.SysProcAttr = sysProcAttr()
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	return done, nil
}
