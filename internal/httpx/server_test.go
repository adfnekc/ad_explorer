package httpx

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/adfnekc/ad_explorer/internal/auth"
	"github.com/adfnekc/ad_explorer/internal/session"
	"github.com/adfnekc/ad_explorer/internal/wsframe"
)

const testPassword = "console-password"

// logCapture records what the server logged, so a test can prove a credential
// never reaches it.
type logCapture struct {
	mu    sync.Mutex
	lines []string
}

func (l *logCapture) Logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, sprintf(format, args...))
}

func (l *logCapture) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}

func sprintf(format string, args ...any) string { return fmt.Sprintf(format, args...) }

func newTestServer(t *testing.T, mutate func(*Config)) (*Server, *logCapture, *auth.Auth) {
	t.Helper()

	a, err := auth.New(auth.Options{
		Secret:   []byte("0123456789abcdef0123456789abcdef"),
		Password: testPassword,
		TTL:      time.Hour,
	})
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}

	logs := &logCapture{}
	cfg := Config{
		Auth:        a,
		Sessions:    session.NewRegistry(session.Options{Max: 4}),
		Secure:      true,
		DialTimeout: 3 * time.Second,
		Logf:        logs.Logf,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	srv, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return srv, logs, a
}

// do runs a request against the handler directly, with no socket.
func do(srv *Server, method, target, body string, headers map[string]string) *httptest.ResponseRecorder {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, target, nil)
	} else {
		r = httptest.NewRequest(method, target, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, r)
	return w
}

func loginCookie(t *testing.T, srv *Server) *http.Cookie {
	t.Helper()
	w := do(srv, "POST", "/api/login", `{"password":"`+testPassword+`"}`, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("login failed: %d %s", w.Code, w.Body.String())
	}
	for _, c := range w.Result().Cookies() {
		if c.Name == CookieName {
			return c
		}
	}
	t.Fatal("login did not set the session cookie")
	return nil
}

func decodeBody(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatalf("response is not JSON: %v (%s)", err, w.Body.String())
	}
	return m
}

// ---------------------------------------------------------------- login

func TestLogin(t *testing.T) {
	srv, _, _ := newTestServer(t, nil)

	t.Run("wrong password", func(t *testing.T) {
		w := do(srv, "POST", "/api/login", `{"password":"nope"}`, nil)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", w.Code)
		}
		body := decodeBody(t, w)
		if body["code"] != wsframe.ErrCodeLoginFailed {
			t.Errorf("code = %v, want %v", body["code"], wsframe.ErrCodeLoginFailed)
		}
		if left, ok := body["attemptsLeft"].(float64); !ok || left <= 0 {
			t.Errorf("attemptsLeft = %v, want a positive number so the operator knows where they stand", body["attemptsLeft"])
		}
	})

	t.Run("right password", func(t *testing.T) {
		w := do(srv, "POST", "/api/login", `{"password":"`+testPassword+`"}`, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
		}
		cookies := w.Result().Cookies()
		if len(cookies) != 1 {
			t.Fatalf("got %d cookies, want 1", len(cookies))
		}
		c := cookies[0]
		if !c.HttpOnly {
			t.Error("the cookie is readable from script; it must be HttpOnly")
		}
		if !c.Secure {
			t.Error("the cookie is not Secure although the server was configured for it")
		}
		if c.SameSite != http.SameSiteLaxMode {
			t.Errorf("SameSite = %v, want Lax", c.SameSite)
		}
		if c.Path != "/" {
			t.Errorf("Path = %q, want /", c.Path)
		}
		if c.MaxAge <= 0 {
			t.Errorf("MaxAge = %d, want the configured TTL", c.MaxAge)
		}
	})

	t.Run("cookie is not Secure when the server is not", func(t *testing.T) {
		// A plain-HTTP deployment cannot set Secure, or the browser drops the
		// cookie; the flag has to follow the configuration.
		plain, _, _ := newTestServer(t, func(c *Config) { c.Secure = false })
		c := loginCookie(t, plain)
		if c.Secure {
			t.Error("the cookie is Secure although the server was told it is not")
		}
	})
}

