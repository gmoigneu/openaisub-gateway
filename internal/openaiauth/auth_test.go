// These fixtures verify token rotation and loopback sign-in without a live OpenAI account.
package openaiauth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
)

type memoryStore struct {
	mu            sync.Mutex
	c             Credentials
	saves, clears int
}

func (s *memoryStore) LoadCredentials(context.Context) (Credentials, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.c, nil
}
func (s *memoryStore) SaveCredentials(_ context.Context, c Credentials) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c.Revision != s.c.Revision {
		return ErrConflict
	}
	c.Revision++
	s.c = c
	s.saves++
	return nil
}
func (s *memoryStore) ClearCredentials(_ context.Context, revision int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if revision != s.c.Revision {
		return ErrConflict
	}
	s.c.AccessToken = ""
	s.c.RefreshToken = ""
	s.c.IDToken = ""
	s.c.Revision++
	s.clears++
	return nil
}
func expiredCredentials() Credentials {
	return Credentials{ClientID: "issued", Subject: "owner", Issuer: Issuer, AccessToken: "old-access", RefreshToken: "old-refresh", Scopes: []string{DirectScope}, ExpiresAt: time.Now().Add(-time.Hour)}
}

func TestConcurrentRefreshRotatesOnce(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		if r.Form.Get("refresh_token") != "old-refresh" || r.Form.Get("client_id") != "issued" || r.Form.Get("resource") != Resource || r.Form.Get("scope") != "" {
			t.Errorf("wrong refresh form: %v", r.Form)
		}
		json.NewEncoder(w).Encode(tokenResponse{AccessToken: "new-access", RefreshToken: "new-refresh", TokenType: "Bearer", ExpiresIn: 3600})
	}))
	defer server.Close()
	store := &memoryStore{c: expiredCredentials()}
	m := NewManager(store, server.Client())
	m.endpoints.token = server.URL
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			token, err := m.AccessToken(context.Background())
			if err != nil || token != "new-access" {
				t.Errorf("token %q error %v", token, err)
			}
		})
	}
	wg.Wait()
	if calls.Load() != 1 || store.saves != 1 || store.c.RefreshToken != "new-refresh" {
		t.Fatalf("rotation count %d store %+v", calls.Load(), store)
	}
}

