package credentials

import (
	"context"
	"errors"
	"io"
	"os/exec"
	"time"
)

// runRefreshCommand executes the locally configured helper. Its stdout and
// stderr are discarded (nothing the child prints may reach the adapter's
// logs), and the run is bounded by the configured timeout, mirroring
// subprocess.run(..., stdout=DEVNULL, stderr=DEVNULL, timeout=...).
func runRefreshCommand(command []string, timeout float64) bool {
	if len(command) == 0 {
		return true
	}
	if timeout <= 0 {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeout*float64(time.Second)))
	defer cancel()
	cmd := exec.CommandContext(ctx, command[0], command[1:]...)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	cmd.Stdin = nil
	if err := cmd.Run(); err != nil {
		// Non-zero exits, missing binaries, and timeouts all mean "the
		// helper did not refresh", like the Python OSError /
		// TimeoutExpired / returncode handling.
		return false
	}
	return true
}

// Refresh runs the optional helper, then reports whether the snapshot moved.
func (s *FileCredentialSource) Refresh() (bool, error) {
	s.refreshLock.Lock()
	defer s.refreshLock.Unlock()
	before := s.last
	if !s.lastSet {
		var err error
		before, err = s.snapshot()
		if err != nil {
			return false, err
		}
	}
	if len(s.RefreshCommand) > 0 {
		if s.RefreshTimeout <= 0 {
			return false, errors.New("refresh_timeout must be positive")
		}
		if !runRefreshCommand(s.RefreshCommand, s.RefreshTimeout) {
			return false, nil
		}
	}
	current, err := s.snapshot()
	if err != nil {
		return false, err
	}
	s.last = current
	s.lastSet = true
	return current.Cookie != "" && current != before, nil
}