func TestLoginLocksOut(t *testing.T) {
	srv, _, _ := newTestServer(t, nil)

	var last *httptest.ResponseRecorder
	for i := 0; i < auth.DefaultMaxAttempts; i++ {
		last = do(srv, "POST", "/api/login", `{"password":"nope"}`, nil)
	}
	// The final failure trips the lockout, so the next attempt is refused
	// before the password is even considered.
	w := do(srv, "POST", "/api/login", `{"password":"nope"}`, nil)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 (previous was %d)", w.Code, last.Code)
	}
	body := decodeBody(t, w)
	if body["code"] != wsframe.ErrCodeLoginLocked {
		t.Errorf("code = %v, want %v", body["code"], wsframe.ErrCodeLoginLocked)
	}
	if retry, ok := body["retryAfter"].(float64); !ok || retry <= 0 {
		t.Errorf("retryAfter = %v, want seconds", body["retryAfter"])
	}

	// The right password must not bypass an active lockout.
	w = do(srv, "POST", "/api/login", `{"password":"`+testPassword+`"}`, nil)
	if w.Code != http.StatusTooManyRequests {
		t.Errorf("the correct password got %d while locked out, want 429", w.Code)
	}
}

func TestLoginRejectsMalformedBodies(t *testing.T) {
	srv, _, _ := newTestServer(t, nil)

	cases := []struct {
		name    string
		body    string
		headers map[string]string
		status  int
	}{
		{"empty", "", nil, http.StatusBadRequest},
		{"not json", "nonsense", nil, http.StatusBadRequest},
		{"array", "[]", nil, http.StatusBadRequest},
		{"unknown field", `{"passwrd":"x"}`, nil, http.StatusBadRequest},
		{"wrong content type", `{"password":"x"}`, map[string]string{"Content-Type": "text/plain"}, http.StatusUnsupportedMediaType},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := do(srv, "POST", "/api/login", tc.body, tc.headers)
			if w.Code != tc.status {
				t.Errorf("status = %d, want %d (%s)", w.Code, tc.status, w.Body.String())
			}
		})
	}
}

func TestLogoutClearsTheCookie(t *testing.T) {
	srv, _, _ := newTestServer(t, nil)
	c := loginCookie(t, srv)

	w := do(srv, "POST", "/api/logout", "", map[string]string{
		"Cookie": CookieName + "=" + c.Value,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	cleared := w.Result().Cookies()
	if len(cleared) != 1 || cleared[0].MaxAge >= 0 {
		t.Errorf("logout did not clear the cookie: %+v", cleared)
	}
}

// ---------------------------------------------------------------- auth gate

func TestAPIRequiresTheCookie(t *testing.T) {
	srv, _, _ := newTestServer(t, nil)

	routes := []struct {
		method, target string
	}{
		{"GET", "/api/sessions"},
		{"POST", "/api/sessions"},
		{"DELETE", "/api/sessions/abc"},
		{"GET", "/api/sessions/abc/stream"},
		{"POST", "/api/logout"},
	}
	for _, rt := range routes {
		t.Run(rt.method+" "+rt.target, func(t *testing.T) {
			w := do(srv, rt.method, rt.target, "", nil)
			if w.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", w.Code)
			}
			if code := decodeBody(t, w)["code"]; code != wsframe.ErrCodeAuthRequired {
				t.Errorf("code = %v, want %v", code, wsframe.ErrCodeAuthRequired)
			}
		})
	}
}

func TestRejectedCookieIsCleared(t *testing.T) {
	srv, _, _ := newTestServer(t, nil)

	for _, cookie := range []string{"", "garbage", "a.b", "eyJ4IjoxfQ.forged"} {
		t.Run("cookie "+cookie, func(t *testing.T) {
			headers := map[string]string{}
			if cookie != "" {
				headers["Cookie"] = CookieName + "=" + cookie
			}
			w := do(srv, "GET", "/api/sessions", "", headers)
			if w.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", w.Code)
			}
			// An expired or forged cookie is dropped so the client stops retrying
			// with something that cannot work. A request that carried no cookie
			// has nothing to clear.
			if cookie == "" {
				return
			}
			cleared := false
			for _, c := range w.Result().Cookies() {
				if c.Name == CookieName && c.MaxAge < 0 {
					cleared = true
				}
			}
			if !cleared {
				t.Error("the rejected cookie was not cleared")
			}
		})
	}
}

