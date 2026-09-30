package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jkaninda/okapi"
)

const (
	sessionCookie = "gb_admin"
	sessionTTL    = 12 * time.Hour
)

// AdminAuth guards the admin console with a single shared ADMIN_TOKEN.
//
// Sessions are stateless HMAC-signed cookies keyed on the token, so any
// replica can verify a session issued by another, and rotating ADMIN_TOKEN
// logs everyone out.
type AdminAuth struct {
	token string
	key   []byte
}

// NewAdminAuth returns an AdminAuth; an empty token disables the console.
func NewAdminAuth(token string) *AdminAuth {
	sum := sha256.Sum256([]byte("guestbook-admin-session:" + token))
	return &AdminAuth{token: token, key: sum[:]}
}

// Enabled reports whether an admin token is configured.
func (a *AdminAuth) Enabled() bool { return a.token != "" }

// CheckToken compares a candidate against ADMIN_TOKEN in constant time.
func (a *AdminAuth) CheckToken(candidate string) bool {
	return a.Enabled() && subtle.ConstantTimeCompare([]byte(candidate), []byte(a.token)) == 1
}

// Issue returns a signed session value expiring after sessionTTL.
func (a *AdminAuth) Issue(now time.Time) string {
	exp := strconv.FormatInt(now.Add(sessionTTL).Unix(), 10)
	return exp + "." + a.sign(exp)
}

// Verify reports whether a session value is authentic and unexpired.
func (a *AdminAuth) Verify(value string, now time.Time) bool {
	if !a.Enabled() {
		return false
	}
	exp, sig, ok := strings.Cut(value, ".")
	if !ok || !hmac.Equal([]byte(sig), []byte(a.sign(exp))) {
		return false
	}
	unix, err := strconv.ParseInt(exp, 10, 64)
	return err == nil && now.Unix() < unix
}

func (a *AdminAuth) sign(payload string) string {
	m := hmac.New(sha256.New, a.key)
	m.Write([]byte(payload))
	return hex.EncodeToString(m.Sum(nil))
}

// Authenticated accepts either a session cookie (the console) or an
// "Authorization: Bearer <ADMIN_TOKEN>" header (curl during a demo).
func (a *AdminAuth) Authenticated(c *okapi.Context) bool {
	if bearer, ok := strings.CutPrefix(c.Header("Authorization"), "Bearer "); ok {
		return a.CheckToken(strings.TrimSpace(bearer))
	}
	v, err := c.Cookie(sessionCookie)
	return err == nil && a.Verify(v, time.Now())
}

// Require is middleware that rejects unauthenticated requests.
func (a *AdminAuth) Require(c *okapi.Context) error {
	if !a.Enabled() {
		return c.JSON(http.StatusNotFound, okapi.M{"error": "admin console is disabled (set ADMIN_TOKEN)"})
	}
	if !a.Authenticated(c) {
		return c.JSON(http.StatusUnauthorized, okapi.M{"error": "authentication required"})
	}
	return c.Next()
}

// SetSession writes (or, with an empty value, clears) the session cookie.
func (a *AdminAuth) SetSession(c *okapi.Context, value string) {
	maxAge := int(sessionTTL.Seconds())
	if value == "" {
		maxAge = -1
	}
	r := c.Request()
	http.SetCookie(c.ResponseWriter(), &http.Cookie{
		Name:     sessionCookie,
		Value:    value,
		Path:     "/",
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https",
		SameSite: http.SameSiteStrictMode,
	})
}
