package api

import (
	"crypto/subtle"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"cpa-usage-keeper/internal/auth"
	"github.com/gin-gonic/gin"
)

const sessionCookieName = "cpa_usage_keeper_session"

const maxFailedLoginAttempts = 5

type AuthConfig struct {
	Enabled       bool
	LoginPassword string
	SessionTTL    time.Duration
	BasePath      string
}

type authHandler struct {
	config   AuthConfig
	sessions *auth.SessionManager

	mu             sync.Mutex
	failedAttempts map[string]int
}

type loginRequest struct {
	Password string `json:"password"`
}

type sessionResponse struct {
	Authenticated bool `json:"authenticated"`
}

type loginResponse struct {
	Token     string `json:"token"`
	ExpiresAt string `json:"expires_at"`
}

func NewAuthHandler(config AuthConfig, sessions *auth.SessionManager) *authHandler {
	return &authHandler{config: config, sessions: sessions, failedAttempts: make(map[string]int)}
}

func (h *authHandler) registerRoutes(router gin.IRoutes) {
	router.GET("/session", h.getSession)
	router.POST("/login", h.login)
	router.POST("/logout", h.logout)
}

func (h *authHandler) extractToken(c *gin.Context) string {
	if authHeader := c.GetHeader("Authorization"); strings.HasPrefix(authHeader, "Bearer ") {
		if token := strings.TrimSpace(strings.TrimPrefix(authHeader, "Bearer ")); token != "" {
			return token
		}
	}
	if token := strings.TrimSpace(c.GetHeader("X-Auth-Token")); token != "" {
		return token
	}
	if token, err := c.Cookie(sessionCookieName); err == nil && token != "" {
		return token
	}
	return ""
}

func (h *authHandler) middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if h == nil || !h.config.Enabled {
			c.Next()
			return
		}
		if h.sessions == nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
			return
		}

		token := h.extractToken(c)
		if token == "" || !h.sessions.Validate(token) {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
			return
		}

		c.Next()
	}
}

func (h *authHandler) getSession(c *gin.Context) {
	c.Header("Cache-Control", "no-store, no-cache, must-revalidate, max-age=0")
	c.Header("Pragma", "no-cache")

	if h == nil || !h.config.Enabled {
		c.JSON(http.StatusOK, sessionResponse{Authenticated: true})
		return
	}
	if h.sessions == nil {
		c.JSON(http.StatusOK, sessionResponse{Authenticated: false})
		return
	}

	token := h.extractToken(c)
	if token == "" {
		c.JSON(http.StatusOK, sessionResponse{Authenticated: false})
		return
	}

	c.JSON(http.StatusOK, sessionResponse{Authenticated: h.sessions.Validate(token)})
}

func (h *authHandler) login(c *gin.Context) {
	if h == nil || !h.config.Enabled {
		c.Status(http.StatusNoContent)
		return
	}
	if h.sessions == nil {
		writeInternalError(c, "session manager is not configured", nil)
		return
	}

	var request loginRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	clientKey := loginClientKey(c)
	passwordMatches := subtle.ConstantTimeCompare([]byte(request.Password), []byte(h.config.LoginPassword)) == 1
	if h.tooManyFailedAttempts(clientKey) && !passwordMatches {
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "too many failed login attempts"})
		return
	}

	if !passwordMatches {
		h.recordFailedAttempt(clientKey)
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid password"})
		return
	}
	h.clearFailedAttempts(clientKey)

	token, expiresAt, err := h.sessions.Create()
	if err != nil {
		writeInternalError(c, "create auth session failed", err)
		return
	}

	proto := c.GetHeader("X-Forwarded-Proto")
	secure := c.Request.TLS != nil || strings.HasPrefix(strings.ToLower(proto), "https") || strings.EqualFold(c.GetHeader("X-Forwarded-Ssl"), "on")
	cookiePath := h.config.BasePath
	if cookiePath == "" {
		cookiePath = "/"
	}
	http.SetCookie(c.Writer, &http.Cookie{
		Name:     sessionCookieName,
		Value:    token,
		Path:     cookiePath,
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
		Expires:  expiresAt,
		MaxAge:   int(time.Until(expiresAt).Seconds()),
	})
	c.JSON(http.StatusOK, loginResponse{
		Token:     token,
		ExpiresAt: expiresAt.Format(time.RFC3339),
	})
}

func (h *authHandler) logout(c *gin.Context) {
	if h == nil || !h.config.Enabled {
		c.Status(http.StatusNoContent)
		return
	}
	if h.sessions != nil {
		if token := h.extractToken(c); token != "" {
			h.sessions.Delete(token)
		}
	}
	clearSessionCookie(c, h.config.BasePath)
	c.Status(http.StatusNoContent)
}

func (h *authHandler) tooManyFailedAttempts(key string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.failedAttempts[key] >= maxFailedLoginAttempts
}

func (h *authHandler) recordFailedAttempt(key string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.failedAttempts[key]++
}

func (h *authHandler) clearFailedAttempts(key string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.failedAttempts, key)
}

func loginClientKey(c *gin.Context) string {
	host, _, err := net.SplitHostPort(c.Request.RemoteAddr)
	if err == nil && host != "" {
		return host
	}
	return c.ClientIP()
}

func clearSessionCookie(c *gin.Context, basePath string) {
	cookiePath := basePath
	if cookiePath == "" {
		cookiePath = "/"
	}
	http.SetCookie(c.Writer, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     cookiePath,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Expires:  time.Unix(0, 0),
		MaxAge:   -1,
	})
}
