package server

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Sirkolko/terraform-recovery/internal/recovery"
	"github.com/Sirkolko/terraform-recovery/internal/terraform"
)

type noTF struct{}

func (noTF) Version(context.Context) (terraform.Version, error) {
	return terraform.Version{Raw: "1.9.0", Major: 1, Minor: 9}, nil
}
func (noTF) Init(context.Context, io.Writer) error            { return errors.New("not used") }
func (noTF) Get(context.Context, io.Writer) error             { return errors.New("not used") }
func (noTF) StateList(context.Context) ([]string, error)      { return nil, nil }
func (noTF) StatePull(context.Context) ([]byte, error)        { return nil, nil }
func (noTF) ShowText(context.Context, string) (string, error) { return "", nil }
func (noTF) ShowJSON(context.Context, string) ([]byte, error) { return nil, nil }
func (noTF) Apply(context.Context, io.Writer, string) error   { return errors.New("not used") }
func (noTF) Validate(context.Context) (*terraform.ValidateResult, error) {
	return &terraform.ValidateResult{Valid: true}, nil
}
func (noTF) Plan(context.Context, io.Writer, string, []string) (int, error) { return 1, nil }

func newTestServer(t *testing.T) (*Server, http.Handler, string) {
	t.Helper()
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "main.tf"), []byte(`resource "aws_vpc" "main" { cidr_block = "10.0.0.0/16" }`), 0o644)
	svc, err := recovery.New(context.Background(), recovery.Options{ProjectDir: dir, Terraform: noTF{},
		NewDiscoverer: func(context.Context, string) (recovery.Discoverer, error) { return nil, errors.New("offline") }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(svc.Close)
	s, err := New(svc, nil, "test")
	if err != nil {
		t.Fatal(err)
	}
	s.port = 4242
	return s, s.Handler(), "127.0.0.1:4242"
}

func do(h http.Handler, method, target, host string, body string, headers map[string]string, cookie *http.Cookie) *httptest.ResponseRecorder {
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, target, r)
	req.Host = host
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// freshToken issues a new one-time login token.
func freshToken(s *Server) string {
	s.NewLoginURL()
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loginToken
}

func login(t *testing.T, s *Server, h http.Handler, host string) *http.Cookie {
	t.Helper()
	token := freshToken(s)
	rec := do(h, "GET", "/?token="+token, host, "", nil, nil)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/" {
		t.Fatalf("login = %d %s", rec.Code, rec.Header().Get("Location"))
	}
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode {
		t.Fatalf("session cookie = %+v", cookies)
	}
	if cookies[0].Value == token {
		t.Fatal("the session cookie must not be the login token")
	}
	return cookies[0]
}

// browser returns the headers a browser sends with a same-origin fetch.
func browser(s *Server, host string) map[string]string {
	return map[string]string{
		"Content-Type": "application/json", "X-CSRF-Token": s.csrf,
		"Origin": "http://" + host, "Sec-Fetch-Site": "same-origin",
	}
}

