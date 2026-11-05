package server

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/imshodj/oceanic/internal/protocol"
)

// ---------------- organizations ----------------

type org struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	EnrollToken string `json:"enroll_token"`
	CreatedAt   int64  `json:"created_at"`
	Devices     int    `json:"devices"`
	Online      int    `json:"online"`
}

func (s *Server) createOrg(name string) (org, error) {
	o := org{ID: newID(), Name: name, EnrollToken: randToken(24), CreatedAt: time.Now().Unix()}
	_, err := s.db.Exec(`INSERT INTO orgs(id,name,enroll_token,created_at) VALUES(?,?,?,?)`, o.ID, o.Name, o.EnrollToken, o.CreatedAt)
	return o, err
}

func (s *Server) getOrg(id string) (org, error) {
	var o org
	err := s.db.QueryRow(`SELECT id,name,enroll_token,created_at FROM orgs WHERE id=?`, id).Scan(&o.ID, &o.Name, &o.EnrollToken, &o.CreatedAt)
	return o, err
}

func (s *Server) onlineCutoff() int64 { return time.Now().Add(-s.cfg.OfflineAfter).Unix() }

func (s *Server) handleListOrgs(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.Query(`SELECT o.id,o.name,o.enroll_token,o.created_at,
		(SELECT COUNT(*) FROM devices d WHERE d.org_id=o.id),
		(SELECT COUNT(*) FROM devices d WHERE d.org_id=o.id AND d.last_seen>=?)
		FROM orgs o ORDER BY o.name COLLATE NOCASE`, s.onlineCutoff())
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer rows.Close()
	out := []org{}
	for rows.Next() {
		var o org
		rows.Scan(&o.ID, &o.Name, &o.EnrollToken, &o.CreatedAt, &o.Devices, &o.Online)
		out = append(out, o)
	}
	writeJSON(w, 200, out)
}

func (s *Server) handleCreateOrg(w http.ResponseWriter, r *http.Request) {
	var req struct{ Name string }
	if err := readJSON(r, &req, 4096); err != nil || strings.TrimSpace(req.Name) == "" {
		writeErr(w, 400, "name is required")
		return
	}
	o, err := s.createOrg(strings.TrimSpace(req.Name))
	if err != nil {
		writeErr(w, 409, "an organization with that name already exists")
		return
	}
	s.audit(user(r), "org.create", o.ID, o.Name)
	writeJSON(w, 201, o)
}

func (s *Server) handleRenameOrg(w http.ResponseWriter, r *http.Request) {
	var req struct{ Name string }
	if err := readJSON(r, &req, 4096); err != nil || strings.TrimSpace(req.Name) == "" {
		writeErr(w, 400, "name is required")
		return
	}
	if _, err := s.db.Exec(`UPDATE orgs SET name=? WHERE id=?`, strings.TrimSpace(req.Name), r.PathValue("id")); err != nil {
		writeErr(w, 409, "an organization with that name already exists")
		return
	}
	s.audit(user(r), "org.rename", r.PathValue("id"), req.Name)
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) handleDeleteOrg(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var n int
	s.db.QueryRow(`SELECT COUNT(*) FROM devices WHERE org_id=?`, id).Scan(&n)
	if n > 0 {
		writeErr(w, 409, "move or delete this organization's devices first")
		return
	}
	s.db.Exec(`DELETE FROM orgs WHERE id=?`, id)
	s.audit(user(r), "org.delete", id, "")
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) handleRotateToken(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	tok := randToken(24)
	if _, err := s.db.Exec(`UPDATE orgs SET enroll_token=? WHERE id=?`, tok, id); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	s.audit(user(r), "org.rotate_token", id, "")
	writeJSON(w, 200, map[string]string{"enroll_token": tok})
}

// ---------------- devices ----------------

type deviceRow struct {
	ID           string            `json:"id"`
	OrgID        string            `json:"org_id"`
	OrgName      string            `json:"org_name"`
	Hostname     string            `json:"hostname"`
	DisplayName  string            `json:"display_name"`
	OS           string            `json:"os"`
	Platform     string            `json:"platform"`
	AgentVersion string            `json:"agent_version"`
	RemoteIP     string            `json:"remote_ip"`
	Notes        string            `json:"notes"`
	EnrolledAt   int64             `json:"enrolled_at"`
	LastSeen     int64             `json:"last_seen"`
	Online       bool              `json:"online"`
	Metrics      protocol.Metrics  `json:"metrics"`
	SysInfo      *protocol.SysInfo `json:"sysinfo,omitempty"`
}

