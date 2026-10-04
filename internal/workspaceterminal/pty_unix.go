//go:build linux || darwin

package workspaceterminal

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"time"
	"unsafe"
)

type ptyProcess struct {
	master *os.File
	cmd    *exec.Cmd
}

func (p *ptyProcess) Read(b []byte) (int, error) {
	n, err := p.master.Read(b)
	// Linux reports EIO on the master once the shell side has closed.
	if err != nil && errors.Is(err, syscall.EIO) {
		err = os.ErrClosed
	}
	return n, err
}

func (p *ptyProcess) Write(b []byte) (int, error) { return p.master.Write(b) }

func (p *ptyProcess) Resize(cols, rows int) error {
	return setSize(p.master, cols, rows)
}

func (p *ptyProcess) Wait() (int, error) {
	err := p.cmd.Wait()
	_ = p.master.Close()
	if exit, ok := err.(*exec.ExitError); ok {
		return exit.ExitCode(), nil
	}
	if err != nil {
		return -1, err
	}
	return 0, nil
}

// Kill hangs up the shell's whole session, then makes sure it is gone.
func (p *ptyProcess) Kill() {
	if p.cmd.Process == nil {
		return
	}
	pid := p.cmd.Process.Pid
	_ = syscall.Kill(-pid, syscall.SIGHUP)
	go func() {
		time.Sleep(2 * time.Second)
		_ = syscall.Kill(-pid, syscall.SIGKILL)
	}()
}

func setSize(f *os.File, cols, rows int) error {
	size := struct{ rows, cols, x, y uint16 }{uint16(rows), uint16(cols), 0, 0}
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), uintptr(syscall.TIOCSWINSZ), uintptr(unsafe.Pointer(&size))); errno != 0 {
		return errno
	}
	return nil
}

func ioctl(f *os.File, request uintptr, arg unsafe.Pointer) error {
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), request, uintptr(arg)); errno != 0 {
		return errno
	}
	return nil
}

// loginShell is the user's shell when it is an existing absolute path.
func loginShell() string {
	if shell := os.Getenv("SHELL"); len(shell) > 1 && shell[0] == '/' {
		if info, err := os.Stat(shell); err == nil && !info.IsDir() {
			return shell
		}
	}
	for _, shell := range []string{"/bin/bash", "/bin/zsh", "/bin/sh"} {
		if _, err := os.Stat(shell); err == nil {
			return shell
		}
	}
	return "/bin/sh"
}

func startShell(dir string, cols, rows int) (Process, error) {
	master, slaveName, err := openPTY()
	if err != nil {
		return nil, err
	}
	slave, err := os.OpenFile(slaveName, os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		_ = master.Close()
		return nil, err
	}
	defer slave.Close()
	if cols > 0 && rows > 0 {
		_ = setSize(master, cols, rows)
	}
	cmd := exec.Command(loginShell(), "-l")
	cmd.Dir, cmd.Env = dir, shellEnv()
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	if err := cmd.Start(); err != nil {
		_ = master.Close()
		return nil, err
	}
	return &ptyProcess{master: master, cmd: cmd}, nil
}
