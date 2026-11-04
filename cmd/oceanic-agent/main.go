// Command oceanic-agent is the OCeanic RMM endpoint agent.
//
// Downloaded from the dashboard, the binary carries its server URL and
// enrollment token, so on Windows you can simply double-click it (or run
// "setup.exe install --silent" from an RMM/GPO/Intune deployment).
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"

	"github.com/imshodj/oceanic/internal/agent"
	"github.com/imshodj/oceanic/internal/protocol"
)

const usage = `OCeanic RMM agent %s

Usage:
  oceanic-agent install [--silent] [--server URL --token TOKEN]
                         Install as a service (uses the config embedded by the
                         server unless --server/--token are given)
  oceanic-agent uninstall [--silent]
                         Stop and remove the service, binary and config
  oceanic-agent run      Run in the foreground (debugging / non-systemd hosts)
  oceanic-agent status   Show installed config
  oceanic-agent version  Print version

Double-clicking a downloaded installer on Windows performs "install".
`

func main() {
	if agent.IsService() {
		if err := agent.RunService(); err != nil {
			log.Fatal(err)
		}
		return
	}

	args := os.Args[1:]
	cmd := ""
	if len(args) > 0 {
		cmd, args = args[0], args[1:]
	}
	if cmd == "" {
		if _, err := agent.EmbeddedConfig(); err == nil && runtime.GOOS == "windows" {
			cmd = "install" // double-clicked installer
		} else {
			fmt.Printf(usage, protocol.AgentVersion)
			return
		}
	}

	switch cmd {
	case "install":
		os.Exit(doInstall(args))
	case "uninstall":
		os.Exit(doUninstall(args))
	case "run":
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		if err := agent.Run(ctx); err != nil && ctx.Err() == nil {
			log.Fatal(err)
		}
	case "service":
		// Launched with "service" arg but not under SCM: run in foreground.
		if err := agent.RunService(); err != nil {
			log.Fatal(err)
		}
	case "status":
		if ec, err := agent.EmbeddedConfig(); err == nil {
			fmt.Printf("embedded installer config: server=%s org=%q\n", ec.ServerURL, ec.OrgName)
		}
		c, err := agent.LoadConfig()
		if err != nil {
			fmt.Println("not installed:", err)
			os.Exit(1)
		}
		fmt.Printf("config:   %s\nserver:   %s\nagent id: %s\nenrolled: %v\n", agent.ConfigPath(), c.ServerURL, c.AgentID, c.Enrolled())
	case "version", "--version", "-v":
		fmt.Println(protocol.AgentVersion)
	default:
		fmt.Printf(usage, protocol.AgentVersion)
		os.Exit(2)
	}
}

func doInstall(args []string) int {
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	silent := fs.Bool("silent", false, "no dialogs")
	fs.BoolVar(silent, "S", false, "alias for --silent")
	fs.BoolVar(silent, "quiet", false, "alias for --silent")
	server := fs.String("server", "", "server URL (overrides embedded)")
	token := fs.String("token", "", "enrollment token (overrides embedded)")
	if err := fs.Parse(normalizeFlags(args)); err != nil {
		return 2
	}
	fail := func(msg string) int {
		fmt.Fprintln(os.Stderr, "ERROR:", msg)
		if !*silent {
			agent.MessageBox("OCeanic Agent Setup", msg, true)
		}
		return 1
	}

	cfg := &agent.Config{}
	if ec, err := agent.EmbeddedConfig(); err == nil {
		cfg.ServerURL, cfg.EnrollToken, cfg.OrgName = ec.ServerURL, ec.EnrollToken, ec.OrgName
		cfg.CFAccessClientID, cfg.CFAccessClientSecret = ec.CFAccessClientID, ec.CFAccessClientSecret
	}
	if *server != "" {
		cfg.ServerURL = strings.TrimRight(*server, "/")
	}
	if *token != "" {
		cfg.EnrollToken = *token
	}
	if cfg.ServerURL == "" || cfg.EnrollToken == "" {
		return fail("This agent has no embedded server configuration.\nDownload the installer from your OCeanic dashboard, or pass --server and --token.")
	}

	if !agent.IsElevated() {
		if runtime.GOOS == "windows" {
			relaunch := append([]string{"install"}, args...)
			if err := agent.Elevate(relaunch); err != nil {
				return fail("Administrator rights are required to install the agent.")
			}
			return 0
		}
		return fail("must be run as root (try sudo)")
	}

	fmt.Printf("Installing OCeanic agent for %q -> %s\n", cfg.OrgName, cfg.ServerURL)
	if err := agent.Install(cfg); err != nil {
		return fail("Installation failed: " + err.Error())
	}
	msg := "OCeanic agent installed and running.\n\nThis device will appear in your dashboard"
	if cfg.OrgName != "" {
		msg += " under \"" + cfg.OrgName + "\""
	}
	msg += " within a few seconds."
	fmt.Println(msg)
	if !*silent {
		agent.MessageBox("OCeanic Agent Setup", msg, false)
	}
	return 0
}

func doUninstall(args []string) int {
	silent := false
	for _, a := range normalizeFlags(args) {
		if a == "--silent" || a == "-S" || a == "--quiet" {
			silent = true
		}
	}
	if !agent.IsElevated() {
		if runtime.GOOS == "windows" {
			if err := agent.Elevate(append([]string{"uninstall"}, args...)); err != nil {
				return 1
			}
			return 0
		}
		fmt.Fprintln(os.Stderr, "must be run as root")
		return 1
	}
	if err := agent.Uninstall(); err != nil {
		fmt.Fprintln(os.Stderr, "uninstall:", err)
		if !silent {
			agent.MessageBox("OCeanic Agent", "Uninstall failed: "+err.Error(), true)
		}
		return 1
	}
	fmt.Println("OCeanic agent removed.")
	if !silent {
		agent.MessageBox("OCeanic Agent", "OCeanic agent removed.", false)
	}
	return 0
}

// normalizeFlags accepts Windows-style /S and /silent switches.
func normalizeFlags(args []string) []string {
	out := make([]string, 0, len(args))
	for _, a := range args {
		switch strings.ToLower(a) {
		case "/s", "/silent", "/quiet", "/q":
			out = append(out, "--silent")
		default:
			out = append(out, a)
		}
	}
	return out
}
