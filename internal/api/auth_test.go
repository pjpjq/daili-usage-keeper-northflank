package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"cpa-usage-keeper/internal/auth"
)

func TestAuthSessionReportsAuthenticatedWhenDisabled(t *testing.T) {
	router := NewRouter(nil, nil, nil, nil, AuthConfig{Enabled: false}, nil, "")
	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/session", nil)

	router.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK || !contains(resp.Body.String(), `"authenticated":true`) {
		t.Fatalf("unexpected response: %d %s", resp.Code, resp.Body.String())
	}
}

func TestAuthProtectedRouteRequiresSessionWhenEnabled(t *testing.T) {
	sessions := auth.NewSessionManager(time.Hour)
	config := AuthConfig{Enabled: true, LoginPassword: "secret", SessionTTL: time.Hour}
	router := NewRouter(nil, nil, nil, nil, config, NewAuthHandler(config, sessions), "")
	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/usage/overview", nil)

	router.ServeHTTP(resp, req)

	if resp.Code != http.StatusUnauthorized {
		t.Fatalf("expected status 401, got %d", resp.Code)
	}
}

func TestAuthLoginSetsCookieAndUnlocksProtectedRoute(t *testing.T) {
	sessions := auth.NewSessionManager(time.Hour)
	config := AuthConfig{Enabled: true, LoginPassword: "secret", SessionTTL: time.Hour}
	handler := NewAuthHandler(config, sessions)
	router := NewRouter(nil, nil, nil, nil, config, handler, "")

	loginResp := httptest.NewRecorder()
	loginReq := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(`{"password":"secret"}`))
	loginReq.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(loginResp, loginReq)

	if loginResp.Code != http.StatusOK {
		t.Fatalf("expected login status 200, got %d", loginResp.Code)
	}
	if !contains(loginResp.Body.String(), `"token":`) {
		t.Fatalf("expected login response to contain token: %s", loginResp.Body.String())
	}
	cookie := loginResp.Result().Cookies()
	if len(cookie) == 0 {
		t.Fatal("expected auth cookie to be set")
	}
	if cookie[0].Name != sessionCookieName {
		t.Fatalf("expected cookie %q, got %q", sessionCookieName, cookie[0].Name)
	}
	if cookie[0].Path != "/" {
		t.Fatalf("expected root cookie path '/', got %q", cookie[0].Path)
	}

	usageResp := httptest.NewRecorder()
	usageReq := httptest.NewRequest(http.MethodGet, "/api/v1/usage/overview", nil)
	usageReq.AddCookie(cookie[0])
	router.ServeHTTP(usageResp, usageReq)

	if usageResp.Code != http.StatusOK {
		t.Fatalf("expected protected route to succeed, got %d %s", usageResp.Code, usageResp.Body.String())
	}
}

func TestAuthLoginRejectsWrongPassword(t *testing.T) {
	sessions := auth.NewSessionManager(time.Hour)
	config := AuthConfig{Enabled: true, LoginPassword: "secret", SessionTTL: time.Hour}
	router := NewRouter(nil, nil, nil, nil, config, NewAuthHandler(config, sessions), "")
	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(`{"password":"wrong"}`))
	req.Header.Set("Content-Type", "application/json")

	router.ServeHTTP(resp, req)

	if resp.Code != http.StatusUnauthorized {
		t.Fatalf("expected status 401, got %d", resp.Code)
	}
}

func TestAuthLoginRateLimitsRepeatedFailures(t *testing.T) {
	sessions := auth.NewSessionManager(time.Hour)
	config := AuthConfig{Enabled: true, LoginPassword: "secret", SessionTTL: time.Hour}
	router := NewRouter(nil, nil, nil, nil, config, NewAuthHandler(config, sessions), "")

	for i := 0; i < 5; i++ {
		resp := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(`{"password":"wrong"}`))
		req.Header.Set("Content-Type", "application/json")
		req.RemoteAddr = "198.51.100.1:1234"
		router.ServeHTTP(resp, req)
		if resp.Code != http.StatusUnauthorized {
			t.Fatalf("expected failed attempt %d to return 401, got %d", i+1, resp.Code)
		}
	}

	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(`{"password":"wrong"}`))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "198.51.100.1:1234"
	router.ServeHTTP(resp, req)

	if resp.Code != http.StatusTooManyRequests {
		t.Fatalf("expected repeated failed attempts to return 429, got %d", resp.Code)
	}
}

