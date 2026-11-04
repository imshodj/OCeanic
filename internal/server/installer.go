package server

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"text/template"

	"github.com/imshodj/oceanic/internal/protocol"
	"github.com/imshodj/oceanic/internal/trailer"
)

// Prebuilt agent binaries shipped inside the Docker image (see Dockerfile).
var platforms = map[string]string{
	"windows-amd64": "oceanic-agent-windows-amd64.exe",
	"windows-arm64": "oceanic-agent-windows-arm64.exe",
	"linux-amd64":   "oceanic-agent-linux-amd64",
	"linux-arm64":   "oceanic-agent-linux-arm64",
	"linux-arm":     "oceanic-agent-linux-arm",
}

var platformOrder = []string{"windows-amd64", "windows-arm64", "linux-amd64", "linux-arm64", "linux-arm"}

func (s *Server) agentPath(platform string) (string, bool) {
	name, ok := platforms[platform]
	if !ok {
		return "", false
	}
	p := filepath.Join(s.cfg.AgentsDir, name)
	if _, err := os.Stat(p); err != nil {
		return "", false
	}
	return p, true
}

func (s *Server) embeddedConfig(r *http.Request, org org) *protocol.EmbeddedConfig {
	return &protocol.EmbeddedConfig{
		ServerURL:            s.publicURL(r),
		EnrollToken:          org.EnrollToken,
		OrgName:              org.Name,
		CFAccessClientID:     s.cfg.CFClientID,
		CFAccessClientSecret: s.cfg.CFClientSecret,
	}
}

var unsafeChars = regexp.MustCompile(`[^A-Za-z0-9_-]+`)

func installerFilename(orgName, platform string) string {
	slug := strings.Trim(unsafeChars.ReplaceAllString(orgName, "-"), "-")
	if slug == "" {
		slug = "Default"
	}
	if strings.HasPrefix(platform, "windows") {
		return fmt.Sprintf("OCeanic-Agent-%s-%s-Setup.exe", slug, strings.TrimPrefix(platform, "windows-"))
	}
	return fmt.Sprintf("oceanic-agent-%s-%s", strings.ToLower(slug), platform)
}

// serveAgentBinary streams the prebuilt agent and, when cfg is non-nil,
// appends the enrollment config so the binary becomes a one-click installer.
func (s *Server) serveAgentBinary(w http.ResponseWriter, r *http.Request, platform string, cfg *protocol.EmbeddedConfig, filename string) {
	p, ok := s.agentPath(platform)
	if !ok {
		writeErr(w, 404, "agent binary for "+platform+" is not available on this server")
		return
	}
	f, err := os.Open(p)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer f.Close()
	st, _ := f.Stat()
	var tail []byte
	if cfg != nil {
		if tail, err = trailer.Build(cfg); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(st.Size()+int64(len(tail)), 10))
	w.Header().Set("Cache-Control", "no-store")
	if filename != "" {
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))
	}
	if r.Method == http.MethodHead {
		return
	}
	io.Copy(w, f)
	w.Write(tail)
}

func (s *Server) orgByToken(token string) (org, bool) {
	var o org
	err := s.db.QueryRow(`SELECT id,name,enroll_token FROM orgs WHERE enroll_token=?`, token).Scan(&o.ID, &o.Name, &o.EnrollToken)
	return o, err == nil
}

func (s *Server) handlePublicAgentDownload(w http.ResponseWriter, r *http.Request) {
	o, ok := s.orgByToken(r.PathValue("token"))
	if !ok {
		writeErr(w, 404, "invalid or rotated installer link")
		return
	}
	platform := r.PathValue("platform")
	s.serveAgentBinary(w, r, platform, s.embeddedConfig(r, o), installerFilename(o.Name, platform))
}

func (s *Server) handleAdminInstallerDownload(w http.ResponseWriter, r *http.Request) {
	o, err := s.getOrg(r.PathValue("id"))
	if err != nil {
		writeErr(w, 404, "organization not found")
		return
	}
	platform := r.PathValue("platform")
	s.audit(user(r), "installer.download", o.ID, platform)
	s.serveAgentBinary(w, r, platform, s.embeddedConfig(r, o), installerFilename(o.Name, platform))
}

type scriptVars struct {
	Base, Token, Org, CFID, CFSecret string
}

