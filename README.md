# OCeanic RMM

A self-hosted remote monitoring and management (RMM) tool for homelabs, similar in spirit to NinjaOne, that runs from a single Docker container.

- **No open ports.** The server sits behind a Cloudflare Tunnel. Agents only make outbound HTTPS calls, so nothing on your router or firewall changes.
- **One-click Windows installers.** The dashboard generates a `Setup.exe` for each organization with the server URL and enrollment token built in. Double-click it, run it silently with `/S` (GPO, Intune, PDQ), or use a PowerShell one-liner.
- **Linux agents too.** Builds for x64, ARM64 and ARMv7 (Raspberry Pi), installed as a systemd service.

## Features

| | |
|---|---|
| **Monitoring** | Online/offline status, CPU, RAM and per-disk usage, uptime, logged-in users, local and public IP. Includes 7-day performance charts. |
| **Inventory** | OS and build, CPU, RAM, manufacturer, model, serial, NICs, installed software (registry, dpkg, rpm, apk, pacman) and services. |
| **Alerts** | Fires when a device goes offline, a disk reaches 90%, or memory reaches 95%. Shown on the dashboard and optionally pushed to a Discord, Slack, Mattermost or ntfy webhook. |
| **Remote scripting** | PowerShell, pwsh, cmd, bash, sh and Python, run as SYSTEM or root. Each job has a timeout and captures output and exit code. Jobs reach online agents in under a second through long-polling. Offline devices pick them up when they reconnect. |
| **Automation library** | Built-in scripts: Windows Update (list or install), winget upgrade-all, Defender scan, BitLocker status, SMART disk health, event log errors, temp cleanup, install any MSI/EXE from a URL, Linux package updates, health summary, Docker status. You can add your own. |
| **Actions** | Reboot, shutdown, start/stop/restart services, refresh inventory, self-update agent, remote uninstall. All of these work in bulk across selected devices. |
| **Organizations** | Group devices by site or client. Each organization has its own installer and an enrollment token you can rotate. |
| **Audit log** | Records every login, job, installer download and change. |

## Quick start

### 1. Create a Cloudflare Tunnel

