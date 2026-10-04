// Login runs the browser callback on loopback and binds every result to its pending attempt.
package openaiauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

type LoginOptions struct {
	HostID          string
	ClientID        string
	Subject         string
	IDTokenHint     string
	Email           string
	ListenAddress   string
	OnAuthorization func(string)
	OnRegistration  func(string) error
}

func Login(ctx context.Context, options LoginOptions) (Credentials, error) {
	return login(ctx, options, safeClient(nil), officialEndpoints())
}

func randomString() string {
	var b [32]byte
	_, _ = rand.Read(b[:])
	return base64.RawURLEncoding.EncodeToString(b[:])
}

func login(ctx context.Context, o LoginOptions, client *http.Client, e endpoints) (Credentials, error) {
	if o.HostID == "" || o.OnAuthorization == nil {
		return Credentials{}, errors.New("host ID and authorization handler are required")
	}
	if o.ClientID == "dynamic_agent_client" {
		return Credentials{}, errors.New("registration must use an issued client ID")
	}
	if o.ClientID == "" && o.Subject != "" {
		return Credentials{}, errors.New("returning account identity requires its issued client ID")
	}
	if o.ListenAddress == "" {
		o.ListenAddress = "127.0.0.1:1455"
	}
	host, _, err := net.SplitHostPort(o.ListenAddress)
	if err != nil || host != "127.0.0.1" {
		return Credentials{}, errors.New("OAuth callback must listen on 127.0.0.1")
	}
	listener, err := net.Listen("tcp4", o.ListenAddress)
	if err != nil {
		return Credentials{}, errors.New("cannot start loopback sign-in listener")
	}
	defer listener.Close()
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	state, nonce, verifier := randomString(), randomString(), randomString()
	challenge := sha256.Sum256([]byte(verifier))
	redirect := "http://" + listener.Addr().String() + "/auth/callback"
	clientID := o.ClientID
	if clientID == "" {
		clientID = "dynamic_agent_client"
	}
	authURL, err := url.Parse(e.authorize)
	if err != nil {
		return Credentials{}, errors.New("invalid authorization endpoint")
	}
	query := url.Values{"client_id": {clientID}, "ext_agent_host_id": {o.HostID}, "response_type": {"code"}, "redirect_uri": {redirect}, "scope": {"openid profile email offline_access resource.invoke " + DirectScope}, "resource": {Resource}, "state": {state}, "nonce": {nonce}, "code_challenge_method": {"S256"}, "code_challenge": {base64.RawURLEncoding.EncodeToString(challenge[:])}}
	if o.ClientID == "" {
		query.Set("agent_name_hint", "OpenAI Subscription Gateway")
	} else {
		if o.IDTokenHint != "" {
			query.Set("id_token_hint", o.IDTokenHint)
		}
		if o.Email != "" {
			query.Set("login_hint", o.Email)
		}
	}
	authURL.RawQuery = query.Encode()
	type result struct {
		credentials Credentials
		err         error
	}
	results := make(chan result, 1)
	var once sync.Once
	mux := http.NewServeMux()
	mux.HandleFunc("/auth/callback", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Referrer-Policy", "no-referrer")
		if r.Method != http.MethodGet || r.URL.Path != "/auth/callback" || r.Host != listener.Addr().String() {
			http.Error(w, "Invalid callback", http.StatusBadRequest)
			return
		}
		q, err := url.ParseQuery(r.URL.RawQuery)
		if err != nil || len(q["state"]) != 1 || subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(state)) != 1 {
			http.Error(w, "Invalid sign-in state", http.StatusBadRequest)
			return
		}
		for _, key := range []string{"code", "client_id", "error"} {
			if len(q[key]) > 1 {
				http.Error(w, "Invalid callback", http.StatusBadRequest)
				return
			}
		}
		processed := false
		once.Do(func() {
			processed = true
			credentials, err := finishLogin(ctx, client, e, o, q, redirect, verifier, nonce)
			if err != nil {
				http.Error(w, "Sign-in failed. Return to the terminal.", http.StatusBadRequest)
			} else {
				_, _ = w.Write([]byte("Sign-in complete. You can close this window."))
			}
			results <- result{credentials, err}
		})
		if !processed {
			http.Error(w, "Sign-in already processed", http.StatusConflict)
		}
	})
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 30 * time.Second, MaxHeaderBytes: 16 << 10}
	defer func() {
		// Let the browser receive the result before closing the loopback listener.
		shutdownCtx, stop := context.WithTimeout(context.Background(), 2*time.Second)
		defer stop()
		if server.Shutdown(shutdownCtx) != nil {
			server.Close()
		}
	}()
	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			select {
			case results <- result{err: errors.New("sign-in callback stopped")}:
			default:
			}
		}
	}()
	o.OnAuthorization(authURL.String())
	select {
	case <-ctx.Done():
		return Credentials{}, ctx.Err()
	case result := <-results:
		return result.credentials, result.err
	}
}

func finishLogin(ctx context.Context, client *http.Client, e endpoints, o LoginOptions, q url.Values, redirect, verifier, nonce string) (Credentials, error) {
	if q.Get("error") != "" {
		return Credentials{}, errors.New("OpenAI sign-in was declined or failed")
	}
	if q.Get("code") == "" {
		return Credentials{}, errors.New("authorization code is missing")
	}
	issued := q.Get("client_id")
	if o.ClientID != "" {
		if issued != "" && issued != o.ClientID {
			return Credentials{}, errors.New("OpenAI client does not match the selected registration")
		}
		issued = o.ClientID
	}
	if issued == "" || issued == "dynamic_agent_client" {
		return Credentials{}, errors.New("OpenAI did not issue a client registration")
	}
	// Keep the issued registration even if code exchange requires a fresh attempt.
	if o.ClientID == "" && o.OnRegistration != nil {
		if err := o.OnRegistration(issued); err != nil {
			return Credentials{}, errors.New("cannot retain OpenAI client registration")
		}
	}
	t, err := exchange(ctx, client, e.token, url.Values{"grant_type": {"authorization_code"}, "client_id": {issued}, "code": {q.Get("code")}, "code_verifier": {verifier}, "redirect_uri": {redirect}, "resource": {Resource}})
	if err != nil {
		return Credentials{}, err
	}
	id, err := verifyID(ctx, client, e, issued, t.IDToken)
	if err != nil {
		return Credentials{}, err
	}
	if subtle.ConstantTimeCompare([]byte(id.Nonce), []byte(nonce)) != 1 {
		return Credentials{}, errors.New("OpenAI identity nonce does not match")
	}
	if o.Subject != "" && o.Subject != id.Subject {
		return Credentials{}, errors.New("OpenAI account does not match the selected registration")
	}
	if t.RefreshToken == "" || !hasScope(strings.Fields(t.Scope), DirectScope) {
		return Credentials{}, errors.New("OpenAI did not grant renewable ChatGPT plan access")
	}
	var claims struct {
		Email string `json:"email"`
	}
	if err := id.Claims(&claims); err != nil {
		return Credentials{}, errors.New("invalid OpenAI identity claims")
	}
	return Credentials{ClientID: issued, HostID: o.HostID, Subject: id.Subject, Issuer: id.Issuer, Email: claims.Email, AccessToken: t.AccessToken, RefreshToken: t.RefreshToken, IDToken: t.IDToken, Scopes: strings.Fields(t.Scope), ExpiresAt: time.Now().UTC().Add(time.Duration(t.ExpiresIn) * time.Second)}, nil
}