const deviceCols = `d.id,d.org_id,o.name,d.hostname,d.display_name,d.os,d.platform,d.agent_version,d.remote_ip,d.notes,d.enrolled_at,d.last_seen,d.metrics`

func (s *Server) scanDevice(sc interface{ Scan(...any) error }) (deviceRow, error) {
	var d deviceRow
	var m string
	err := sc.Scan(&d.ID, &d.OrgID, &d.OrgName, &d.Hostname, &d.DisplayName, &d.OS, &d.Platform, &d.AgentVersion, &d.RemoteIP, &d.Notes, &d.EnrolledAt, &d.LastSeen, &m)
	if err != nil {
		return d, err
	}
	json.Unmarshal([]byte(m), &d.Metrics)
	d.Online = d.LastSeen >= s.onlineCutoff()
	return d, nil
}

func (s *Server) listDevices(orgID string) ([]deviceRow, error) {
	q := `SELECT ` + deviceCols + ` FROM devices d JOIN orgs o ON o.id=d.org_id`
	var args []any
	if orgID != "" {
		q += ` WHERE d.org_id=?`
		args = append(args, orgID)
	}
	q += ` ORDER BY d.hostname COLLATE NOCASE`
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []deviceRow{}
	for rows.Next() {
		d, err := s.scanDevice(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, nil
}

func (s *Server) handleListDevices(w http.ResponseWriter, r *http.Request) {
	out, err := s.listDevices(r.URL.Query().Get("org"))
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, out)
}

func (s *Server) handleGetDevice(w http.ResponseWriter, r *http.Request) {
	row := s.db.QueryRow(`SELECT `+deviceCols+`, d.sysinfo FROM devices d JOIN orgs o ON o.id=d.org_id WHERE d.id=?`, r.PathValue("id"))
	var d deviceRow
	var m, si string
	err := row.Scan(&d.ID, &d.OrgID, &d.OrgName, &d.Hostname, &d.DisplayName, &d.OS, &d.Platform, &d.AgentVersion, &d.RemoteIP, &d.Notes, &d.EnrolledAt, &d.LastSeen, &m, &si)
	if err == sql.ErrNoRows {
		writeErr(w, 404, "device not found")
		return
	} else if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	json.Unmarshal([]byte(m), &d.Metrics)
	d.SysInfo = &protocol.SysInfo{}
	json.Unmarshal([]byte(si), d.SysInfo)
	d.Online = d.LastSeen >= s.onlineCutoff()
	writeJSON(w, 200, d)
}

func (s *Server) handleUpdateDevice(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req struct {
		DisplayName *string `json:"display_name"`
		Notes       *string `json:"notes"`
		OrgID       *string `json:"org_id"`
	}
	if err := readJSON(r, &req, 1<<16); err != nil {
		writeErr(w, 400, "bad request")
		return
	}
	if req.DisplayName != nil {
		s.db.Exec(`UPDATE devices SET display_name=? WHERE id=?`, strings.TrimSpace(*req.DisplayName), id)
	}
	if req.Notes != nil {
		s.db.Exec(`UPDATE devices SET notes=? WHERE id=?`, *req.Notes, id)
	}
	if req.OrgID != nil {
		if _, err := s.getOrg(*req.OrgID); err != nil {
			writeErr(w, 400, "unknown organization")
			return
		}
		s.db.Exec(`UPDATE devices SET org_id=? WHERE id=?`, *req.OrgID, id)
	}
	s.audit(user(r), "device.update", id, "")
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) handleDeleteDevice(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var host string
	s.db.QueryRow(`SELECT hostname FROM devices WHERE id=?`, id).Scan(&host)
	s.db.Exec(`DELETE FROM devices WHERE id=?`, id)
	s.hub.notify(id)
	s.audit(user(r), "device.delete", id, host)
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) handleDeviceMetrics(w http.ResponseWriter, r *http.Request) {
	hours, _ := strconv.Atoi(r.URL.Query().Get("hours"))
	if hours <= 0 || hours > 24*30 {
		hours = 24
	}
	since := time.Now().Add(-time.Duration(hours) * time.Hour).Unix()
	// Bucket so the chart stays ~300 points regardless of range.
	bucket := int64(hours*3600) / 300
	if bucket < 1 {
		bucket = 1
	}
	rows, err := s.db.Query(`SELECT (ts/?)*? AS b, AVG(cpu), AVG(mem), MAX(disk) FROM metrics_history
		WHERE device_id=? AND ts>=? GROUP BY b ORDER BY b`, bucket, bucket, r.PathValue("id"), since)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer rows.Close()
	type pt struct {
		T    int64   `json:"t"`
		CPU  float64 `json:"cpu"`
		Mem  float64 `json:"mem"`
		Disk float64 `json:"disk"`
	}
	out := []pt{}
	for rows.Next() {
		var p pt
		rows.Scan(&p.T, &p.CPU, &p.Mem, &p.Disk)
		out = append(out, p)
	}
	writeJSON(w, 200, out)
}

// ---------------- jobs ----------------

type jobRow struct {
	ID         string `json:"id"`
	DeviceID   string `json:"device_id"`
	Hostname   string `json:"hostname"`
	Type       string `json:"type"`
	Name       string `json:"name"`
	Shell      string `json:"shell"`
	Script     string `json:"script,omitempty"`
	TimeoutSec int    `json:"timeout_sec"`
	Status     string `json:"status"`
	ExitCode   *int   `json:"exit_code"`
	Output     string `json:"output,omitempty"`
	Error      string `json:"error"`
	CreatedBy  string `json:"created_by"`
	CreatedAt  int64  `json:"created_at"`
	SentAt     *int64 `json:"sent_at"`
	FinishedAt *int64 `json:"finished_at"`
}

const jobCols = `j.id,j.device_id,COALESCE(NULLIF(d.display_name,''),d.hostname),j.type,j.name,j.shell,j.timeout_sec,j.status,j.exit_code,j.error,j.created_by,j.created_at,j.sent_at,j.finished_at`

func scanJob(sc interface{ Scan(...any) error }, extra ...any) (jobRow, error) {
	var j jobRow
	dst := []any{&j.ID, &j.DeviceID, &j.Hostname, &j.Type, &j.Name, &j.Shell, &j.TimeoutSec, &j.Status, &j.ExitCode, &j.Error, &j.CreatedBy, &j.CreatedAt, &j.SentAt, &j.FinishedAt}
	err := sc.Scan(append(dst, extra...)...)
	return j, err
}

func (s *Server) queryJobs(where string, args ...any) ([]jobRow, error) {
	rows, err := s.db.Query(`SELECT `+jobCols+` FROM jobs j JOIN devices d ON d.id=j.device_id `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []jobRow{}
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, nil
}

func (s *Server) handleDeviceJobs(w http.ResponseWriter, r *http.Request) {
	out, err := s.queryJobs(`WHERE j.device_id=? ORDER BY j.created_at DESC LIMIT 100`, r.PathValue("id"))
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, out)
}

func (s *Server) handleListJobs(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	out, err := s.queryJobs(`ORDER BY j.created_at DESC LIMIT ?`, limit)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, out)
}

