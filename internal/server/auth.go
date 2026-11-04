package server

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"net/http"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const (
	sessionCookie = "oceanic_session"
	sessionTTL    = 7 * 24 * time.Hour
	csrfHeader    = "X-OCeanic-Request"
)

type ctxKey int

const (
	ctxUser ctxKey = iota
	ctxDevice
)

func (s *Server) setAdminPassword(pw string) error {
	h, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	return s.setSetting("admin_password_hash", string(h))
}

func (s *Server) checkAdmin(user, pw string) bool {
	hash := s.getSetting("admin_password_hash")
	userOK := subtle.ConstantTimeCompare([]byte(user), []byte(s.cfg.AdminUser)) == 1
	pwOK := bcrypt.CompareHashAndPassword([]byte(hash), []byte(pw)) == nil
	return userOK && pwOK
}

// tooManyFailures implements a simple per-IP lockout: 10 failures / 15 min.
func (s *Server) tooManyFailures(ip string, record bool) bool {
	s.loginMu.Lock()
	defer s.loginMu.Unlock()
	cut := time.Now().Add(-15 * time.Minute)
	var recent []time.Time
	for _, t := range s.loginFailures[ip] {
		if t.After(cut) {
			recent = append(recent, t)
		}
	}
	if record {
		recent = append(recent, time.Now())
	}
	s.loginFailures[ip] = recent
	return len(recent) >= 10
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	if s.tooManyFailures(ip, false) {
		writeErr(w, http.StatusTooManyRequests, "too many failed attempts, try again later")
		return
	}
	var req struct{ Username, Password string }
	if err := readJSON(r, &req, 4096); err != nil {
		writeErr(w, http.StatusBadRequest, "bad request")
		return
	}
	if !s.checkAdmin(req.Username, req.Password) {
		s.tooManyFailures(ip, true)
		s.audit(req.Username, "login.failed", "", ip)
		time.Sleep(500 * time.Millisecond)
		writeErr(w, http.StatusUnauthorized, "invalid username or password")
		return
	}
	tok := randToken(32)
	exp := time.Now().Add(sessionTTL)
	if _, err := s.db.Exec(`INSERT INTO sessions(token_hash,username,expires_at) VALUES(?,?,?)`, sha(tok), req.Username, exp.Unix()); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: tok, Path: "/", Expires: exp,
		HttpOnly: true, Secure: s.cfg.SecureCookies, SameSite: http.SameSiteStrictMode,
	})
	s.audit(req.Username, "login", "", ip)
	writeJSON(w, 200, map[string]string{"username": req.Username})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		s.db.Exec(`DELETE FROM sessions WHERE token_hash=?`, sha(c.Value))
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: s.cfg.SecureCookies, SameSite: http.SameSiteStrictMode})
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{
		"username":   user(r),
		"public_url": s.publicURL(r),
	})
}

func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Current string `json:"current"`
		New     string `json:"new"`
	}
	if err := readJSON(r, &req, 4096); err != nil {
		writeErr(w, 400, "bad request")
		return
	}
	if !s.checkAdmin(user(r), req.Current) {
		writeErr(w, 403, "current password is incorrect")
		return
	}
	if len(req.New) < 10 {
		writeErr(w, 400, "new password must be at least 10 characters")
		return
	}
	if err := s.setAdminPassword(req.New); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	// Invalidate every other session.
	if c, err := r.Cookie(sessionCookie); err == nil {
		s.db.Exec(`DELETE FROM sessions WHERE token_hash<>?`, sha(c.Value))
	}
	s.audit(user(r), "password.change", "", "")
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func user(r *http.Request) string {
	u, _ := r.Context().Value(ctxUser).(string)
	return u
}

func deviceID(r *http.Request) string {
	d, _ := r.Context().Value(ctxDevice).(string)
	return d
}

// adminAuth requires a valid session cookie. State-changing requests must
// also carry a custom header, which cross-site forms cannot set (CSRF guard,
// in addition to SameSite=Strict cookies).
func (s *Server) adminAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(sessionCookie)
		if err != nil {
			writeErr(w, 401, "not logged in")
			return
		}
		var username string
		var exp int64
		err = s.db.QueryRow(`SELECT username, expires_at FROM sessions WHERE token_hash=?`, sha(c.Value)).Scan(&username, &exp)
		if err != nil || exp < time.Now().Unix() {
			writeErr(w, 401, "session expired")
			return
		}
		if r.Method != http.MethodGet && r.Header.Get(csrfHeader) != "1" {
			writeErr(w, 403, "missing CSRF header")
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), ctxUser, username)))
	}
}

// agentAuth validates "Authorization: Bearer <agent_id>.<agent_secret>".
func (s *Server) agentAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		h := r.Header.Get("Authorization")
		tok, ok := strings.CutPrefix(h, "Bearer ")
		if !ok {
			writeErr(w, 401, "missing agent credentials")
			return
		}
		id, secret, ok := strings.Cut(tok, ".")
		if !ok {
			writeErr(w, 401, "malformed agent credentials")
			return
		}
		var hash string
		err := s.db.QueryRow(`SELECT secret_hash FROM devices WHERE id=?`, id).Scan(&hash)
		if err == sql.ErrNoRows {
			// 410 tells the agent it was deleted and should re-enroll / stop.
			writeErr(w, http.StatusGone, "unknown agent")
			return
		}
		if err != nil {
			writeErr(w, 500, "db error")
			return
		}
		if subtle.ConstantTimeCompare([]byte(hash), []byte(sha(secret))) != 1 {
			writeErr(w, 401, "bad agent credentials")
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), ctxDevice, id)))
	}
}
