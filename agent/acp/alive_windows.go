//go:build windows

package acp

// processAlive has no cheap zombie probe on Windows; rely on the session's
// alive flag, which the I/O goroutine clears once the process is observed
// dead.
func processAlive(pid int) bool {
	return true
}