func (s *Server) handleGetJob(w http.ResponseWriter, r *http.Request) {
	row := s.db.QueryRow(`SELECT `+jobCols+`, j.script, j.output FROM jobs j JOIN devices d ON d.id=j.device_id WHERE j.id=?`, r.PathValue("id"))
	var script, output string
	j, err := scanJob(row, &script, &output)
	if err != nil {
		writeErr(w, 404, "job not found")
		return
	}
	j.Script, j.Output = script, output
	writeJSON(w, 200, j)
}

func (s *Server) handleCancelJob(w http.ResponseWriter, r *http.Request) {
	res, _ := s.db.Exec(`UPDATE jobs SET status='cancelled', finished_at=? WHERE id=? AND status='queued'`, time.Now().Unix(), r.PathValue("id"))
	if n, _ := res.RowsAffected(); n == 0 {
		writeErr(w, 409, "only queued jobs can be cancelled")
		return
	}
	s.audit(user(r), "job.cancel", r.PathValue("id"), "")
	writeJSON(w, 200, map[string]bool{"ok": true})
}

var validShells = map[string]bool{
	protocol.ShellPowerShell: true, protocol.ShellPwsh: true, protocol.ShellCmd: true,
	protocol.ShellBash: true, protocol.ShellSh: true, protocol.ShellZsh: true, protocol.ShellPython: true,
}

