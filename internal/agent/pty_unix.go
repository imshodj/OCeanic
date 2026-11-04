//go:build !windows

package agent

import (
	"errors"
	"os"
	"os/exec"
	"os/user"
	"syscall"

	"github.com/creack/pty"
)

type unixPTY struct {
	f   *os.File
	cmd *exec.Cmd
}

func startPTY(shell string, cols, rows int) (ptyProc, error) {
	var path string
	var args []string
	switch shell {
	case "pwsh":
		path, args = "pwsh", []string{"-NoLogo"}
	case "sh":
		path, args = "/bin/sh", []string{"-l"}
	default: // bash, falling back to sh on minimal systems (Alpine, BusyBox)
		if p, err := exec.LookPath("bash"); err == nil {
			path, args = p, []string{"-l"}
		} else {
			path, args = "/bin/sh", []string{"-l"}
		}
	}
	cmd := exec.Command(path, args...)
	home := "/root"
	if u, err := user.Current(); err == nil && u.HomeDir != "" {
		home = u.HomeDir
	}
	if st, err := os.Stat(home); err != nil || !st.IsDir() {
		home = "/"
	}
	cmd.Dir = home
	cmd.Env = append(os.Environ(), "TERM=xterm-256color", "COLORTERM=truecolor", "HOME="+home, "OCEANIC_TERMINAL=1")
	f, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
	if err != nil {
		return nil, err
	}
	return &unixPTY{f: f, cmd: cmd}, nil
}

func (u *unixPTY) Read(p []byte) (int, error)  { return u.f.Read(p) }
func (u *unixPTY) Write(p []byte) (int, error) { return u.f.Write(p) }
func (u *unixPTY) Resize(cols, rows int) error {
	return pty.Setsize(u.f, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
}

func (u *unixPTY) Wait() (int, error) {
	err := u.cmd.Wait()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), nil
	}
	if err != nil {
		return -1, err
	}
	return 0, nil
}

func (u *unixPTY) Close() error {
	if u.cmd.Process != nil {
		// pty.Start puts the shell in its own session; hang up the whole group.
		syscall.Kill(-u.cmd.Process.Pid, syscall.SIGHUP)
		syscall.Kill(-u.cmd.Process.Pid, syscall.SIGKILL)
	}
	return u.f.Close()
}