func TestAuthLoginAllowsCorrectPasswordAfterRateLimitThreshold(t *testing.T) {
	sessions := auth.NewSessionManager(time.Hour)
	config := AuthConfig{Enabled: true, LoginPassword: "secret", SessionTTL: time.Hour}
	router := NewRouter(nil, nil, nil, nil, config, NewAuthHandler(config, sessions), "")

	for i := 0; i < 5; i++ {
		resp := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(`{"password":"wrong"}`))
		req.Header.Set("Content-Type", "application/json")
		req.RemoteAddr = "198.51.100.2:1234"
		router.ServeHTTP(resp, req)
		if resp.Code != http.StatusUnauthorized {
			t.Fatalf("expected failed attempt %d to return 401, got %d", i+1, resp.Code)
		}
	}

	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(`{"password":"secret"}`))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "198.51.100.2:1234"
	router.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("expected correct password to clear failed attempts and login, got %d", resp.Code)
	}
}

func TestAuthLogoutDeletesSessionCookie(t *testing.T) {
	sessions := auth.NewSessionManager(time.Hour)
	config := AuthConfig{Enabled: true, LoginPassword: "secret", SessionTTL: time.Hour}
	handler := NewAuthHandler(config, sessions)
	router := NewRouter(nil, nil, nil, nil, config, handler, "")

	loginResp := httptest.NewRecorder()
	loginReq := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(`{"password":"secret"}`))
	loginReq.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(loginResp, loginReq)
	if loginResp.Code != http.StatusOK {
		t.Fatalf("expected login status 200, got %d", loginResp.Code)
	}
	cookies := loginResp.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatal("expected auth cookie to be set")
	}

	logoutResp := httptest.NewRecorder()
	logoutReq := httptest.NewRequest(http.MethodPost, "/api/v1/auth/logout", nil)
	logoutReq.AddCookie(cookies[0])
	router.ServeHTTP(logoutResp, logoutReq)
	if logoutResp.Code != http.StatusNoContent {
		t.Fatalf("expected logout status 204, got %d", logoutResp.Code)
	}
	clearCookies := logoutResp.Result().Cookies()
	if len(clearCookies) == 0 || clearCookies[0].Name != sessionCookieName || clearCookies[0].MaxAge >= 0 {
		t.Fatalf("expected logout to clear session cookie, got %+v", clearCookies)
	}

	usageResp := httptest.NewRecorder()
	usageReq := httptest.NewRequest(http.MethodGet, "/api/v1/usage/overview", nil)
	usageReq.AddCookie(cookies[0])
	router.ServeHTTP(usageResp, usageReq)
	if usageResp.Code != http.StatusUnauthorized {
		t.Fatalf("expected logged out session to be rejected, got %d", usageResp.Code)
	}
}

