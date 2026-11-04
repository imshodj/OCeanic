//go:build windows

package agent

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/imshodj/oceanic/internal/protocol"
	"github.com/shirou/gopsutil/v4/process"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

func installDir() string {
	pf := os.Getenv("ProgramFiles")
	if pf == "" {
		pf = `C:\Program Files`
	}
	return filepath.Join(pf, "OCeanic")
}

func dataDir() string {
	pd := os.Getenv("ProgramData")
	if pd == "" {
		pd = `C:\ProgramData`
	}
	return filepath.Join(pd, "OCeanic")
}

func InstalledExe() string { return filepath.Join(installDir(), "oceanic-agent.exe") }
func ConfigPath() string   { return filepath.Join(dataDir(), "agent.json") }
func LogPath() string      { return filepath.Join(dataDir(), "agent.log") }
func workDir() string {
	d := filepath.Join(dataDir(), "work")
	os.MkdirAll(d, 0o700)
	return d
}

// restrictConfigACL limits the config (which holds the agent secret) to SYSTEM and Administrators.
func restrictConfigACL(path string) error {
	sd, err := windows.SecurityDescriptorFromString("D:P(A;;FA;;;SY)(A;;FA;;;BA)")
	if err != nil {
		return err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	for _, p := range []string{path, filepath.Dir(path)} {
		windows.SetNamedSecurityInfo(p, windows.SE_FILE_OBJECT,
			windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil)
	}
	return nil
}

func prepareCmd(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
}

func IsElevated() bool {
	return windows.GetCurrentProcessToken().IsElevated()
}

// Elevate relaunches this executable with a UAC prompt and waits for it.
func Elevate(args []string) error {
	exe, _ := os.Executable()
	verb, _ := windows.UTF16PtrFromString("runas")
	file, _ := windows.UTF16PtrFromString(exe)
	quoted := make([]string, len(args))
	for i, a := range args {
		if strings.ContainsAny(a, " \t\"") {
			a = `"` + strings.ReplaceAll(a, `"`, `\"`) + `"`
		}
		quoted[i] = a
	}
	params, _ := windows.UTF16PtrFromString(strings.Join(quoted, " "))
	cwd, _ := windows.UTF16PtrFromString(filepath.Dir(exe))
	return windows.ShellExecute(0, verb, file, params, cwd, windows.SW_NORMAL)
}

// MessageBox shows a dialog for double-click installs.
func MessageBox(title, text string, isError bool) {
	t, _ := windows.UTF16PtrFromString(title)
	m, _ := windows.UTF16PtrFromString(text)
	flags := uint32(windows.MB_OK | windows.MB_ICONINFORMATION)
	if isError {
		flags = windows.MB_OK | windows.MB_ICONERROR
	}
	windows.MessageBox(0, m, t, flags|windows.MB_SETFOREGROUND)
}

// IsService reports whether we were launched by the Service Control Manager.
func IsService() bool {
	ok, _ := svc.IsWindowsService()
	return ok
}

type winService struct{}

func (winService) Execute(args []string, req <-chan svc.ChangeRequest, status chan<- svc.Status) (bool, uint32) {
	status <- svc.Status{State: svc.StartPending}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx) }()
	status <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
	for {
		select {
		case c := <-req:
			switch c.Cmd {
			case svc.Interrogate:
				status <- c.CurrentStatus
			case svc.Stop, svc.Shutdown:
				status <- svc.Status{State: svc.StopPending}
				cancel()
				select {
				case <-done:
				case <-time.After(10 * time.Second):
				}
				return false, 0
			}
		case err := <-done:
			cancel()
			if err != nil {
				log.Printf("agent stopped: %v", err)
				return true, 1
			}
			return false, 0
		}
	}
}

// RunService runs under the SCM, logging to ProgramData.
func RunService() error {
	setupFileLog()
	return svc.Run(ServiceName, winService{})
}