func TestExpiredCookieIsRejected(t *testing.T) {
	secret := []byte("0123456789abcdef0123456789abcdef")

	issuer, err := auth.New(auth.Options{Secret: secret, Password: testPassword, TTL: time.Millisecond})
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}
	token, err := issuer.Issue()
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	time.Sleep(5 * time.Millisecond)

	verifier, err := auth.New(auth.Options{Secret: secret, Password: testPassword, TTL: time.Hour})
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}
	srv, _, _ := newTestServer(t, func(c *Config) { c.Auth = verifier })

	w := do(srv, "GET", "/api/sessions", "", map[string]string{"Cookie": CookieName + "=" + token})
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d for an expired cookie, want 401", w.Code)
	}
}

func TestHealthNeedsNoCookie(t *testing.T) {
	srv, _, _ := newTestServer(t, nil)

	w := do(srv, "GET", "/api/health", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if body := decodeBody(t, w); body["status"] != "ok" {
		t.Errorf("body = %v", body)
	}
}

// ---------------------------------------------------------------- origin

func TestCrossOriginStateChangeIsRefused(t *testing.T) {
	srv, _, _ := newTestServer(t, nil)
	c := loginCookie(t, srv)

	cases := []struct {
		name   string
		origin string
		want   int
	}{
		{"same origin", "http://example.com", http.StatusNotFound},
		{"cross origin", "http://evil.example", http.StatusForbidden},
		{"cross origin with a port", "http://example.com:8080", http.StatusForbidden},
		{"origin that merely looks similar", "http://example.com.evil.example", http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := do(srv, "DELETE", "/api/sessions/nope", "", map[string]string{
				"Cookie": CookieName + "=" + c.Value,
				"Origin": tc.origin,
				"Host":   "example.com",
			})
			if w.Code != tc.want {
				t.Errorf("status = %d, want %d (%s)", w.Code, tc.want, w.Body.String())
			}
		})
	}

	// A read is not a state change, so it is allowed from anywhere as long as
	// the cookie is there. SameSite already stops the cookie travelling.
	w := do(srv, "GET", "/api/sessions", "", map[string]string{
		"Cookie": CookieName + "=" + c.Value,
		"Origin": "http://evil.example",
		"Host":   "example.com",
	})
	if w.Code != http.StatusOK {
		t.Errorf("a cross-origin read got %d, want 200", w.Code)
	}

	// A missing Origin is allowed: not every client sends one.
	w = do(srv, "GET", "/api/sessions", "", map[string]string{"Cookie": CookieName + "=" + c.Value})
	if w.Code != http.StatusOK {
		t.Errorf("a request with no Origin got %d, want 200", w.Code)
	}
}

// ---------------------------------------------------------------- headers

func TestSecurityHeaders(t *testing.T) {
	srv, _, _ := newTestServer(t, nil)
	w := do(srv, "GET", "/api/health", "", nil)

	want := map[string]string{
		"X-Content-Type-Options":       "nosniff",
		"Referrer-Policy":              "no-referrer",
		"Cross-Origin-Opener-Policy":   "same-origin",
		"Cross-Origin-Resource-Policy": "same-origin",
	}
	for k, v := range want {
		if got := w.Header().Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}

	csp := w.Header().Get("Content-Security-Policy")
	for _, want := range []string{"default-src 'self'", "frame-ancestors 'none'", "base-uri 'none'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("the policy is missing %q: %s", want, csp)
		}
	}
	// Inline script would defeat the policy, so the frontend must never need it.
	if strings.Contains(csp, "script-src 'self' 'unsafe-inline'") ||
		strings.Contains(csp, "script-src 'unsafe-inline'") {
		t.Errorf("the policy allows inline script: %s", csp)
	}
}

// ---------------------------------------------------------------- sessions

func TestListSessionsIsEmptyAtFirst(t *testing.T) {
	srv, _, _ := newTestServer(t, nil)
	c := loginCookie(t, srv)

	w := do(srv, "GET", "/api/sessions", "", map[string]string{"Cookie": CookieName + "=" + c.Value})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	body := decodeBody(t, w)
	sessions, ok := body["sessions"].([]any)
	if !ok {
		t.Fatalf("sessions is %T, want an array", body["sessions"])
	}
	if len(sessions) != 0 {
		t.Errorf("got %d sessions, want 0", len(sessions))
	}
}

