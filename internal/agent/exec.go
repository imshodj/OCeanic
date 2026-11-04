package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/imshodj/oceanic/internal/protocol"
)

const maxOutput = 1 << 20

// limitedBuffer keeps the first maxOutput bytes of combined stdout/stderr.
type limitedBuffer struct {
	mu        sync.Mutex
	buf       bytes.Buffer
	truncated bool
}

func (l *limitedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	room := maxOutput - l.buf.Len()
	if room <= 0 {
		l.truncated = true
		return len(p), nil
	}
	if len(p) > room {
		l.buf.Write(p[:room])
		l.truncated = true
		return len(p), nil
	}
	return l.buf.Write(p)
}

func (l *limitedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	s := l.buf.String()
	if l.truncated {
		s += "\n... [output truncated]"
	}
	return s
}

// runScript writes the script to a temp file and executes it with the
// requested interpreter. Returns combined output, exit code and an error string.
func runScript(ctx context.Context, j protocol.Job) (string, int, string) {
	timeout := time.Duration(j.TimeoutSec) * time.Second
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	dir, err := os.MkdirTemp(workDir(), "job-")
	if err != nil {
		return "", -1, err.Error()
	}
	defer os.RemoveAll(dir)

	name, argv, body, err := interpreter(j.Shell, j.Script, dir)
	if err != nil {
		return "", -1, err.Error()
	}
	if err := os.WriteFile(filepath.Join(dir, name), body, 0o700); err != nil {
		return "", -1, err.Error()
	}

	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, argv[0], argv[1:]...)
	cmd.Dir = dir
	var out limitedBuffer
	cmd.Stdout, cmd.Stderr = &out, &out
	cmd.Env = append(os.Environ(), "OCEANIC_JOB_ID="+j.ID)
	prepareCmd(cmd)
	cmd.WaitDelay = 10 * time.Second

	err = cmd.Run()
	code := 0
	errStr := ""
	if err != nil {
		var ee *exec.ExitError
		switch {
		case errors.Is(cctx.Err(), context.DeadlineExceeded):
			code, errStr = -1, fmt.Sprintf("timed out after %s", timeout)
		case errors.As(err, &ee):
			code = ee.ExitCode()
		default:
			code, errStr = -1, err.Error()
		}
	}
	return out.String(), code, errStr
}

func interpreter(shell, script, dir string) (file string, argv []string, body []byte, err error) {
	path := func(f string) string { return filepath.Join(dir, f) }
	switch shell {
	case protocol.ShellPowerShell, protocol.ShellPwsh:
		exe := "powershell.exe"
		if shell == protocol.ShellPwsh {
			exe = "pwsh"
		} else if runtime.GOOS != "windows" {
			exe = "pwsh"
		}
		// UTF-8 BOM so Windows PowerShell 5.1 reads non-ASCII correctly,
		// and force UTF-8 output so the dashboard renders it properly.
		prelude := "[Console]::OutputEncoding = [System.Text.Encoding]::UTF8\n$ProgressPreference = 'SilentlyContinue'\n"
		body = append([]byte{0xEF, 0xBB, 0xBF}, []byte(prelude+script)...)
		return "script.ps1", []string{exe, "-NoLogo", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", path("script.ps1")}, body, nil
	case protocol.ShellCmd:
		if runtime.GOOS != "windows" {
			return "", nil, nil, errors.New("cmd scripts can only run on Windows")
		}
		body = []byte("@echo off\r\nchcp 65001 >nul\r\n" + strings.ReplaceAll(script, "\r\n", "\n"))
		body = bytes.ReplaceAll(body, []byte("\n"), []byte("\r\n"))
		body = bytes.ReplaceAll(body, []byte("\r\r\n"), []byte("\r\n"))
		return "script.cmd", []string{"cmd.exe", "/d", "/c", path("script.cmd")}, body, nil
	case protocol.ShellZsh:
		if runtime.GOOS == "windows" {
			return "", nil, nil, errors.New("zsh scripts are not supported on Windows")
		}
		return "script.sh", []string{"zsh", path("script.sh")}, []byte(normalizeLF(script)), nil
	case protocol.ShellBash:
		return "script.sh", []string{"bash", path("script.sh")}, []byte(normalizeLF(script)), nil
	case protocol.ShellSh:
		if runtime.GOOS == "windows" {
			return "", nil, nil, errors.New("sh scripts are not supported on Windows; use powershell or cmd")
		}
		return "script.sh", []string{"/bin/sh", path("script.sh")}, []byte(normalizeLF(script)), nil
	case protocol.ShellPython:
		exe := "python3"
		if runtime.GOOS == "windows" {
			exe = "python"
		}
		return "script.py", []string{exe, path("script.py")}, []byte(script), nil
	}
	return "", nil, nil, fmt.Errorf("unsupported shell %q", shell)
}

func normalizeLF(s string) string { return strings.ReplaceAll(s, "\r\n", "\n") }
