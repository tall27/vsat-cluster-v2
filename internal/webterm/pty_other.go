//go:build !linux

package webterm

import (
	"errors"
	"os/exec"
)

// startPTY is unsupported off Linux; the app only runs in production on the
// Linux host. This stub keeps the package buildable on dev machines.
func startPTY(_ *exec.Cmd) (ptySession, error) {
	return nil, errors.New("in-browser terminal is only supported on Linux")
}
