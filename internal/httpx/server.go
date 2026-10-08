// Package httpx is the HTTP surface: cookie authentication, the REST endpoints
// for sessions, the WebSocket upgrade, and the embedded frontend.
//
// It holds no credentials. The browser owns the host list and the passwords;
// this layer only sees the one password it is asked to use for a session, keeps
// it no longer than the session, and never logs it.
package httpx

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/adfnekc/ad_explorer/internal/auth"
	"github.com/adfnekc/ad_explorer/internal/rdp"
	"github.com/adfnekc/ad_explorer/internal/session"
	"github.com/adfnekc/ad_explorer/internal/wsframe"
)

// CookieName is the session cookie that carries the console token.
const CookieName = "adx_session"

// Limits on what a request may carry.
const (
	// maxBody caps a request body. The largest legitimate one is a host
	// password, so this is generous.
	maxBody = 64 << 10
	// Bounds on the size a client may ask the remote to be. Width and height
	// have separate minimums because 640x480 is a real resolution and a single
	// floor of 640 would reject it.
	minWidth   = 640
	minHeight  = 480
	maxDesktop = 7680
)

// DefaultDialTimeout bounds how long a create-session request may spend
// connecting.
const DefaultDialTimeout = 60 * time.Second

// Config assembles a Server.
type Config struct {
	// Auth verifies the console password and its cookies.
	Auth *auth.Auth
	// Sessions owns the live remote desktops.
	Sessions *session.Registry
	// Assets is the built frontend. Nil serves a placeholder.
	Assets fs.FS
	// Secure marks the cookie Secure. It must be true whenever the gateway is
	// reached over HTTPS, which is the only supported deployment: the password
	// for a remote host travels in a request body.
	Secure bool
	// TrustProxy makes the rate limiter believe X-Forwarded-For. It must only
	// be set when a reverse proxy is the only way in, because otherwise any
	// client can claim any address and sidestep the lockout.
	TrustProxy bool
	// DialTimeout bounds the connect step of a new session.
	DialTimeout time.Duration
	// Logf receives one line per request. It is never given a request body, so
	// a password cannot reach it through here.
	Logf func(format string, args ...any)
}

// Server is the gateway's HTTP handler.
type Server struct {
	cfg Config
	mux *http.ServeMux
}

// New builds the routes.
func New(cfg Config) (*Server, error) {
	switch {
	case cfg.Auth == nil:
		return nil, errors.New("httpx: Auth is required")
	case cfg.Sessions == nil:
		return nil, errors.New("httpx: Sessions is required")
	}
	if cfg.DialTimeout <= 0 {
		cfg.DialTimeout = DefaultDialTimeout
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}

	s := &Server{cfg: cfg, mux: http.NewServeMux()}

	// Unauthenticated: a probe for a load balancer, and the way in. Login cannot
	// be behind the cookie check for the obvious reason, but it still gets the
	// origin check and a bounded body.
	s.mux.HandleFunc("GET /api/health", s.handleHealth)
	s.mux.HandleFunc("POST /api/login", s.public(s.handleLogin))

	// Authenticated.
	s.mux.HandleFunc("POST /api/logout", s.guarded(s.handleLogout))
	s.mux.HandleFunc("GET /api/sessions", s.guarded(s.handleListSessions))
	s.mux.HandleFunc("POST /api/sessions", s.guarded(s.handleCreateSession))
	s.mux.HandleFunc("DELETE /api/sessions/{id}", s.guarded(s.handleDeleteSession))
	s.mux.HandleFunc("GET /api/sessions/{id}/stream", s.guarded(s.handleStream))

	s.mux.Handle("/", s.staticHandler())

	return s, nil
}

// ServeHTTP implements http.Handler, adding the headers and the request line.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	applySecurityHeaders(w)
	rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
	start := time.Now()

	s.mux.ServeHTTP(rec, r)

	s.cfg.Logf("%s %s %d %s %s", r.Method, r.URL.Path, rec.status,
		time.Since(start).Round(time.Millisecond), s.clientOf(r))
}

// ---------------------------------------------------------------- middleware

// guarded wraps a handler with the console cookie check and the same-origin
// requirement for anything that changes state.
func (s *Server) guarded(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.checkOrigin(w, r) {
			return
		}

		cookie, err := r.Cookie(CookieName)
		if err != nil || cookie.Value == "" {
			writeError(w, http.StatusUnauthorized, wsframe.ErrCodeAuthRequired,
				"the console session is missing or has expired")
			return
		}
		if err := s.cfg.Auth.Verify(cookie.Value); err != nil {
			// The cookie is expired or forged; clearing it saves the client a
			// pointless retry.
			clearCookie(w, s.cfg.Secure)
			writeError(w, http.StatusUnauthorized, wsframe.ErrCodeAuthRequired,
				"the console session is missing or has expired")
			return
		}

		limitBody(w, r)
		h(w, r)
	}
}

