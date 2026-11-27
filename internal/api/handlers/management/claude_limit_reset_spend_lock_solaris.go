//go:build solaris

package management

import (
	"errors"
	"os"
)

// errClaudeResetLedgerLockUnsupported refuses the spend ledger where the
// per-open-file lock the ledger relies on is unavailable.
var errClaudeResetLedgerLockUnsupported = errors.New("the spend ledger's per-file lock is not available on this platform")

// lockClaudeResetLedgerFile fails closed on Solaris and illumos: they have no
// flock(2), and fcntl record locks belong to the process rather than to the
// open file, so they could not refuse a second open of the same ledger. The
// ledger then refuses every spend instead of risking a double claim.
func lockClaudeResetLedgerFile(*os.File) error {
	return errClaudeResetLedgerLockUnsupported
}
