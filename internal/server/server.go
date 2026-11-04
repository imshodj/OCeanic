// Package server implements the OCeanic RMM control plane: the admin web UI,
// the admin REST API, the agent API and the installer generator.
package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed web
var webFS embed.FS

// Config is populated from environment variables (see README).
type Config struct {
	Listen        string
	DataDir       string
	AgentsDir     string
	PublicURL     string
	AdminUser     string
	AdminPassword string
	CFClientID    string
	CFClientSecret string
	AlertWebhook  string
	CheckinSec    int
	OfflineAfter  time.Duration
	MetricsDays   int
	JobsDays      int
	SecureCookies bool
}

func ConfigFromEnv() Config {
	c := Config{
		Listen:         env("OCEANIC_LISTEN", ":8080"),
		DataDir:        env("OCEANIC_DATA_DIR", "./data"),
		AgentsDir:      env("OCEANIC_AGENTS_DIR", "./dist/agents"),
		PublicURL:      strings.TrimRight(env("OCEANIC_PUBLIC_URL", ""), "/"),
		AdminUser:      env("OCEANIC_ADMIN_USER", "admin"),
		AdminPassword:  os.Getenv("OCEANIC_ADMIN_PASSWORD"),
		CFClientID:     os.Getenv("OCEANIC_CF_ACCESS_CLIENT_ID"),
		CFClientSecret: os.Getenv("OCEANIC_CF_ACCESS_CLIENT_SECRET"),
		AlertWebhook:   os.Getenv("OCEANIC_ALERT_WEBHOOK"),
		CheckinSec:     envInt("OCEANIC_CHECKIN_SECONDS", 30),
		MetricsDays:    envInt("OCEANIC_METRICS_RETENTION_DAYS", 7),
		JobsDays:       envInt("OCEANIC_JOBS_RETENTION_DAYS", 90),
	}
	c.OfflineAfter = time.Duration(c.CheckinSec*3+15) * time.Second
	// Cloudflare terminates TLS, so the browser always sees https when a
	// public https URL is configured.
	c.SecureCookies = strings.HasPrefix(c.PublicURL, "https://") && os.Getenv("OCEANIC_INSECURE_COOKIES") == ""
	return c
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envInt(k string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(k)); err == nil && v > 0 {
		return v
	}
	return def
}

// Server is the running control plane.
type Server struct {
	cfg Config
	db  *sql.DB
	hub *hub

	loginMu       sync.Mutex
	loginFailures map[string][]time.Time

	alertMu    sync.Mutex
	alertState map[string]bool // key -> currently alerting
}

func New(cfg Config) (*Server, error) {
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return nil, err
	}
	db, err := openDB(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	s := &Server{
		cfg:           cfg,
		db:            db,
		hub:           newHub(),
		loginFailures: map[string][]time.Time{},
		alertState:    map[string]bool{},
	}
	if err := s.bootstrap(); err != nil {
		return nil, err
	}
	return s, nil
}

