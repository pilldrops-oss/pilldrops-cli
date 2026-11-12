package cli

import "os"

// notifyResize does nothing on Windows, which has no resize signal; the size
// sent when the session opens stands.
func notifyResize(chan<- os.Signal) {}