// public wraps a handler that does not need the console cookie. It still
// requires a same-origin state change and still bounds the body.
func (s *Server) public(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.checkOrigin(w, r) {
			return
		}
		limitBody(w, r)
		h(w, r)
	}
}

// checkOrigin refuses a state-changing request from another site. It reports
// false after writing the response.
func (s *Server) checkOrigin(w http.ResponseWriter, r *http.Request) bool {
	if r.Method == http.MethodGet || r.Method == http.MethodHead || sameOrigin(r) {
		return true
	}
	writeError(w, http.StatusForbidden, wsframe.ErrCodeBadOrigin,
		"the request came from another site")
	return false
}

func limitBody(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
}

// applySecurityHeaders sets the headers that make a browser refuse to help an
// attacker. The policy is strict because the frontend is a compiled bundle with
// no inline script and no third-party origins.
func applySecurityHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Security-Policy",
		"default-src 'self'; img-src 'self' data: blob:; "+
			"style-src 'self' 'unsafe-inline'; script-src 'self'; "+
			"connect-src 'self' ws: wss:; frame-ancestors 'none'; "+
			"base-uri 'none'; form-action 'self'")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Cross-Origin-Opener-Policy", "same-origin")
	h.Set("Cross-Origin-Resource-Policy", "same-origin")
	h.Set("Permissions-Policy", "geolocation=(), microphone=(), camera=()")
}

// sameOrigin reports whether a state-changing request came from this site.
//
// A missing Origin is allowed: the cookie is SameSite=Lax, so a cross-site form
// post does not carry it, and some non-browser clients do not send the header.
func sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	return strings.EqualFold(u.Host, r.Host)
}

// clientOf identifies the caller for the rate limiter.
func (s *Server) clientOf(r *http.Request) string {
	if s.cfg.TrustProxy {
		if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
			// The first entry is the original client when every hop appends.
			if first, _, ok := strings.Cut(fwd, ","); ok {
				return strings.TrimSpace(first)
			}
			return strings.TrimSpace(fwd)
		}
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

// ---------------------------------------------------------------- handlers

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":   "ok",
		"sessions": s.cfg.Sessions.Count(),
	})
}

type loginRequest struct {
	Password string `json:"password"`
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	client := s.clientOf(r)
	token, err := s.cfg.Auth.Login(client, req.Password)
	if err != nil {
		if errors.Is(err, auth.ErrLockedOut) {
			retry, _ := s.cfg.Auth.Allow(client)
			writeJSON(w, http.StatusTooManyRequests, map[string]any{
				"code":       wsframe.ErrCodeLoginLocked,
				"message":    "too many failed attempts",
				"retryAfter": int(retry.Seconds()),
			})
			return
		}
		// The message never says whether the password was close, and the timing
		// does not either: the comparison is constant time.
		writeJSON(w, http.StatusUnauthorized, map[string]any{
			"code":         wsframe.ErrCodeLoginFailed,
			"message":      "wrong password",
			"attemptsLeft": s.cfg.Auth.AttemptsLeft(client),
		})
		return
	}

	setCookie(w, token, s.cfg.Auth.TTL(), s.cfg.Secure)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":        true,
		"expiresIn": int(s.cfg.Auth.TTL().Seconds()),
	})
}

func (s *Server) handleLogout(w http.ResponseWriter, _ *http.Request) {
	clearCookie(w, s.cfg.Secure)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleListSessions(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"sessions": s.cfg.Sessions.List()})
}

type createSessionRequest struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	User     string `json:"user"`
	Password string `json:"password"`
	Protocol string `json:"protocol"`
	Width    int    `json:"width"`
	Height   int    `json:"height"`
	Name     string `json:"name"`
}

func (s *Server) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	var req createSessionRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Port == 0 {
		req.Port = 3389
	}
	if req.Width == 0 {
		req.Width = 1280
	}
	if req.Height == 0 {
		req.Height = 720
	}
	if req.Name == "" {
		req.Name = req.Host
	}

	switch {
	case strings.TrimSpace(req.Host) == "":
		writeError(w, http.StatusBadRequest, wsframe.ErrCodeInvalidRequest, "host is required")
		return
	case req.Port < 1 || req.Port > 65535:
		writeError(w, http.StatusBadRequest, wsframe.ErrCodeInvalidRequest,
			"port must be between 1 and 65535")
		return
	case req.Width < minWidth || req.Width > maxDesktop ||
		req.Height < minHeight || req.Height > maxDesktop:
		writeError(w, http.StatusBadRequest, wsframe.ErrCodeInvalidRequest,
			fmt.Sprintf("width must be %d..%d and height %d..%d",
				minWidth, maxDesktop, minHeight, maxDesktop))
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.DialTimeout)
	defer cancel()

	// The password is read from the body, handed to grdp, and never copied
	// anywhere else: no log, no store, no error message.
	cfg := rdp.Config{
		Host:     net.JoinHostPort(req.Host, strconv.Itoa(req.Port)),
		User:     req.User,
		Password: req.Password,
		Protocol: req.Protocol,
		Width:    req.Width,
		Height:   req.Height,
		Name:     req.Name,
	}

	sess, err := s.cfg.Sessions.Create(ctx, cfg)
	if err != nil {
		s.writeSessionError(w, err)
		return
	}

	info := sess.Info()
	writeJSON(w, http.StatusCreated, map[string]any{
		"id":     info.ID,
		"name":   info.Name,
		"host":   info.Host,
		"width":  info.Width,
		"height": info.Height,
	})
}