var validJobTypes = map[string]bool{
	protocol.JobScript: true, protocol.JobReboot: true, protocol.JobShutdown: true,
	protocol.JobInventory: true, protocol.JobUninstall: true, protocol.JobUpdate: true,
}

func (s *Server) handleCreateJobs(w http.ResponseWriter, r *http.Request) {
	var req struct {
		DeviceIDs  []string `json:"device_ids"`
		Type       string   `json:"type"`
		ScriptID   string   `json:"script_id"`
		Name       string   `json:"name"`
		Shell      string   `json:"shell"`
		Script     string   `json:"script"`
		TimeoutSec int      `json:"timeout_sec"`
	}
	if err := readJSON(r, &req, 1<<20); err != nil {
		writeErr(w, 400, "bad request")
		return
	}
	if req.Type == "" {
		req.Type = protocol.JobScript
	}
	if !validJobTypes[req.Type] {
		writeErr(w, 400, "unknown job type")
		return
	}
	if len(req.DeviceIDs) == 0 {
		writeErr(w, 400, "select at least one device")
		return
	}
	if req.Type == protocol.JobScript {
		if req.ScriptID != "" {
			var sc scriptRow
			err := s.db.QueryRow(`SELECT name,shell,body,timeout_sec FROM scripts WHERE id=?`, req.ScriptID).Scan(&sc.Name, &sc.Shell, &sc.Body, &sc.TimeoutSec)
			if err != nil {
				writeErr(w, 404, "script not found")
				return
			}
			req.Name, req.Shell, req.Script = sc.Name, sc.Shell, sc.Body
			if req.TimeoutSec == 0 {
				req.TimeoutSec = sc.TimeoutSec
			}
		}
		if !validShells[req.Shell] {
			writeErr(w, 400, "unknown shell")
			return
		}
		if strings.TrimSpace(req.Script) == "" {
			writeErr(w, 400, "script is empty")
			return
		}
		if req.Name == "" {
			req.Name = "Ad-hoc " + req.Shell
		}
	} else if req.Name == "" {
		req.Name = strings.ToUpper(req.Type[:1]) + req.Type[1:]
	}
	if req.TimeoutSec <= 0 || req.TimeoutSec > 24*3600 {
		req.TimeoutSec = 300
	}
	now := time.Now().Unix()
	var ids []string
	for _, dev := range req.DeviceIDs {
		id := newID()
		_, err := s.db.Exec(`INSERT INTO jobs(id,device_id,type,name,shell,script,timeout_sec,created_by,created_at) VALUES(?,?,?,?,?,?,?,?,?)`,
			id, dev, req.Type, req.Name, req.Shell, req.Script, req.TimeoutSec, user(r), now)
		if err != nil {
			continue // unknown device id (FK)
		}
		ids = append(ids, id)
		s.hub.notify(dev)
		s.audit(user(r), "job.create", dev, req.Type+": "+req.Name)
	}
	writeJSON(w, 201, map[string]any{"job_ids": ids})
}

// ---------------- scripts ----------------

type scriptRow struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Shell       string `json:"shell"`
	Body        string `json:"body"`
	OS          string `json:"os"`
	TimeoutSec  int    `json:"timeout_sec"`
	Builtin     bool   `json:"builtin"`
	UpdatedAt   int64  `json:"updated_at"`
}

func (s *Server) handleListScripts(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.Query(`SELECT id,name,description,shell,body,os,timeout_sec,builtin,updated_at FROM scripts ORDER BY builtin DESC, name COLLATE NOCASE`)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer rows.Close()
	out := []scriptRow{}
	for rows.Next() {
		var sc scriptRow
		rows.Scan(&sc.ID, &sc.Name, &sc.Description, &sc.Shell, &sc.Body, &sc.OS, &sc.TimeoutSec, &sc.Builtin, &sc.UpdatedAt)
		out = append(out, sc)
	}
	writeJSON(w, 200, out)
}

