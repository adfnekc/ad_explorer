// Package auth guards the console: one operator password, stateless HMAC
// session tokens, and a per-client login rate limit.
//
// It deliberately knows nothing about HTTP, so the rules can be tested on
// their own and there is exactly one place where a password is compared.
package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

var (
	// ErrBadPassword means the console password did not match.
	ErrBadPassword = errors.New("auth: wrong password")
	// ErrBadToken means the token is malformed, forged, or signed with a
	// different secret.
	ErrBadToken = errors.New("auth: malformed or forged token")
	// ErrExpired means the token was valid but is past its expiry.
	ErrExpired = errors.New("auth: token expired")
	// ErrLockedOut means too many failed attempts from this client.
	ErrLockedOut = errors.New("auth: too many attempts")
)

// Defaults for the console session.
const (
	DefaultTTL         = 8 * time.Hour
	DefaultMaxAttempts = 5
	DefaultLockout     = 10 * time.Minute

	// tokenVersion is part of the signed payload so a future format change
	// cannot be replayed against this one.
	tokenVersion = 1
	payloadLen   = 1 + 8 + 8 // version, expiry, nonce

	// maxTrackedClients bounds the rate limiter's memory. Real deployments
	// see a handful of clients; this is a guard against a spray of source
	// addresses, not a load balancer.
	maxTrackedClients = 4096
)

// b64 is the token alphabet. Strict decoding is used so a token has exactly
// one valid spelling: without it, two different strings decode to the same
// bytes and the tag comparison still passes, which makes the token malleable.
var b64 = base64.RawURLEncoding.Strict()

// Options configures an Auth.
type Options struct {
	// Secret signs session tokens. Changing it invalidates every session,
	// which is what happens by default on restart.
	Secret []byte
	// Password is the console password, compared in constant time.
	Password string
	// TTL is how long an issued token stays valid. Zero means DefaultTTL.
	TTL time.Duration
	// MaxAttempts is how many consecutive failures lock a client out.
	// Zero means DefaultMaxAttempts.
	MaxAttempts int
	// Lockout is how long a locked-out client must wait. Zero means
	// DefaultLockout.
	Lockout time.Duration
	// Now is the clock, injectable for tests. Nil means time.Now.
	Now func() time.Time
}

// Auth issues and verifies console session tokens.
//
// It is safe for concurrent use.
type Auth struct {
	secret  []byte
	passSum [sha256.Size]byte
	ttl     time.Duration
	maxFail int
	lockout time.Duration
	now     func() time.Time

	mu      sync.Mutex
	clients map[string]*clientState
}

type clientState struct {
	fails       int
	windowStart time.Time
	lockedUntil time.Time
	lastSeen    time.Time
}

// New builds an Auth. Secret must not be empty: a token signed with an empty
// key is not a session, it is a formality.
func New(o Options) (*Auth, error) {
	if len(o.Secret) == 0 {
		return nil, errors.New("auth: Secret must not be empty")
	}
	if o.Password == "" {
		return nil, errors.New("auth: Password must not be empty")
	}
	a := &Auth{
		secret:  append([]byte(nil), o.Secret...),
		passSum: sha256.Sum256([]byte(o.Password)),
		ttl:     orDuration(o.TTL, DefaultTTL),
		maxFail: orInt(o.MaxAttempts, DefaultMaxAttempts),
		lockout: orDuration(o.Lockout, DefaultLockout),
		now:     o.Now,
		clients: make(map[string]*clientState),
	}
	if a.now == nil {
		a.now = time.Now
	}
	return a, nil
}

// TTL reports the configured session lifetime.
func (a *Auth) TTL() time.Duration { return a.ttl }

// CheckPassword reports whether pw is the console password.
//
// It compares SHA-256 digests rather than the strings so the comparison is
// constant time regardless of length, and so a wrong password of a different
// length cannot be distinguished by timing.
func (a *Auth) CheckPassword(pw string) bool {
	sum := sha256.Sum256([]byte(pw))
	return subtle.ConstantTimeCompare(sum[:], a.passSum[:]) == 1
}

// Issue mints a session token valid for the configured TTL.
func (a *Auth) Issue() (string, error) {
	var payload [payloadLen]byte
	payload[0] = tokenVersion
	binary.BigEndian.PutUint64(payload[1:9], uint64(a.now().Add(a.ttl).Unix()))
	if _, err := rand.Read(payload[9:17]); err != nil {
		return "", fmt.Errorf("auth: entropy: %w", err)
	}
	tag := a.sign(payload[:])
	return b64.EncodeToString(payload[:]) + "." + b64.EncodeToString(tag), nil
}

