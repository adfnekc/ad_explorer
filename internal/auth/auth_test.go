package auth

import (
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testSecret = "0123456789abcdef0123456789abcdef"

func newTestAuth(t *testing.T, o Options) *Auth {
	t.Helper()
	if len(o.Secret) == 0 {
		o.Secret = []byte(testSecret)
	}
	if o.Password == "" {
		o.Password = "correct horse"
	}
	a, err := New(o)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return a
}

func TestNewRejectsMissingSecrets(t *testing.T) {
	if _, err := New(Options{Password: "x"}); err == nil {
		t.Error("New accepted an empty Secret")
	}
	if _, err := New(Options{Secret: []byte("s")}); err == nil {
		t.Error("New accepted an empty Password")
	}
}

func TestCheckPassword(t *testing.T) {
	a := newTestAuth(t, Options{})

	cases := []struct {
		name string
		pw   string
		want bool
	}{
		{"correct", "correct horse", true},
		{"wrong", "correct hors", false},
		{"wrong, longer", "correct horse battery", false},
		{"wrong, shorter", "correct", false},
		{"empty", "", false},
		{"same length, one byte different", "correct horsf", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := a.CheckPassword(tc.pw); got != tc.want {
				t.Errorf("CheckPassword(%q) = %v, want %v", tc.pw, got, tc.want)
			}
		})
	}
}

func TestIssueVerifyRoundTrip(t *testing.T) {
	a := newTestAuth(t, Options{})

	tok, err := a.Issue()
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if err := a.Verify(tok); err != nil {
		t.Fatalf("Verify(fresh token): %v", err)
	}
}

func TestIssueIsNotDeterministic(t *testing.T) {
	a := newTestAuth(t, Options{})

	seen := make(map[string]bool, 64)
	for i := 0; i < 64; i++ {
		tok, err := a.Issue()
		if err != nil {
			t.Fatalf("Issue: %v", err)
		}
		if seen[tok] {
			t.Fatal("Issue returned the same token twice; the nonce is not being used")
		}
		seen[tok] = true
	}
}

