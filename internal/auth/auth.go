// Package auth provides password hashing and signed-cookie session handling for
// the single-static-password web gate.
package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"net/http"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// HashPassword returns a bcrypt hash of the given plaintext password.
func HashPassword(plaintext string) (string, error) {
	if strings.TrimSpace(plaintext) == "" {
		return "", errors.New("auth: password must not be empty")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(plaintext), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	return string(hash), nil
}

// VerifyPassword reports whether plaintext matches the stored bcrypt hash.
func VerifyPassword(hash, plaintext string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(plaintext)) == nil
}

// SessionManager issues and validates HMAC-signed session cookies.
type SessionManager struct {
	secret     []byte
	ttl        time.Duration
	cookieName string
	secure     bool
}

// DefaultCookieName is the name of the session cookie.
const DefaultCookieName = "vsat_session"

// NewSessionManager builds a manager. secure marks the cookie Secure (HTTPS-only).
func NewSessionManager(secret []byte, ttl time.Duration, secure bool) *SessionManager {
	if ttl <= 0 {
		ttl = 12 * time.Hour
	}
	return &SessionManager{
		secret:     secret,
		ttl:        ttl,
		cookieName: DefaultCookieName,
		secure:     secure,
	}
}

// Issue sets a fresh signed session cookie on the response.
func (m *SessionManager) Issue(w http.ResponseWriter) {
	expiry := time.Now().Add(m.ttl)
	http.SetCookie(w, &http.Cookie{
		Name:     m.cookieName,
		Value:    m.sign(expiry),
		Path:     "/",
		Expires:  expiry,
		HttpOnly: true,
		Secure:   m.secure,
		SameSite: http.SameSiteLaxMode,
	})
}

// Clear removes the session cookie (logout).
func (m *SessionManager) Clear(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     m.cookieName,
		Value:    "",
		Path:     "/",
		Expires:  time.Unix(0, 0),
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   m.secure,
		SameSite: http.SameSiteLaxMode,
	})
}

// Valid reports whether the request carries a valid, unexpired session.
func (m *SessionManager) Valid(r *http.Request) bool {
	c, err := r.Cookie(m.cookieName)
	if err != nil {
		return false
	}
	return m.verify(c.Value)
}

// sign encodes "<expiryUnix>|<hmac>" as a single base64url token.
func (m *SessionManager) sign(expiry time.Time) string {
	buf := make([]byte, 8)
	binary.BigEndian.PutUint64(buf, uint64(expiry.Unix()))
	mac := m.mac(buf)
	token := append(buf, mac...)
	return base64.RawURLEncoding.EncodeToString(token)
}

func (m *SessionManager) verify(value string) bool {
	token, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(token) != 8+sha256.Size {
		return false
	}
	expiryBytes, mac := token[:8], token[8:]
	expected := m.mac(expiryBytes)
	if subtle.ConstantTimeCompare(mac, expected) != 1 {
		return false
	}
	expiry := int64(binary.BigEndian.Uint64(expiryBytes))
	return time.Now().Unix() < expiry
}

func (m *SessionManager) mac(data []byte) []byte {
	h := hmac.New(sha256.New, m.secret)
	h.Write(data)
	return h.Sum(nil)
}

// RequireSession wraps next so unauthenticated requests are redirected to /login
// (or rejected with 401 for non-GET/WebSocket requests).
func (m *SessionManager) RequireSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if m.Valid(r) {
			next.ServeHTTP(w, r)
			return
		}
		if r.Method == http.MethodGet && !isWebSocket(r) {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	})
}

func isWebSocket(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("Upgrade"), "websocket")
}
