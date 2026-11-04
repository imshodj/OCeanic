//go:build !windows

package agent

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/imshodj/oceanic/internal/protocol"
	"github.com/shirou/gopsutil/v4/host"
)

const unitPath = "/etc/systemd/system/oceanic-agent.service"

func InstalledExe() string { return "/opt/oceanic/oceanic-agent" }
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

func IsElevated() bool                              { return os.Geteuid() == 0 }
func Elevate(args []string) error                   { return fmt.Errorf("please re-run with sudo") }
func MessageBox(title, text string, isError bool)   {}
func IsService() bool                               { return false }
func RunService() error                             { return Run(context.Background()) }

const unitTmpl = `[Unit]
Description=%s
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=%s run
Restart=always
RestartSec=5
KillMode=process

[Install]
WantedBy=multi-user.target
`

func Install(cfg *Config) error {
	if !IsElevated() {
		return fmt.Errorf("must be run as root")
	}
	if _, err := exec.LookPath("systemctl"); err != nil {
		return fmt.Errorf("systemd is required for 'install'; run '%s run' under your init system instead", os.Args[0])
	}
	exec.Command("systemctl", "stop", "oceanic-agent").Run()
	if err := os.MkdirAll(filepath.Dir(InstalledExe()), 0o755); err != nil {
		return err
	}
	exe, _ := os.Executable()
	if filepath.Clean(exe) != InstalledExe() {
		tmp := InstalledExe() + ".tmp"
		if err := copyFile(exe, tmp); err != nil {
			return err
		}
		if err := os.Rename(tmp, InstalledExe()); err != nil {
			return err
		}
	}
	if old, err := LoadConfig(); err == nil && old.Enrolled() && strings.EqualFold(old.ServerURL, cfg.ServerURL) {
		cfg.AgentID, cfg.AgentSecret = old.AgentID, old.AgentSecret
	}
	if err := cfg.Save(); err != nil {
		return err
	}
	if err := os.WriteFile(unitPath, []byte(fmt.Sprintf(unitTmpl, ServiceDisplayName, InstalledExe())), 0o644); err != nil {
		return err
	}
	for _, args := range [][]string{{"daemon-reload"}, {"enable", "--now", "oceanic-agent"}} {
		if out, err := exec.Command("systemctl", args...).CombinedOutput(); err != nil {
			return fmt.Errorf("systemctl %s: %v: %s", strings.Join(args, " "), err, out)
		}
	}
	return nil
}

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

func Uninstall() error {
	exec.Command("systemctl", "disable", "--now", "oceanic-agent").Run()
	os.Remove(unitPath)
	exec.Command("systemctl", "daemon-reload").Run()
	os.RemoveAll(filepath.Dir(InstalledExe()))
	os.RemoveAll(filepath.Dir(ConfigPath()))
	os.RemoveAll("/var/lib/oceanic")
	return nil
}

// scheduleUninstall runs the removal outside our cgroup so stopping the unit
// doesn't kill it halfway.
func scheduleUninstall() error {
	script := fmt.Sprintf("sleep 3; systemctl disable --now oceanic-agent; rm -f %s; systemctl daemon-reload; rm -rf %s %s /var/lib/oceanic",
		unitPath, filepath.Dir(InstalledExe()), filepath.Dir(ConfigPath()))
	if _, err := exec.LookPath("systemd-run"); err == nil {
		return exec.Command("systemd-run", "--unit=oceanic-uninstall", "--collect", "/bin/sh", "-c", script).Run()
	}
	cmd := exec.Command("/bin/sh", "-c", script)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return cmd.Start()
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

func hardwareInfo() (manufacturer, model, serial string) {
	return readTrim("/sys/class/dmi/id/sys_vendor"), readTrim("/sys/class/dmi/id/product_name"), readTrim("/sys/class/dmi/id/product_serial")
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

func installedSoftware() []protocol.Software {
	var out []protocol.Software
	parse := func(b []byte) {
		sc := bufio.NewScanner(bytes.NewReader(b))
		for sc.Scan() {
			f := strings.Split(sc.Text(), "\t")
			if len(f) >= 2 && f[0] != "" {
				s := protocol.Software{Name: f[0], Version: f[1]}
				if len(f) > 2 {
					s.Publisher = f[2]
				}
				out = append(out, s)
			}
		}
	}
	switch {
	case len(runOut("dpkg-query", "--version")) > 0:
		parse(runOut("dpkg-query", "-W", "-f=${Package}\t${Version}\t${Maintainer}\n"))
	case len(runOut("rpm", "--version")) > 0:
		parse(runOut("rpm", "-qa", "--qf", "%{NAME}\t%{VERSION}-%{RELEASE}\t%{VENDOR}\n"))
	case len(runOut("apk", "--version")) > 0:
		for _, l := range strings.Split(string(runOut("apk", "info", "-v")), "\n") {
			if i := strings.LastIndex(l, "-"); i > 0 {
				if j := strings.LastIndex(l[:i], "-"); j > 0 {
					out = append(out, protocol.Software{Name: l[:j], Version: l[j+1:]})
				}
			}
		}
	case len(runOut("pacman", "--version")) > 0:
		for _, l := range strings.Split(string(runOut("pacman", "-Q")), "\n") {
			if n, v, ok := strings.Cut(l, " "); ok {
				out = append(out, protocol.Software{Name: n, Version: v})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func services() []protocol.Service {
	b := runOut("systemctl", "list-units", "--type=service", "--all", "--no-legend", "--plain", "--no-pager")
	var out []protocol.Service
	for _, l := range strings.Split(string(b), "\n") {
		f := strings.Fields(l)
		if len(f) < 4 {
			continue
		}
		out = append(out, protocol.Service{
			Name:        strings.TrimSuffix(f[0], ".service"),
			DisplayName: strings.Join(f[4:], " "),
			Status:      f[3],
			StartType:   f[1],
		})
	}
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

func prettyPlatform(si protocol.SysInfo) string {
	if v := osRelease("PRETTY_NAME"); v != "" {
		return v
	}
	return strings.TrimSpace(si.Platform + " " + si.PlatformVersion)
}

func osRelease(key string) string {
	b, err := os.ReadFile("/etc/os-release")
	if err != nil {
		return ""
	}
	for _, l := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(l, key+"="); ok {
			return strings.Trim(v, `"`)
		}
	}
	return ""
}
