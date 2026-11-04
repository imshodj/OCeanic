//go:build darwin

package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/imshodj/oceanic/internal/protocol"
)

const (
	launchdLabel = "com.oceanic.agent"
	plistPath    = "/Library/LaunchDaemons/com.oceanic.agent.plist"
	macLogDir    = "/Library/Logs/OCeanic"
)

func InstalledExe() string { return "/usr/local/oceanic/oceanic-agent" }

// AbandonProcessGroup lets the self-uninstall script and desktop helpers
// outlive a restart of the daemon; KeepAlive restarts us after self-update.
const plistTmpl = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key><string>%s</string>
	<key>ProgramArguments</key>
	<array><string>%s</string><string>run</string></array>
	<key>RunAtLoad</key><true/>
	<key>KeepAlive</key><true/>
	<key>ThrottleInterval</key><integer>5</integer>
	<key>AbandonProcessGroup</key><true/>
	<key>StandardOutPath</key><string>%s/agent.log</string>
	<key>StandardErrorPath</key><string>%s/agent.log</string>
</dict>
</plist>
`

func Install(cfg *Config) error {
	if !IsElevated() {
		return fmt.Errorf("must be run as root (sudo)")
	}
	exec.Command("launchctl", "bootout", "system/"+launchdLabel).Run()
	if err := os.MkdirAll(filepath.Dir(InstalledExe()), 0o755); err != nil {
		return err
	}
	os.MkdirAll(macLogDir, 0o755)
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
	// Downloads via curl aren't quarantined, but strip the flag in case the
	// binary arrived through a browser.
	exec.Command("xattr", "-d", "com.apple.quarantine", InstalledExe()).Run()

	if old, err := LoadConfig(); err == nil && old.Enrolled() && strings.EqualFold(old.ServerURL, cfg.ServerURL) {
		cfg.AgentID, cfg.AgentSecret = old.AgentID, old.AgentSecret
	}
	if err := cfg.Save(); err != nil {
		return err
	}
	plist := fmt.Sprintf(plistTmpl, launchdLabel, InstalledExe(), macLogDir, macLogDir)
	if err := os.WriteFile(plistPath, []byte(plist), 0o644); err != nil {
		return err
	}
	if out, err := exec.Command("launchctl", "bootstrap", "system", plistPath).CombinedOutput(); err != nil {
		// Older macOS: fall back to the legacy verb.
		if out2, err2 := exec.Command("launchctl", "load", "-w", plistPath).CombinedOutput(); err2 != nil {
			return fmt.Errorf("launchctl bootstrap: %v: %s / %s", err, out, out2)
		}
	}
	exec.Command("launchctl", "enable", "system/"+launchdLabel).Run()
	exec.Command("launchctl", "kickstart", "-k", "system/"+launchdLabel).Run()
	return nil
}

func Uninstall() error {
	exec.Command("launchctl", "bootout", "system/"+launchdLabel).Run()
	os.Remove(plistPath)
	os.RemoveAll(filepath.Dir(InstalledExe()))
	os.RemoveAll(filepath.Dir(ConfigPath()))
	os.RemoveAll("/var/lib/oceanic")
	return nil
}

func scheduleUninstall() error {
	script := fmt.Sprintf("sleep 3; launchctl bootout system/%s; rm -f %s; rm -rf %s %s /var/lib/oceanic",
		launchdLabel, plistPath, filepath.Dir(InstalledExe()), filepath.Dir(ConfigPath()))
	cmd := exec.Command("/bin/sh", "-c", script)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return cmd.Start()
}

func sysctl(name string) string {
	return strings.TrimSpace(string(runOut("sysctl", "-n", name)))
}

func hardwareInfo() (manufacturer, model, serial string) {
	manufacturer = "Apple"
	model = sysctl("hw.model")
	var hw struct {
		SPHardwareDataType []struct {
			MachineName  string `json:"machine_name"`
			MachineModel string `json:"machine_model"`
			ModelNumber  string `json:"model_number"`
			Serial       string `json:"serial_number"`
		}
	}
	if json.Unmarshal(runOut("system_profiler", "-json", "SPHardwareDataType"), &hw) == nil && len(hw.SPHardwareDataType) > 0 {
		h := hw.SPHardwareDataType[0]
		if h.MachineName != "" {
			model = strings.TrimSpace(h.MachineName + " (" + h.MachineModel + ")")
		}
		serial = h.Serial
	}
	return
}

func installedSoftware() []protocol.Software {
	var apps struct {
		SPApplicationsDataType []struct {
			Name     string   `json:"_name"`
			Version  string   `json:"version"`
			Obtained string   `json:"obtained_from"`
			Path     string   `json:"path"`
			Modified string   `json:"lastModified"`
			SignedBy []string `json:"signed_by"`
		}
	}
	if json.Unmarshal(runOut("system_profiler", "-json", "-detailLevel", "mini", "SPApplicationsDataType"), &apps) != nil {
		return nil
	}
	var out []protocol.Software
	seen := map[string]bool{}
	for _, a := range apps.SPApplicationsDataType {
		// Skip helper apps buried inside other bundles and system internals.
		if strings.Contains(a.Path, ".app/") || strings.HasPrefix(a.Path, "/System/Library/") || strings.HasPrefix(a.Path, "/Library/Apple/") {
			continue
		}
		if seen[a.Name+a.Version] {
			continue
		}
		seen[a.Name+a.Version] = true
		pub := a.Obtained
		if len(a.SignedBy) > 0 {
			pub = strings.TrimPrefix(a.SignedBy[0], "Developer ID Application: ")
		}
		out = append(out, protocol.Software{Name: a.Name, Version: a.Version, Publisher: pub, InstallDate: a.Modified})
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name) })
	return out
}

// services lists system launchd jobs (daemons).
func services() []protocol.Service {
	var out []protocol.Service
	for _, l := range strings.Split(string(runOut("launchctl", "list")), "\n")[1:] {
		f := strings.Fields(l)
		if len(f) < 3 || strings.HasPrefix(f[2], "com.apple.") {
			continue
		}
		status := "stopped"
		if f[0] != "-" {
			status = "running"
		} else if f[1] != "0" {
			status = "failed"
		}
		out = append(out, protocol.Service{Name: f[2], DisplayName: f[2], Status: status, StartType: "launchd"})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func prettyPlatform(si protocol.SysInfo) string {
	name := strings.TrimSpace(string(runOut("sw_vers", "-productName")))
	ver := strings.TrimSpace(string(runOut("sw_vers", "-productVersion")))
	if name == "" {
		name = "macOS"
	}
	return strings.TrimSpace(name + " " + ver)
}