func setupFileLog() {
	os.MkdirAll(dataDir(), 0o700)
	if st, err := os.Stat(LogPath()); err == nil && st.Size() > 5<<20 {
		os.Rename(LogPath(), LogPath()+".1")
	}
	f, err := os.OpenFile(LogPath(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err == nil {
		log.SetOutput(f)
	}
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

// Install copies the binary to Program Files, writes config and registers
// an auto-start service with crash recovery.
func Install(cfg *Config) error {
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("connect to service manager (are you Administrator?): %w", err)
	}
	defer m.Disconnect()

	// Stop an existing install so the exe can be replaced (upgrade / reinstall).
	if s, err := m.OpenService(ServiceName); err == nil {
		s.Control(svc.Stop)
		waitStopped(s, 20*time.Second)
		s.Close()
	}

	if err := os.MkdirAll(installDir(), 0o755); err != nil {
		return err
	}
	exe, _ := os.Executable()
	if !strings.EqualFold(filepath.Clean(exe), filepath.Clean(InstalledExe())) {
		os.Rename(InstalledExe(), InstalledExe()+".old")
		if err := copyFile(exe, InstalledExe()); err != nil {
			return fmt.Errorf("copy agent: %w", err)
		}
		os.Remove(InstalledExe() + ".old")
	}

	// Keep existing credentials when reinstalling against the same server.
	if old, err := LoadConfig(); err == nil && old.Enrolled() && strings.EqualFold(old.ServerURL, cfg.ServerURL) {
		cfg.AgentID, cfg.AgentSecret = old.AgentID, old.AgentSecret
	}
	if err := cfg.Save(); err != nil {
		return fmt.Errorf("write config: %w", err)
	}

	s, err := m.OpenService(ServiceName)
	if err != nil {
		s, err = m.CreateService(ServiceName, InstalledExe(), mgr.Config{
			DisplayName:  ServiceDisplayName,
			Description:  ServiceDescription,
			StartType:    mgr.StartAutomatic,
			ErrorControl: mgr.ErrorNormal,
		}, "service")
		if err != nil {
			return fmt.Errorf("create service: %w", err)
		}
	} else {
		c, _ := s.Config()
		c.BinaryPathName = `"` + InstalledExe() + `" service`
		c.StartType = mgr.StartAutomatic
		s.UpdateConfig(c)
	}
	defer s.Close()
	s.SetRecoveryActions([]mgr.RecoveryAction{
		{Type: mgr.ServiceRestart, Delay: 5 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 15 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 60 * time.Second},
	}, 86400)
	s.SetRecoveryActionsOnNonCrashFailures(true)
	if err := s.Start(); err != nil {
		return fmt.Errorf("start service: %w", err)
	}
	return nil
}

func waitStopped(s *mgr.Service, d time.Duration) {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		st, err := s.Query()
		if err != nil || st.State == svc.Stopped {
			return
		}
		time.Sleep(300 * time.Millisecond)
	}
}

// Uninstall stops and removes the service and files (run interactively).
func Uninstall() error {
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	if s, err := m.OpenService(ServiceName); err == nil {
		s.Control(svc.Stop)
		waitStopped(s, 20*time.Second)
		s.Delete()
		s.Close()
	}
	os.RemoveAll(dataDir())
	exe, _ := os.Executable()
	if strings.EqualFold(filepath.Clean(exe), filepath.Clean(InstalledExe())) {
		// Can't delete ourselves while running; leave it to a detached cmd.
		return detached(`ping -n 3 127.0.0.1 >nul & rmdir /s /q "` + installDir() + `"`)
	}
	return os.RemoveAll(installDir())
}

func detached(cmdline string) error {
	cmd := exec.Command("cmd.exe")
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CmdLine:       `cmd.exe /d /c "` + cmdline + `"`,
		CreationFlags: windows.CREATE_NO_WINDOW | windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP | windows.CREATE_BREAKAWAY_FROM_JOB,
	}
	return cmd.Start()
}

// scheduleUninstall is used by the "uninstall" job while running as the service.
func scheduleUninstall() error {
	return detached(fmt.Sprintf(`ping -n 4 127.0.0.1 >nul & sc.exe stop %s & ping -n 4 127.0.0.1 >nul & sc.exe delete %s & rmdir /s /q "%s" & rmdir /s /q "%s"`,
		ServiceName, ServiceName, installDir(), dataDir()))
}

func powerAction(reboot bool) error {
	flag := "/s"
	if reboot {
		flag = "/r"
	}
	return exec.Command("shutdown.exe", flag, "/t", "5", "/c", "OCeanic RMM requested "+map[bool]string{true: "restart", false: "shutdown"}[reboot]).Run()
}

// selfUpdate downloads the latest binary and swaps it in; the running exe is
// renamed (Windows allows renaming, not overwriting, a running image).
func (a *Agent) selfUpdate(ctx context.Context) error {
	exe, _ := os.Executable()
	tmp := exe + ".new"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	err = a.c.download(ctx, "windows-"+runtimeArch(), f)
	f.Close()
	if err != nil {
		os.Remove(tmp)
		return err
	}
	os.Remove(exe + ".old")
	if err := os.Rename(exe, exe+".old"); err != nil {
		return err
	}
	if err := os.Rename(tmp, exe); err != nil {
		os.Rename(exe+".old", exe)
		return err
	}
	return nil
}

