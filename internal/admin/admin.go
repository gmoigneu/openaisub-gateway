// Package admin serves owner controls on a listener separate from inference.
package admin

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"html/template"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gmoigneu/openaisub-gateway/internal/openaiauth"
	"github.com/gmoigneu/openaisub-gateway/internal/store"
)

type Disconnector interface{ Disconnect(context.Context) error }
type session struct {
	csrf    string
	expires time.Time
}
type Handler struct {
	store    *store.Store
	manager  Disconnector
	secret   [32]byte
	mu       sync.Mutex
	sessions map[string]session
	failed   int
	reset    time.Time
}
type page struct {
	CSRF, Email, State, Token, Message string
	Keys                               []store.Key
	Login                              bool
}

func New(s *store.Store, m Disconnector, secret string) *Handler {
	return &Handler{store: s, manager: m, secret: sha256.Sum256([]byte(secret)), sessions: make(map[string]session)}
}
func randomToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic("secure randomness unavailable")
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
func (h *Handler) authorized(value string) bool {
	digest := sha256.Sum256([]byte(value))
	return subtle.ConstantTimeCompare(digest[:], h.secret[:]) == 1
}
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
	if strings.HasPrefix(r.URL.Path, "/internal/") {
		h.internal(w, r)
		return
	}
	if r.URL.Path == "/login" && r.Method == http.MethodPost {
		h.login(w, r)
		return
	}
	var current session
	var token string
	if c, err := r.Cookie("gateway_session"); err == nil {
		token = c.Value
		h.mu.Lock()
		current = h.sessions[token]
		if time.Now().After(current.expires) {
			delete(h.sessions, token)
			current = session{}
		}
		h.mu.Unlock()
	}
	if current.csrf == "" {
		if r.Method != http.MethodGet || r.URL.Path != "/" {
			http.Error(w, "sign in required", http.StatusUnauthorized)
			return
		}
		h.render(w, page{Login: true})
		return
	}
	if r.Method == http.MethodPost {
		if !sameOrigin(r) {
			http.Error(w, "invalid request origin", http.StatusForbidden)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 4096)
		if err := r.ParseForm(); err != nil || r.PostForm.Get("csrf") != current.csrf {
			http.Error(w, "invalid form token", http.StatusForbidden)
			return
		}
		switch r.URL.Path {
		case "/keys":
			name := strings.TrimSpace(r.PostForm.Get("name"))
			if name == "" || len(name) > 80 {
				http.Error(w, "key name must be 1-80 bytes", 400)
				return
			}
			keys, err := h.store.Keys(r.Context())
			if err != nil {
				http.Error(w, "database unavailable", 503)
				return
			}
			if len(keys) >= 1000 {
				http.Error(w, "key limit reached", 409)
				return
			}
			_, key, err := h.store.CreateKey(r.Context(), name)
			if err != nil {
				http.Error(w, "cannot create key", 503)
				return
			}
			h.dashboard(w, r, current.csrf, key, "")
			return
		case "/revoke":
			if err := h.store.RevokeKey(r.Context(), r.PostForm.Get("id")); err != nil {
				http.Error(w, "cannot revoke key", 503)
				return
			}
		case "/disconnect":
			ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
			defer cancel()
			if err := h.manager.Disconnect(ctx); err != nil {
				h.dashboard(w, r, current.csrf, "", "Remote revocation was not confirmed. Check connection status and disconnect this app in ChatGPT Settings.")
				return
			}
		case "/logout":
			h.mu.Lock()
			delete(h.sessions, token)
			h.mu.Unlock()
			http.SetCookie(w, &http.Cookie{Name: "gateway_session", Value: "", Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: -1, Secure: r.TLS != nil})
		default:
			http.NotFound(w, r)
			return
		}
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	if r.Method != http.MethodGet || r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	h.dashboard(w, r, current.csrf, "", "")
}
func sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return false
	}
	u, err := url.Parse(origin)
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return err == nil && u.Scheme == scheme && u.Host == r.Host && u.Path == "" && u.RawQuery == "" && u.Fragment == "" && u.User == nil
}
func (h *Handler) login(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		http.Error(w, "invalid request origin", 403)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", 400)
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	now := time.Now()
	if now.After(h.reset) {
		h.failed = 0
		h.reset = now.Add(time.Minute)
	}
	if h.failed >= 5 {
		w.Header().Set("Retry-After", "60")
		http.Error(w, "try again later", 429)
		return
	}
	if !h.authorized(r.PostForm.Get("password")) {
		h.failed++
		http.Error(w, "invalid administrator password", 401)
		return
	}
	for k, v := range h.sessions {
		if now.After(v.expires) {
			delete(h.sessions, k)
		}
	}
	if len(h.sessions) >= 16 {
		http.Error(w, "session limit reached", 429)
		return
	}
	token := randomToken()
	h.sessions[token] = session{csrf: randomToken(), expires: now.Add(8 * time.Hour)}
	http.SetCookie(w, &http.Cookie{Name: "gateway_session", Value: token, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: 8 * 60 * 60, Secure: r.TLS != nil})
	http.Redirect(w, r, "/", 303)
}
func (h *Handler) dashboard(w http.ResponseWriter, r *http.Request, csrf, key, message string) {
	p := page{CSRF: csrf, State: "Not connected", Token: key, Message: message}
	c, err := h.store.LoadCredentials(r.Context())
	if err == nil {
		p.Email = c.Email
		if c.RefreshToken != "" {
			p.State = "Connected"
		} else {
			p.State = "Sign in again"
		}
	} else if !errors.Is(err, openaiauth.ErrNotConnected) {
		http.Error(w, "credential storage unavailable", 503)
		return
	}
	p.Keys, err = h.store.Keys(r.Context())
	if err != nil {
		http.Error(w, "key storage unavailable", 503)
		return
	}
	h.render(w, p)
}
func (h *Handler) render(w http.ResponseWriter, p page) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = dashboard.Execute(w, p)
}
func (h *Handler) internal(w http.ResponseWriter, r *http.Request) {
	// CLI-only routes never authenticate with browser cookies.
	header := r.Header.Get("Authorization")
	if r.Header.Get("Origin") != "" || r.Header.Get("Cookie") != "" || !strings.HasPrefix(header, "Bearer ") || !h.authorized(strings.TrimPrefix(header, "Bearer ")) {
		http.Error(w, "unauthorized", 401)
		return
	}
	switch {
	case r.URL.Path == "/internal/identity" && r.Method == http.MethodGet:
		host, err := h.store.HostID(r.Context())
		if err != nil {
			http.Error(w, "storage unavailable", 503)
			return
		}
		c, err := h.store.LoadCredentials(r.Context())
		if err != nil && !errors.Is(err, openaiauth.ErrNotConnected) {
			http.Error(w, "storage unavailable", 503)
			return
		}
		if c.ClientID == "" {
			c.ClientID, err = h.store.PendingRegistration(r.Context())
			if err != nil {
				http.Error(w, "storage unavailable", 503)
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"host_id": host, "client_id": c.ClientID, "subject": c.Subject})
	case r.URL.Path == "/internal/registration" && r.Method == http.MethodPost:
		r.Body = http.MaxBytesReader(w, r.Body, 4096)
		var registration struct {
			ClientID string `json:"client_id"`
		}
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if dec.Decode(&registration) != nil || dec.Decode(new(any)) != io.EOF || registration.ClientID == "" || registration.ClientID == "dynamic_agent_client" || len(registration.ClientID) > 512 || strings.ContainsAny(registration.ClientID, " \t\r\n") {
			http.Error(w, "invalid registration", 400)
			return
		}
		old, err := h.store.LoadCredentials(r.Context())
		if err != nil && !errors.Is(err, openaiauth.ErrNotConnected) {
			http.Error(w, "storage unavailable", 503)
			return
		}
		if old.ClientID != "" && old.ClientID != registration.ClientID {
			http.Error(w, "registration cannot replace an active account", 409)
			return
		}
		if err := h.store.Register(r.Context(), registration.ClientID); err != nil {
			http.Error(w, "registration changed; restart sign-in", 409)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case r.URL.Path == "/internal/import" && r.Method == http.MethodPost:
		r.Body = http.MaxBytesReader(w, r.Body, 128<<10)
		var c openaiauth.Credentials
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&c); err != nil {
			http.Error(w, "invalid credential record", 400)
			return
		}
		if err := dec.Decode(new(any)); err != io.EOF {
			http.Error(w, "invalid trailing credential data", 400)
			return
		}
		host, err := h.store.HostID(r.Context())
		if err != nil {
			http.Error(w, "storage unavailable", 503)
			return
		}
		hasScope := false
		for _, scope := range c.Scopes {
			if scope == "chatgpt.tokens.use.direct" {
				hasScope = true
			}
		}
		if c.HostID != host || c.Subject == "" || c.ClientID == "" || c.ClientID == "dynamic_agent_client" || c.Issuer != "https://auth.openai.com" || c.AccessToken == "" || c.RefreshToken == "" || !hasScope || !c.ExpiresAt.After(time.Now()) {
			http.Error(w, "credential identity or permission is invalid", 400)
			return
		}
		old, err := h.store.LoadCredentials(r.Context())
		if err != nil && !errors.Is(err, openaiauth.ErrNotConnected) {
			http.Error(w, "storage unavailable", 503)
			return
		}
		if old.ClientID != "" && (old.ClientID != c.ClientID || old.Subject != c.Subject) {
			http.Error(w, "this gateway is bound to another registration", 409)
			return
		}
		pending, err := h.store.PendingRegistration(r.Context())
		if err != nil {
			http.Error(w, "storage unavailable", 503)
			return
		}
		if pending != "" && pending != c.ClientID {
			http.Error(w, "credentials do not match the pending registration", 409)
			return
		}
		if err = openaiauth.ValidateCredentials(r.Context(), c); err != nil {
			http.Error(w, "OpenAI credential validation failed", 400)
			return
		}
		c.Revision = old.Revision
		if err = h.store.ImportCredentials(r.Context(), c); err != nil {
			http.Error(w, "credential state changed; retry import", 409)
			return
		}
		w.WriteHeader(204)
	default:
		http.NotFound(w, r)
	}
}

