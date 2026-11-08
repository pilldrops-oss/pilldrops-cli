//go:build !windows

package cli

import (
	"os"
	"os/signal"
	"syscall"
)

// notifyResize delivers a signal on c whenever the terminal is resized.
func notifyResize(c chan<- os.Signal) { signal.Notify(c, syscall.SIGWINCH) }