func TestVerifyRejectsTampering(t *testing.T) {
	a := newTestAuth(t, Options{})
	tok, err := a.Issue()
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	body, tag, _ := strings.Cut(tok, ".")

	// Flip the expiry to far in the future, keeping a valid-looking shape.
	raw, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		t.Fatalf("decode body: %v", err)
	}
	forged := append([]byte(nil), raw...)
	forged[8] = 0xff
	forgedBody := base64.RawURLEncoding.EncodeToString(forged)

	cases := []struct {
		name  string
		token string
		want  error
	}{
		{"forged expiry, original tag", forgedBody + "." + tag, ErrBadToken},
		{"flipped tag byte", body + "." + tamper(t, tag), ErrBadToken},
		{"no separator", strings.ReplaceAll(tok, ".", ""), ErrBadToken},
		{"empty", "", ErrBadToken},
		{"dot only", ".", ErrBadToken},
		{"tag is not base64", body + ".!!!!", ErrBadToken},
		{"body is not base64", "!!!!." + tag, ErrBadToken},
		{"body truncated", base64.RawURLEncoding.EncodeToString(forged[:8]) + "." + tag, ErrBadToken},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := a.Verify(tc.token)
			if !errors.Is(err, tc.want) {
				t.Errorf("Verify() = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestVerifyRejectsOtherSecret(t *testing.T) {
	issuer := newTestAuth(t, Options{Secret: []byte(testSecret)})
	verifier := newTestAuth(t, Options{Secret: []byte("ffffffffffffffffffffffffffffffff")})

	tok, err := issuer.Issue()
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if err := verifier.Verify(tok); !errors.Is(err, ErrBadToken) {
		t.Errorf("Verify(token from another secret) = %v, want ErrBadToken", err)
	}
}

func TestVerifyExpiry(t *testing.T) {
	now := time.Date(2026, time.September, 30, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	a := newTestAuth(t, Options{TTL: time.Hour, Now: clock})

	tok, err := a.Issue()
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	now = now.Add(59 * time.Minute)
	if err := a.Verify(tok); err != nil {
		t.Errorf("Verify at 59m: %v, want nil", err)
	}

	now = now.Add(2 * time.Minute)
	if err := a.Verify(tok); !errors.Is(err, ErrExpired) {
		t.Errorf("Verify at 61m: %v, want ErrExpired", err)
	}
}

func TestLoginSetsCookieTTLFromOptions(t *testing.T) {
	a := newTestAuth(t, Options{TTL: 90 * time.Minute})
	if got := a.TTL(); got != 90*time.Minute {
		t.Errorf("TTL() = %v, want 90m", got)
	}
	if got := newTestAuth(t, Options{}).TTL(); got != DefaultTTL {
		t.Errorf("default TTL() = %v, want %v", got, DefaultTTL)
	}
}

func TestLockoutAfterRepeatedFailures(t *testing.T) {
	now := time.Date(2026, time.September, 30, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	a := newTestAuth(t, Options{Now: clock, MaxAttempts: 3, Lockout: 10 * time.Minute})

	for i := 1; i <= 3; i++ {
		if _, err := a.Login("10.0.0.1", "nope"); !errors.Is(err, ErrBadPassword) {
			t.Fatalf("attempt %d: %v, want ErrBadPassword", i, err)
		}
		// Still inside the allowance until the limit is reached, but after
		// the third failure the lockout must already be in force.
		if i < 3 {
			if _, ok := a.Allow("10.0.0.1"); !ok {
				t.Fatalf("locked out after only %d failures", i)
			}
		}
	}

	if _, ok := a.Allow("10.0.0.1"); ok {
		t.Fatal("Allow() = true after reaching MaxAttempts")
	}
	// The correct password must not bypass an active lockout.
	if _, err := a.Login("10.0.0.1", "correct horse"); !errors.Is(err, ErrLockedOut) {
		t.Errorf("Login(correct password while locked) = %v, want ErrLockedOut", err)
	}

	retry, ok := a.Allow("10.0.0.1")
	if ok {
		t.Fatal("Allow() = true while locked")
	}
	if retry <= 0 || retry > 10*time.Minute {
		t.Errorf("retryAfter = %v, want (0, 10m]", retry)
	}

	now = now.Add(10*time.Minute + time.Second)
	if _, ok := a.Allow("10.0.0.1"); !ok {
		t.Error("Allow() = false after the lockout elapsed")
	}
	if _, err := a.Login("10.0.0.1", "correct horse"); err != nil {
		t.Errorf("Login after lockout: %v, want nil", err)
	}
}

func TestFailuresDoNotAccumulateAcrossWindows(t *testing.T) {
	now := time.Date(2026, time.September, 30, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	a := newTestAuth(t, Options{Now: clock, MaxAttempts: 3, Lockout: 10 * time.Minute})

	for i := 0; i < 3; i++ {
		if _, err := a.Login("10.0.0.1", "nope"); !errors.Is(err, ErrBadPassword) {
			t.Fatalf("attempt %d: %v", i, err)
		}
		now = now.Add(11 * time.Minute) // each failure lands in a fresh window
	}
	if _, ok := a.Allow("10.0.0.1"); !ok {
		t.Error("occasional typos spread over hours must not lock the operator out")
	}
}

func TestSuccessClearsFailures(t *testing.T) {
	a := newTestAuth(t, Options{MaxAttempts: 3, Lockout: time.Hour})

	for i := 0; i < 2; i++ {
		if _, err := a.Login("10.0.0.1", "nope"); !errors.Is(err, ErrBadPassword) {
			t.Fatalf("attempt %d: %v", i, err)
		}
	}
	if _, err := a.Login("10.0.0.1", "correct horse"); err != nil {
		t.Fatalf("Login with the right password: %v", err)
	}
	// Two more failures must not trip the limit, because the counter reset.
	for i := 0; i < 2; i++ {
		if _, err := a.Login("10.0.0.1", "nope"); !errors.Is(err, ErrBadPassword) {
			t.Fatalf("post-success attempt %d: %v", i, err)
		}
	}
}

func TestLockoutIsPerClient(t *testing.T) {
	a := newTestAuth(t, Options{MaxAttempts: 2, Lockout: time.Hour})

	for i := 0; i < 2; i++ {
		if _, err := a.Login("10.0.0.1", "nope"); !errors.Is(err, ErrBadPassword) {
			t.Fatalf("attempt %d: %v", i, err)
		}
	}
	if _, ok := a.Allow("10.0.0.1"); ok {
		t.Error("10.0.0.1 should be locked")
	}
	if _, ok := a.Allow("10.0.0.2"); !ok {
		t.Error("a lockout on one client must not affect another")
	}
}

func TestLoginSuccessIssuesVerifiableToken(t *testing.T) {
	a := newTestAuth(t, Options{})

	tok, err := a.Login("10.0.0.1", "correct horse")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if err := a.Verify(tok); err != nil {
		t.Errorf("the token from Login does not verify: %v", err)
	}
}

func TestClientMapStaysBounded(t *testing.T) {
	a := newTestAuth(t, Options{MaxAttempts: 1, Lockout: time.Millisecond})
	for i := 0; i < maxTrackedClients+64; i++ {
		a.Fail(string(rune('a' + i%26)))
	}
	a.mu.Lock()
	n := len(a.clients)
	a.mu.Unlock()
	if n > maxTrackedClients {
		t.Errorf("client map grew to %d, want <= %d", n, maxTrackedClients)
	}
}

func TestAttemptsLeft(t *testing.T) {
	now := time.Date(2026, time.September, 30, 12, 0, 0, 0, time.UTC)
	a := newTestAuth(t, Options{
		Now:         func() time.Time { return now },
		MaxAttempts: 3,
		Lockout:     10 * time.Minute,
	})

	if got := a.AttemptsLeft("10.0.0.1"); got != 3 {
		t.Errorf("AttemptsLeft for an unseen client = %d, want 3", got)
	}

	a.Fail("10.0.0.1")
	if got := a.AttemptsLeft("10.0.0.1"); got != 2 {
		t.Errorf("after one failure = %d, want 2", got)
	}

	// A lockout reports zero rather than a negative number.
	a.Fail("10.0.0.1")
	a.Fail("10.0.0.1")
	if got := a.AttemptsLeft("10.0.0.1"); got != 0 {
		t.Errorf("while locked out = %d, want 0", got)
	}

	// After the lockout elapses the allowance is whole again.
	now = now.Add(11 * time.Minute)
	if got := a.AttemptsLeft("10.0.0.1"); got != 3 {
		t.Errorf("after the lockout = %d, want 3", got)
	}

	// Success clears the count.
	a.Fail("10.0.0.1")
	a.Succeed("10.0.0.1")
	if got := a.AttemptsLeft("10.0.0.1"); got != 3 {
		t.Errorf("after a success = %d, want 3", got)
	}
}

func TestLoadPassword(t *testing.T) {
	dir := t.TempDir()

	write := func(name, content string, mode os.FileMode) string {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), mode); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		if err := os.Chmod(p, mode); err != nil {
			t.Fatalf("chmod %s: %v", name, err)
		}
		return p
	}

	t.Run("strips trailing newline", func(t *testing.T) {
		p := write("ok", "s3cret\n", 0o600)
		got, err := LoadPassword(p)
		if err != nil {
			t.Fatalf("LoadPassword: %v", err)
		}
		if got != "s3cret" {
			t.Errorf("got %q, want %q", got, "s3cret")
		}
	})

	t.Run("strips CRLF", func(t *testing.T) {
		p := write("crlf", "s3cret\r\n", 0o600)
		got, err := LoadPassword(p)
		if err != nil {
			t.Fatalf("LoadPassword: %v", err)
		}
		if got != "s3cret" {
			t.Errorf("got %q, want %q", got, "s3cret")
		}
	})

	t.Run("keeps an interior space", func(t *testing.T) {
		p := write("space", "correct horse\n", 0o600)
		got, err := LoadPassword(p)
		if err != nil {
			t.Fatalf("LoadPassword: %v", err)
		}
		if got != "correct horse" {
			t.Errorf("got %q, want %q", got, "correct horse")
		}
	})

	t.Run("rejects a world readable file", func(t *testing.T) {
		p := write("loose", "s3cret\n", 0o644)
		if _, err := LoadPassword(p); err == nil {
			t.Error("LoadPassword accepted a 0644 password file")
		}
	})

	t.Run("rejects an empty file", func(t *testing.T) {
		p := write("empty", "\n", 0o600)
		if _, err := LoadPassword(p); err == nil {
			t.Error("LoadPassword accepted an empty file")
		}
	})

	t.Run("reports a missing file", func(t *testing.T) {
		if _, err := LoadPassword(filepath.Join(dir, "nope")); err == nil {
			t.Error("LoadPassword accepted a missing file")
		}
	})
}

func TestVerifyRejectsNonCanonicalEncoding(t *testing.T) {
	a := newTestAuth(t, Options{})
	tok, err := a.Issue()
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	body, tag, _ := strings.Cut(tok, ".")

	// Re-encode the tag with non-zero padding bits, which decodes to the same
	// bytes. Strict decoding must refuse it, or one token would have many
	// spellings.
	raw, err := base64.RawURLEncoding.DecodeString(tag)
	if err != nil {
		t.Fatalf("decode tag: %v", err)
	}
	mangled := base64.RawURLEncoding.EncodeToString(append(raw, 0xff))
	if err := a.Verify(body + "." + mangled); !errors.Is(err, ErrBadToken) {
		t.Errorf("Verify(non-canonical tag) = %v, want ErrBadToken", err)
	}
}

func TestNewSecret(t *testing.T) {
	a, err := NewSecret(32)
	if err != nil {
		t.Fatalf("NewSecret: %v", err)
	}
	if len(a) != 32 {
		t.Fatalf("len = %d, want 32", len(a))
	}
	b, err := NewSecret(32)
	if err != nil {
		t.Fatalf("NewSecret: %v", err)
	}
	if string(a) == string(b) {
		t.Error("NewSecret returned the same bytes twice")
	}
}

// tamper decodes a base64 segment, changes a byte, and re-encodes it.
//
// Changing a character of the encoded text is not enough: the low bits of the
// final base64 character can be padding, so a one-character edit may decode to
// exactly the same bytes and leave the signature valid.
func tamper(t *testing.T, seg string) string {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(seg)
	if err != nil {
		t.Fatalf("tamper: decode %q: %v", seg, err)
	}
	if len(raw) == 0 {
		t.Fatal("tamper: empty segment")
	}
	raw[0] ^= 0x01
	return base64.RawURLEncoding.EncodeToString(raw)
}