func TestDeleteUnknownSession(t *testing.T) {
	srv, _, _ := newTestServer(t, nil)
	c := loginCookie(t, srv)

	w := do(srv, "DELETE", "/api/sessions/nope", "", map[string]string{"Cookie": CookieName + "=" + c.Value})
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
	if code := decodeBody(t, w)["code"]; code != wsframe.ErrCodeSessionGone {
		t.Errorf("code = %v, want %v", code, wsframe.ErrCodeSessionGone)
	}
}

func TestStreamForUnknownSessionIsRefusedBeforeUpgrading(t *testing.T) {
	srv, _, _ := newTestServer(t, nil)
	c := loginCookie(t, srv)

	w := do(srv, "GET", "/api/sessions/nope/stream", "", map[string]string{"Cookie": CookieName + "=" + c.Value})
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 rather than a failed upgrade", w.Code)
	}
}

func TestCreateSessionValidatesTheRequest(t *testing.T) {
	srv, _, _ := newTestServer(t, nil)
	c := loginCookie(t, srv)
	headers := map[string]string{"Cookie": CookieName + "=" + c.Value}

	cases := []struct {
		name string
		body string
		code string
	}{
		{"no host", `{"user":"u","password":"p"}`, wsframe.ErrCodeInvalidRequest},
		{"empty host", `{"host":"","user":"u","password":"p"}`, wsframe.ErrCodeInvalidRequest},
		{"port zero is allowed as a default", `{"host":"127.0.0.1","port":0,"width":640,"height":480}`, ""},
		{"the classic 640x480 is allowed", `{"host":"127.0.0.1","width":640,"height":480}`, ""},
		{"port too high", `{"host":"127.0.0.1","port":70000}`, wsframe.ErrCodeInvalidRequest},
		{"port negative", `{"host":"127.0.0.1","port":-1}`, wsframe.ErrCodeInvalidRequest},
		{"desktop too small", `{"host":"127.0.0.1","width":10,"height":10}`, wsframe.ErrCodeInvalidRequest},
		{"height alone too small", `{"host":"127.0.0.1","width":1280,"height":100}`, wsframe.ErrCodeInvalidRequest},
		{"desktop too large", `{"host":"127.0.0.1","width":99999,"height":480}`, wsframe.ErrCodeInvalidRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := do(srv, "POST", "/api/sessions", tc.body, headers)
			if tc.code == "" {
				// It got past validation, so it tried to connect. Anything but
				// a validation error is acceptable here.
				if code := decodeBody(t, w)["code"]; code == wsframe.ErrCodeInvalidRequest {
					t.Fatalf("a valid request was rejected: %s", w.Body.String())
				}
				return
			}
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (%s)", w.Code, w.Body.String())
			}
			if code := decodeBody(t, w)["code"]; code != tc.code {
				t.Errorf("code = %v, want %v", code, tc.code)
			}
		})
	}
}

func TestCreateSessionReportsAnUnreachableHost(t *testing.T) {
	srv, _, _ := newTestServer(t, func(c *Config) { c.DialTimeout = 2 * time.Second })
	c := loginCookie(t, srv)

	// Port 1 on the loopback interface has nothing listening, so the dial is
	// refused immediately rather than timing out.
	w := do(srv, "POST", "/api/sessions",
		`{"host":"127.0.0.1","port":1,"user":"u","password":"p","width":640,"height":480}`,
		map[string]string{"Cookie": CookieName + "=" + c.Value})

	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (%s)", w.Code, w.Body.String())
	}
	body := decodeBody(t, w)
	if body["code"] == "" || body["code"] == nil {
		t.Errorf("no error code in %v", body)
	}
	if !strings.Contains(body["message"].(string), "127.0.0.1:1") {
		t.Errorf("the message does not name the address: %v", body["message"])
	}
}

// ---------------------------------------------------------------- secrecy

