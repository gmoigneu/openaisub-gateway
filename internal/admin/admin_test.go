// Admin tests exercise HTTP boundaries and persistent key effects without external services.
package admin

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/gmoigneu/openaisub-gateway/internal/openaiauth"
	"github.com/gmoigneu/openaisub-gateway/internal/store"
)

const testSecret = "admin-secret-with-at-least-32-characters"

type fakeDisconnector struct{ calls int }

func (f *fakeDisconnector) Disconnect(context.Context) error { f.calls++; return nil }

func setupAdmin(t *testing.T) (*Handler, *store.Store) {
	t.Helper()
	s, err := store.Open(t.TempDir(), make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return New(s, &fakeDisconnector{}, testSecret), s
}
func adminRequest(h http.Handler, method, path, body, origin string, cookie *http.Cookie) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "http://gateway.test"+path, strings.NewReader(body))
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	if method == http.MethodPost {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if cookie != nil {
		r.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}
func signIn(t *testing.T, h *Handler) (*http.Cookie, string) {
	t.Helper()
	w := adminRequest(h, http.MethodPost, "/login", url.Values{"password": {testSecret}}.Encode(), "http://gateway.test", nil)
	if w.Code != 303 {
		t.Fatalf("login HTTP %d: %s", w.Code, w.Body.String())
	}
	cs := w.Result().Cookies()
	if len(cs) != 1 {
		t.Fatal("session cookie missing")
	}
	w = adminRequest(h, http.MethodGet, "/", "", "", cs[0])
	match := regexp.MustCompile(`name="csrf" value="([^"]+)"`).FindStringSubmatch(w.Body.String())
	if len(match) != 2 {
		t.Fatal("CSRF token missing")
	}
	return cs[0], match[1]
}

func TestLoginRequiresMatchingOrigin(t *testing.T) {
	h, _ := setupAdmin(t)
	for _, origin := range []string{"", "null", "http://attacker.test", "https://gateway.test", "http://gateway.test/path", "http://user@gateway.test", "http://gateway.test?x=1"} {
		w := adminRequest(h, http.MethodPost, "/login", url.Values{"password": {testSecret}}.Encode(), origin, nil)
		if w.Code != 403 || len(w.Result().Cookies()) != 0 {
			t.Fatalf("accepted origin %q: HTTP %d", origin, w.Code)
		}
	}
	_, _ = signIn(t, h)
}

func TestAdminPagesPreserveSameOriginFormPosts(t *testing.T) {
	h, _ := setupAdmin(t)
	cookie, _ := signIn(t, h)
	for _, tc := range []struct {
		name   string
		cookie *http.Cookie
	}{{"login", nil}, {"dashboard", cookie}} {
		t.Run(tc.name, func(t *testing.T) {
			w := adminRequest(h, http.MethodGet, "/", "", "", tc.cookie)
			// Fetch makes a non-CORS form POST's Origin null under no-referrer.
			// same-origin preserves local form origins without cross-site referrers.
			if w.Code != http.StatusOK || w.Header().Get("Referrer-Policy") != "same-origin" {
				t.Fatalf("form page must preserve its origin: HTTP %d, Referrer-Policy %q", w.Code, w.Header().Get("Referrer-Policy"))
			}
		})
	}
}

func TestLoginCookieProtectionAndExpiry(t *testing.T) {
	h, _ := setupAdmin(t)
	cookie, _ := signIn(t, h)
	if !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode || cookie.Path != "/" || cookie.MaxAge != 8*60*60 || cookie.Secure {
		t.Fatalf("unsafe HTTP cookie %+v", cookie)
	}
	r := httptest.NewRequest(http.MethodPost, "https://gateway.test/login", strings.NewReader(url.Values{"password": {testSecret}}.Encode()))
	r.TLS = &tls.ConnectionState{}
	r.Header.Set("Origin", "https://gateway.test")
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 303 || !w.Result().Cookies()[0].Secure {
		t.Fatal("HTTPS cookie is not secure")
	}
	h.mu.Lock()
	entry := h.sessions[cookie.Value]
	entry.expires = time.Now().Add(-time.Second)
	h.sessions[cookie.Value] = entry
	h.mu.Unlock()
	w = adminRequest(h, http.MethodPost, "/keys", "csrf=ignored&name=agent", "http://gateway.test", cookie)
	if w.Code != 401 {
		t.Fatalf("expired session accepted: %d", w.Code)
	}
}

func TestLoginThrottlesBadPasswords(t *testing.T) {
	h, _ := setupAdmin(t)
	for range 5 {
		w := adminRequest(h, http.MethodPost, "/login", "password=wrong", "http://gateway.test", nil)
		if w.Code != 401 {
			t.Fatalf("expected rejection, got %d", w.Code)
		}
	}
	w := adminRequest(h, http.MethodPost, "/login", url.Values{"password": {testSecret}}.Encode(), "http://gateway.test", nil)
	if w.Code != 429 || w.Header().Get("Retry-After") == "" {
		t.Fatal("password throttle absent")
	}
	h.mu.Lock()
	h.reset = time.Now().Add(-time.Second)
	h.mu.Unlock()
	_, _ = signIn(t, h)
}

func TestKeyManagementRequiresCSRFAndShowsSecretOnce(t *testing.T) {
	h, s := setupAdmin(t)
	cookie, csrf := signIn(t, h)
	for _, tc := range []struct{ token, origin string }{{"", "http://gateway.test"}, {"wrong", "http://gateway.test"}, {csrf, "http://attacker.test"}, {csrf, ""}} {
		w := adminRequest(h, http.MethodPost, "/keys", url.Values{"csrf": {tc.token}, "name": {"agent"}}.Encode(), tc.origin, cookie)
		if w.Code != 403 {
			t.Fatalf("unsafe mutation accepted: %d", w.Code)
		}
	}
	keys, err := s.Keys(context.Background())
	if err != nil || len(keys) != 0 {
		t.Fatal("rejected form changed keys")
	}
	w := adminRequest(h, http.MethodPost, "/keys", url.Values{"csrf": {csrf}, "name": {"Mastra"}}.Encode(), "http://gateway.test", cookie)
	if w.Code != 200 {
		t.Fatalf("create failed: %s", w.Body.String())
	}
	token := regexp.MustCompile(`osk_[A-Za-z0-9_-]{43}`).FindString(w.Body.String())
	if token == "" || !s.Authenticate(context.Background(), token) {
		t.Fatal("created key unusable")
	}
	w = adminRequest(h, http.MethodGet, "/", "", "", cookie)
	if strings.Contains(w.Body.String(), token) {
		t.Fatal("dashboard retained full key")
	}
	keys, err = s.Keys(context.Background())
	if err != nil || len(keys) != 1 {
		t.Fatal("created key missing")
	}
	w = adminRequest(h, http.MethodPost, "/revoke", url.Values{"csrf": {csrf}, "id": {keys[0].ID}}.Encode(), "http://gateway.test", cookie)
	if w.Code != 303 || s.Authenticate(context.Background(), token) {
		t.Fatal("revoked key remains valid")
	}
}

func TestClientKeyCannotAdminister(t *testing.T) {
	h, s := setupAdmin(t)
	_, key, err := s.CreateKey(context.Background(), "agent")
	if err != nil {
		t.Fatal(err)
	}
	w := adminRequest(h, http.MethodPost, "/login", url.Values{"password": {key}}.Encode(), "http://gateway.test", nil)
	if w.Code != 401 {
		t.Fatal("client key accepted as admin password")
	}
	for _, path := range []string{"/keys", "/internal/identity", "/internal/import"} {
		r := httptest.NewRequest(http.MethodPost, "http://gateway.test"+path, strings.NewReader(`{}`))
		r.Header.Set("Authorization", "Bearer "+key)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 401 {
			t.Fatalf("client key authorized %s: %d", path, w.Code)
		}
	}
}

func TestInternalRoutesRejectBrowserRequests(t *testing.T) {
	h, _ := setupAdmin(t)
	cookie, _ := signIn(t, h)
	for _, tc := range []struct {
		bearer, origin string
		cookie         *http.Cookie
	}{{"", "", cookie}, {"Bearer " + testSecret, "http://gateway.test", nil}, {"Bearer " + testSecret, "null", cookie}, {"Bearer " + testSecret, "", cookie}} {
		r := httptest.NewRequest(http.MethodGet, "http://gateway.test/internal/identity", nil)
		r.Header.Set("Authorization", tc.bearer)
		r.Header.Set("Origin", tc.origin)
		if tc.cookie != nil {
			r.AddCookie(tc.cookie)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 401 {
			t.Fatal("browser credentials accepted by internal route")
		}
	}
	r := httptest.NewRequest(http.MethodGet, "http://gateway.test/internal/identity", nil)
	r.Header.Set("Authorization", "Bearer "+testSecret)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "host_id") || strings.Contains(w.Body.String(), testSecret) {
		t.Fatal("invalid CLI identity result")
	}
}

func TestInternalImportRejectsMalformedCredentials(t *testing.T) {
	h, s := setupAdmin(t)
	for _, body := range []string{`{`, `{}`, `{"unknown":true}`, `{} {}`, `{"client_id":"dynamic_agent_client"}`, strings.Repeat("x", 129<<10)} {
		r := httptest.NewRequest(http.MethodPost, "http://gateway.test/internal/import", strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+testSecret)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 400 {
			t.Fatalf("malformed import returned %d", w.Code)
		}
		if _, err := s.LoadCredentials(context.Background()); !errors.Is(err, openaiauth.ErrNotConnected) {
			t.Fatal("invalid import changed credentials")
		}
	}
	// An invalid compact JWT fails parsing before JWKS retrieval.
	host, err := s.HostID(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	c := openaiauth.Credentials{HostID: host, ClientID: "issued", Subject: "owner", Issuer: openaiauth.Issuer, AccessToken: "access", RefreshToken: "refresh", IDToken: "invalid", Scopes: []string{openaiauth.DirectScope}, ExpiresAt: time.Now().Add(time.Hour)}
	data, _ := json.Marshal(c)
	r := httptest.NewRequest(http.MethodPost, "http://gateway.test/internal/import", strings.NewReader(string(data)))
	r.Header.Set("Authorization", "Bearer "+testSecret)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 400 {
		t.Fatalf("unsigned import accepted: %d", w.Code)
	}
}

func TestDashboardEscapesUntrustedNamesAndIdentity(t *testing.T) {
	h, s := setupAdmin(t)
	xss := `<script>alert("x")</script>`
	if _, _, err := s.CreateKey(context.Background(), xss); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveCredentials(context.Background(), openaiauth.Credentials{ClientID: "issued", Email: xss, RefreshToken: "secret-refresh"}); err != nil {
		t.Fatal(err)
	}
	cookie, _ := signIn(t, h)
	w := adminRequest(h, http.MethodGet, "/", "", "", cookie)
	if strings.Contains(w.Body.String(), xss) || !strings.Contains(w.Body.String(), "&lt;script&gt;") || strings.Contains(w.Body.String(), "secret-refresh") {
		t.Fatal("dashboard leaked credentials or unescaped content")
	}
	if w.Header().Get("Cache-Control") != "no-store" || !strings.Contains(w.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'") {
		t.Fatal("missing dashboard protections")
	}
}

func TestLogoutInvalidatesSession(t *testing.T) {
	h, _ := setupAdmin(t)
	cookie, csrf := signIn(t, h)
	w := adminRequest(h, http.MethodPost, "/logout", url.Values{"csrf": {csrf}}.Encode(), "http://gateway.test", cookie)
	if w.Code != 303 || w.Result().Cookies()[0].MaxAge != -1 {
		t.Fatal("logout did not expire cookie")
	}
	w = adminRequest(h, http.MethodPost, "/keys", url.Values{"csrf": {csrf}, "name": {"agent"}}.Encode(), "http://gateway.test", cookie)
	if w.Code != 401 {
		t.Fatal("logged out session still authorized")
	}
}

func TestPendingRegistrationDoesNotAuthenticateAccount(t *testing.T) {
	h, s := setupAdmin(t)
	call := func(method, path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "http://gateway.test"+path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+testSecret)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if w := call("POST", "/internal/registration", `{"client_id":"oaiapp_pending"}`); w.Code != 204 {
		t.Fatalf("register: %d", w.Code)
	}
	if w := call("POST", "/internal/registration", `{"client_id":"oaiapp_pending"}`); w.Code != 204 {
		t.Fatalf("retry: %d", w.Code)
	}
	if w := call("POST", "/internal/registration", `{"client_id":"oaiapp_other"}`); w.Code != 409 {
		t.Fatal("registration overwritten")
	}
	w := call("GET", "/internal/identity", "")
	var identity map[string]string
	if json.Unmarshal(w.Body.Bytes(), &identity) != nil || identity["client_id"] != "oaiapp_pending" || identity["subject"] != "" {
		t.Fatal("pending identity was lost or treated as validated")
	}
	if _, err := s.LoadCredentials(context.Background()); !errors.Is(err, openaiauth.ErrNotConnected) {
		t.Fatal("registration created credentials")
	}
}
