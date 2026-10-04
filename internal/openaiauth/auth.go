// Package openaiauth manages the owner's renewable OpenAI session without exposing tokens to clients.
package openaiauth

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"
)

const (
	Issuer      = "https://auth.openai.com"
	Resource    = "https://api.openai.com/v1"
	DirectScope = "chatgpt.tokens.use.direct"
)

var (
	ErrNotConnected = errors.New("OpenAI is not connected")
	ErrConflict     = errors.New("credentials changed during the operation")
	ErrReconnect    = errors.New("OpenAI sign-in is required")
)

type Credentials struct {
	ClientID     string    `json:"client_id"`
	HostID       string    `json:"ext_agent_host_id"`
	Subject      string    `json:"subject"`
	Issuer       string    `json:"issuer"`
	Email        string    `json:"email,omitempty"`
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	IDToken      string    `json:"id_token"`
	Scopes       []string  `json:"scopes"`
	ExpiresAt    time.Time `json:"expires_at"`
	Revision     int64     `json:"-"`
}

// Store must atomically compare Revision before replacing or clearing credentials.
type Store interface {
	LoadCredentials(context.Context) (Credentials, error)
	SaveCredentials(context.Context, Credentials) error
	ClearCredentials(context.Context, int64) error
}

type endpoints struct{ issuer, authorize, token, jwks, discovery string }

func officialEndpoints() endpoints {
	return endpoints{Issuer, Issuer + "/api/accounts/authorize", Issuer + "/api/accounts/oauth/token", Issuer + "/.well-known/jwks.json", Issuer + "/.well-known/openid-configuration"}
}

func safeClient(client *http.Client) *http.Client {
	if client == nil {
		client = http.DefaultClient
	}
	c := *client
	c.Timeout = 20 * time.Second
	// A redirect could send credentials to a different host.
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &c
}

type Manager struct {
	store     Store
	client    *http.Client
	endpoints endpoints
	mu        sync.Mutex
}

func NewManager(store Store, client *http.Client) *Manager {
	return &Manager{store: store, client: safeClient(client), endpoints: officialEndpoints()}
}

func hasScope(scopes []string, want string) bool {
	for _, scope := range scopes {
		if scope == want {
			return true
		}
	}
	return false
}

func (m *Manager) AccessToken(ctx context.Context) (string, error) {
	// Read again under the lock so waiting requests see a rotated token.
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	c, err := m.store.LoadCredentials(ctx)
	if err != nil {
		return "", err
	}
	if c.AccessToken == "" {
		return "", ErrNotConnected
	}
	if !hasScope(c.Scopes, DirectScope) || c.Issuer != m.endpoints.issuer {
		return "", ErrReconnect
	}
	if time.Until(c.ExpiresAt) > time.Minute {
		return c.AccessToken, nil
	}
	if c.RefreshToken == "" || c.ClientID == "" || c.ClientID == "dynamic_agent_client" {
		return "", ErrReconnect
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	// The issuer may consume a rotating token before the caller disconnects.
	refreshCtx, stopRefresh := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
	updated, err := m.refresh(refreshCtx, c)
	stopRefresh()
	if err != nil {
		if errors.Is(err, ErrReconnect) {
			persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			if clearErr := m.store.ClearCredentials(persistCtx, c.Revision); clearErr != nil {
				return "", clearErr
			}
		}
		return "", err
	}
	// Once rotation succeeds, losing the caller must not discard its replacement.
	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := m.store.SaveCredentials(persistCtx, updated); err != nil {
		return "", err
	}
	return updated.AccessToken, nil
}
