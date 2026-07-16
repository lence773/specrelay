package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lyming99/specrelay/backend/internal/mcpapi"
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
	if !auth.MCPTokenConfigured() {
		t.Fatal("configured MCP token should be reported without exposing its value")
	}
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
	auth.SetMCPToken("")
	if auth.MCPTokenConfigured() {
		t.Fatal("cleared MCP token should be reported as unconfigured")
	}
	if auth.Allowed(mcp) {
		t.Fatal("cleared MCP token must not be accepted")
	}
}

func TestMCPBearerCannotAccessBrowserSettingsRoutes(t *testing.T) {
	auth, _ := NewAuth("browser-secret", "mcp-secret")
	server := &Server{Auth: auth, MCP: mcpapi.Handler(nil, nil)}
	handler := server.Handler()
	for _, route := range []struct {
		method string
		path   string
	}{
		{method: http.MethodGet, path: "/api/v1/settings/mcp"},
		{method: http.MethodPost, path: "/api/v1/settings/mcp/diagnostics"},
		{method: http.MethodPost, path: "/api/v1/settings/mcp-token/rotate"},
	} {
		req := httptest.NewRequest(route.method, "http://127.0.0.1:43846"+route.path, nil)
		req.Header.Set("Authorization", "Bearer mcp-secret")
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, req)
		if recorder.Code != http.StatusUnauthorized {
			t.Fatalf("MCP bearer %s %s status=%d body=%s", route.method, route.path, recorder.Code, recorder.Body.String())
		}
	}
}

func TestMCPSettingsAndDiagnosticDoNotExposeToken(t *testing.T) {
	const token = "mcp-secret-not-for-settings"
	auth, _ := NewAuth("browser-secret", token)
	server := &Server{Auth: auth, MCP: mcpapi.Handler(nil, nil)}

	infoRecorder := httptest.NewRecorder()
	server.mcpConnectionInfo(infoRecorder, httptest.NewRequest(http.MethodGet, "/api/v1/settings/mcp", nil))
	if infoRecorder.Code != http.StatusOK {
		t.Fatalf("connection info status=%d body=%s", infoRecorder.Code, infoRecorder.Body.String())
	}
	if strings.Contains(infoRecorder.Body.String(), token) {
		t.Fatalf("connection info exposed MCP token: %s", infoRecorder.Body.String())
	}
	var info mcpConnectionInfo
	if err := json.NewDecoder(infoRecorder.Body).Decode(&info); err != nil {
		t.Fatal(err)
	}
	if info.EndpointPath != mcpapi.EndpointPath || info.Transport != mcpapi.Transport || info.Authentication.Scheme != mcpapi.AuthenticationScheme || info.Token.State != "configured" {
		t.Fatalf("unexpected MCP connection info: %+v", info)
	}
	tools := mcpapi.Tools()
	if len(info.Tools) != len(tools) {
		t.Fatalf("settings tools=%d, registered tools=%d", len(info.Tools), len(tools))
	}
	for i := range tools {
		if info.Tools[i] != tools[i] {
			t.Fatalf("settings tool %d=%+v, registered tool=%+v", i, info.Tools[i], tools[i])
		}
	}

	diagnosticRecorder := httptest.NewRecorder()
	server.diagnoseMCP(diagnosticRecorder, httptest.NewRequest(http.MethodPost, "/api/v1/settings/mcp/diagnostics", nil))
	if diagnosticRecorder.Code != http.StatusOK {
		t.Fatalf("diagnostic status=%d body=%s", diagnosticRecorder.Code, diagnosticRecorder.Body.String())
	}
	if strings.Contains(diagnosticRecorder.Body.String(), token) {
		t.Fatalf("diagnostic exposed MCP token: %s", diagnosticRecorder.Body.String())
	}
	var diagnostic mcpDiagnostic
	if err := json.NewDecoder(diagnosticRecorder.Body).Decode(&diagnostic); err != nil {
		t.Fatal(err)
	}
	if !diagnostic.Success || diagnostic.CheckedAt.IsZero() || diagnostic.Failure != "" {
		t.Fatalf("unexpected diagnostic result: %+v", diagnostic)
	}
}

func TestMCPDiagnosticSanitizesMCPHandlerFailures(t *testing.T) {
	const handlerSecret = "mcp-handler-secret"
	server := &Server{MCP: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" || !strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
			t.Fatalf("unexpected diagnostic request: method=%s contentType=%q accept=%q", r.Method, r.Header.Get("Content-Type"), r.Header.Get("Accept"))
		}
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(handlerSecret))
	})}
	recorder := httptest.NewRecorder()
	server.diagnoseMCP(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/settings/mcp/diagnostics", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("diagnostic status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), handlerSecret) {
		t.Fatalf("diagnostic exposed MCP handler response: %s", recorder.Body.String())
	}
	var diagnostic mcpDiagnostic
	if err := json.NewDecoder(recorder.Body).Decode(&diagnostic); err != nil {
		t.Fatal(err)
	}
	if diagnostic.Success || diagnostic.CheckedAt.IsZero() || diagnostic.Failure == "" {
		t.Fatalf("unexpected diagnostic result: %+v", diagnostic)
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