// Verify checks a token's signature and expiry.
func (a *Auth) Verify(token string) error {
	body, tagPart, ok := strings.Cut(token, ".")
	if !ok {
		return ErrBadToken
	}
	payload, err := b64.DecodeString(body)
	if err != nil || len(payload) != payloadLen {
		return ErrBadToken
	}
	tag, err := b64.DecodeString(tagPart)
	if err != nil {
		return ErrBadToken
	}
	if !hmac.Equal(tag, a.sign(payload)) {
		return ErrBadToken
	}
	if payload[0] != tokenVersion {
		return ErrBadToken
	}
	if a.now().Unix() >= int64(binary.BigEndian.Uint64(payload[1:9])) {
		return ErrExpired
	}
	return nil
}

func (a *Auth) sign(payload []byte) []byte {
	m := hmac.New(sha256.New, a.secret)
	m.Write(payload)
	return m.Sum(nil)
}

// Allow reports whether client may attempt a login now, and if not, how long
// it must wait. Call it before checking the password.
func (a *Auth) Allow(client string) (retryAfter time.Duration, ok bool) {
	now := a.now()
	a.mu.Lock()
	defer a.mu.Unlock()
	a.pruneLocked(now)

	c, found := a.clients[client]
	if !found {
		return 0, true
	}
	c.lastSeen = now
	if now.Before(c.lockedUntil) {
		return c.lockedUntil.Sub(now), false
	}
	return 0, true
}

// Fail records a failed attempt and locks the client out once it reaches the
// configured limit.
func (a *Auth) Fail(client string) {
	now := a.now()
	a.mu.Lock()
	defer a.mu.Unlock()

	c, found := a.clients[client]
	if !found {
		c = &clientState{windowStart: now}
		a.clients[client] = c
	}
	// A window that has already elapsed starts over, so occasional typos
	// spread over days never accumulate into a lockout.
	if now.Sub(c.windowStart) > a.lockout {
		c.fails = 0
		c.windowStart = now
	}
	c.fails++
	c.lastSeen = now
	if c.fails >= a.maxFail {
		c.lockedUntil = now.Add(a.lockout)
		c.fails = 0
		c.windowStart = now
	}
}

// Succeed clears the client's failure state.
func (a *Auth) Succeed(client string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.clients, client)
}

// Login composes the three steps in the order a handler must use them:
// rate limit first, then password, then issue.
func (a *Auth) Login(client, password string) (string, error) {
	if _, ok := a.Allow(client); !ok {
		return "", ErrLockedOut
	}
	if !a.CheckPassword(password) {
		a.Fail(client)
		return "", ErrBadPassword
	}
	a.Succeed(client)
	return a.Issue()
}

// AttemptsLeft reports how many failures remain before client is locked out.
//
// It exists so a wrong password can say how many tries are left instead of
// simply refusing, which turns a lockout into a surprise.
func (a *Auth) AttemptsLeft(client string) int {
	now := a.now()
	a.mu.Lock()
	defer a.mu.Unlock()

	c, found := a.clients[client]
	if !found {
		return a.maxFail
	}
	if now.Before(c.lockedUntil) {
		return 0
	}
	if now.Sub(c.windowStart) > a.lockout {
		return a.maxFail
	}
	if left := a.maxFail - c.fails; left > 0 {
		return left
	}
	return 0
}

// pruneLocked drops entries that are neither locked nor recently seen, so a
// spray of source addresses cannot grow the map without bound.
func (a *Auth) pruneLocked(now time.Time) {
	if len(a.clients) < maxTrackedClients {
		return
	}
	for k, c := range a.clients {
		if now.After(c.lockedUntil) && now.Sub(c.lastSeen) > a.lockout {
			delete(a.clients, k)
		}
	}
}

// LoadPassword reads the console password from a file, stripping the trailing
// newline an editor or `echo` leaves behind.
//
// A file is preferred over a flag because a command-line argument is visible to
// every user on the box through ps(1) and /proc/<pid>/cmdline.
func LoadPassword(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("auth: password file: %w", err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("auth: password file %s is group/world readable (%o); chmod 600 it",
			path, info.Mode().Perm())
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("auth: password file: %w", err)
	}
	pw := strings.TrimRight(string(raw), "\r\n")
	if pw == "" {
		return "", errors.New("auth: password file is empty")
	}
	return pw, nil
}

// NewSecret returns n bytes of crypto/rand output for Options.Secret.
func NewSecret(n int) ([]byte, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return nil, fmt.Errorf("auth: entropy: %w", err)
	}
	return b, nil
}

func orDuration(v, def time.Duration) time.Duration {
	if v <= 0 {
		return def
	}
	return v
}

func orInt(v, def int) int {
	if v <= 0 {
		return def
	}
	return v
}
