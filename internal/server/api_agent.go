package server

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"time"

	"github.com/imshodj/oceanic/internal/protocol"
)

const longPollWait = 25 * time.Second // well under Cloudflare's 100s proxy timeout

func (s *Server) handleEnroll(w http.ResponseWriter, r *http.Request) {
	var req protocol.EnrollRequest
	if err := readJSON(r, &req, 4<<20); err != nil {
		writeErr(w, 400, "bad request")
		return
	}
	var orgID string
	if err := s.db.QueryRow(`SELECT id FROM orgs WHERE enroll_token=?`, req.EnrollToken).Scan(&orgID); err != nil {
		writeErr(w, 403, "invalid enrollment token")
		return
	}
	if req.Hostname == "" {
		req.Hostname = req.SysInfo.Hostname
	}
	secret := randToken(32)
	sysJSON, _ := json.Marshal(req.SysInfo)
	now := time.Now().Unix()

	// Re-installing on the same machine reuses the existing device record.
	var id string
	if req.MachineID != "" {
		s.db.QueryRow(`SELECT id FROM devices WHERE org_id=? AND machine_id=?`, orgID, req.MachineID).Scan(&id)
	}
	if id != "" {
		_, err := s.db.Exec(`UPDATE devices SET secret_hash=?, hostname=?, os=?, platform=?, agent_version=?, sysinfo=?, remote_ip=?, last_seen=? WHERE id=?`,
			sha(secret), req.Hostname, req.SysInfo.OS, req.SysInfo.Platform, req.SysInfo.AgentVersion, string(sysJSON), clientIP(r), now, id)
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
	} else {
		id = newID()
		_, err := s.db.Exec(`INSERT INTO devices(id,org_id,secret_hash,machine_id,hostname,os,platform,agent_version,sysinfo,remote_ip,enrolled_at,last_seen)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
			id, orgID, sha(secret), req.MachineID, req.Hostname, req.SysInfo.OS, req.SysInfo.Platform, req.SysInfo.AgentVersion, string(sysJSON), clientIP(r), now, now)
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
	}
	s.audit("agent:"+req.Hostname, "device.enroll", id, clientIP(r))
	writeJSON(w, 200, protocol.EnrollResponse{AgentID: id, AgentSecret: secret})
}

func (s *Server) handleCheckin(w http.ResponseWriter, r *http.Request) {
	id := deviceID(r)
	var req protocol.CheckinRequest
	if err := readJSON(r, &req, 8<<20); err != nil {
		writeErr(w, 400, "bad request")
		return
	}
	now := time.Now().Unix()
	mJSON, _ := json.Marshal(req.Metrics)
	if req.SysInfo != nil {
		sJSON, _ := json.Marshal(req.SysInfo)
		s.db.Exec(`UPDATE devices SET sysinfo=?, hostname=?, os=?, platform=?, refresh_inv=0 WHERE id=?`,
			string(sJSON), req.SysInfo.Hostname, req.SysInfo.OS, req.SysInfo.Platform, id)
	}
	s.db.Exec(`UPDATE devices SET metrics=?, last_seen=?, agent_version=?, remote_ip=? WHERE id=?`,
		string(mJSON), now, req.AgentVersion, clientIP(r), id)

	var maxDisk float64
	for _, d := range req.Metrics.Disks {
		if d.Pct > maxDisk {
			maxDisk = d.Pct
		}
	}
	s.db.Exec(`INSERT INTO metrics_history(device_id,ts,cpu,mem,disk) VALUES(?,?,?,?,?)`, id, now, req.Metrics.CPUPct, req.Metrics.MemPct, maxDisk)

	var refresh int
	s.db.QueryRow(`SELECT refresh_inv FROM devices WHERE id=?`, id).Scan(&refresh)
	writeJSON(w, 200, protocol.CheckinResponse{Interval: s.cfg.CheckinSec, RefreshInventory: refresh == 1})
}

// claimJobs atomically moves queued jobs to "sent" and returns them.
func (s *Server) claimJobs(id string) ([]protocol.Job, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.Query(`SELECT id,type,shell,script,timeout_sec FROM jobs WHERE device_id=? AND status='queued' ORDER BY created_at LIMIT 20`, id)
	if err != nil {
		return nil, err
	}
	var jobs []protocol.Job
	for rows.Next() {
		var j protocol.Job
		if err := rows.Scan(&j.ID, &j.Type, &j.Shell, &j.Script, &j.TimeoutSec); err != nil {
			rows.Close()
			return nil, err
		}
		jobs = append(jobs, j)
	}
	rows.Close()
	now := time.Now().Unix()
	for _, j := range jobs {
		if _, err := tx.Exec(`UPDATE jobs SET status='sent', sent_at=? WHERE id=?`, now, j.ID); err != nil {
			return nil, err
		}
	}
	return jobs, tx.Commit()
}

func (s *Server) handleAgentJobs(w http.ResponseWriter, r *http.Request) {
	id := deviceID(r)
	deadline := time.NewTimer(longPollWait)
	defer deadline.Stop()
	for {
		// Grab the waiter channel *before* checking the DB so a job queued
		// in between is never missed.
		wake := s.hub.wait(id)
		jobs, err := s.claimJobs(id)
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		if len(jobs) > 0 {
			writeJSON(w, 200, protocol.JobsResponse{Jobs: jobs})
			return
		}
		select {
		case <-wake:
			continue
		case <-deadline.C:
			s.db.Exec(`UPDATE devices SET last_seen=? WHERE id=?`, time.Now().Unix(), id)
			writeJSON(w, 200, protocol.JobsResponse{Jobs: []protocol.Job{}})
			return
		case <-r.Context().Done():
			return
		}
	}
}

func (s *Server) handleAgentResult(w http.ResponseWriter, r *http.Request) {
	id := deviceID(r)
	var res protocol.JobResult
	if err := readJSON(r, &res, 8<<20); err != nil {
		writeErr(w, 400, "bad request")
		return
	}
	status := "done"
	if res.ExitCode != 0 || res.Error != "" {
		status = "failed"
	}
	out := res.Output
	if len(out) > 1<<20 {
		out = out[:1<<20] + "\n... [output truncated at 1 MiB]"
	}
	_, err := s.db.Exec(`UPDATE jobs SET status=?, exit_code=?, output=?, error=?, finished_at=? WHERE id=? AND device_id=? AND status<>'cancelled'`,
		status, res.ExitCode, out, res.Error, time.Now().Unix(), res.JobID, id)
	if err != nil && err != sql.ErrNoRows {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) handleAgentBinary(w http.ResponseWriter, r *http.Request) {
	s.serveAgentBinary(w, r, r.PathValue("platform"), nil, "")
}
