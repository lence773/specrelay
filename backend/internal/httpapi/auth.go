package httpapi

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

type Auth struct {
	browserHash, mcpHash [32]byte
	browserOneTime       bool
	browserConsumed      bool
	mu                   sync.RWMutex
	sessions             map[string]time.Time
}

type Tokens struct {
	Browser, MCP                   string
	BrowserGenerated, MCPGenerated bool
}

func NewAuth(browserToken, mcpToken string) (*Auth, Tokens) {
	tokens := Tokens{Browser: browserToken, MCP: mcpToken}
	if tokens.Browser == "" {
		tokens.Browser = randomToken()
		tokens.BrowserGenerated = true
	}
	if tokens.MCP == "" {
		tokens.MCP = randomToken()
		tokens.MCPGenerated = true
	}
	return &Auth{
		browserHash:    sha256.Sum256([]byte(tokens.Browser)),
		mcpHash:        sha256.Sum256([]byte(tokens.MCP)),
		browserOneTime: tokens.BrowserGenerated,
		sessions:       map[string]time.Time{},
	}, tokens
}

func (a *Auth) Exchange(w http.ResponseWriter, r *http.Request) bool {
	token := r.URL.Query().Get("token")
	if token == "" {
		var body struct {
			Token string `json:"token"`
		}
		if decodeJSON(r, &body) == nil {
			token = body.Token
		}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if !matches(token, a.browserHash) || (a.browserOneTime && a.browserConsumed) {
		return false
	}
	if a.browserOneTime {
		a.browserConsumed = true
	}
	session := randomToken()
	a.sessions[session] = time.Now().Add(30 * 24 * time.Hour)
	http.SetCookie(w, &http.Cookie{Name: "specrelay_session", Value: session, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: false, MaxAge: 30 * 24 * 3600})
	return true
}

func (a *Auth) Allowed(r *http.Request) bool {
	bearer := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	a.mu.RLock()
	defer a.mu.RUnlock()
	if r.URL.Path == "/mcp" || strings.HasPrefix(r.URL.Path, "/mcp/") {
		return matches(bearer, a.mcpHash)
	}
	// A configured ACCESS_TOKEN may be used as a bearer token. A generated
	// bootstrap token is exchange-only and becomes unusable after first use.
	if bearer != "" && !a.browserOneTime && matches(bearer, a.browserHash) {
		return true
	}
	cookie, err := r.Cookie("specrelay_session")
	if err != nil {
		return false
	}
	expires, ok := a.sessions[cookie.Value]
	return ok && expires.After(time.Now())
}

func (a *Auth) SetMCPToken(token string) {
	a.mu.Lock()
	a.mcpHash = sha256.Sum256([]byte(token))
	a.mu.Unlock()
}

func LocalRequest(r *http.Request) bool {
	if !isLoopbackHost(r.Host) {
		return false
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	parsed, err := url.Parse(origin)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.User != nil {
		return false
	}
	return isLoopbackHost(parsed.Host)
}

func isLoopbackHost(hostPort string) bool {
	host, _, err := net.SplitHostPort(hostPort)
	if err != nil {
		host = hostPort
	}
	host = strings.TrimSuffix(strings.Trim(host, "[]"), ".")
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

func matches(token string, hash [32]byte) bool {
	if token == "" {
		return false
	}
	sum := sha256.Sum256([]byte(token))
	return subtle.ConstantTimeCompare(sum[:], hash[:]) == 1
}

func TokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func randomToken() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}
