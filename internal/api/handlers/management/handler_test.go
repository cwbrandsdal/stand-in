package management

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/pluginhost"
)

func TestAuthenticateManagementKey_LocalhostIPBan_BlocksCorrectKeyDuringBan(t *testing.T) {
	h := &Handler{
		cfg:            &config.Config{},
		failedAttempts: make(map[string]*attemptInfo),
		envSecret:      "test-secret",
	}

	for i := 0; i < 5; i++ {
		allowed, statusCode, errMsg := h.AuthenticateManagementKey("127.0.0.1", true, "wrong-secret")
		if allowed {
			t.Fatalf("expected auth to be denied at attempt %d", i+1)
		}
		if statusCode != http.StatusUnauthorized || errMsg != "invalid management key" {
			t.Fatalf("unexpected auth failure at attempt %d: status=%d msg=%q", i+1, statusCode, errMsg)
		}
	}

	allowed, statusCode, errMsg := h.AuthenticateManagementKey("127.0.0.1", true, "test-secret")
	if allowed {
		t.Fatalf("expected correct key to be denied while banned")
	}
	if statusCode != http.StatusForbidden {
		t.Fatalf("expected forbidden status while banned, got %d", statusCode)
	}
	if !strings.HasPrefix(errMsg, "IP banned due to too many failed attempts. Try again in") {
		t.Fatalf("unexpected banned message: %q", errMsg)
	}
}

func TestMiddlewareSetsSupportPluginHeader(t *testing.T) {

	h := &Handler{
		cfg:            &config.Config{},
		failedAttempts: make(map[string]*attemptInfo),
		envSecret:      "test-secret",
	}
	middleware := h.Middleware()

	t.Run("invalid key", func(t *testing.T) {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodGet, "/v0/management/config", nil)
		c.Request.RemoteAddr = "127.0.0.1:12345"
		c.Request.Header.Set("X-Management-Key", "wrong-secret")

		middleware(c)

		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
		}
		if got := rec.Header().Get("X-CPA-SUPPORT-PLUGIN"); got != pluginhost.SupportPluginHeaderValue() {
			t.Fatalf("X-CPA-SUPPORT-PLUGIN = %q, want %q", got, pluginhost.SupportPluginHeaderValue())
		}
	})

	t.Run("valid key", func(t *testing.T) {
		engine := gin.New()
		engine.GET("/v0/management/config", middleware, func(c *gin.Context) {
			c.Status(http.StatusOK)
		})

		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/v0/management/config", nil)
		req.RemoteAddr = "127.0.0.1:12345"
		req.Header.Set("X-Management-Key", "test-secret")
		engine.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
		}
		if got := rec.Header().Get("X-CPA-SUPPORT-PLUGIN"); got != pluginhost.SupportPluginHeaderValue() {
			t.Fatalf("X-CPA-SUPPORT-PLUGIN = %q, want %q", got, pluginhost.SupportPluginHeaderValue())
		}
	})
}

func TestMiddlewareWithReadOnlyKey(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const readOnlyKey = "read-only-key"
	newHandler := func(allowRemote bool) *Handler {
		cfg := &config.Config{}
		cfg.RemoteManagement.AllowRemote = allowRemote
		return &Handler{cfg: cfg, failedAttempts: make(map[string]*attemptInfo), envSecret: "test-secret"}
	}
	readOnly := func(provided string) bool { return provided == readOnlyKey }
	serve := func(middleware gin.HandlerFunc, remoteAddr, key string) (int, bool) {
		engine := gin.New()
		var readOnlyAccess bool
		engine.GET("/v8/management/insights/status", middleware, func(c *gin.Context) {
			readOnlyAccess = c.GetBool(ReadOnlyAccessContextKey)
			c.Status(http.StatusOK)
		})
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/v8/management/insights/status", nil)
		req.RemoteAddr = remoteAddr
		req.Header.Set("Authorization", "Bearer "+key)
		engine.ServeHTTP(rec, req)
		return rec.Code, readOnlyAccess
	}

	t.Run("read-only key is accepted and marked", func(t *testing.T) {
		h := newHandler(true)
		if code, ro := serve(h.MiddlewareWithReadOnlyKey(readOnly), "192.0.2.10:1234", readOnlyKey); code != http.StatusOK || !ro {
			t.Fatalf("status=%d readOnly=%v, want 200 true", code, ro)
		}
	})
	t.Run("management key is not marked read-only", func(t *testing.T) {
		h := newHandler(true)
		if code, ro := serve(h.MiddlewareWithReadOnlyKey(readOnly), "192.0.2.10:1234", "test-secret"); code != http.StatusOK || ro {
			t.Fatalf("status=%d readOnly=%v, want 200 false", code, ro)
		}
	})
	t.Run("plain middleware rejects the read-only key", func(t *testing.T) {
		h := newHandler(true)
		if code, _ := serve(h.Middleware(), "192.0.2.10:1234", readOnlyKey); code != http.StatusUnauthorized {
			t.Fatalf("status=%d, want 401", code)
		}
	})
	t.Run("remote access rule applies", func(t *testing.T) {
		h := newHandler(false)
		if code, _ := serve(h.MiddlewareWithReadOnlyKey(readOnly), "192.0.2.10:1234", readOnlyKey); code != http.StatusForbidden {
			t.Fatalf("status=%d, want 403", code)
		}
	})
	t.Run("failure ban applies", func(t *testing.T) {
		h := newHandler(true)
		middleware := h.MiddlewareWithReadOnlyKey(readOnly)
		for i := 0; i < 5; i++ {
			if code, _ := serve(middleware, "192.0.2.10:1234", "wrong-key"); code != http.StatusUnauthorized {
				t.Fatalf("attempt %d: status=%d, want 401", i+1, code)
			}
		}
		if code, _ := serve(middleware, "192.0.2.10:1234", readOnlyKey); code != http.StatusForbidden {
			t.Fatalf("read-only key during ban: status=%d, want 403", code)
		}
	})
}
