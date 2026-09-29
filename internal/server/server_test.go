package server

import (
	"context"
	"errors"
	"io"
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

func login(t *testing.T, s *Server, h http.Handler, host string) *http.Cookie {
	t.Helper()
	rec := do(h, "GET", "/?token="+s.token, host, "", nil, nil)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/" {
		t.Fatalf("login = %d %s", rec.Code, rec.Header().Get("Location"))
	}
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode {
		t.Fatalf("session cookie = %+v", cookies)
	}
	return cookies[0]
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
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), s.csrf) || strings.Contains(page.Body.String(), s.token) {
		t.Errorf("index must embed the CSRF token but never the session token")
	}
	if rec := do(h, "GET", "/static/app.js", host, "", nil, cookie); rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/javascript") {
		t.Errorf("static = %d %s", rec.Code, rec.Header().Get("Content-Type"))
	}
}

func TestDNSRebindingProtection(t *testing.T) {
	s, h, _ := newTestServer(t)
	for _, host := range []string{"evil.example:4242", "127.0.0.1:9999", "attacker.localhost:4242"} {
		if rec := do(h, "GET", "/?token="+s.token, host, "", nil, nil); rec.Code != http.StatusMisdirectedRequest {
			t.Errorf("host %s = %d", host, rec.Code)
		}
	}
	for _, host := range []string{"127.0.0.1:4242", "localhost:4242", "[::1]:4242"} {
		if rec := do(h, "GET", "/?token="+s.token, host, "", nil, nil); rec.Code != http.StatusSeeOther {
			t.Errorf("host %s = %d", host, rec.Code)
		}
	}
}

func TestCSRFProtection(t *testing.T) {
	s, h, host := newTestServer(t)
	cookie := login(t, s, h, host)
	body := `{"kind":"terraform","key":"aws_vpc.main","reason":"x"}`
	good := map[string]string{"Content-Type": "application/json", "X-CSRF-Token": s.csrf, "Origin": "http://" + host}

	cases := []struct {
		name    string
		headers map[string]string
		want    int
	}{
		{"no csrf token", map[string]string{"Content-Type": "application/json"}, http.StatusForbidden},
		{"wrong csrf token", map[string]string{"Content-Type": "application/json", "X-CSRF-Token": "nope"}, http.StatusForbidden},
		{"cross origin", map[string]string{"Content-Type": "application/json", "X-CSRF-Token": s.csrf, "Origin": "https://evil.example"}, http.StatusForbidden},
		{"form post", map[string]string{"Content-Type": "application/x-www-form-urlencoded", "X-CSRF-Token": s.csrf}, http.StatusUnsupportedMediaType},
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
	headers := map[string]string{"Content-Type": "application/json", "X-CSRF-Token": s.csrf}
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
	ln, err := s.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if !strings.HasPrefix(s.URL(ln), "http://127.0.0.1:") || !strings.Contains(s.URL(ln), "token=") {
		t.Errorf("url = %s", s.URL(ln))
	}
}