// restartSelf exits non-zero so the SCM recovery policy restarts us on the new binary.
func restartSelf() {
	time.Sleep(2 * time.Second)
	os.Exit(1)
}

func hardwareInfo() (manufacturer, model, serial string) {
	if k, err := registry.OpenKey(registry.LOCAL_MACHINE, `HARDWARE\DESCRIPTION\System\BIOS`, registry.QUERY_VALUE); err == nil {
		manufacturer, _, _ = k.GetStringValue("SystemManufacturer")
		model, _, _ = k.GetStringValue("SystemProductName")
		k.Close()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-Command", "(Get-CimInstance Win32_BIOS).SerialNumber")
	prepareCmd(cmd)
	if out, err := cmd.Output(); err == nil {
		serial = strings.TrimSpace(string(out))
	}
	return
}

func installedSoftware() []protocol.Software {
	seen := map[string]bool{}
	var out []protocol.Software
	for _, path := range []string{
		`SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall`,
		`SOFTWARE\WOW6432Node\Microsoft\Windows\CurrentVersion\Uninstall`,
	} {
		k, err := registry.OpenKey(registry.LOCAL_MACHINE, path, registry.ENUMERATE_SUB_KEYS)
		if err != nil {
			continue
		}
		names, _ := k.ReadSubKeyNames(-1)
		k.Close()
		for _, n := range names {
			sk, err := registry.OpenKey(registry.LOCAL_MACHINE, path+`\`+n, registry.QUERY_VALUE)
			if err != nil {
				continue
			}
			name, _, _ := sk.GetStringValue("DisplayName")
			if sys, _, err := sk.GetIntegerValue("SystemComponent"); err == nil && sys == 1 {
				name = ""
			}
			ver, _, _ := sk.GetStringValue("DisplayVersion")
			pub, _, _ := sk.GetStringValue("Publisher")
			date, _, _ := sk.GetStringValue("InstallDate")
			sk.Close()
			if name == "" || seen[name+ver] {
				continue
			}
			seen[name+ver] = true
			out = append(out, protocol.Software{Name: name, Version: ver, Publisher: pub, InstallDate: date})
		}
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name) })
	return out
}

var startTypes = map[uint32]string{0: "boot", 1: "system", 2: "automatic", 3: "manual", 4: "disabled"}
var svcStates = map[svc.State]string{
	svc.Stopped: "stopped", svc.StartPending: "starting", svc.StopPending: "stopping", svc.Running: "running",
	svc.ContinuePending: "continuing", svc.PausePending: "pausing", svc.Paused: "paused",
}

func services() []protocol.Service {
	m, err := mgr.Connect()
	if err != nil {
		return nil
	}
	defer m.Disconnect()
	names, err := m.ListServices()
	if err != nil {
		return nil
	}
	var out []protocol.Service
	for _, n := range names {
		s, err := m.OpenService(n)
		if err != nil {
			continue
		}
		c, err1 := s.Config()
		st, err2 := s.Query()
		s.Close()
		if err1 != nil || err2 != nil || c.ServiceType&windows.SERVICE_WIN32 == 0 {
			continue
		}
		out = append(out, protocol.Service{Name: n, DisplayName: c.DisplayName, Status: svcStates[st.State], StartType: startTypes[c.StartType]})
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].DisplayName) < strings.ToLower(out[j].DisplayName) })
	return out
}

// loggedInUsers finds interactive users via the owners of explorer.exe.
func loggedInUsers() []string {
	procs, err := process.Processes()
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, p := range procs {
		n, err := p.Name()
		if err != nil || !strings.EqualFold(n, "explorer.exe") {
			continue
		}
		u, err := p.Username()
		if err != nil || u == "" || seen[u] {
			continue
		}
		seen[u] = true
		out = append(out, u)
	}
	sort.Strings(out)
	return out
}

func prettyPlatform(si protocol.SysInfo) string {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, `SOFTWARE\Microsoft\Windows NT\CurrentVersion`, registry.QUERY_VALUE)
	if err != nil {
		return si.Platform
	}
	defer k.Close()
	name, _, _ := k.GetStringValue("ProductName")
	disp, _, _ := k.GetStringValue("DisplayVersion")
	build, _, _ := k.GetStringValue("CurrentBuildNumber")
	// Windows 11 still reports "Windows 10" in ProductName; fix up by build number.
	var b int
	fmt.Sscanf(build, "%d", &b)
	if b >= 22000 {
		name = strings.Replace(name, "Windows 10", "Windows 11", 1)
	}
	if disp != "" {
		name += " " + disp
	}
	return strings.TrimSpace(name)
}
