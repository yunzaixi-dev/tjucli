package workspaceterminal

import (
	"bytes"
	"os"
	"syscall"
	"unsafe"
)

// From <sys/ttycom.h>.
const (
	tiocptygrant = 0x20007454
	tiocptyunlk  = 0x20007452
	tiocptygname = 0x40807453
)

func openPTY() (*os.File, string, error) {
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, "", err
	}
	var name [128]byte
	for _, step := range []struct {
		request uintptr
		arg     unsafe.Pointer
	}{{tiocptygrant, nil}, {tiocptyunlk, nil}, {tiocptygname, unsafe.Pointer(&name[0])}} {
		if err := ioctl(master, step.request, step.arg); err != nil {
			_ = master.Close()
			return nil, "", err
		}
	}
	if end := bytes.IndexByte(name[:], 0); end > 0 {
		return master, string(name[:end]), nil
	}
	_ = master.Close()
	return nil, "", syscall.ENOENT
}
