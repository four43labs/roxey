// Package auth provides the relay's account primitives: HMAC-signed
// browser session cookies and bcrypt password hashing. It replaces the
// original single-admin Basic Auth.
package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const SessionCookie = "roxey_session"

// sessionTTL keeps dashboard logins alive for a week of inactivity-free use.
const sessionTTL = 7 * 24 * time.Hour

var errInvalidSession = errors.New("invalid session")

// Sessions issues and verifies HMAC-signed stateless cookies of the form
// base64(userID|expiryUnix|hmac). No server-side session store is needed.
type Sessions struct {
	secret []byte
}

func NewSessions(secret string) *Sessions { return &Sessions{secret: []byte(secret)} }

func (s *Sessions) sign(payload string) []byte {
	mac := hmac.New(sha256.New, s.secret)
	mac.Write([]byte(payload))
	return mac.Sum(nil)
}

// Issue returns a fresh signed cookie value for userID.
func (s *Sessions) Issue(userID string) string {
	exp := strconv.FormatInt(time.Now().Add(sessionTTL).Unix(), 10)
	payload := userID + "|" + exp
	val := base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." +
		base64.RawURLEncoding.EncodeToString(s.sign(payload))
	return val
}

// Verify parses a cookie value and returns its userID if unexpired.
func (s *Sessions) Verify(value string) (string, error) {
	val, sig, ok := strings.Cut(value, ".")
	if !ok {
		return "", errInvalidSession
	}
	payload, err := base64.RawURLEncoding.DecodeString(val)
	if err != nil {
		return "", errInvalidSession
	}
	got, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil {
		return "", errInvalidSession
	}
	if !hmac.Equal(got, s.sign(string(payload))) {
		return "", errInvalidSession
	}
	userID, expStr, ok := strings.Cut(string(payload), "|")
	if !ok {
		return "", errInvalidSession
	}
	exp, err := strconv.ParseInt(expStr, 10, 64)
	if err != nil || time.Now().Unix() > exp {
		return "", errInvalidSession
	}
	return userID, nil
}

// SetSessionCookie writes an HttpOnly session cookie scoped to the admin host.
func SetSessionCookie(w http.ResponseWriter, value string) {
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookie,
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(sessionTTL.Seconds()),
	})
}

// ClearSessionCookie expires the session cookie.
func ClearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name: SessionCookie, Value: "", Path: "/", HttpOnly: true,
		SameSite: http.SameSiteLaxMode, MaxAge: -1,
	})
}

// ── tunnel gate cookies ──────────────────────────────────────────────────

const (
	GateCookie            = "roxey_gate"
	gateGroupCookiePrefix = GateCookie + "_"
)

var errInvalidGate = errors.New("invalid gate token")

// IssueGate returns a signed unlock token bound to a tunnel host.
func (s *Sessions) IssueGate(host string) string {
	exp := strconv.FormatInt(time.Now().Add(sessionTTL).Unix(), 10)
	payload := "gate:" + host + "|" + exp
	return base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." +
		base64.RawURLEncoding.EncodeToString(s.sign(payload))
}

// VerifyGate checks an unlock token against the host it was issued for.
func (s *Sessions) VerifyGate(value, host string) bool {
	val, sig, ok := strings.Cut(value, ".")
	if !ok {
		return false
	}
	payload, err := base64.RawURLEncoding.DecodeString(val)
	if err != nil {
		return false
	}
	got, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil || !hmac.Equal(got, s.sign(string(payload))) {
		return false
	}
	body := strings.TrimPrefix(string(payload), "gate:")
	tokenHost, expStr, ok := strings.Cut(body, "|")
	if !ok || tokenHost != host {
		return false
	}
	exp, err := strconv.ParseInt(expStr, 10, 64)
	return err == nil && time.Now().Unix() <= exp
}

// GateGroupCookieName returns a stable cookie name for one user's preview
// group. Separate names let a browser retain grants for multiple projects.
func GateGroupCookieName(userID, group string) string {
	sum := sha256.Sum256([]byte(userID + "\x00" + group))
	return gateGroupCookiePrefix + hexEncode(sum[:16])
}

func gateGroupBinding(userID, group, protectHash string) string {
	sum := sha256.Sum256([]byte(userID + "\x00" + group + "\x00" + protectHash))
	return hexEncode(sum[:])
}

// IssueGateGroup returns a signed grant bound to an owner, group, and secret
// hash. A matching group name alone is not sufficient to reuse the grant.
func (s *Sessions) IssueGateGroup(userID, group, protectHash string) string {
	exp := strconv.FormatInt(time.Now().Add(sessionTTL).Unix(), 10)
	payload := "gate-group:" + gateGroupBinding(userID, group, protectHash) + "|" + exp
	return base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." +
		base64.RawURLEncoding.EncodeToString(s.sign(payload))
}

// VerifyGateGroup checks a grouped grant against all security boundaries.
func (s *Sessions) VerifyGateGroup(value, userID, group, protectHash string) bool {
	val, sig, ok := strings.Cut(value, ".")
	if !ok {
		return false
	}
	payload, err := base64.RawURLEncoding.DecodeString(val)
	if err != nil {
		return false
	}
	got, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil || !hmac.Equal(got, s.sign(string(payload))) {
		return false
	}
	body, ok := strings.CutPrefix(string(payload), "gate-group:")
	if !ok {
		return false
	}
	binding, expStr, ok := strings.Cut(body, "|")
	if !ok || !hmac.Equal([]byte(binding), []byte(gateGroupBinding(userID, group, protectHash))) {
		return false
	}
	exp, err := strconv.ParseInt(expStr, 10, 64)
	return err == nil && time.Now().Unix() <= exp
}

// IsGateCookieName identifies both the legacy host grant and grouped grants.
func IsGateCookieName(name string) bool {
	return name == GateCookie || strings.HasPrefix(name, gateGroupCookiePrefix)
}

// SetGateCookie writes the unlock cookie scoped to the tunnel's own host so
// it never leaks to other subdomains.
func SetGateCookie(w http.ResponseWriter, value, host string) {
	http.SetCookie(w, &http.Cookie{
		Name:     GateCookie,
		Value:    value,
		Path:     "/",
		Domain:   host,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(sessionTTL.Seconds()),
	})
}

// SetGateGroupCookie writes a hosted grant for all sibling tunnel hosts.
func SetGateGroupCookie(w http.ResponseWriter, name, value, domain string) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     "/",
		Domain:   domain,
		Secure:   true,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(sessionTTL.Seconds()),
	})
}

// HashPassword bcrypts a plaintext password for storage.
func HashPassword(pw string) (string, error) {
	b, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
	return string(b), err
}

// CheckPassword compares a plaintext password against a stored hash.
func CheckPassword(hash, pw string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(pw)) == nil
}

// RandomSecret returns n bytes of hex-encoded randomness (session secrets).
func RandomSecret(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hexEncode(b)
}

func hexEncode(b []byte) string {
	const table = "0123456789abcdef"
	out := make([]byte, len(b)*2)
	for i, v := range b {
		out[i*2] = table[v>>4]
		out[i*2+1] = table[v&0x0f]
	}
	return string(out)
}