func (s *Server) handleDeleteSession(w http.ResponseWriter, r *http.Request) {
	if err := s.cfg.Sessions.Close(r.PathValue("id")); err != nil {
		s.writeSessionError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// writeSessionError maps a registry failure onto a status and a code the client
// knows how to explain.
func (s *Server) writeSessionError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, session.ErrLimit):
		writeError(w, http.StatusTooManyRequests, wsframe.ErrCodeSessionLimit,
			"the gateway is already running as many sessions as it allows")
	case errors.Is(err, session.ErrNotFound):
		writeError(w, http.StatusNotFound, wsframe.ErrCodeSessionGone, "no such session")
	case errors.Is(err, session.ErrShuttingDown):
		writeError(w, http.StatusServiceUnavailable, wsframe.ErrCodeSessionLimit,
			"the gateway is shutting down")
	default:
		code := rdp.CodeOf(err)
		if code == "" {
			code = wsframe.ErrCodeInternal
		}
		// rdp has already scrubbed the password out of the message.
		writeError(w, http.StatusBadGateway, code, err.Error())
	}
}

// ---------------------------------------------------------------- static assets

// staticHandler serves the embedded frontend, falling back to the shell so the
// client's router can handle a deep link.
func (s *Server) staticHandler() http.Handler {
	files := http.FileServerFS(s.cfg.Assets)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			writeError(w, http.StatusNotFound, wsframe.ErrCodeInvalidRequest, "no such endpoint")
			return
		}

		name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if name == "" || name == "." {
			name = "index.html"
		}

		if f, err := s.cfg.Assets.Open(name); err == nil {
			_ = f.Close()
			// Vite fingerprints everything it emits into assets/, so those can
			// be cached forever. The shell must not be, or a deploy would never
			// reach a browser that had already loaded the page.
			if strings.HasPrefix(name, "assets/") {
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			} else {
				w.Header().Set("Cache-Control", "no-cache")
			}
			files.ServeHTTP(w, r)
			return
		}

		// Unknown path: hand back the shell.
		w.Header().Set("Cache-Control", "no-cache")
		r.URL.Path = "/"
		files.ServeHTTP(w, r)
	})
}

// ---------------------------------------------------------------- helpers

func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	if ct := r.Header.Get("Content-Type"); ct != "" {
		if media, _, _ := strings.Cut(ct, ";"); strings.TrimSpace(media) != "application/json" {
			writeError(w, http.StatusUnsupportedMediaType, wsframe.ErrCodeInvalidRequest,
				"the body must be application/json")
			return false
		}
	}
	dec := json.NewDecoder(r.Body)
	// Unknown fields are refused: a mistyped field name should be an error, not
	// a silently missing password.
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		var maxErr *http.MaxBytesError
		switch {
		case errors.As(err, &maxErr):
			writeError(w, http.StatusRequestEntityTooLarge, wsframe.ErrCodeInvalidRequest,
				"the body is too large")
		case errors.Is(err, io.EOF):
			writeError(w, http.StatusBadRequest, wsframe.ErrCodeInvalidRequest, "the body is empty")
		default:
			writeError(w, http.StatusBadRequest, wsframe.ErrCodeInvalidRequest, "malformed JSON body")
		}
		return false
	}
	return true
}

func setCookie(w http.ResponseWriter, token string, ttl time.Duration, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(ttl.Seconds()),
	})
}

func clearCookie(w http.ResponseWriter, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]string{"code": code, "message": message})
}

// statusRecorder remembers the status for the request line, and passes through
// the two interfaces the WebSocket upgrade needs.
type statusRecorder struct {
	http.ResponseWriter
	status  int
	written bool
}

func (r *statusRecorder) WriteHeader(status int) {
	if !r.written {
		r.status = status
		r.written = true
	}
	r.ResponseWriter.WriteHeader(status)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if !r.written {
		r.written = true
	}
	return r.ResponseWriter.Write(b)
}

// Hijack lets the WebSocket upgrade take the connection over.
func (r *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := r.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("httpx: the response writer cannot be hijacked")
	}
	return h.Hijack()
}

// Flush forwards to the underlying writer so a streaming response is not held.
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
