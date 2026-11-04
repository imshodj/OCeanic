package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/imshodj/oceanic/internal/protocol"
)

// Interactive terminals
//
//   browser ──WS──► /api/devices/{id}/terminal ──┐
//                                                 │  server bridges frames
//   agent   ──WS──► /api/agent/terminal/{sid} ───┘
//
// Both legs are connections *into* the server, so this works through a
// Cloudflare Tunnel with no inbound ports on the agent's network. The agent
// learns about a new session through its normal long-poll (a "terminal" job
// whose ID is the session ID), so connection setup takes well under a second.

const (
	termConnectWait = 30 * time.Second
	termIdleTimeout = 30 * time.Minute
	termPingEvery   = 25 * time.Second // keeps Cloudflare's ~100s idle timer happy
	termReadLimit   = 1 << 20
)

var termShells = map[string]bool{
	protocol.ShellPowerShell: true, protocol.ShellPwsh: true, protocol.ShellCmd: true,
	protocol.ShellBash: true, protocol.ShellSh: true,
}

type termSession struct {
	id, deviceID string
	agentCh      chan *websocket.Conn
	done         chan struct{}
}

type termRegistry struct {
	mu       sync.Mutex
	sessions map[string]*termSession
}

func (t *termRegistry) add(s *termSession) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.sessions == nil {
		t.sessions = map[string]*termSession{}
	}
	t.sessions[s.id] = s
}

func (t *termRegistry) get(id string) *termSession {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.sessions[id]
}

func (t *termRegistry) remove(id string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.sessions, id)
}

func sendCtrl(ctx context.Context, c *websocket.Conn, m protocol.TermControl) {
	b, _ := json.Marshal(m)
	wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	c.Write(wctx, websocket.MessageText, b)
}

// handleTerminal is the browser side.
func (s *Server) handleTerminal(w http.ResponseWriter, r *http.Request) {
	devID := r.PathValue("id")
	var host, osName string
	if err := s.db.QueryRow(`SELECT hostname, os FROM devices WHERE id=?`, devID).Scan(&host, &osName); err != nil {
		writeErr(w, 404, "device not found")
		return
	}
	shell := r.URL.Query().Get("shell")
	if shell == "" {
		shell = protocol.ShellBash
		if osName == "windows" {
			shell = protocol.ShellPowerShell
		}
	}
	if !termShells[shell] {
		writeErr(w, 400, "unsupported shell")
		return
	}
	cols, _ := strconv.Atoi(r.URL.Query().Get("cols"))
	rows, _ := strconv.Atoi(r.URL.Query().Get("rows"))

	// Default Accept options reject cross-origin requests (Origin must match
	// Host), which together with the session cookie stops CSWSH.
	browser, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	browser.SetReadLimit(termReadLimit)
	defer browser.CloseNow()
	ctx := r.Context()
	who := user(r)

	sess := &termSession{id: newID(), deviceID: devID, agentCh: make(chan *websocket.Conn, 1), done: make(chan struct{})}
	s.terms.add(sess)
	defer s.terms.remove(sess.id)
	defer close(sess.done)

	started := time.Now()
	_, err = s.db.Exec(`INSERT INTO jobs(id,device_id,type,name,shell,script,timeout_sec,created_by,created_at) VALUES(?,?,?,?,?,?,?,?,?)`,
		sess.id, devID, protocol.JobTerminal, "Terminal ("+shell+")", shell, fmt.Sprintf("%dx%d", cols, rows), int(termIdleTimeout.Seconds()), who, started.Unix())
	if err != nil {
		sendCtrl(ctx, browser, protocol.TermControl{Type: "error", Message: err.Error()})
		return
	}
	s.hub.notify(devID)
	s.audit(who, "terminal.open", devID, shell+" on "+host)

	finish := func(status, msg string) {
		s.db.Exec(`UPDATE jobs SET status=?, output=?, finished_at=? WHERE id=?`, status, msg, time.Now().Unix(), sess.id)
		s.audit(who, "terminal.close", devID, msg)
	}

	var agent *websocket.Conn
	select {
	case agent = <-sess.agentCh:
	case <-time.After(termConnectWait):
		// Never picked up: cancel so an agent that reconnects later doesn't act on it.
		s.db.Exec(`UPDATE jobs SET status='cancelled', error='device did not connect', finished_at=? WHERE id=? AND status='queued'`, time.Now().Unix(), sess.id)
		finish("failed", "device did not open the terminal within 30s")
		sendCtrl(ctx, browser, protocol.TermControl{Type: "error", Message: "The device didn't respond within 30 seconds. Is it online?"})
		browser.Close(websocket.StatusNormalClosure, "timeout")
		return
	case <-ctx.Done():
		s.db.Exec(`UPDATE jobs SET status='cancelled', finished_at=? WHERE id=? AND status='queued'`, time.Now().Unix(), sess.id)
		return
	}
	defer agent.CloseNow()

	reason := s.bridge(ctx, browser, agent)
	dur := time.Since(started).Round(time.Second)
	finish("done", fmt.Sprintf("Interactive %s session by %s, %s. Ended: %s", shell, who, dur, reason))
}

// bridge relays frames both ways until one side closes or the session idles out.
func (s *Server) bridge(parent context.Context, browser, agent *websocket.Conn) string {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	var last atomic.Int64
	last.Store(time.Now().Unix())
	reasons := make(chan string, 4)

	pipe := func(from, to *websocket.Conn, name string) {
		for {
			typ, data, err := from.Read(ctx)
			if err != nil {
				reasons <- name + " disconnected"
				return
			}
			last.Store(time.Now().Unix())
			if err := to.Write(ctx, typ, data); err != nil {
				reasons <- "write failed"
				return
			}
		}
	}
	go pipe(browser, agent, "browser")
	go pipe(agent, browser, "device")

	sendCtrl(ctx, browser, protocol.TermControl{Type: "ready"})
	tick := time.NewTicker(termPingEvery)
	defer tick.Stop()
	for {
		select {
		case r := <-reasons:
			browser.Close(websocket.StatusNormalClosure, "")
			agent.Close(websocket.StatusNormalClosure, "")
			if r == "device disconnected" {
				return "shell exited"
			}
			return r
		case <-ctx.Done():
			return "server shutting down"
		case <-tick.C:
			if time.Since(time.Unix(last.Load(), 0)) > termIdleTimeout {
				sendCtrl(ctx, browser, protocol.TermControl{Type: "error", Message: "Session closed after 30 minutes of inactivity."})
				browser.Close(websocket.StatusNormalClosure, "idle")
				agent.Close(websocket.StatusNormalClosure, "idle")
				return "idle timeout"
			}
			for _, c := range []*websocket.Conn{browser, agent} {
				pctx, pc := context.WithTimeout(ctx, 10*time.Second)
				err := c.Ping(pctx)
				pc()
				if err != nil {
					log.Printf("terminal ping: %v", err)
				}
			}
		}
	}
}

// handleAgentTerminal is the agent side of a session.
func (s *Server) handleAgentTerminal(w http.ResponseWriter, r *http.Request) {
	sess := s.terms.get(r.PathValue("sid"))
	if sess == nil || sess.deviceID != deviceID(r) {
		writeErr(w, 404, "no such terminal session")
		return
	}
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	c.SetReadLimit(termReadLimit)
	select {
	case sess.agentCh <- c:
	default: // a second connection for the same session: refuse
		c.Close(websocket.StatusPolicyViolation, "session already attached")
		return
	}
	// The browser handler owns the connection; keep this handler alive until it's done.
	select {
	case <-sess.done:
	case <-r.Context().Done():
	}
}
