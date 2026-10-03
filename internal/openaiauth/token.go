// Token operations use bounded requests and never include upstream bodies in errors.
package openaiauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
)

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
	TokenType    string `json:"token_type"`
	Scope        string `json:"scope"`
	ExpiresIn    int64  `json:"expires_in"`
	Error        string `json:"error"`
}

func postForm(ctx context.Context, client *http.Client, endpoint string, form url.Values) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, errors.New("invalid OAuth endpoint")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, errors.New("OpenAI authentication request failed; retry later")
	}
	return resp, nil
}

func decodeBounded(body io.Reader, value any) error {
	data, err := io.ReadAll(io.LimitReader(body, 1<<20+1))
	if err != nil || len(data) > 1<<20 {
		return errors.New("invalid OpenAI authentication response")
	}
	if json.Unmarshal(data, value) != nil {
		return errors.New("invalid OpenAI authentication response")
	}
	return nil
}

func exchange(ctx context.Context, client *http.Client, endpoint string, form url.Values) (tokenResponse, error) {
	resp, err := postForm(ctx, client, endpoint, form)
	if err != nil {
		return tokenResponse{}, err
	}
	defer resp.Body.Close()
	var t tokenResponse
	decodeErr := decodeBounded(resp.Body, &t)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Temporary infrastructure failures must not erase a renewable session.
		if resp.StatusCode == 400 || resp.StatusCode == 401 {
			switch t.Error {
			case "invalid_grant", "invalid_refresh_token", "token_expired", "refresh_token_expired", "refresh_token_invalidated", "refresh_token_reused":
				return tokenResponse{}, ErrReconnect
			case "invalid_client":
				return tokenResponse{}, errors.New("OpenAI rejected the registered client; check its configuration")
			}
		}
		return tokenResponse{}, fmt.Errorf("OpenAI authentication returned HTTP %d", resp.StatusCode)
	}
	if decodeErr != nil {
		return tokenResponse{}, decodeErr
	}
	if t.Error != "" || t.AccessToken == "" || !strings.EqualFold(t.TokenType, "Bearer") || t.ExpiresIn <= 0 || t.ExpiresIn > 31536000 {
		return tokenResponse{}, errors.New("incomplete OpenAI token response")
	}
	return t, nil
}

func (m *Manager) refresh(ctx context.Context, c Credentials) (Credentials, error) {
	t, err := exchange(ctx, m.client, m.endpoints.token, url.Values{"grant_type": {"refresh_token"}, "client_id": {c.ClientID}, "refresh_token": {c.RefreshToken}, "resource": {Resource}})
	if err != nil {
		return Credentials{}, err
	}
	if t.Scope != "" {
		c.Scopes = strings.Fields(t.Scope)
	}
	if !hasScope(c.Scopes, DirectScope) {
		return Credentials{}, ErrReconnect
	}
	if t.IDToken != "" {
		id, err := verifyID(ctx, m.client, m.endpoints, c.ClientID, t.IDToken)
		if err != nil {
			return Credentials{}, err
		}
		if id.Subject != c.Subject {
			return Credentials{}, errors.New("refreshed OpenAI identity does not match the registration")
		}
		c.IDToken = t.IDToken
	}
	c.AccessToken = t.AccessToken
	if t.RefreshToken != "" {
		c.RefreshToken = t.RefreshToken
	}
	c.ExpiresAt = time.Now().UTC().Add(time.Duration(t.ExpiresIn) * time.Second)
	return c, nil
}

func verifyID(ctx context.Context, client *http.Client, e endpoints, clientID, raw string) (*oidc.IDToken, error) {
	ctx = oidc.ClientContext(ctx, client)
	keys := oidc.NewRemoteKeySet(ctx, e.jwks)
	id, err := oidc.NewVerifier(e.issuer, keys, &oidc.Config{ClientID: clientID, SupportedSigningAlgs: []string{"RS256"}}).Verify(ctx, raw)
	if err != nil || id.Subject == "" {
		return nil, errors.New("OpenAI identity token validation failed")
	}
	return id, nil
}

// ValidateCredentials rechecks credentials imported through the trusted local helper.
func ValidateCredentials(ctx context.Context, c Credentials) error {
	if c.ClientID == "" || c.ClientID == "dynamic_agent_client" || c.HostID == "" || c.Issuer != Issuer || c.Subject == "" || c.AccessToken == "" || c.RefreshToken == "" || !hasScope(c.Scopes, DirectScope) || !c.ExpiresAt.After(time.Now()) {
		return errors.New("incomplete OpenAI credentials")
	}
	id, err := verifyID(ctx, safeClient(nil), officialEndpoints(), c.ClientID, c.IDToken)
	if err != nil {
		return err
	}
	if id.Subject != c.Subject {
		return errors.New("OpenAI account identity does not match")
	}
	return nil
}

func (m *Manager) Disconnect(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, err := m.store.LoadCredentials(ctx)
	if err != nil {
		return err
	}
	var remoteErr error
	if c.RefreshToken != "" {
		remoteErr = m.revoke(ctx, c)
	}
	// Clear locally even when the remote service cannot confirm revocation.
	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := m.store.ClearCredentials(persistCtx, c.Revision); err != nil {
		return err
	}
	if remoteErr != nil {
		return errors.New("disconnected locally; remote revocation was not confirmed, disconnect the app in ChatGPT Settings")
	}
	return nil
}

func (m *Manager) revoke(ctx context.Context, c Credentials) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.endpoints.discovery, nil)
	if err != nil {
		return err
	}
	resp, err := m.client.Do(req)
	if err != nil {
		return errors.New("discovery failed")
	}
	var d struct {
		Issuer             string `json:"issuer"`
		RevocationEndpoint string `json:"revocation_endpoint"`
	}
	err = decodeBounded(resp.Body, &d)
	resp.Body.Close()
	if err != nil || resp.StatusCode != http.StatusOK || d.Issuer != m.endpoints.issuer {
		return errors.New("invalid discovery")
	}
	u, err := url.Parse(d.RevocationEndpoint)
	want, _ := url.Parse(m.endpoints.issuer)
	if err != nil || u.Scheme != want.Scheme || u.Host != want.Host || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("invalid revocation endpoint")
	}
	for attempt := 0; attempt < 3; attempt++ {
		resp, err = postForm(ctx, m.client, d.RevocationEndpoint, url.Values{"token": {c.RefreshToken}, "token_type_hint": {"refresh_token"}, "client_id": {c.ClientID}})
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
			if resp.StatusCode < 500 {
				return errors.New("revocation rejected")
			}
		}
		if attempt < 2 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(attempt+1) * 200 * time.Millisecond):
			}
		}
	}
	return errors.New("revocation unavailable")
}
