// Package server exposes the recovery session as a local web UI. It binds to
// the loopback interface only and protects the API against other websites
// (DNS rebinding, CSRF) with a Host check, a session token and a CSRF token.
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
	"time"

	"github.com/Sirkolko/terraform-recovery/internal/recovery"
	"github.com/Sirkolko/terraform-recovery/web"
)

const maxBodyBytes = 1 << 20

// Server serves the web UI and JSON API for one recovery session.
type Server struct {
	svc     *recovery.Service
	log     *slog.Logger
	version string
	token   string
	csrf    string
	port    int
	tmpl    *template.Template
	static  fs.FS
	handler http.Handler
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
	s := &Server{svc: svc, log: logger, version: version, token: randomToken(), csrf: randomToken(), tmpl: tmpl, static: static}
	s.handler = s.routes()
	return s, nil
}

// Listen binds a loopback address. Non-loopback addresses are refused: the
// UI can trigger Terraform runs and must never be reachable from the network.
func (s *Server) Listen(addr string) (net.Listener, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("invalid listen address %q: %w", addr, err)
	}
	if host != "localhost" {
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			return nil, fmt.Errorf("refusing to listen on %q: only loopback addresses (127.0.0.1, ::1, localhost) are allowed", addr)
		}
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	s.port = ln.Addr().(*net.TCPAddr).Port
	return ln, nil
}

// URL returns the one-time login URL for the browser.
func (s *Server) URL(ln net.Listener) string {
	return fmt.Sprintf("http://%s/?token=%s", ln.Addr().String(), s.token)
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
		c, err := r.Cookie(s.cookieName())
		if err != nil || subtle.ConstantTimeCompare([]byte(c.Value), []byte(s.token)) != 1 {
			if strings.HasPrefix(r.URL.Path, "/api/") {
				writeError(w, http.StatusUnauthorized, "not authenticated: open the URL printed by terraform-recovery")
				return
			}
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprintln(w, "Open the URL printed in the terminal by terraform-recovery (it contains a one-time access token).")
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

func (s *Server) sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		// Browsers always send Origin on cross-origin POSTs; without it the
		// request comes from a non-browser client that has the cookie.
		return true
	}
	return origin == "http://"+r.Host
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	if subtle.ConstantTimeCompare([]byte(r.URL.Query().Get("token")), []byte(s.token)) != 1 {
		http.Error(w, "invalid token", http.StatusUnauthorized)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: s.cookieName(), Value: s.token, Path: "/",
		HttpOnly: true, SameSite: http.SameSiteStrictMode,
	})
	// Redirect so the token does not stay in the address bar or history.
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
