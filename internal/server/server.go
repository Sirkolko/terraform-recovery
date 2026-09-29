// Package server exposes the recovery session as a local web UI. It binds to
// the loopback interface only and protects the API against other websites
// (DNS rebinding, CSRF) with a Host check, a session cookie obtained with a
// one-time login link, Fetch Metadata/Origin checks and a CSRF token.
package server

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Sirkolko/terraform-recovery/internal/recovery"
	"github.com/Sirkolko/terraform-recovery/web"
)

const maxBodyBytes = 1 << 20

const loginHint = "Open the one-time login link printed in the terminal where terraform-recovery runs. " +
	"Each link works once; press Enter in that terminal to print a new one."

// Server serves the web UI and JSON API for one recovery session.
type Server struct {
	svc     *recovery.Service
	log     *slog.Logger
	version string
	csrf    string
	port    int
	addr    string
	tmpl    *template.Template
	static  fs.FS
	handler http.Handler

	mu sync.Mutex
	// loginToken is the one-time token of the current login link; it is
	// cleared when used. Sessions are separate random IDs, so a login link
	// that leaks (terminal output, browser history, process list) is useless
	// once it has been used.
	loginToken string
	sessions   map[string]bool
}

func randomToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// New creates a server for svc.
func New(svc *recovery.Service, logger *slog.Logger, version string) (*Server, error) {
	tmpl, err := template.ParseFS(web.Files, "templates/*.html")
	if err != nil {
		return nil, err
	}
	static, err := fs.Sub(web.Files, "static")
	if err != nil {
		return nil, err
	}
	if logger == nil {
		logger = slog.Default()
	}
	s := &Server{
		svc: svc, log: logger, version: version, csrf: randomToken(), tmpl: tmpl, static: static,
		loginToken: randomToken(), sessions: map[string]bool{},
	}
	s.handler = s.routes()
	return s, nil
}

// Listen binds a loopback address. Non-loopback addresses are refused: the
// UI can trigger Terraform runs and must never be reachable from the network.
func (s *Server) Listen(addr string) (net.Listener, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("invalid listen address %q: %w", addr, err)
	}
	if host == "localhost" {
		// Bind the loopback address itself instead of whatever "localhost"
		// resolves to on this machine.
		addr = net.JoinHostPort("127.0.0.1", port)
	} else if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return nil, fmt.Errorf("refusing to listen on %q: only loopback addresses (127.0.0.1, ::1, localhost) are allowed", addr)
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	s.port = ln.Addr().(*net.TCPAddr).Port
	s.addr = ln.Addr().String()
	return ln, nil
}

// LoginURL returns the login link with the current one-time token.
func (s *Server) LoginURL() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return fmt.Sprintf("http://%s/?token=%s", s.addr, s.loginToken)
}

// NewLoginURL replaces the login token, used or not, with a new one-time
// token and returns its link. Existing browser sessions stay valid.
func (s *Server) NewLoginURL() string {
	s.mu.Lock()
	s.loginToken = randomToken()
	s.mu.Unlock()
	return s.LoginURL()
}

// Serve serves until ctx is cancelled.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	srv := &http.Server{
		Handler:           s.handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      2 * time.Minute,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    64 << 10,
		ErrorLog:          slog.NewLogLogger(s.log.Handler(), slog.LevelWarn),
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
		return nil
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// Handler returns the HTTP handler (used by tests).
func (s *Server) Handler() http.Handler { return s.handler }

func (s *Server) cookieName() string { return "trr_session_" + strconv.Itoa(s.port) }

// secure wraps every request with the security checks.
func (s *Server) secure(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; font-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Cross-Origin-Resource-Policy", "same-origin")
		h.Set("Cache-Control", "no-store")

		if !s.allowedHost(r.Host) {
			http.Error(w, "invalid host", http.StatusMisdirectedRequest)
			return
		}
		if r.URL.Path == "/" && r.URL.Query().Has("token") {
			s.login(w, r)
			return
		}
		if !s.authenticated(r) {
			if strings.HasPrefix(r.URL.Path, "/api/") {
				writeError(w, http.StatusUnauthorized, "not logged in. "+loginHint)
				return
			}
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprintln(w, "Not logged in. "+loginHint)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if !s.sameOrigin(r) {
				writeError(w, http.StatusForbidden, "cross-origin request refused")
				return
			}
			if subtle.ConstantTimeCompare([]byte(r.Header.Get("X-CSRF-Token")), []byte(s.csrf)) != 1 {
				writeError(w, http.StatusForbidden, "missing or invalid CSRF token")
				return
			}
			if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt != "application/json" {
				writeError(w, http.StatusUnsupportedMediaType, "requests must be JSON")
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
		}
		next.ServeHTTP(w, r)
	})
}

// allowedHost defeats DNS rebinding: only our own loopback host names.
func (s *Server) allowedHost(host string) bool {
	port := strconv.Itoa(s.port)
	switch host {
	case "127.0.0.1:" + port, "localhost:" + port, "[::1]:" + port:
		return true
	}
	return false
}

// sameOrigin requires positive evidence that a state-changing request comes
// from our own page: Fetch Metadata (Sec-Fetch-Site) and/or an Origin header
// that matches this server. Requests with neither are refused.
func (s *Server) sameOrigin(r *http.Request) bool {
	site := r.Header.Get("Sec-Fetch-Site")
	if site != "" && site != "same-origin" {
		return false
	}
	if origin := r.Header.Get("Origin"); origin != "" {
		return origin == "http://"+r.Host
	}
	return site == "same-origin"
}

// authenticated reports whether the request carries a valid session cookie.
func (s *Server) authenticated(r *http.Request) bool {
	c, err := r.Cookie(s.cookieName())
	if err != nil || c.Value == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessions[c.Value]
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	if s.authenticated(r) {
		// Already logged in (for example an old link opened from the
		// history): just drop the token from the address bar.
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	token := r.URL.Query().Get("token")
	s.mu.Lock()
	valid := s.loginToken != "" && subtle.ConstantTimeCompare([]byte(token), []byte(s.loginToken)) == 1
	session := ""
	if valid {
		s.loginToken = "" // the link works once
		session = randomToken()
		s.sessions[session] = true
	}
	s.mu.Unlock()
	if !valid {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprintln(w, "This login link is invalid or has already been used. "+loginHint)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: s.cookieName(), Value: session, Path: "/",
		HttpOnly: true, SameSite: http.SameSiteStrictMode,
	})
	// Redirect so the token does not stay in the address bar.
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	data := map[string]any{
		"CSRF":    s.csrf,
		"Version": s.version,
		"Project": path.Base(s.svc.ProjectDir()),
		"DryRun":  s.svc.DryRun(),
	}
	if err := s.tmpl.ExecuteTemplate(w, "index.html", data); err != nil {
		s.log.Error("render index", "error", err)
	}
}

func (s *Server) staticFile(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("file")
	data, err := fs.ReadFile(s.static, name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	switch path.Ext(name) {
	case ".js":
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	case ".css":
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
	case ".svg":
		w.Header().Set("Content-Type", "image/svg+xml")
	default:
		w.Header().Set("Content-Type", "application/octet-stream")
	}
	w.Write(data)
}
