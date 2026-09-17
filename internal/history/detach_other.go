//go:build !unix

package history

import "os/exec"

func detach(*exec.Cmd) {}
