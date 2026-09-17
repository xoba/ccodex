//go:build unix

package history

import (
	"os/exec"
	"syscall"
)

// detach keeps a terminal's Ctrl-C from reaching sqlite3, which would abandon
// the write of a reset outcome that ccodex has already learned. Cancellation
// still reaches the process through its context.
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}
