package agent

import (
	"net"
	"os"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/imshodj/oceanic/internal/protocol"
	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/host"
	"github.com/shirou/gopsutil/v4/mem"
	psnet "github.com/shirou/gopsutil/v4/net"
)

// MachineID returns a stable per-OS-install identifier (MachineGuid on
// Windows, /etc/machine-id on Linux) so reinstalls map to the same device.
func MachineID() string {
	id, err := host.HostID()
	if err != nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(id))
}

var skipFS = map[string]bool{
	"tmpfs": true, "devtmpfs": true, "overlay": true, "squashfs": true, "proc": true, "sysfs": true,
	"cgroup": true, "cgroup2": true, "devpts": true, "mqueue": true, "nsfs": true, "autofs": true,
	"tracefs": true, "debugfs": true, "securityfs": true, "pstore": true, "bpf": true, "fusectl": true,
	"configfs": true, "hugetlbfs": true, "ramfs": true, "binfmt_misc": true, "efivarfs": true, "fuse.lxcfs": true,
	"iso9660": true, "udf": true, "nfs": false,
}

func collectDisks() []protocol.Disk {
	parts, err := disk.Partitions(false)
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var out []protocol.Disk
	for _, p := range parts {
		if skipFS[p.Fstype] || seen[p.Mountpoint] {
			continue
		}
		// macOS: "/" is the sealed system volume and user data lives on
		// /System/Volumes/Data; the other system volumes are noise.
		if runtime.GOOS == "darwin" && ((strings.HasPrefix(p.Mountpoint, "/System/Volumes/") && p.Mountpoint != "/System/Volumes/Data") ||
			strings.HasPrefix(p.Mountpoint, "/private/var/vm") || p.Fstype == "devfs" || p.Fstype == "nullfs") {
			continue
		}
		if runtime.GOOS == "linux" && (strings.HasPrefix(p.Mountpoint, "/snap/") || strings.HasPrefix(p.Mountpoint, "/var/lib/docker") || strings.HasPrefix(p.Mountpoint, "/boot/efi")) {
			continue
		}
		u, err := disk.Usage(p.Mountpoint)
		if err != nil || u.Total == 0 {
			continue
		}
		seen[p.Mountpoint] = true
		out = append(out, protocol.Disk{Mount: p.Mountpoint, FSType: p.Fstype, Total: u.Total, Used: u.Used, Pct: round1(u.UsedPercent)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Mount < out[j].Mount })
	return out
}

func round1(f float64) float64 { return float64(int(f*10+0.5)) / 10 }

// localIP returns the address used for outbound traffic (no packets are sent).
func localIP() string {
	c, err := net.Dial("udp", "1.1.1.1:80")
	if err != nil {
		return ""
	}
	defer c.Close()
	return c.LocalAddr().(*net.UDPAddr).IP.String()
}

// CollectMetrics takes a quick health sample (~1s, for CPU measurement).
func CollectMetrics() protocol.Metrics {
	m := protocol.Metrics{SampledAt: time.Now().UTC(), LocalIP: localIP()}
	if p, err := cpu.Percent(time.Second, false); err == nil && len(p) > 0 {
		m.CPUPct = round1(p[0])
	}
	if v, err := mem.VirtualMemory(); err == nil {
		m.MemPct, m.MemUsed = round1(v.UsedPercent), v.Used
	}
	if up, err := host.Uptime(); err == nil {
		m.Uptime = up
	}
	m.Disks = collectDisks()
	m.Users = loggedInUsers()
	return m
}

// CollectSysInfo gathers inventory. full=false skips the slow parts
// (software and services) for a quick enrollment.
func CollectSysInfo(full bool) protocol.SysInfo {
	si := protocol.SysInfo{
		OS:           runtime.GOOS,
		Arch:         runtime.GOARCH,
		AgentVersion: protocol.AgentVersion,
		CollectedAt:  time.Now().UTC(),
	}
	si.Hostname, _ = os.Hostname()
	if h, err := host.Info(); err == nil {
		si.Platform = h.Platform
		si.PlatformVersion = h.PlatformVersion
		si.KernelVersion = h.KernelVersion
		si.BootTime = int64(h.BootTime)
		if h.Hostname != "" {
			si.Hostname = h.Hostname
		}
	}
	if c, err := cpu.Info(); err == nil && len(c) > 0 {
		si.CPUModel = strings.TrimSpace(c[0].ModelName)
	}
	if n, err := cpu.Counts(true); err == nil {
		si.CPUCores = n
	}
	if v, err := mem.VirtualMemory(); err == nil {
		si.MemTotal = v.Total
	}
	si.Disks = collectDisks()
	if ifs, err := psnet.Interfaces(); err == nil {
		for _, i := range ifs {
			if i.HardwareAddr == "" || isLoopbackOrVirtual(i) {
				continue
			}
			nic := protocol.NIC{Name: i.Name, MAC: i.HardwareAddr}
			for _, a := range i.Addrs {
				if !strings.HasPrefix(a.Addr, "fe80") {
					nic.Addrs = append(nic.Addrs, a.Addr)
				}
			}
			if len(nic.Addrs) > 0 {
				si.NICs = append(si.NICs, nic)
			}
		}
	}
	si.Manufacturer, si.Model, si.SerialNumber = hardwareInfo()
	si.LoggedInUsers = loggedInUsers()
	si.Platform = prettyPlatform(si)
	if full {
		si.Software = installedSoftware()
		si.Services = services()
	}
	return si
}

func isLoopbackOrVirtual(i psnet.InterfaceStat) bool {
	for _, f := range i.Flags {
		if f == "loopback" {
			return true
		}
	}
	n := strings.ToLower(i.Name)
	for _, p := range []string{"veth", "docker", "br-", "virbr", "lo", "cni", "flannel", "cali", "utun", "awdl", "llw", "anpi", "ap1", "gif", "stf", "bridge"} {
		if strings.HasPrefix(n, p) {
			return true
		}
	}
	return false
}