func (s *Server) handleSaveScript(w http.ResponseWriter, r *http.Request) {
	var sc scriptRow
	if err := readJSON(r, &sc, 1<<20); err != nil {
		writeErr(w, 400, "bad request")
		return
	}
	sc.Name = strings.TrimSpace(sc.Name)
	if sc.Name == "" || strings.TrimSpace(sc.Body) == "" {
		writeErr(w, 400, "name and body are required")
		return
	}
	if !validShells[sc.Shell] {
		writeErr(w, 400, "unknown shell")
		return
	}
	if sc.OS == "" {
		sc.OS = "any"
	}
	if sc.TimeoutSec <= 0 {
		sc.TimeoutSec = 300
	}
	now := time.Now().Unix()
	if id := r.PathValue("id"); id != "" {
		_, err := s.db.Exec(`UPDATE scripts SET name=?,description=?,shell=?,body=?,os=?,timeout_sec=?,updated_at=? WHERE id=?`,
			sc.Name, sc.Description, sc.Shell, sc.Body, sc.OS, sc.TimeoutSec, now, id)
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		sc.ID = id
		s.audit(user(r), "script.update", id, sc.Name)
	} else {
		sc.ID = newID()
		_, err := s.db.Exec(`INSERT INTO scripts(id,name,description,shell,body,os,timeout_sec,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?)`,
			sc.ID, sc.Name, sc.Description, sc.Shell, sc.Body, sc.OS, sc.TimeoutSec, now, now)
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		s.audit(user(r), "script.create", sc.ID, sc.Name)
	}
	writeJSON(w, 200, sc)
}

func (s *Server) handleDeleteScript(w http.ResponseWriter, r *http.Request) {
	s.db.Exec(`DELETE FROM scripts WHERE id=?`, r.PathValue("id"))
	s.audit(user(r), "script.delete", r.PathValue("id"), "")
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// ---------------- dashboard / audit ----------------

type alert struct {
	Severity string `json:"severity"` // critical, warning
	DeviceID string `json:"device_id"`
	Device   string `json:"device"`
	Message  string `json:"message"`
	Key      string `json:"-"`
}

func (s *Server) computeAlerts(devs []deviceRow) []alert {
	out := []alert{}
	for _, d := range devs {
		name := d.Hostname
		if d.DisplayName != "" {
			name = d.DisplayName
		}
		if !d.Online {
			out = append(out, alert{"critical", d.ID, name, "Device is offline (last seen " + time.Unix(d.LastSeen, 0).UTC().Format("2006-01-02 15:04 UTC") + ")", "offline:" + d.ID})
			continue
		}
		for _, disk := range d.Metrics.Disks {
			if disk.Pct >= 90 {
				out = append(out, alert{"warning", d.ID, name, "Disk " + disk.Mount + " is " + strconv.Itoa(int(disk.Pct)) + "% full", "disk:" + d.ID + ":" + disk.Mount})
			}
		}
		if d.Metrics.MemPct >= 95 {
			out = append(out, alert{"warning", d.ID, name, "Memory usage is " + strconv.Itoa(int(d.Metrics.MemPct)) + "%", "mem:" + d.ID})
		}
	}
	return out
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	devs, err := s.listDevices("")
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	var online int
	byOS := map[string]int{}
	for _, d := range devs {
		if d.Online {
			online++
		}
		byOS[d.OS]++
	}
	var queued, failed24 int
	s.db.QueryRow(`SELECT COUNT(*) FROM jobs WHERE status IN ('queued','sent')`).Scan(&queued)
	s.db.QueryRow(`SELECT COUNT(*) FROM jobs WHERE status='failed' AND created_at>=?`, time.Now().Add(-24*time.Hour).Unix()).Scan(&failed24)
	recent, _ := s.queryJobs(`ORDER BY j.created_at DESC LIMIT 8`)
	writeJSON(w, 200, map[string]any{
		"total":        len(devs),
		"online":       online,
		"offline":      len(devs) - online,
		"by_os":        byOS,
		"jobs_pending": queued,
		"jobs_failed":  failed24,
		"alerts":       s.computeAlerts(devs),
		"recent_jobs":  recent,
	})
}

func (s *Server) handleAudit(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.Query(`SELECT ts,actor,action,target,detail FROM audit ORDER BY ts DESC LIMIT 300`)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer rows.Close()
	type ev struct {
		TS     int64  `json:"ts"`
		Actor  string `json:"actor"`
		Action string `json:"action"`
		Target string `json:"target"`
		Detail string `json:"detail"`
	}
	out := []ev{}
	for rows.Next() {
		var e ev
		rows.Scan(&e.TS, &e.Actor, &e.Action, &e.Target, &e.Detail)
		out = append(out, e)
	}
	writeJSON(w, 200, out)
}