// bootstrap ensures an admin password, a default org and the builtin scripts exist.
func (s *Server) bootstrap() error {
	if s.cfg.AdminPassword != "" {
		if err := s.setAdminPassword(s.cfg.AdminPassword); err != nil {
			return err
		}
	} else if s.getSetting("admin_password_hash") == "" {
		pw := randToken(12)
		if err := s.setAdminPassword(pw); err != nil {
			return err
		}
		log.Printf("=================================================================")
		log.Printf(" No OCEANIC_ADMIN_PASSWORD set. Generated admin credentials:")
		log.Printf("   user: %s   password: %s", s.cfg.AdminUser, pw)
		log.Printf(" Change it in Settings, or set OCEANIC_ADMIN_PASSWORD.")
		log.Printf("=================================================================")
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM orgs`).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		if _, err := s.createOrg("Homelab"); err != nil {
			return err
		}
	}
	return s.seedScripts()
}

// Handler builds the HTTP routing table.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// --- public ---
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	mux.HandleFunc("GET /install/{token}/windows.ps1", s.handleInstallPS1)
	mux.HandleFunc("GET /install/{token}/linux.sh", s.handleInstallSh)
	mux.HandleFunc("GET /install/{token}/agent/{platform}", s.handlePublicAgentDownload)

	// --- agent API (agent bearer auth) ---
	mux.HandleFunc("POST /api/agent/enroll", s.handleEnroll)
	mux.HandleFunc("POST /api/agent/checkin", s.agentAuth(s.handleCheckin))
	mux.HandleFunc("GET /api/agent/jobs", s.agentAuth(s.handleAgentJobs))
	mux.HandleFunc("POST /api/agent/result", s.agentAuth(s.handleAgentResult))
	mux.HandleFunc("GET /api/agent/binary/{platform}", s.agentAuth(s.handleAgentBinary))

	// --- admin auth ---
	mux.HandleFunc("POST /api/login", s.handleLogin)
	mux.HandleFunc("POST /api/logout", s.handleLogout)
	mux.HandleFunc("GET /api/me", s.adminAuth(s.handleMe))
	mux.HandleFunc("POST /api/password", s.adminAuth(s.handleChangePassword))

	// --- admin API ---
	mux.HandleFunc("GET /api/dashboard", s.adminAuth(s.handleDashboard))
	mux.HandleFunc("GET /api/orgs", s.adminAuth(s.handleListOrgs))
	mux.HandleFunc("POST /api/orgs", s.adminAuth(s.handleCreateOrg))
	mux.HandleFunc("PATCH /api/orgs/{id}", s.adminAuth(s.handleRenameOrg))
	mux.HandleFunc("DELETE /api/orgs/{id}", s.adminAuth(s.handleDeleteOrg))
	mux.HandleFunc("POST /api/orgs/{id}/rotate-token", s.adminAuth(s.handleRotateToken))
	mux.HandleFunc("GET /api/orgs/{id}/installer/{platform}", s.adminAuth(s.handleAdminInstallerDownload))
	mux.HandleFunc("GET /api/installers", s.adminAuth(s.handleInstallerInfo))

	mux.HandleFunc("GET /api/devices", s.adminAuth(s.handleListDevices))
	mux.HandleFunc("GET /api/devices/{id}", s.adminAuth(s.handleGetDevice))
	mux.HandleFunc("PATCH /api/devices/{id}", s.adminAuth(s.handleUpdateDevice))
	mux.HandleFunc("DELETE /api/devices/{id}", s.adminAuth(s.handleDeleteDevice))
	mux.HandleFunc("GET /api/devices/{id}/metrics", s.adminAuth(s.handleDeviceMetrics))
	mux.HandleFunc("GET /api/devices/{id}/jobs", s.adminAuth(s.handleDeviceJobs))

	mux.HandleFunc("GET /api/jobs", s.adminAuth(s.handleListJobs))
	mux.HandleFunc("POST /api/jobs", s.adminAuth(s.handleCreateJobs))
	mux.HandleFunc("GET /api/jobs/{id}", s.adminAuth(s.handleGetJob))
	mux.HandleFunc("POST /api/jobs/{id}/cancel", s.adminAuth(s.handleCancelJob))

	mux.HandleFunc("GET /api/scripts", s.adminAuth(s.handleListScripts))
	mux.HandleFunc("POST /api/scripts", s.adminAuth(s.handleSaveScript))
	mux.HandleFunc("PUT /api/scripts/{id}", s.adminAuth(s.handleSaveScript))
	mux.HandleFunc("DELETE /api/scripts/{id}", s.adminAuth(s.handleDeleteScript))

	mux.HandleFunc("GET /api/audit", s.adminAuth(s.handleAudit))

	// --- static UI ---
	sub, _ := fs.Sub(webFS, "web")
	static := http.FileServer(http.FS(sub))
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		static.ServeHTTP(w, r)
	})

	return securityHeaders(mux)
}

func securityHeaders(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hd := w.Header()
		hd.Set("X-Content-Type-Options", "nosniff")
		hd.Set("X-Frame-Options", "DENY")
		hd.Set("Referrer-Policy", "same-origin")
		hd.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; script-src 'self'; connect-src 'self'; frame-ancestors 'none'")
		h.ServeHTTP(w, r)
	})
}

// Run serves until ctx is cancelled.
func (s *Server) Run(ctx context.Context) error {
	srv := &http.Server{
		Addr:              s.cfg.Listen,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	go s.maintenanceLoop(ctx)
	go s.alertLoop(ctx)
	go func() {
		<-ctx.Done()
		shCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shCtx)
	}()
	log.Printf("OCeanic server listening on %s (public URL: %q)", s.cfg.Listen, s.cfg.PublicURL)
	err := srv.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func (s *Server) maintenanceLoop(ctx context.Context) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		now := time.Now()
		s.db.Exec(`DELETE FROM metrics_history WHERE ts < ?`, now.AddDate(0, 0, -s.cfg.MetricsDays).Unix())
		s.db.Exec(`DELETE FROM jobs WHERE created_at < ? AND status IN ('done','failed','cancelled')`, now.AddDate(0, 0, -s.cfg.JobsDays).Unix())
		s.db.Exec(`DELETE FROM sessions WHERE expires_at < ?`, now.Unix())
		s.db.Exec(`DELETE FROM audit WHERE ts < ?`, now.AddDate(0, 0, -365).Unix())
		// Jobs that were delivered but never reported back are failed after their timeout + grace.
		s.db.Exec(`UPDATE jobs SET status='failed', error='agent did not report a result', finished_at=?
			WHERE status='sent' AND sent_at + timeout_sec + 600 < ?`, now.Unix(), now.Unix())
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// ---------- helpers ----------

func randToken(nBytes int) string {
	b := make([]byte, nBytes)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func newID() string { return randToken(8) }

func sha(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func readJSON(r *http.Request, v any, max int64) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, max))
	return dec.Decode(v)
}

// clientIP prefers Cloudflare's header, since all traffic arrives through the tunnel.
func clientIP(r *http.Request) string {
	if ip := r.Header.Get("CF-Connecting-IP"); ip != "" {
		return ip
	}
	if ip := r.Header.Get("X-Forwarded-For"); ip != "" {
		return strings.TrimSpace(strings.Split(ip, ",")[0])
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// publicURL returns the base URL agents should use.
func (s *Server) publicURL(r *http.Request) string {
	if s.cfg.PublicURL != "" {
		return s.cfg.PublicURL
	}
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" || r.Header.Get("CF-Visitor") != "" {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

func (s *Server) getSetting(k string) string {
	var v string
	s.db.QueryRow(`SELECT value FROM settings WHERE key=?`, k).Scan(&v)
	return v
}

func (s *Server) setSetting(k, v string) error {
	_, err := s.db.Exec(`INSERT INTO settings(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, k, v)
	return err
}

func (s *Server) audit(actor, action, target, detail string) {
	s.db.Exec(`INSERT INTO audit(ts,actor,action,target,detail) VALUES(?,?,?,?,?)`, time.Now().Unix(), actor, action, target, detail)
}

// ---------- long-poll hub ----------

// hub wakes up an agent's pending long-poll when a job is queued for it.
type hub struct {
	mu      sync.Mutex
	waiters map[string]chan struct{}
}

func newHub() *hub { return &hub{waiters: map[string]chan struct{}{}} }

func (h *hub) wait(deviceID string) chan struct{} {
	h.mu.Lock()
	defer h.mu.Unlock()
	ch, ok := h.waiters[deviceID]
	if !ok {
		ch = make(chan struct{})
		h.waiters[deviceID] = ch
	}
	return ch
}

func (h *hub) notify(deviceID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if ch, ok := h.waiters[deviceID]; ok {
		close(ch)
		delete(h.waiters, deviceID)
	}
}

// HealthCheck is used by the Docker HEALTHCHECK (the image has no curl).
func HealthCheck() int {
	addr := env("OCEANIC_LISTEN", ":8080")
	if strings.HasPrefix(addr, ":") {
		addr = "127.0.0.1" + addr
	}
	c := &http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get("http://" + addr + "/healthz")
	if err != nil || resp.StatusCode != 200 {
		return 1
	}
	return 0
}