1. Go to [Cloudflare Zero Trust](https://one.dash.cloudflare.com/), then **Networks → Tunnels → Create a tunnel**, and pick **Cloudflared**.
2. Name the tunnel (for example `homelab-rmm`). On the "Install connector" step, copy the **token**, which is the long string after `--token`. You don't need to install anything; docker compose runs the connector.
3. Under **Public Hostname**, add:
   - Subdomain/domain: `rmm.example.com`
   - Service: **HTTP** → `oceanic:8080`

### 2. Configure and start the stack

```bash
git clone https://github.com/imshodj/oceanic.git && cd oceanic
cp .env.example .env
# edit .env: CLOUDFLARE_TUNNEL_TOKEN, OCEANIC_PUBLIC_URL=https://rmm.example.com, OCEANIC_ADMIN_PASSWORD
docker compose up -d --build
docker compose logs -f oceanic        # shows a generated password if you left it blank
```

Open `https://rmm.example.com` and sign in.

### 3. Deploy agents

In the dashboard, go to **Deploy agents**. Then use one of these:

- **Windows (interactive):** click **Windows x64 (.exe)** and run the file on the target PC. It asks for UAC elevation, installs the `OCeanic RMM Agent` service, and shows a confirmation.
- **Windows (silent, GPO, Intune or any RMM):** `OCeanic-Agent-Homelab-amd64-Setup.exe /S`
- **Windows (one-liner, elevated PowerShell):**
  ```powershell
  irm https://rmm.example.com/install/<token>/windows.ps1 | iex
  ```
- **Linux:**
  ```bash
  curl -fsSL https://rmm.example.com/install/<token>/linux.sh | sudo sh
  ```

The device shows up within a few seconds.

> **SmartScreen / Defender:** the installers are not code-signed, so Windows may show "Windows protected your PC". Click **More info → Run anyway**. Some antivirus products flag any unsigned remote-management tool. If yours does, add an exclusion for `C:\Program Files\OCeanic\`.

## Securing the dashboard with Cloudflare Access (recommended)

Whoever controls the dashboard can run code as SYSTEM/root on every enrolled machine. Put Cloudflare Access (SSO, email OTP, etc.) in front of the UI, and let agent traffic bypass it.

In Zero Trust, go to **Access → Applications** and create three self-hosted applications on the same hostname:

| Application | Path | Policy |
|---|---|---|
| OCeanic dashboard | `rmm.example.com` (no path) | **Allow**: your email or identity provider |
| OCeanic agents | `rmm.example.com/api/agent` | **Bypass**: Everyone |
| OCeanic installers | `rmm.example.com/install` | **Bypass**: Everyone |

Cloudflare applies the most specific path first. Agent endpoints still authenticate on their own: each agent has a per-device secret, and installer links are gated by the organization's enrollment token.

If you'd rather put the whole hostname behind Access, create a **Service Token** and set `OCEANIC_CF_ACCESS_CLIENT_ID` and `OCEANIC_CF_ACCESS_CLIENT_SECRET`. The token gets built into every installer and one-liner, and agents send it on every request.

If you use Bot Fight Mode or strict WAF rules, add a skip rule for `/api/agent/*` so agent check-ins aren't challenged.

## Configuration

All settings are environment variables (see `.env.example`).

| Variable | Default | Purpose |
|---|---|---|
| `OCEANIC_PUBLIC_URL` | *(from request)* | URL built into installers. Set this to your tunnel hostname. |
| `OCEANIC_ADMIN_USER` | `admin` | Dashboard username |
| `OCEANIC_ADMIN_PASSWORD` | *(generated)* | If set, it resets the password on every start |
| `OCEANIC_ALERT_WEBHOOK` | — | Discord, Slack, Mattermost or ntfy URL for alerts |
| `OCEANIC_CF_ACCESS_CLIENT_ID` / `_SECRET` | — | Cloudflare Access service token for agents |
| `OCEANIC_CHECKIN_SECONDS` | `30` | Agent heartbeat interval |
| `OCEANIC_METRICS_RETENTION_DAYS` | `7` | How long performance history is kept |
| `OCEANIC_JOBS_RETENTION_DAYS` | `90` | How long finished job output is kept |
| `OCEANIC_LISTEN` | `:8080` | Listen address inside the container |

Data, a single SQLite file, lives in the `oceanic-data` volume at `/data/oceanic.db`. To back up, copy that file. Run `sqlite3 oceanic.db ".backup x.db"` first if the server is running.

## How it works

```
 Windows / Linux agent ──HTTPS (outbound only)──► Cloudflare edge ◄── cloudflared (outbound) ── oceanic container
        │                                                                                           │
        ├─ POST /api/agent/checkin   every 30s: metrics (+ inventory every 6h)                     SQLite
        ├─ GET  /api/agent/jobs      long-poll (25s); returns immediately when a job is queued
        └─ POST /api/agent/result    script output + exit code
```

- **Installer generation:** the image ships prebuilt agent binaries. When you download an installer, the server streams the binary for that platform and appends a small JSON trailer with the server URL, enrollment token and organization. Windows and Linux both ignore data after the end of an executable. When the agent runs, it reads its own trailer. No compiler or signing step runs at download time.
- **Enrollment:** the agent trades the organization token for a per-device ID and secret. These are stored in `C:\ProgramData\OCeanic\agent.json` (readable only by SYSTEM and Administrators) or `/etc/oceanic/agent.json` (mode 0600). The token itself is not kept. Reinstalling on the same machine reuses the existing device record, matched by machine GUID or `/etc/machine-id`.
- **Windows service:** `C:\Program Files\OCeanic\oceanic-agent.exe`, auto-start, with crash recovery that restarts it after 5s, 15s, then 60s. Logs go to `C:\ProgramData\OCeanic\agent.log`.
- **Self-update:** the **Update agent** action downloads the server's current agent build. The running binary is swapped out, and the service restarts on the new version.

### Agent CLI

```
oceanic-agent install [--silent] [--server URL --token TOKEN]
oceanic-agent uninstall [--silent]
oceanic-agent run        # foreground, for debugging or non-systemd hosts
oceanic-agent status
oceanic-agent version
```

To uninstall locally on Windows, run `"C:\Program Files\OCeanic\oceanic-agent.exe" uninstall` as Administrator, or use **Uninstall agent** in the dashboard.

## Building from source

Requires Go 1.26+.

```bash
make all      # dist/oceanic-server + dist/agents/* for every platform
make run      # serves on :8080 using ./data
make test     # vet (linux + windows) and unit tests
make docker   # builds oceanic-rmm:latest
```

## Security notes

- Agents run as SYSTEM or root, so treat dashboard access like domain admin. Use a strong password and Cloudflare Access.
- Admin sessions use HttpOnly, SameSite=Strict cookies, with an additional CSRF header check on every change. Logins lock out after 10 failures in 15 minutes.
- Agent secrets and session tokens are stored only as SHA-256 hashes, and the admin password as bcrypt.
- If an installer link leaks, use **Rotate token** on the Deploy page. Agents that are already installed keep working.
