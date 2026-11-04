// Package protocol defines the wire types shared by the OCeanic server and agent.
//
// All agent traffic is agent-initiated HTTPS (outbound only), which is what
// lets the whole system sit behind a Cloudflare Tunnel with zero open ports.
package protocol

import "time"

// AgentVersion is stamped at build time via -ldflags.
var AgentVersion = "dev"

// EnrollRequest is sent once by a fresh agent using an organization's enrollment token.
type EnrollRequest struct {
	EnrollToken string  `json:"enroll_token"`
	MachineID   string  `json:"machine_id"`
	Hostname    string  `json:"hostname"`
	SysInfo     SysInfo `json:"sysinfo"`
}

// EnrollResponse carries the credentials the agent uses for all later calls.
type EnrollResponse struct {
	AgentID     string `json:"agent_id"`
	AgentSecret string `json:"agent_secret"`
}

// SysInfo is the relatively static inventory of a device.
type SysInfo struct {
	Hostname        string     `json:"hostname"`
	OS              string     `json:"os"`       // windows, linux, darwin
	Platform        string     `json:"platform"` // e.g. "Microsoft Windows 11 Pro"
	PlatformVersion string     `json:"platform_version"`
	KernelVersion   string     `json:"kernel_version"`
	Arch            string     `json:"arch"`
	CPUModel        string     `json:"cpu_model"`
	CPUCores        int        `json:"cpu_cores"`
	MemTotal        uint64     `json:"mem_total"`
	BootTime        int64      `json:"boot_time"`
	Manufacturer    string     `json:"manufacturer,omitempty"`
	Model           string     `json:"model,omitempty"`
	SerialNumber    string     `json:"serial_number,omitempty"`
	Domain          string     `json:"domain,omitempty"`
	LoggedInUsers   []string   `json:"logged_in_users,omitempty"`
	Disks           []Disk     `json:"disks"`
	NICs            []NIC      `json:"nics"`
	Software        []Software `json:"software,omitempty"`
	Services        []Service  `json:"services,omitempty"`
	AgentVersion    string     `json:"agent_version"`
	CollectedAt     time.Time  `json:"collected_at"`
}

type Disk struct {
	Mount  string  `json:"mount"`
	FSType string  `json:"fstype"`
	Total  uint64  `json:"total"`
	Used   uint64  `json:"used"`
	Pct    float64 `json:"pct"`
}

type NIC struct {
	Name  string   `json:"name"`
	MAC   string   `json:"mac"`
	Addrs []string `json:"addrs"`
}

type Software struct {
	Name        string `json:"name"`
	Version     string `json:"version"`
	Publisher   string `json:"publisher"`
	InstallDate string `json:"install_date,omitempty"`
}

type Service struct {
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
	Status      string `json:"status"`
	StartType   string `json:"start_type"`
}

// Metrics is a lightweight, frequent health sample.
type Metrics struct {
	CPUPct    float64   `json:"cpu_pct"`
	MemPct    float64   `json:"mem_pct"`
	MemUsed   uint64    `json:"mem_used"`
	Uptime    uint64    `json:"uptime"`
	Disks     []Disk    `json:"disks"`
	PublicIP  string    `json:"public_ip,omitempty"`
	LocalIP   string    `json:"local_ip,omitempty"`
	Users     []string  `json:"users,omitempty"`
	SampledAt time.Time `json:"sampled_at"`
}

// CheckinRequest is the periodic heartbeat. SysInfo is only included when
// inventory is refreshed (on start and every few hours).
type CheckinRequest struct {
	AgentVersion string   `json:"agent_version"`
	Metrics      Metrics  `json:"metrics"`
	SysInfo      *SysInfo `json:"sysinfo,omitempty"`
}

type CheckinResponse struct {
	// Seconds until next heartbeat.
	Interval int `json:"interval"`
	// RefreshInventory asks the agent to send a full SysInfo next checkin.
	RefreshInventory bool `json:"refresh_inventory"`
}

// Job types.
const (
	JobScript    = "script"    // run Script with Shell
	JobReboot    = "reboot"    // reboot the machine
	JobShutdown  = "shutdown"  // power off
	JobInventory = "inventory" // send fresh inventory
	JobUninstall = "uninstall" // remove the agent
	JobUpdate    = "update"    // self-update agent binary from server
	JobTerminal  = "terminal"  // open an interactive PTY over WebSocket (job ID = session ID)
)

// Shells supported by script jobs.
const (
	ShellPowerShell = "powershell"
	ShellPwsh       = "pwsh"
	ShellCmd        = "cmd"
	ShellBash       = "bash"
	ShellSh         = "sh"
	ShellPython     = "python"
)

// Job is a unit of work delivered to an agent via long-poll.
type Job struct {
	ID         string `json:"id"`
	Type       string `json:"type"`
	Shell      string `json:"shell,omitempty"`
	Script     string `json:"script,omitempty"`
	Args       string `json:"args,omitempty"`
	TimeoutSec int    `json:"timeout_sec,omitempty"`
	Cols       int    `json:"cols,omitempty"` // terminal jobs: initial size
	Rows       int    `json:"rows,omitempty"`
}

// TermControl is a JSON text frame on a terminal WebSocket. Raw terminal
// bytes travel as binary frames in both directions.
type TermControl struct {
	Type    string `json:"type"` // resize | exit | error | ready
	Cols    int    `json:"cols,omitempty"`
	Rows    int    `json:"rows,omitempty"`
	Code    int    `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
}

type JobsResponse struct {
	Jobs []Job `json:"jobs"`
}

// JobResult is posted back when a job completes.
type JobResult struct {
	JobID      string    `json:"job_id"`
	ExitCode   int       `json:"exit_code"`
	Output     string    `json:"output"`
	Error      string    `json:"error,omitempty"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
}

// EmbeddedConfig is baked onto the end of generated installers.
type EmbeddedConfig struct {
	ServerURL            string `json:"server_url"`
	EnrollToken          string `json:"enroll_token"`
	OrgName              string `json:"org_name,omitempty"`
	CFAccessClientID     string `json:"cf_access_client_id,omitempty"`
	CFAccessClientSecret string `json:"cf_access_client_secret,omitempty"`
}