var dashboard = template.Must(template.New("dashboard").Parse(`<!doctype html><html lang="en"><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>OpenAI subscription gateway</title><style>body{max-width:760px;margin:48px auto;padding:0 20px;font:16px/1.6 system-ui;color:#182321;background:#f6f8f7}h1{font-size:28px}section{background:white;border:1px solid #d3ded8;border-radius:12px;padding:24px;margin:20px 0}input,button{font:inherit;padding:8px 12px;margin:4px 0}button{cursor:pointer}code{overflow-wrap:anywhere}table{width:100%;text-align:left}th,td{padding:8px 4px;border-bottom:1px solid #ddd}.notice{padding:16px;background:#fff0ca}</style><h1>OpenAI subscription gateway</h1>{{if .Login}}<section><h2>Administrator sign-in</h2><form action="/login" method="post"><label>Password <input name="password" type="password" required autocomplete="current-password"></label> <button>Sign in</button></form></section>{{else}}{{if .Message}}<p class="notice">{{.Message}}</p>{{end}}<section><h2>OpenAI connection</h2><p><strong>{{.State}}</strong> {{.Email}}</p><p>To connect or reconnect, run the gateway login helper on the computer with your browser. Use the SSH option for a remote Docker host. Follow the repository setup guide.</p><p><code>gateway auth login</code></p><form method="post" action="/disconnect"><input type="hidden" name="csrf" value="{{.CSRF}}"><button>Disconnect OpenAI</button></form></section><section><h2>Client API keys</h2>{{if .Token}}<p class="notice">Copy this key now. It will not be shown again.<br><code>{{.Token}}</code></p>{{end}}<form method="post" action="/keys"><input type="hidden" name="csrf" value="{{.CSRF}}"><label>App name <input name="name" required maxlength="80" placeholder="Mastra agents"></label> <button>Create key</button></form><table><thead><tr><th>App</th><th>Key prefix</th><th>Created</th><th>Access</th></tr></thead><tbody>{{range .Keys}}<tr><td>{{.Name}}</td><td><code>{{.Prefix}}</code></td><td>{{.CreatedAt}}</td><td>{{if .Revoked}}Revoked{{else}}<form method="post" action="/revoke"><input type="hidden" name="csrf" value="{{$.CSRF}}"><input type="hidden" name="id" value="{{.ID}}"><button>Revoke</button></form>{{end}}</td></tr>{{end}}</tbody></table></section><form method="post" action="/logout"><input type="hidden" name="csrf" value="{{.CSRF}}"><button>Sign out</button></form>{{end}}</html>`))
