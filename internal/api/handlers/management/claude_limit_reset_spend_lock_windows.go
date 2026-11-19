//go:build windows

package management

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// lockClaudeResetLedgerFile takes the exclusive lock on the spend ledger
// directory's lock file without waiting. The lock belongs to this handle:
// another process, or another open of the same file, gets
// errClaudeResetLedgerLocked until it is closed.
func lockClaudeResetLedgerFile(f *os.File) error {
	var overlapped windows.Overlapped
	errLock := windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &overlapped)
	if errors.Is(errLock, windows.ERROR_LOCK_VIOLATION) {
		return errClaudeResetLedgerLocked
	}
	return errLock
}
