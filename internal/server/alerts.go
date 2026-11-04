package server

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"time"
)

// alertLoop evaluates alert conditions every minute and posts state changes
// (new alert / resolved) to OCEANIC_ALERT_WEBHOOK. The payload carries both
// "content" (Discord) and "text" (Slack, Mattermost, Google Chat); ntfy.sh
// URLs get a plain-text body instead.
func (s *Server) alertLoop(ctx context.Context) {
	if s.cfg.AlertWebhook == "" {
		return
	}
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	first := true
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		devs, err := s.listDevices("")
		if err != nil {
			continue
		}
		current := map[string]alert{}
		for _, a := range s.computeAlerts(devs) {
			current[a.Key] = a
		}
		s.alertMu.Lock()
		var msgs []string
		for k, a := range current {
			if !s.alertState[k] {
				s.alertState[k] = true
				if !first { // don't spam everything that was already broken at startup
					icon := "⚠️"
					if a.Severity == "critical" {
						icon = "🔴"
					}
					msgs = append(msgs, icon+" **"+a.Device+"**: "+a.Message)
				}
			}
		}
		for k := range s.alertState {
			if _, still := current[k]; !still {
				delete(s.alertState, k)
				dev := strings.SplitN(k, ":", 3)
				msgs = append(msgs, "✅ Resolved: "+dev[0]+" alert on device "+s.deviceName(dev[1]))
			}
		}
		s.alertMu.Unlock()
		first = false
		if len(msgs) > 0 {
			s.sendWebhook("OCeanic RMM\n" + strings.Join(msgs, "\n"))
		}
	}
}

func (s *Server) deviceName(id string) string {
	var n string
	s.db.QueryRow(`SELECT COALESCE(NULLIF(display_name,''),hostname) FROM devices WHERE id=?`, id).Scan(&n)
	if n == "" {
		return id
	}
	return n
}

func (s *Server) sendWebhook(msg string) {
	var req *http.Request
	if strings.Contains(s.cfg.AlertWebhook, "ntfy") {
		req, _ = http.NewRequest("POST", s.cfg.AlertWebhook, strings.NewReader(strings.ReplaceAll(msg, "**", "")))
		req.Header.Set("Title", "OCeanic RMM alert")
	} else {
		body, _ := json.Marshal(map[string]string{"content": msg, "text": msg})
		req, _ = http.NewRequest("POST", s.cfg.AlertWebhook, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	}
	c := &http.Client{Timeout: 10 * time.Second}
	resp, err := c.Do(req)
	if err != nil {
		log.Printf("alert webhook: %v", err)
		return
	}
	resp.Body.Close()
}
