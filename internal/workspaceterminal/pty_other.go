//go:build !linux && !darwin

package workspaceterminal

import (
	"io"
	"os/exec"
	"runtime"
	"sync"
)

// Without a pseudo-terminal the shell runs on pipes: commands work, but
// full-screen programs and line editing do not, and nothing echoes typing.
type pipeProcess struct {
	cmd  *exec.Cmd
	in   io.WriteCloser
	out  io.ReadCloser
	once sync.Once
}

func (p *pipeProcess) Read(b []byte) (int, error)  { return p.out.Read(b) }
func (p *pipeProcess) Write(b []byte) (int, error) { return p.in.Write(b) }
func (p *pipeProcess) Resize(int, int) error       { return nil }

// Wait follows the output: the goroutine in startShell closes it only once
// the shell has exited, so its state is known by now.
func (p *pipeProcess) Wait() (int, error) {
	if p.cmd.ProcessState == nil {
		return -1, nil
	}
	return p.cmd.ProcessState.ExitCode(), nil
}
func (p *pipeProcess) Kill() {
	p.once.Do(func() {
		_ = p.in.Close()
		if p.cmd.Process != nil {
			_ = p.cmd.Process.Kill()
		}
	})
}

func startShell(dir string, _, _ int) (Process, error) {
	name, args := "/bin/sh", []string{"-i"}
	if runtime.GOOS == "windows" {
		name, args = "powershell.exe", []string{"-NoLogo", "-NoProfile"}
	}
	cmd := exec.Command(name, args...)
	cmd.Dir, cmd.Env = dir, shellEnv()
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	reader, writer := io.Pipe()
	cmd.Stdout, cmd.Stderr = writer, writer
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	go func() {
		_ = cmd.Wait()
		_ = writer.Close()
	}()
	return &pipeProcess{cmd: cmd, in: in, out: reader}, nil
}