func TestPasswordNeverReachesTheLog(t *testing.T) {
	// The invariant the whole design rests on: a remote password is read from a
	// request body, handed to grdp, and never written anywhere. This drives a
	// real create against an unreachable host — which is the path that builds
	// error messages — and then reads back everything that was logged.
	const remotePassword = "sup3r-s3cret-remote-pw"

	srv, logs, _ := newTestServer(t, func(c *Config) { c.DialTimeout = 2 * time.Second })
	c := loginCookie(t, srv)

	w := do(srv, "POST", "/api/sessions",
		`{"host":"127.0.0.1","port":1,"user":"operator","password":"`+remotePassword+`","width":640,"height":480}`,
		map[string]string{"Cookie": CookieName + "=" + c.Value})

	everything := logs.String() + "\n" + w.Body.String()
	if strings.Contains(everything, remotePassword) {
		t.Fatalf("the remote password appears in the output:\n%s\n%s", logs.String(), w.Body.String())
	}
	// The console password must not leak either.
	if strings.Contains(everything, testPassword) {
		t.Fatalf("the console password appears in the output:\n%s", everything)
	}
	// And the request line is still useful.
	if !strings.Contains(logs.String(), "POST /api/sessions") {
		t.Errorf("the access log line is missing:\n%s", logs.String())
	}
}

func TestErrorBodyNeverCarriesACredential(t *testing.T) {
	// rdp.redact scrubs the password from any error it returns; this checks the
	// HTTP layer does not undo that by adding anything of its own.
	const remotePassword = "another-secret-pw"
	srv, _, _ := newTestServer(t, func(c *Config) { c.DialTimeout = 2 * time.Second })
	c := loginCookie(t, srv)

	w := do(srv, "POST", "/api/sessions",
		`{"host":"127.0.0.1","port":1,"user":"operator","password":"`+remotePassword+`","width":640,"height":480}`,
		map[string]string{"Cookie": CookieName + "=" + c.Value})

	if strings.Contains(w.Body.String(), remotePassword) {
		t.Fatalf("the response body carries the password: %s", w.Body.String())
	}
}

// ---------------------------------------------------------------- static

func testAssets() fstest.MapFS {
	return fstest.MapFS{
		"index.html":        &fstest.MapFile{Data: []byte("<!doctype html><title>shell</title>")},
		"assets/app-abc.js": &fstest.MapFile{Data: []byte("console.log(1)")},
	}
}

func TestSPAFallbackServesTheShell(t *testing.T) {
	srv, _, _ := newTestServer(t, func(c *Config) { c.Assets = testAssets() })

	t.Run("a known asset is served with a long cache", func(t *testing.T) {
		w := do(srv, "GET", "/assets/app-abc.js", "", nil)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d", w.Code)
		}
		if cc := w.Header().Get("Cache-Control"); !strings.Contains(cc, "immutable") {
			t.Errorf("Cache-Control = %q; a fingerprinted asset should be immutable", cc)
		}
	})

	t.Run("the shell must not be cached", func(t *testing.T) {
		w := do(srv, "GET", "/", "", nil)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d", w.Code)
		}
		if cc := w.Header().Get("Cache-Control"); cc != "no-cache" {
			t.Errorf("Cache-Control = %q, want no-cache: a stale shell would hide a deploy", cc)
		}
	})

	t.Run("a deep link gets the shell so the client router can handle it", func(t *testing.T) {
		w := do(srv, "GET", "/sessions/abc", "", nil)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d", w.Code)
		}
		if !strings.Contains(w.Body.String(), "shell") {
			t.Errorf("body = %q, want the shell", w.Body.String())
		}
	})

	t.Run("an unknown api path stays a 404", func(t *testing.T) {
		w := do(srv, "GET", "/api/nope", "", nil)
		if w.Code != http.StatusNotFound {
			t.Errorf("status = %d, want 404: a mistyped endpoint must not silently return HTML", w.Code)
		}
	})
}

func TestNewRequiresItsDependencies(t *testing.T) {
	a, err := auth.New(auth.Options{Secret: []byte("s"), Password: "p"})
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}
	reg := session.NewRegistry(session.Options{Max: 1})

	if _, err := New(Config{Sessions: reg}); err == nil {
		t.Error("New accepted a nil Auth")
	}
	if _, err := New(Config{Auth: a}); err == nil {
		t.Error("New accepted a nil Sessions")
	}
}