func TestSubpathAuthUsesPrefixedRoutesAndCookiePath(t *testing.T) {
	sessions := auth.NewSessionManager(time.Hour)
	config := AuthConfig{Enabled: true, LoginPassword: "secret", SessionTTL: time.Hour, BasePath: "/cpa"}
	handler := NewAuthHandler(config, sessions)
	router := NewRouter(nil, nil, nil, nil, config, handler, "/cpa")

	sessionResp := httptest.NewRecorder()
	sessionReq := httptest.NewRequest(http.MethodGet, "/cpa/api/v1/auth/session", nil)
	router.ServeHTTP(sessionResp, sessionReq)
	if sessionResp.Code != http.StatusOK || !contains(sessionResp.Body.String(), `"authenticated":false`) {
		t.Fatalf("unexpected session response: %d %s", sessionResp.Code, sessionResp.Body.String())
	}

	loginResp := httptest.NewRecorder()
	loginReq := httptest.NewRequest(http.MethodPost, "/cpa/api/v1/auth/login", strings.NewReader(`{"password":"secret"}`))
	loginReq.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(loginResp, loginReq)
	if loginResp.Code != http.StatusOK {
		t.Fatalf("expected login status 200, got %d", loginResp.Code)
	}
	cookies := loginResp.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatal("expected auth cookie to be set")
	}
	if cookies[0].Path != "/cpa" {
		t.Fatalf("expected subpath cookie path '/cpa', got %q", cookies[0].Path)
	}

	usageResp := httptest.NewRecorder()
	usageReq := httptest.NewRequest(http.MethodGet, "/cpa/api/v1/usage/overview", nil)
	usageReq.AddCookie(cookies[0])
	router.ServeHTTP(usageResp, usageReq)
	if usageResp.Code != http.StatusOK {
		t.Fatalf("expected protected route under subpath to succeed, got %d %s", usageResp.Code, usageResp.Body.String())
	}

	unprefixedResp := httptest.NewRecorder()
	unprefixedReq := httptest.NewRequest(http.MethodGet, "/api/v1/usage/overview", nil)
	unprefixedReq.AddCookie(cookies[0])
	router.ServeHTTP(unprefixedResp, unprefixedReq)
	if unprefixedResp.Code != http.StatusNotFound {
		t.Fatalf("expected unprefixed route to 404, got %d", unprefixedResp.Code)
	}
}

func TestAuthBearerTokenAndHeaders(t *testing.T) {
	sessions := auth.NewSessionManager(time.Hour)
	config := AuthConfig{Enabled: true, LoginPassword: "secret", SessionTTL: time.Hour}
	handler := NewAuthHandler(config, sessions)
	router := NewRouter(nil, nil, nil, nil, config, handler, "")

	// Login and obtain token
	loginResp := httptest.NewRecorder()
	loginReq := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(`{"password":"secret"}`))
	loginReq.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(loginResp, loginReq)

	if loginResp.Code != http.StatusOK {
		t.Fatalf("expected login status 200, got %d", loginResp.Code)
	}

	cookies := loginResp.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatal("expected cookie")
	}
	token := cookies[0].Value

	// Session check with Authorization Bearer header (no cookie)
	sessResp := httptest.NewRecorder()
	sessReq := httptest.NewRequest(http.MethodGet, "/api/v1/auth/session", nil)
	sessReq.Header.Set("Authorization", "Bearer "+token)
	router.ServeHTTP(sessResp, sessReq)

	if sessResp.Code != http.StatusOK || !contains(sessResp.Body.String(), `"authenticated":true`) {
		t.Fatalf("expected session check to succeed with Bearer token, got %d %s", sessResp.Code, sessResp.Body.String())
	}
	if cacheCtrl := sessResp.Header().Get("Cache-Control"); !strings.Contains(cacheCtrl, "no-store") {
		t.Fatalf("expected Cache-Control: no-store header, got %q", cacheCtrl)
	}

	// Protected route with Bearer header (no cookie)
	usageResp := httptest.NewRecorder()
	usageReq := httptest.NewRequest(http.MethodGet, "/api/v1/usage/overview", nil)
	usageReq.Header.Set("Authorization", "Bearer "+token)
	router.ServeHTTP(usageResp, usageReq)

	if usageResp.Code != http.StatusOK {
		t.Fatalf("expected protected route to succeed with Bearer token, got %d %s", usageResp.Code, usageResp.Body.String())
	}

	// Protected route with X-Auth-Token header (no cookie)
	xResp := httptest.NewRecorder()
	xReq := httptest.NewRequest(http.MethodGet, "/api/v1/usage/overview", nil)
	xReq.Header.Set("X-Auth-Token", token)
	router.ServeHTTP(xResp, xReq)

	if xResp.Code != http.StatusOK {
		t.Fatalf("expected protected route to succeed with X-Auth-Token, got %d %s", xResp.Code, xResp.Body.String())
	}

	// Invalid Bearer token
	badResp := httptest.NewRecorder()
	badReq := httptest.NewRequest(http.MethodGet, "/api/v1/usage/overview", nil)
	badReq.Header.Set("Authorization", "Bearer invalid-token")
	router.ServeHTTP(badResp, badReq)

	if badResp.Code != http.StatusUnauthorized {
		t.Fatalf("expected invalid Bearer token to be rejected with 401, got %d", badResp.Code)
	}
}
