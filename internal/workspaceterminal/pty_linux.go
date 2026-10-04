package workspaceterminal

import (
	"os"
	"strconv"
	"syscall"
	"unsafe"
)

func openPTY() (*os.File, string, error) {
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, "", err
	}
	unlock := int32(0)
	if err := ioctl(master, syscall.TIOCSPTLCK, unsafe.Pointer(&unlock)); err != nil {
		_ = master.Close()
		return nil, "", err
	}
	var n uint32
	if err := ioctl(master, syscall.TIOCGPTN, unsafe.Pointer(&n)); err != nil {
		_ = master.Close()
		return nil, "", err
	}
	return master, "/dev/pts/" + strconv.FormatUint(uint64(n), 10), nil
}