func TestRefreshFailuresPreserveOrClearCredentials(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  int
		body    string
		cleared bool
	}{
		{"invalid grant", 400, `{"error":"invalid_grant"}`, true},
		{"reused", 401, `{"error":"refresh_token_reused"}`, true},
		{"invalid client", 400, `{"error":"invalid_client"}`, false},
		{"temporary", 503, `{"error":"invalid_grant","detail":"sensitive-token"}`, false},
		{"malformed", 200, `oops sensitive-token`, false},
		{"scope lost", 200, `{"access_token":"new","token_type":"Bearer","expires_in":3600,"scope":"openid"}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(tc.status); w.Write([]byte(tc.body)) }))
			defer server.Close()
			store := &memoryStore{c: expiredCredentials()}
			m := NewManager(store, server.Client())
			m.endpoints.token = server.URL
			_, err := m.AccessToken(context.Background())
			if err == nil || strings.Contains(err.Error(), "sensitive-token") {
				t.Fatalf("unexpected error %v", err)
			}
			if (store.clears == 1) != tc.cleared {
				t.Fatalf("cleared %d, want %v", store.clears, tc.cleared)
			}
			if store.c.ClientID != "issued" {
				t.Fatal("lost registration")
			}
		})
	}
}

func TestRefreshDoesNotFollowRedirect(t *testing.T) {
	var leaked atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked.Store(true) }))
	defer target.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	m := NewManager(&memoryStore{c: expiredCredentials()}, server.Client())
	m.endpoints.token = server.URL
	if _, err := m.AccessToken(context.Background()); err == nil {
		t.Fatal("redirect accepted")
	}
	if leaked.Load() {
		t.Fatal("credentials sent to redirect")
	}
}

type loginFixture struct {
	server                       *httptest.Server
	signer                       jose.Signer
	nonce, challenge, redirect   string
	sub, nonceOverride, audience string
	scope                        string
	expired                      bool
	tokenCalls                   atomic.Int32
}

func newLoginFixture(t *testing.T) *loginFixture {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key}, (&jose.SignerOptions{}).WithHeader("kid", "test-key"))
	if err != nil {
		t.Fatal(err)
	}
	f := &loginFixture{signer: signer, sub: "owner", audience: "issued", scope: DirectScope}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/jwks":
			json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "test-key", Algorithm: "RS256", Use: "sig"}}})
		case "/token":
			f.tokenCalls.Add(1)
			r.ParseForm()
			digest := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
			if base64.RawURLEncoding.EncodeToString(digest[:]) != f.challenge || r.Form.Get("redirect_uri") != f.redirect || r.Form.Get("client_id") != "issued" || r.Form.Get("resource") != Resource {
				t.Error("PKCE, redirect, client or resource mismatch")
			}
			nonce := f.nonce
			if f.nonceOverride != "" {
				nonce = f.nonceOverride
			}
			expiry := time.Now().Add(time.Hour)
			if f.expired {
				expiry = time.Now().Add(-time.Hour)
			}
			claims, _ := json.Marshal(map[string]any{"iss": f.server.URL, "aud": f.audience, "sub": f.sub, "nonce": nonce, "exp": expiry.Unix(), "iat": time.Now().Unix(), "email": "owner@example.test"})
			signed, _ := f.signer.Sign(claims)
			raw, _ := signed.CompactSerialize()
			json.NewEncoder(w).Encode(tokenResponse{AccessToken: "access", RefreshToken: "refresh", IDToken: raw, TokenType: "Bearer", ExpiresIn: 3600, Scope: f.scope})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.server.Close)
	return f
}
func (f *loginFixture) endpoints() endpoints {
	return endpoints{issuer: f.server.URL, authorize: f.server.URL + "/authorize", token: f.server.URL + "/token", jwks: f.server.URL + "/jwks"}
}
func (f *loginFixture) callback(t *testing.T, modify func(url.Values)) func(string) {
	return func(raw string) {
		u, err := url.Parse(raw)
		if err != nil {
			t.Error(err)
			return
		}
		q := u.Query()
		f.nonce = q.Get("nonce")
		f.challenge = q.Get("code_challenge")
		f.redirect = q.Get("redirect_uri")
		if q.Get("code_challenge_method") != "S256" || q.Get("ext_agent_host_id") != "host" || !strings.Contains(q.Get("scope"), DirectScope) {
			t.Error("missing required authorization parameters")
		}
		v := url.Values{"state": {q.Get("state")}, "code": {"auth-code"}, "client_id": {"issued"}}
		if modify != nil {
			modify(v)
		}
		resp, err := http.Get(f.redirect + "?" + v.Encode())
		if err != nil {
			t.Error(err)
			return
		}
		resp.Body.Close()
	}
}

func TestLoopbackLoginValidatesSignedIdentity(t *testing.T) {
	f := newLoginFixture(t)
	c, err := login(context.Background(), LoginOptions{HostID: "host", ListenAddress: "127.0.0.1:0", OnAuthorization: f.callback(t, nil)}, safeClient(f.server.Client()), f.endpoints())
	if err != nil {
		t.Fatal(err)
	}
	if c.Subject != "owner" || c.Email != "owner@example.test" || c.ClientID != "issued" || c.RefreshToken != "refresh" || time.Until(c.ExpiresAt) < 59*time.Minute {
		t.Fatalf("bad credentials %+v", c)
	}
}

func TestLoopbackLoginRejectsInvalidIdentityAndRegistration(t *testing.T) {
	for _, name := range []string{"nonce", "audience", "subject", "client", "scope", "denied", "expired", "signature"} {
		t.Run(name, func(t *testing.T) {
			f := newLoginFixture(t)
			o := LoginOptions{HostID: "host", ListenAddress: "127.0.0.1:0"}
			var modify func(url.Values)
			switch name {
			case "nonce":
				f.nonceOverride = "wrong"
			case "audience":
				f.audience = "other-client"
			case "subject":
				o.ClientID = "issued"
				o.Subject = "different-owner"
			case "client":
				o.ClientID = "issued"
				o.Subject = "owner"
				modify = func(v url.Values) { v.Set("client_id", "other") }
			case "scope":
				f.scope = "openid"
			case "denied":
				modify = func(v url.Values) { v.Set("error", "access_denied") }
			case "expired":
				f.expired = true
			case "signature":
				otherKey, err := rsa.GenerateKey(rand.Reader, 2048)
				if err != nil {
					t.Fatal(err)
				}
				f.signer, err = jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: otherKey}, (&jose.SignerOptions{}).WithHeader("kid", "test-key"))
				if err != nil {
					t.Fatal(err)
				}
			}
			o.OnAuthorization = f.callback(t, modify)
			if _, err := login(context.Background(), o, safeClient(f.server.Client()), f.endpoints()); err == nil {
				t.Fatal("invalid sign-in accepted")
			}
			if (name == "client" || name == "denied") && f.tokenCalls.Load() != 0 {
				t.Fatal("invalid callback exchanged")
			}
		})
	}
}

func TestBadStateDoesNotConsumeLogin(t *testing.T) {
	f := newLoginFixture(t)
	onAuth := func(raw string) {
		u, _ := url.Parse(raw)
		redirect := u.Query().Get("redirect_uri")
		resp, err := http.Get(redirect + "?state=wrong&code=bad&client_id=issued")
		if err != nil {
			t.Error(err)
			return
		}
		resp.Body.Close()
		if resp.StatusCode != 400 {
			t.Error("bad state accepted")
		}
		f.callback(t, nil)(raw)
	}
	if _, err := login(context.Background(), LoginOptions{HostID: "host", ListenAddress: "127.0.0.1:0", OnAuthorization: onAuth}, safeClient(f.server.Client()), f.endpoints()); err != nil {
		t.Fatal(err)
	}
	if f.tokenCalls.Load() != 1 {
		t.Fatal("wrong state exchanged")
	}
}

func TestLoginRejectsNonLoopback(t *testing.T) {
	for _, address := range []string{"0.0.0.0:1455", "localhost:1455", "[::1]:1455", "192.168.1.2:1455"} {
		if _, err := Login(context.Background(), LoginOptions{HostID: "host", ListenAddress: address, OnAuthorization: func(string) { t.Error("opened browser") }}); err == nil {
			t.Fatalf("accepted %s", address)
		}
	}
}

func TestDisconnectClearsTokensWhenRevocationFails(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }))
	defer server.Close()
	store := &memoryStore{c: expiredCredentials()}
	m := NewManager(store, server.Client())
	m.endpoints.discovery = server.URL
	err := m.Disconnect(context.Background())
	if err == nil || !strings.Contains(err.Error(), "not confirmed") {
		t.Fatalf("unexpected error %v", err)
	}
	if store.c.AccessToken != "" || store.c.RefreshToken != "" || store.c.ClientID != "issued" {
		t.Fatal("wrong disconnect state")
	}
	if _, err := m.AccessToken(context.Background()); !errors.Is(err, ErrNotConnected) {
		t.Fatal(err)
	}
}

func TestDisconnectDiscoversRevocationAndRetries(t *testing.T) {
	var server *httptest.Server
	var attempts atomic.Int32
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/discovery" {
			json.NewEncoder(w).Encode(map[string]string{"issuer": server.URL, "revocation_endpoint": server.URL + "/revoke"})
			return
		}
		if r.URL.Path != "/revoke" {
			t.Error("wrong revocation path")
			http.NotFound(w, r)
			return
		}
		r.ParseForm()
		if r.Form.Get("token") != "old-refresh" || r.Form.Get("token_type_hint") != "refresh_token" || r.Form.Get("client_id") != "issued" {
			t.Error("wrong revocation form")
		}
		if attempts.Add(1) == 1 {
			w.WriteHeader(503)
			return
		}
		w.WriteHeader(200)
	}))
	defer server.Close()
	store := &memoryStore{c: expiredCredentials()}
	m := NewManager(store, server.Client())
	m.endpoints.issuer = server.URL
	m.endpoints.discovery = server.URL + "/discovery"
	if err := m.Disconnect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if attempts.Load() != 2 || store.clears != 1 || store.c.AccessToken != "" {
		t.Fatal("revocation did not complete")
	}
}

func TestReturningLoginRetainsRegistration(t *testing.T) {
	f := newLoginFixture(t)
	onAuth := func(raw string) {
		u, _ := url.Parse(raw)
		q := u.Query()
		if q.Get("client_id") != "issued" || q.Get("agent_name_hint") != "" || q.Get("id_token_hint") != "old-id" || q.Get("login_hint") != "owner@example.test" {
			t.Error("incorrect returning login parameters")
		}
		f.callback(t, func(v url.Values) { v.Del("client_id") })(raw)
	}
	c, err := login(context.Background(), LoginOptions{HostID: "host", ClientID: "issued", Subject: "owner", Email: "owner@example.test", IDTokenHint: "old-id", ListenAddress: "127.0.0.1:0", OnAuthorization: onAuth}, safeClient(f.server.Client()), f.endpoints())
	if err != nil {
		t.Fatal(err)
	}
	if c.ClientID != "issued" {
		t.Fatal("registration replaced")
	}
}

func TestLoginCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	_, err := Login(ctx, LoginOptions{HostID: "host", ListenAddress: "127.0.0.1:0", OnAuthorization: func(string) { cancel() }})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("unexpected error %v", err)
	}
}

func TestRefreshDoesNotOverwriteNewRegistration(t *testing.T) {
	store := &memoryStore{c: expiredCredentials()}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		store.mu.Lock()
		store.c.Revision++
		store.c.AccessToken = "other-login"
		store.mu.Unlock()
		json.NewEncoder(w).Encode(tokenResponse{AccessToken: "new-access", RefreshToken: "new-refresh", TokenType: "Bearer", ExpiresIn: 3600})
	}))
	defer server.Close()
	m := NewManager(store, server.Client())
	m.endpoints.token = server.URL
	if _, err := m.AccessToken(context.Background()); !errors.Is(err, ErrConflict) {
		t.Fatalf("unexpected error %v", err)
	}
	if store.c.AccessToken != "other-login" || store.saves != 0 {
		t.Fatal("new registration overwritten")
	}
}

type cancelAwareStore struct {
	*memoryStore
	beforeSave func()
}

func (s *cancelAwareStore) SaveCredentials(ctx context.Context, c Credentials) error {
	if s.beforeSave != nil {
		s.beforeSave()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.memoryStore.SaveCredentials(ctx, c)
}

func (s *cancelAwareStore) ClearCredentials(ctx context.Context, revision int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.memoryStore.ClearCredentials(ctx, revision)
}

func TestRotatedTokensSurviveRequestCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(tokenResponse{AccessToken: "new-access", RefreshToken: "new-refresh", TokenType: "Bearer", ExpiresIn: 3600})
	}))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := &cancelAwareStore{memoryStore: &memoryStore{c: expiredCredentials()}, beforeSave: cancel}
	m := NewManager(store, server.Client())
	m.endpoints.token = server.URL
	token, err := m.AccessToken(ctx)
	if err != nil || token != "new-access" {
		t.Fatalf("rotation was lost after client cancellation: %v", err)
	}
	if ctx.Err() == nil || store.c.RefreshToken != "new-refresh" || store.saves != 1 {
		t.Fatal("replacement credentials were not persisted")
	}
}

type authRoundTripFunc func(*http.Request) (*http.Response, error)

func (f authRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestDisconnectClearsLocallyAfterRevocationDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	var revokeCalled bool
	client := &http.Client{Transport: authRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodGet {
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"issuer":"https://auth.openai.com","revocation_endpoint":"https://auth.openai.com/revoke"}`))}, nil
		}
		revokeCalled = true
		<-r.Context().Done()
		return nil, r.Context().Err()
	})}
	store := &cancelAwareStore{memoryStore: &memoryStore{c: expiredCredentials()}}
	err := NewManager(store, client).Disconnect(ctx)
	if !revokeCalled || !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatal("test did not expire during revocation")
	}
	if err == nil || !strings.Contains(err.Error(), "not confirmed") {
		t.Fatalf("missing remote revocation warning: %v", err)
	}
	if store.c.AccessToken != "" || store.c.RefreshToken != "" || store.clears != 1 || store.c.ClientID != "issued" {
		t.Fatal("disconnect did not clear local tokens and preserve registration")
	}
}
