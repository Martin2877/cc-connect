//go:build !windows

package acp

import "syscall"

// processAlive reports whether pid is a live, non-zombie child process.
// kill(pid, 0) alone is insufficient: it succeeds for zombies, and a
// wedged reaper goroutine can leave a killed child as a zombie that then
// masquerades as a running session. Wait4(WNOHANG) both detects and reaps
// it — after this returns false the kernel no longer has the child.
func processAlive(pid int) bool {
	var ws syscall.WaitStatus
	wpid, err := syscall.Wait4(pid, &ws, syscall.WNOHANG, nil)
	if err != nil {
		// ECHILD (already reaped elsewhere, or not our child) — fall back
		// to the session's own alive flag rather than declare it dead on a
		// probe error.
		return err != syscall.ESRCH
	}
	return wpid != pid
}
