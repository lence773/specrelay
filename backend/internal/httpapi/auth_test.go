package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGeneratedBrowserTokenIsOneTime(t *testing.T) {
	auth, tokens := NewAuth("", "mcp-secret")
	first := httptest.NewRequest(http.MethodPost, "/api/v1/auth/exchange?token="+tokens.Browser, nil)
	recorder := httptest.NewRecorder()
	if !auth.Exchange(recorder, first) {
		t.Fatal("first exchange should succeed")
	}
	cookies := recorder.Result().Cookies()
	if len(cookies) != 1 || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode {
		t.Fatalf("unexpected session cookie: %#v", cookies)
	}
	second := httptest.NewRequest(http.MethodPost, "/api/v1/auth/exchange?token="+tokens.Browser, nil)
	if auth.Exchange(httptest.NewRecorder(), second) {
		t.Fatal("generated bootstrap token must not be reusable")
	}
	authed := httptest.NewRequest(http.MethodGet, "/api/v1/projects", nil)
	authed.AddCookie(cookies[0])
	if !auth.Allowed(authed) {
		t.Fatal("issued session cookie should be accepted")
	}
}

func TestConfiguredBearerAndMCPRotation(t *testing.T) {
	auth, _ := NewAuth("browser-secret", "mcp-secret")
	browser := httptest.NewRequest(http.MethodGet, "/api/v1/projects", nil)
	browser.Header.Set("Authorization", "Bearer browser-secret")
	if !auth.Allowed(browser) {
		t.Fatal("configured browser bearer should be accepted")
	}
	mcp := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	mcp.Header.Set("Authorization", "Bearer mcp-secret")
	if !auth.Allowed(mcp) {
		t.Fatal("MCP bearer should be accepted")
	}
	auth.SetMCPToken("rotated")
	if auth.Allowed(mcp) {
		t.Fatal("old MCP token should be invalid after rotation")
	}
	mcp.Header.Set("Authorization", "Bearer rotated")
	if !auth.Allowed(mcp) {
		t.Fatal("rotated MCP token should be accepted")
	}
}

func TestLocalRequestUsesExactOriginHost(t *testing.T) {
	allowed := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:43846/api/v1/projects", nil)
	allowed.Host = "localhost:43846"
	allowed.Header.Set("Origin", "http://127.0.0.1:43847")
	if !LocalRequest(allowed) {
		t.Fatal("loopback host and origin should be allowed")
	}
	for _, origin := range []string{"http://localhost.evil.test", "https://127.0.0.1.evil.test"} {
		req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:43846/api/v1/projects", nil)
		req.Host = "localhost:43846"
		req.Header.Set("Origin", origin)
		if LocalRequest(req) {
			t.Fatalf("origin %q should be rejected", origin)
		}
	}
}

func TestDecodeJSONRejectsTrailingValues(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"version":1} {"version":2}`))
	var in struct {
		Version int64 `json:"version"`
	}
	if err := decodeJSON(req, &in); err == nil {
		t.Fatal("expected trailing JSON value to be rejected")
	}
}