var ps1Tmpl = template.Must(template.New("ps1").Parse(`# OCeanic RMM agent installer for {{.Org}}
# Usage (elevated PowerShell):  irm {{.Base}}/install/{{.Token}}/windows.ps1 | iex
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
$principal = New-Object Security.Principal.WindowsPrincipal([Security.Principal.WindowsIdentity]::GetCurrent())
if (-not $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
    Write-Error 'Please run this from an elevated (Administrator) PowerShell.'
    return
}
[Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12
$arch = if ($env:PROCESSOR_ARCHITECTURE -eq 'ARM64') { 'arm64' } else { 'amd64' }
$headers = @{}
{{- if .CFID}}
$headers['CF-Access-Client-Id'] = '{{.CFID}}'
$headers['CF-Access-Client-Secret'] = '{{.CFSecret}}'
{{- end}}
$exe = Join-Path $env:TEMP ('oceanic-setup-' + [guid]::NewGuid().ToString('N') + '.exe')
Write-Host "Downloading OCeanic agent ($arch)..."
Invoke-WebRequest -UseBasicParsing -Headers $headers -Uri "{{.Base}}/install/{{.Token}}/agent/windows-$arch" -OutFile $exe
Write-Host 'Installing service...'
$p = Start-Process -FilePath $exe -ArgumentList 'install','--silent' -Wait -PassThru -NoNewWindow
Remove-Item $exe -Force -ErrorAction SilentlyContinue
if ($p.ExitCode -ne 0) { Write-Error "Installer failed with exit code $($p.ExitCode)"; return }
Write-Host 'OCeanic agent installed. The device will appear in the dashboard within a few seconds.' -ForegroundColor Green
`))

var shTmpl = template.Must(template.New("sh").Parse(`#!/bin/sh
# OCeanic RMM agent installer for {{.Org}}
# Usage:  curl -fsSL {{.Base}}/install/{{.Token}}/linux.sh | sudo sh
set -eu
if [ "$(id -u)" -ne 0 ]; then echo "Please run as root (sudo)." >&2; exit 1; fi
case "$(uname -m)" in
  x86_64|amd64) ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  armv7l|armv6l|arm) ARCH=arm ;;
  *) echo "Unsupported architecture: $(uname -m)" >&2; exit 1 ;;
esac
TMP="$(mktemp)"
trap 'rm -f "$TMP"' EXIT
echo "Downloading OCeanic agent (linux-$ARCH)..."
if command -v curl >/dev/null 2>&1; then
  curl -fsSL {{if .CFID}}-H 'CF-Access-Client-Id: {{.CFID}}' -H 'CF-Access-Client-Secret: {{.CFSecret}}' {{end}}-o "$TMP" "{{.Base}}/install/{{.Token}}/agent/linux-$ARCH"
else
  wget -q {{if .CFID}}--header='CF-Access-Client-Id: {{.CFID}}' --header='CF-Access-Client-Secret: {{.CFSecret}}' {{end}}-O "$TMP" "{{.Base}}/install/{{.Token}}/agent/linux-$ARCH"
fi
chmod +x "$TMP"
"$TMP" install --silent
echo "OCeanic agent installed."
`))

func (s *Server) scriptVarsFor(r *http.Request, o org) scriptVars {
	return scriptVars{Base: s.publicURL(r), Token: o.EnrollToken, Org: o.Name, CFID: s.cfg.CFClientID, CFSecret: s.cfg.CFClientSecret}
}

func (s *Server) handleInstallPS1(w http.ResponseWriter, r *http.Request) {
	o, ok := s.orgByToken(r.PathValue("token"))
	if !ok {
		http.Error(w, "Write-Error 'Invalid or rotated installer link'", 404)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	ps1Tmpl.Execute(w, s.scriptVarsFor(r, o))
}

func (s *Server) handleInstallSh(w http.ResponseWriter, r *http.Request) {
	o, ok := s.orgByToken(r.PathValue("token"))
	if !ok {
		http.Error(w, "echo 'Invalid or rotated installer link' >&2; exit 1", 404)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	shTmpl.Execute(w, s.scriptVarsFor(r, o))
}

// handleInstallerInfo tells the UI which platforms can be built.
func (s *Server) handleInstallerInfo(w http.ResponseWriter, r *http.Request) {
	var avail []string
	for _, p := range platformOrder {
		if _, ok := s.agentPath(p); ok {
			avail = append(avail, p)
		}
	}
	writeJSON(w, 200, map[string]any{
		"platforms":        avail,
		"public_url":       s.publicURL(r),
		"public_url_set":   s.cfg.PublicURL != "",
		"cf_access_tokens": s.cfg.CFClientID != "",
	})
}
