package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const (
	SessionCookie = "share_session"
	sessionTTL    = 30 * 24 * time.Hour

	// Brute-force lockout: after lockThreshold consecutive failures each
	// further failure locks logins for an exponentially growing window.
	lockThreshold = 5
	lockBase      = 2 * time.Second
	lockMax       = 5 * time.Minute
	failSleep     = 500 * time.Millisecond
)

// SessionStore persists login sessions (keyed by token hash) so they survive
// server restarts.
type SessionStore interface {
	CreateSession(tokenHash string, expiresAt int64) error
	// SessionExpiry returns 0 when the session does not exist.
	SessionExpiry(tokenHash string) (int64, error)
	DeleteSession(tokenHash string) error
	DeleteExpiredSessions(now int64) error
}

type Manager struct {
	secret   []byte // HMAC key for download tokens
	sessions SessionStore

	credMu       sync.RWMutex
	username     string
	passwordHash []byte

	// loginMu serializes login attempts so parallel requests can't multiply
	// guessing throughput; failures/lockedUntil implement the lockout.
	loginMu     sync.Mutex
	failures    int
	lockedUntil time.Time
}

// New creates a Manager. The HMAC secret is persisted under dataDir so
// download tokens survive restarts.
func New(dataDir, username, password string, sessions SessionStore) (*Manager, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return nil, err
	}
	return NewWithHash(dataDir, username, string(hash), sessions)
}

// NewWithHash creates a Manager from an existing bcrypt hash (e.g. one
// persisted by the settings page).
func NewWithHash(dataDir, username, passwordHash string, sessions SessionStore) (*Manager, error) {
	secret, err := loadOrCreateSecret(filepath.Join(dataDir, "secret"))
	if err != nil {
		return nil, err
	}
	return &Manager{
		username:     username,
		passwordHash: []byte(passwordHash),
		secret:       secret,
		sessions:     sessions,
	}, nil
}

func (m *Manager) creds() (string, []byte) {
	m.credMu.RLock()
	defer m.credMu.RUnlock()
	return m.username, m.passwordHash
}

// UpdateCredentials swaps the login credentials at runtime. Existing sessions
// stay valid.
func (m *Manager) UpdateCredentials(username, passwordHash string) {
	m.credMu.Lock()
	defer m.credMu.Unlock()
	m.username = username
	m.passwordHash = []byte(passwordHash)
}

// Username returns the current login username.
func (m *Manager) Username() string {
	u, _ := m.creds()
	return u
}

// UpdateUsername changes only the username, keeping the password hash.
func (m *Manager) UpdateUsername(username string) {
	m.credMu.Lock()
	defer m.credMu.Unlock()
	m.username = username
}

// CheckPassword verifies the current password (used to confirm settings
// changes; does not count toward the login lockout).
func (m *Manager) CheckPassword(password string) bool {
	_, hash := m.creds()
	return bcrypt.CompareHashAndPassword(hash, []byte(password)) == nil
}

func loadOrCreateSecret(path string) ([]byte, error) {
	if b, err := os.ReadFile(path); err == nil && len(b) >= 32 {
		return b, nil
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		return nil, err
	}
	return b, nil
}

// Login verifies credentials. It returns a session token on success. On
// failure the token is "" and retryAfter is non-zero if logins are currently
// locked out (the attempt was rejected without being checked).
func (m *Manager) Login(username, password string) (token string, retryAfter time.Duration) {
	m.loginMu.Lock()
	defer m.loginMu.Unlock()

	if remaining := time.Until(m.lockedUntil); remaining > 0 {
		return "", remaining
	}

	wantUser, wantHash := m.creds()
	userOK := subtle.ConstantTimeCompare([]byte(username), []byte(wantUser)) == 1
	passOK := bcrypt.CompareHashAndPassword(wantHash, []byte(password)) == nil
	if !userOK || !passOK {
		m.failures++
		if m.failures >= lockThreshold {
			d := min(lockBase<<uint(min(m.failures-lockThreshold, 30)), lockMax)
			m.lockedUntil = time.Now().Add(d)
		}
		time.Sleep(failSleep) // slow down even pre-lockout guessing
		return "", 0
	}

	m.failures = 0
	token = randomToken()
	m.sessions.DeleteExpiredSessions(time.Now().Unix())
	if err := m.sessions.CreateSession(hashToken(token), time.Now().Add(sessionTTL).Unix()); err != nil {
		return "", 0
	}
	return token, 0
}

func (m *Manager) Logout(token string) {
	m.sessions.DeleteSession(hashToken(token))
}

func (m *Manager) Valid(token string) bool {
	exp, err := m.sessions.SessionExpiry(hashToken(token))
	return err == nil && exp > 0 && time.Now().Unix() < exp
}

// hashToken keeps only a digest in the database so a leaked DB doesn't leak
// usable session cookies.
func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func randomToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// Authed reports whether the request carries a valid session cookie.
func (m *Manager) Authed(r *http.Request) bool {
	c, err := r.Cookie(SessionCookie)
	return err == nil && m.Valid(c.Value)
}

// --- download tokens (proof that a share password was entered) ---

// MakeToken returns "expiry.signature" bound to slug.
func (m *Manager) MakeToken(slug string, ttl time.Duration) string {
	exp := time.Now().Add(ttl).Unix()
	return fmt.Sprintf("%d.%s", exp, m.sign(slug, exp))
}

func (m *Manager) CheckToken(slug, token string) bool {
	expStr, sig, ok := strings.Cut(token, ".")
	if !ok {
		return false
	}
	exp, err := strconv.ParseInt(expStr, 10, 64)
	if err != nil || time.Now().Unix() > exp {
		return false
	}
	return hmac.Equal([]byte(sig), []byte(m.sign(slug, exp)))
}

func (m *Manager) sign(slug string, exp int64) string {
	mac := hmac.New(sha256.New, m.secret)
	fmt.Fprintf(mac, "%s|%d", slug, exp)
	return hex.EncodeToString(mac.Sum(nil))
}
