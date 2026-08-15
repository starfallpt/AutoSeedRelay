package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

const (
	sessionCookieName = "autoseed_session"
	csrfCookieName    = "csrf_token"
	csrfHeaderName    = "X-CSRF-Token"

	sessionTTL       = 24 * time.Hour
	sessionHMACBytes = sha256.Size // 32
	csrfTokenBytes   = 32
)

// StartSession issues a fresh 24h session cookie (value = base64(expiry || hmac))
// and a fresh CSRF double-submit token (cookie + X-CSRF-Token response header).
func (m *Manager) StartSession(c *gin.Context) {
	exp := m.now().Add(sessionTTL).Unix()
	expBytes := make([]byte, 8)
	binary.BigEndian.PutUint64(expBytes, uint64(exp))
	mac := m.sign(expBytes)

	payload := make([]byte, 0, 8+sessionHMACBytes)
	payload = append(payload, expBytes...)
	payload = append(payload, mac...)
	m.setCookie(c, sessionCookieName, base64.RawURLEncoding.EncodeToString(payload), int(sessionTTL.Seconds()))

	m.issueCSRF(c)
}

// EndSession clears the session and CSRF cookies (logout).
func (m *Manager) EndSession(c *gin.Context) {
	m.setCookie(c, sessionCookieName, "", -1)
	m.setCookie(c, csrfCookieName, "", -1)
}

// IssueCSRF generates and delivers a fresh CSRF token (cookie + header) without
// touching the session. /me uses it to refresh the token.
func (m *Manager) IssueCSRF(c *gin.Context) {
	m.issueCSRF(c)
}

// issueCSRF stores a fresh 32-byte CSRF token in the HttpOnly csrf_token
// cookie and echoes it in the X-CSRF-Token response header for the client to
// submit back on state-changing requests (double submit).
func (m *Manager) issueCSRF(c *gin.Context) {
	b := make([]byte, csrfTokenBytes)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand failure is effectively unreachable; fall back so the flow
		// does not stall. Session integrity is independent of the CSRF token.
		b = []byte(fmt.Sprintf("%d", m.now().UnixNano()))
	}
	token := base64.RawURLEncoding.EncodeToString(b)
	m.setCookie(c, csrfCookieName, token, int(sessionTTL.Seconds()))
	c.Header(csrfHeaderName, token)
}

// authenticated reports whether the request carries a valid, unexpired session
// cookie. Expiry is checked first, then the HMAC is compared in constant time.
func (m *Manager) authenticated(c *gin.Context) bool {
	val, err := c.Cookie(sessionCookieName)
	if err != nil || val == "" {
		return false
	}
	payload, err := base64.RawURLEncoding.DecodeString(val)
	if err != nil || len(payload) != 8+sessionHMACBytes {
		return false
	}
	exp := int64(binary.BigEndian.Uint64(payload[:8]))
	if exp <= m.now().Unix() {
		return false
	}
	expected := m.sign(payload[:8])
	return hmac.Equal(expected, payload[8:])
}

// csrfValid reports whether the X-CSRF-Token request header matches the
// csrf_token cookie, compared in constant time.
func (m *Manager) csrfValid(c *gin.Context) bool {
	headerTok := c.GetHeader(csrfHeaderName)
	cookieTok, err := c.Cookie(csrfCookieName)
	if err != nil || headerTok == "" || cookieTok == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(headerTok), []byte(cookieTok)) == 1
}

// sign computes HMAC-SHA256(secret, data).
func (m *Manager) sign(data []byte) []byte {
	h := hmac.New(sha256.New, m.secret)
	h.Write(data)
	return h.Sum(nil)
}

// setCookie writes an HttpOnly, SameSite=Lax cookie. Secure is not forced (it
// is decided by the TLS layer); maxAge <= 0 expires/clears the cookie.
func (m *Manager) setCookie(c *gin.Context, name, value string, maxAge int) {
	c.SetSameSite(http.SameSiteLaxMode)
	c.SetCookie(name, value, maxAge, "/", "", false, true)
}
