package auth

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// isAuthExempt reports whether path is reachable without a valid session:
// health, login, and the setup endpoints (which must work before initialization).
func isAuthExempt(path string) bool {
	switch path {
	case "/api/v2/health", "/api/v2/auth/login":
		return true
	}
	return strings.HasPrefix(path, "/api/v2/setup/")
}

// Middleware enforces authentication and CSRF on the API. Non-API paths (the
// embedded web UI and static assets, served by webfs) are left public so the SPA
// can render the login/setup page. Within /api/v2:
//
//   - /api/v2/health, /api/v2/auth/login, and /api/v2/setup/* pass through;
//   - every other route returns 403 while the system is uninitialized, 401 while
//     the caller is unauthenticated, and 403 when a POST/PUT/DELETE fails CSRF.
func (m *Manager) Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		p := c.Request.URL.Path
		if !strings.HasPrefix(p, "/api/v2/") || isAuthExempt(p) {
			c.Next()
			return
		}
		if !m.SetupState() {
			c.AbortWithStatus(http.StatusForbidden)
			return
		}
		if !m.authenticated(c) {
			c.AbortWithStatus(http.StatusUnauthorized)
			return
		}
		switch c.Request.Method {
		case http.MethodPost, http.MethodPut, http.MethodDelete:
			if !m.csrfValid(c) {
				c.AbortWithStatus(http.StatusForbidden)
				return
			}
		}
		c.Next()
	}
}
