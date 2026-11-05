package server

import "time"

type seedScript struct {
	id, name, desc, shell, os, body string
	timeout                         int
}

// Built-in automation library, inspired by the "scripts" a typical RMM ships.
// They're only inserted once, so you can edit or delete them freely.
var builtinScripts = []seedScript{
	{"builtin-win-updates-list", "Windows Update: list pending updates", "Queries the Windows Update Agent for applicable, not-yet-installed updates.", "powershell", "windows", `$s = New-Object -ComObject Microsoft.Update.Session
$r = $s.CreateUpdateSearcher().Search("IsInstalled=0 and IsHidden=0")
if ($r.Updates.Count -eq 0) { "No pending updates."; exit 0 }
$r.Updates | ForEach-Object {
  [pscustomobject]@{ Title = $_.Title; KB = ($_.KBArticleIDs -join ','); SizeMB = [math]::Round($_.MaxDownloadSize/1MB,1); Severity = $_.MsrcSeverity }
} | Format-Table -AutoSize | Out-String -Width 250
"$($r.Updates.Count) update(s) pending."`, 600},

	{"builtin-win-updates-install", "Windows Update: install all (no reboot)", "Downloads and installs every pending update. Reboot separately if required.", "powershell", "windows", `$s = New-Object -ComObject Microsoft.Update.Session
$r = $s.CreateUpdateSearcher().Search("IsInstalled=0 and IsHidden=0 and Type='Software'")
if ($r.Updates.Count -eq 0) { "No pending updates."; exit 0 }
$coll = New-Object -ComObject Microsoft.Update.UpdateColl
foreach ($u in $r.Updates) { if (-not $u.EulaAccepted) { $u.AcceptEula() }; [void]$coll.Add($u); "Queued: $($u.Title)" }
$d = $s.CreateUpdateDownloader(); $d.Updates = $coll; "Downloading..."; [void]$d.Download()
$i = $s.CreateUpdateInstaller(); $i.Updates = $coll; "Installing..."
$res = $i.Install()
"Result code: $($res.ResultCode)  (2 = succeeded, 3 = succeeded with errors)"
"Reboot required: $($res.RebootRequired)"`, 7200},

	{"builtin-win-winget-upgrade", "winget: upgrade all apps", "Runs winget as SYSTEM to upgrade every package with an available update.", "powershell", "windows", `$wg = Get-ChildItem "$env:ProgramFiles\WindowsApps\Microsoft.DesktopAppInstaller_*_x64__8wekyb3d8bbwe\winget.exe" -ErrorAction SilentlyContinue | Sort-Object FullName -Descending | Select-Object -First 1
if (-not $wg) { "winget not found on this machine."; exit 1 }
& $wg.FullName upgrade --all --silent --accept-source-agreements --accept-package-agreements --disable-interactivity`, 3600},

	{"builtin-win-top-procs", "Top processes (CPU & memory)", "Shows the 15 busiest processes.", "powershell", "windows", `Get-Process | Sort-Object CPU -Descending | Select-Object -First 15 Name, Id, @{n='CPU(s)';e={[math]::Round($_.CPU,1)}}, @{n='RAM(MB)';e={[math]::Round($_.WorkingSet64/1MB)}} | Format-Table -AutoSize | Out-String -Width 200`, 120},

	{"builtin-win-stopped-services", "Stopped automatic services", "Lists services set to Automatic that aren't running.", "powershell", "windows", `Get-CimInstance Win32_Service -Filter "StartMode='Auto' AND State<>'Running'" | Select-Object Name, DisplayName, State, ExitCode | Format-Table -AutoSize | Out-String -Width 200`, 120},

	{"builtin-win-events", "Recent critical/error events (24h)", "System & Application log errors from the last day.", "powershell", "windows", `Get-WinEvent -FilterHashtable @{LogName='System','Application'; Level=1,2; StartTime=(Get-Date).AddDays(-1)} -MaxEvents 50 -ErrorAction SilentlyContinue |
  Select-Object TimeCreated, LogName, ProviderName, Id, @{n='Message';e={($_.Message -split "\r?\n")[0]}} | Format-Table -AutoSize -Wrap | Out-String -Width 250`, 180},

	{"builtin-win-cleanup", "Clean temp files", "Empties Windows and user temp folders and the recycle bin.", "powershell", "windows", `$before = (Get-PSDrive C).Free
$paths = @("$env:windir\Temp\*", "C:\Users\*\AppData\Local\Temp\*")
foreach ($p in $paths) { Remove-Item $p -Recurse -Force -ErrorAction SilentlyContinue }
Clear-RecycleBin -Force -ErrorAction SilentlyContinue
$freed = ((Get-PSDrive C).Free - $before) / 1MB
"Freed {0:N0} MB on C:" -f $freed`, 900},

	{"builtin-win-defender", "Defender: status + quick scan", "Shows Microsoft Defender health and runs a quick scan.", "powershell", "windows", `Get-MpComputerStatus | Select-Object AMServiceEnabled, AntivirusEnabled, RealTimeProtectionEnabled, AntivirusSignatureLastUpdated, QuickScanAge | Format-List | Out-String
Update-MpSignature -ErrorAction SilentlyContinue
Start-MpScan -ScanType QuickScan
"Quick scan complete."
Get-MpThreatDetection | Select-Object -First 10 | Format-Table -AutoSize | Out-String`, 3600},

	{"builtin-win-disk-health", "Disk health (SMART)", "Physical disk health and reliability counters.", "powershell", "windows", `Get-PhysicalDisk | Select-Object FriendlyName, MediaType, HealthStatus, OperationalStatus, @{n='SizeGB';e={[math]::Round($_.Size/1GB)}} | Format-Table -AutoSize | Out-String
Get-PhysicalDisk | Get-StorageReliabilityCounter -ErrorAction SilentlyContinue | Select-Object DeviceId, Temperature, Wear, ReadErrorsTotal, WriteErrorsTotal, PowerOnHours | Format-Table -AutoSize | Out-String`, 120},

	{"builtin-win-bitlocker", "BitLocker status", "Encryption status of every volume.", "powershell", "windows", `Get-BitLockerVolume | Select-Object MountPoint, VolumeStatus, ProtectionStatus, EncryptionPercentage, EncryptionMethod | Format-Table -AutoSize | Out-String`, 60},

	{"builtin-win-install-url", "Install software from URL (MSI/EXE)", "Edit the URL and arguments, then run. Works for any MSI or silent-capable EXE installer.", "powershell", "windows", `# ---- edit these ----
$Url  = 'https://example.com/path/to/installer.msi'
$InstallArgs = '/qn /norestart'          # MSI: /qn /norestart   |  EXE: often /S, /silent or /quiet
# --------------------
$name = [IO.Path]::GetFileName(([Uri]$Url).AbsolutePath)
if (-not $name) { $name = 'installer.msi' }
$file = Join-Path $env:TEMP $name
[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
"Downloading $Url"
Invoke-WebRequest -UseBasicParsing -Uri $Url -OutFile $file
if ($file -like '*.msi') {
  $p = Start-Process msiexec.exe -ArgumentList "/i ""$file"" $InstallArgs /l*v ""$file.log""" -Wait -PassThru
} else {
  $p = Start-Process $file -ArgumentList $InstallArgs -Wait -PassThru
}
"Installer exit code: $($p.ExitCode)  (0 = success, 3010 = success, reboot required)"
Remove-Item $file -Force -ErrorAction SilentlyContinue
exit $(if ($p.ExitCode -eq 3010) { 0 } else { $p.ExitCode })`, 1800},

	{"builtin-mac-updates-list", "macOS: list software updates", "Shows pending macOS and Apple app updates.", "zsh", "darwin", `softwareupdate --list 2>&1`, 600},

	{"builtin-mac-updates-install", "macOS: install all updates (no restart)", "Installs every recommended update. Major OS upgrades on Apple Silicon may still need a user to approve them on the Mac.", "zsh", "darwin", `softwareupdate --install --all --agree-to-license 2>&1
echo
softwareupdate --list 2>&1 | grep -qi restart && echo "*** One or more updates need a restart ***" || true`, 7200},

	{"builtin-mac-health", "macOS: health summary", "OS version, uptime, memory pressure, disks, and top processes.", "zsh", "darwin", `sw_vers
echo; echo "== Uptime =="; uptime
echo; echo "== Memory pressure =="; memory_pressure | tail -n 1
echo; echo "== Disks =="; df -h / /System/Volumes/Data 2>/dev/null
echo; echo "== Battery =="; pmset -g batt | tail -n +2
echo; echo "== Top processes (CPU) =="; ps -Ao pid,%cpu,%mem,comm -r | head -n 11`, 120},

	{"builtin-mac-security", "macOS: security status", "FileVault, Gatekeeper, SIP, firewall, and automatic-update settings.", "zsh", "darwin", `echo "FileVault:   $(fdesetup status | head -n 1)"
echo "Gatekeeper:  $(spctl --status 2>&1)"
echo "SIP:         $(csrutil status 2>&1)"
echo "Firewall:    $(/usr/libexec/ApplicationFirewall/socketfilterfw --getglobalstate 2>&1)"
echo "Auto-update: $(defaults read /Library/Preferences/com.apple.SoftwareUpdate AutomaticallyInstallMacOSUpdates 2>/dev/null || echo 'not set (system default)')"`, 120},

	{"builtin-linux-updates", "Linux: check for package updates", "Works with apt, dnf, yum, zypper, pacman and apk.", "sh", "linux", `if command -v apt-get >/dev/null; then apt-get update -qq && apt list --upgradable 2>/dev/null
elif command -v dnf >/dev/null; then dnf -q check-update; true
elif command -v yum >/dev/null; then yum -q check-update; true
elif command -v zypper >/dev/null; then zypper -q list-updates
elif command -v pacman >/dev/null; then pacman -Sy >/dev/null && pacman -Qu
elif command -v apk >/dev/null; then apk update -q && apk version -l '<'
else echo "No supported package manager found"; exit 1; fi`, 600},

	{"builtin-linux-upgrade", "Linux: install package updates", "Non-interactive upgrade of all packages.", "sh", "linux", `export DEBIAN_FRONTEND=noninteractive
if command -v apt-get >/dev/null; then apt-get update -qq && apt-get -y -o Dpkg::Options::=--force-confold upgrade
elif command -v dnf >/dev/null; then dnf -y upgrade
elif command -v yum >/dev/null; then yum -y update
elif command -v zypper >/dev/null; then zypper -n update
elif command -v pacman >/dev/null; then pacman -Syu --noconfirm
elif command -v apk >/dev/null; then apk upgrade
else echo "No supported package manager found"; exit 1; fi
[ -f /var/run/reboot-required ] && echo "*** Reboot required ***" || true`, 3600},

	{"builtin-linux-health", "Linux: health summary", "Uptime, load, memory, disks, failed units, top processes.", "sh", "linux", `echo "== Uptime =="; uptime
echo; echo "== Memory =="; free -h
echo; echo "== Disks =="; df -hT -x tmpfs -x devtmpfs -x overlay 2>/dev/null || df -h
echo; echo "== Failed systemd units =="; systemctl --failed --no-legend 2>/dev/null || echo "n/a"
echo; echo "== Top processes =="; ps aux --sort=-%cpu 2>/dev/null | head -n 11 || ps | head -n 11`, 120},

	{"builtin-linux-docker", "Docker: containers & disk usage", "Lists containers and Docker disk usage.", "sh", "linux", `command -v docker >/dev/null || { echo "docker not installed"; exit 1; }
docker ps -a --format 'table {{.Names}}\t{{.Image}}\t{{.Status}}'
echo; docker system df`, 120},
}

func (s *Server) seedScripts() error {
	now := time.Now().Unix()
	for _, b := range builtinScripts {
		_, err := s.db.Exec(`INSERT OR IGNORE INTO scripts(id,name,description,shell,body,os,timeout_sec,builtin,created_at,updated_at) VALUES(?,?,?,?,?,?,?,1,?,?)`,
			b.id, b.name, b.desc, b.shell, b.body, b.os, b.timeout, now, now)
		if err != nil {
			return err
		}
	}
	return nil
}
