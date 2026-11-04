//go:build windows

package agent

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/UserExistsError/conpty"
	"golang.org/x/sys/windows"
)

type winPTY struct {
	c *conpty.ConPty
}

// startPTY launches the shell inside a Windows pseudo console (ConPTY,
// Windows 10 1809 / Server 2019 and newer), so PowerShell's line editing,
// colors, tab completion and full-screen tools work in the browser.
func startPTY(shell string, cols, rows int) (ptyProc, error) {
	if !conpty.IsConPtyAvailable() {
		return nil, errNoPTY
	}
	sys := os.Getenv("SystemRoot")
	if sys == "" {
		sys = `C:\Windows`
	}
	var cmdline string
	switch shell {
	case "cmd":
		cmdline = filepath.Join(sys, `System32\cmd.exe`)
	case "pwsh":
		p, err := exec.LookPath("pwsh.exe")
		if err != nil {
			p = filepath.Join(os.Getenv("ProgramFiles"), `PowerShell\7\pwsh.exe`)
		}
		cmdline = `"` + p + `" -NoLogo`
	default:
		cmdline = filepath.Join(sys, `System32\WindowsPowerShell\v1.0\powershell.exe`) + " -NoLogo"
	}
	dir := os.Getenv("SystemDrive") + `\`
	if dir == `\` {
		dir = `C:\`
	}
	c, err := conpty.Start(cmdline, conpty.ConPtyDimensions(cols, rows), conpty.ConPtyWorkDir(dir),
		conpty.ConPtyEnv(append(os.Environ(), "OCEANIC_TERMINAL=1")))
	if err != nil {
		return nil, err
	}
	return &winPTY{c: c}, nil
}

func (w *winPTY) Read(p []byte) (int, error)  { return w.c.Read(p) }
func (w *winPTY) Write(p []byte) (int, error) { return w.c.Write(p) }
func (w *winPTY) Resize(cols, rows int) error { return w.c.Resize(cols, rows) }

func (w *winPTY) Wait() (int, error) {
	code, err := w.c.Wait(context.Background())
	return int(code), err
}

func (w *winPTY) Close() error {
	// Terminate the shell explicitly (child processes it started are left to
	// ConPTY's console-close signal), then release the pseudo console.
	if h, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, uint32(w.c.Pid())); err == nil {
		windows.TerminateProcess(h, 1)
		windows.CloseHandle(h)
	}
	return w.c.Close()
}
