//go:build !windows

package agent

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/shirou/gopsutil/v4/host"
)

func ConfigPath() string {
	if p := os.Getenv("OCEANIC_AGENT_CONFIG"); p != "" {
		return p
	}
	return "/etc/oceanic/agent.json"
}
func workDir() string {
	d := "/var/lib/oceanic/work"
	if os.MkdirAll(d, 0o700) != nil {
		return os.TempDir()
	}
	return d
}

func restrictConfigACL(path string) error { return os.Chmod(path, 0o600) }

func prepareCmd(cmd *exec.Cmd) {
	// Own process group so a timeout kills the whole tree.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}

func IsElevated() bool                            { return os.Geteuid() == 0 }
func Elevate(args []string) error                 { return fmt.Errorf("please re-run with sudo") }
func MessageBox(title, text string, isError bool) {}
func IsService() bool                             { return false }
func RunService() error                           { return Run(context.Background()) }

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func powerAction(reboot bool) error {
	time.Sleep(5 * time.Second)
	if reboot {
		return exec.Command("shutdown", "-r", "now").Run()
	}
	return exec.Command("shutdown", "-h", "now").Run()
}

func (a *Agent) selfUpdate(ctx context.Context) error {
	exe, _ := os.Executable()
	tmp := exe + ".new"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	err = a.c.download(ctx, runtime.GOOS+"-"+runtimeArch(), f)
	f.Close()
	if err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, exe) // atomic on Linux, even while running
}

// restartSelf exits non-zero; systemd's Restart=always brings us back.
func restartSelf() {
	time.Sleep(2 * time.Second)
	os.Exit(1)
}

func readTrim(p string) string {
	b, _ := os.ReadFile(p)
	return strings.TrimSpace(string(b))
}

func runOut(name string, args ...string) []byte {
	if _, err := exec.LookPath(name); err != nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, _ := exec.CommandContext(ctx, name, args...).Output()
	return out
}

func loggedInUsers() []string {
	us, err := host.Users()
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, u := range us {
		if !seen[u.User] {
			seen[u.User] = true
			out = append(out, u.User)
		}
	}
	sort.Strings(out)
	return out
}
