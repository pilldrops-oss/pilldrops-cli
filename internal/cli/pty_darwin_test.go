package cli

import (
	"os"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// openPTY returns a connected (master, slave) pseudo-terminal pair. Darwin's
// recipe: clone /dev/ptmx, grant and unlock the slave, then ask for its name.
func openPTY() (*os.File, *os.File, error) {
	m, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		return nil, nil, err
	}
	fd := int(m.Fd())
	if err := unix.IoctlSetInt(fd, unix.TIOCPTYGRANT, 0); err != nil {
		_ = m.Close()
		return nil, nil, err
	}
	if err := unix.IoctlSetInt(fd, unix.TIOCPTYUNLK, 0); err != nil {
		_ = m.Close()
		return nil, nil, err
	}
	// TIOCPTYGNAME writes the slave's path into a 128-byte buffer; x/sys has
	// no typed wrapper for it, so this is the raw ioctl.
	var buf [128]byte
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, m.Fd(),
		uintptr(unix.TIOCPTYGNAME), uintptr(unsafe.Pointer(&buf[0]))); e != 0 {
		_ = m.Close()
		return nil, nil, e
	}
	n := 0
	for n < len(buf) && buf[n] != 0 {
		n++
	}
	s, err := os.OpenFile(string(buf[:n]), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		_ = m.Close()
		return nil, nil, err
	}
	return m, s, nil
}
