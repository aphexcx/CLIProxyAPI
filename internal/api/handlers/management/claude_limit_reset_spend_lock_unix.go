//go:build !windows && !solaris

package management

import (
	"errors"
	"os"
	"syscall"
)

// lockClaudeResetLedgerFile takes the exclusive lock on the spend ledger
// directory's lock file without waiting. The lock belongs to this open file:
// another process, or another open of the same file, gets
// errClaudeResetLedgerLocked until it is closed.
func lockClaudeResetLedgerFile(f *os.File) error {
	for {
		errFlock := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		switch {
		case errFlock == nil:
			return nil
		case errors.Is(errFlock, syscall.EINTR):
			continue
		case errors.Is(errFlock, syscall.EWOULDBLOCK):
			return errClaudeResetLedgerLocked
		default:
			return errFlock
		}
	}
}