func TestAuthentication(t *testing.T) {
	s, h, host := newTestServer(t)
	if rec := do(h, "GET", "/api/session", host, "", nil, nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("unauthenticated API = %d", rec.Code)
	}
	if rec := do(h, "GET", "/?token=wrong", host, "", nil, nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("wrong token = %d", rec.Code)
	}
	cookie := login(t, s, h, host)
	rec := do(h, "GET", "/api/session", host, "", nil, cookie)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"terraform"`) {
		t.Fatalf("session = %d %s", rec.Code, rec.Body.String())
	}
	if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "script-src 'self'") || strings.Contains(csp, "unsafe") {
		t.Errorf("CSP = %q", csp)
	}
	page := do(h, "GET", "/", host, "", nil, cookie)
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), s.csrf) || strings.Contains(page.Body.String(), cookie.Value) {
		t.Errorf("index must embed the CSRF token but never the session ID")
	}
	if rec := do(h, "GET", "/static/app.js", host, "", nil, cookie); rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/javascript") {
		t.Errorf("static = %d %s", rec.Code, rec.Header().Get("Content-Type"))
	}
}

func TestOneTimeLoginLink(t *testing.T) {
	s, h, host := newTestServer(t)
	token := freshToken(s)
	first := do(h, "GET", "/?token="+token, host, "", nil, nil)
	if first.Code != http.StatusSeeOther {
		t.Fatalf("first login = %d", first.Code)
	}
	// A leaked or reused link is worthless after the first login.
	if rec := do(h, "GET", "/?token="+token, host, "", nil, nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("reused login link = %d", rec.Code)
	}
	// The token itself is not a session.
	fake := &http.Cookie{Name: s.cookieName(), Value: token}
	if rec := do(h, "GET", "/api/session", host, "", nil, fake); rec.Code != http.StatusUnauthorized {
		t.Errorf("login token accepted as session = %d", rec.Code)
	}
	// The browser that logged in keeps working, also when it reopens the old link.
	cookie := first.Result().Cookies()[0]
	if rec := do(h, "GET", "/api/session", host, "", nil, cookie); rec.Code != http.StatusOK {
		t.Errorf("session after login = %d", rec.Code)
	}
	if rec := do(h, "GET", "/?token="+token, host, "", nil, cookie); rec.Code != http.StatusSeeOther {
		t.Errorf("logged-in browser reopening the link = %d", rec.Code)
	}
	// A new link replaces an unused one.
	unused := freshToken(s)
	newer := freshToken(s)
	if rec := do(h, "GET", "/?token="+unused, host, "", nil, nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("replaced link = %d", rec.Code)
	}
	if rec := do(h, "GET", "/?token="+newer, host, "", nil, nil); rec.Code != http.StatusSeeOther {
		t.Errorf("new link = %d", rec.Code)
	}
}

func TestDNSRebindingProtection(t *testing.T) {
	s, h, _ := newTestServer(t)
	for _, host := range []string{"evil.example:4242", "127.0.0.1:9999", "attacker.localhost:4242"} {
		if rec := do(h, "GET", "/?token="+freshToken(s), host, "", nil, nil); rec.Code != http.StatusMisdirectedRequest {
			t.Errorf("host %s = %d", host, rec.Code)
		}
	}
	for _, host := range []string{"127.0.0.1:4242", "localhost:4242", "[::1]:4242"} {
		if rec := do(h, "GET", "/?token="+freshToken(s), host, "", nil, nil); rec.Code != http.StatusSeeOther {
			t.Errorf("host %s = %d", host, rec.Code)
		}
	}
}

func TestCSRFProtection(t *testing.T) {
	s, h, host := newTestServer(t)
	cookie := login(t, s, h, host)
	body := `{"kind":"terraform","key":"aws_vpc.main","reason":"x"}`
	good := browser(s, host)
	json := map[string]string{"Content-Type": "application/json", "X-CSRF-Token": s.csrf}
	with := func(extra map[string]string) map[string]string {
		out := map[string]string{}
		for k, v := range json {
			out[k] = v
		}
		for k, v := range extra {
			out[k] = v
		}
		return out
	}

	cases := []struct {
		name    string
		headers map[string]string
		want    int
	}{
		{"no csrf token", map[string]string{"Content-Type": "application/json", "Origin": "http://" + host}, http.StatusForbidden},
		{"wrong csrf token", map[string]string{"Content-Type": "application/json", "X-CSRF-Token": "nope", "Origin": "http://" + host}, http.StatusForbidden},
		{"cross origin", with(map[string]string{"Origin": "https://evil.example"}), http.StatusForbidden},
		{"cross site fetch metadata", with(map[string]string{"Sec-Fetch-Site": "cross-site"}), http.StatusForbidden},
		{"same site but other origin", with(map[string]string{"Sec-Fetch-Site": "same-site", "Origin": "http://" + host}), http.StatusForbidden},
		{"no origin and no fetch metadata", json, http.StatusForbidden},
		{"form post", with(map[string]string{"Content-Type": "application/x-www-form-urlencoded", "Origin": "http://" + host}), http.StatusUnsupportedMediaType},
		{"fetch metadata only", with(map[string]string{"Sec-Fetch-Site": "same-origin"}), http.StatusOK},
		{"origin only", with(map[string]string{"Origin": "http://" + host}), http.StatusOK},
		{"valid", good, http.StatusOK},
	}
	for _, c := range cases {
		if rec := do(h, "POST", "/api/ignore", host, body, c.headers, cookie); rec.Code != c.want {
			t.Errorf("%s: %d %s", c.name, rec.Code, rec.Body.String())
		}
	}
	// Unknown fields and oversized bodies are rejected.
	if rec := do(h, "POST", "/api/unignore", host, `{"kind":"terraform","key":"aws_vpc.main","extra":1}`, good, cookie); rec.Code != http.StatusBadRequest {
		t.Errorf("unknown field = %d", rec.Code)
	}
	big := `{"kind":"terraform","key":"` + strings.Repeat("a", 2<<20) + `"}`
	if rec := do(h, "POST", "/api/unignore", host, big, good, cookie); rec.Code != http.StatusBadRequest {
		t.Errorf("oversized body = %d", rec.Code)
	}
}

func TestPlanRefusedWithoutMappings(t *testing.T) {
	s, h, host := newTestServer(t)
	cookie := login(t, s, h, host)
	headers := browser(s, host)
	rec := do(h, "POST", "/api/plan", host, `{}`, headers, cookie)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "No confirmed mappings") {
		t.Errorf("plan = %d %s", rec.Code, rec.Body.String())
	}
	rec = do(h, "POST", "/api/apply", host, `{"plan_id":"x","confirm":true}`, headers, cookie)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("apply without plan = %d %s", rec.Code, rec.Body.String())
	}
}

func TestListenRefusesNonLoopback(t *testing.T) {
	s, _, _ := newTestServer(t)
	for _, addr := range []string{"0.0.0.0:0", "[::]:0", "192.168.1.10:8080", "example.com:80"} {
		if ln, err := s.Listen(addr); err == nil {
			ln.Close()
			t.Errorf("listen on %s must be refused", addr)
		}
	}
	// "localhost" binds the loopback address itself, whatever it resolves to.
	ln, err := s.Listen("localhost:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if ip := ln.Addr().(*net.TCPAddr).IP; !ip.Equal(net.IPv4(127, 0, 0, 1)) {
		t.Errorf("localhost bound to %s", ip)
	}
	url := s.LoginURL()
	if !strings.HasPrefix(url, "http://127.0.0.1:") || !strings.Contains(url, "token=") {
		t.Errorf("url = %s", url)
	}
}
