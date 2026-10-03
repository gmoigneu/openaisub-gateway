// CLI tests check secret setup and listener boundaries without Docker or a live subscription.
package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gmoigneu/openaisub-gateway/internal/inference"
	"github.com/gmoigneu/openaisub-gateway/internal/store"
)

func TestImportRetryPreservesPayloadAndHonorsCancellation(t *testing.T) {
	payload := []byte(`{"fixture":"credentials"}`)
	attempts := 0
	err := importWithRetry(context.Background(), payload, func(action string, body []byte) ([]byte, error) {
		attempts++
		if action != "import" || !bytes.Equal(body, payload) {
			t.Fatal("transfer changed payload")
		}
		if attempts == 1 {
			return nil, errors.New("temporary SSH failure")
		}
		return nil, nil
	})
	if err != nil || attempts != 2 {
		t.Fatalf("retry failed: attempts=%d err=%v", attempts, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err = importWithRetry(ctx, payload, func(string, []byte) ([]byte, error) { t.Fatal("canceled transfer started"); return nil, nil })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %v", err)
	}
}

func TestInitCreatesPrivateSecretsAndPreservesExistingValues(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "secrets")
	if err := initialize([]string{"--secrets-dir", dir}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(dir)
	if err != nil || info.Mode().Perm() != 0700 {
		t.Fatalf("directory permissions: %v, %v", info, err)
	}
	original := map[string]string{}
	for _, name := range []string{"admin_secret", "encryption_key"} {
		path := filepath.Join(dir, name)
		value, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(string(value)))
		if err != nil || len(decoded) != 32 {
			t.Fatalf("%s does not hold 256 random bits", name)
		}
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("unsafe %s permissions", name)
		}
		original[name] = string(value)
	}
	if original["admin_secret"] == original["encryption_key"] {
		t.Fatal("secrets were reused")
	}
	if err := initialize([]string{"--secrets-dir", dir}); err != nil {
		t.Fatal(err)
	}
	for name, want := range original {
		got, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || string(got) != want {
			t.Fatalf("existing %s overwritten", name)
		}
	}
}

func TestInitDoesNotReplaceExistingSecret(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "admin_secret")
	want := "existing-value\n"
	if err := os.WriteFile(path, []byte(want), 0600); err != nil {
		t.Fatal(err)
	}
	if err := initialize([]string{"--secrets-dir", dir}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != want {
		t.Fatal("existing secret overwritten")
	}
	if _, err := os.Stat(filepath.Join(dir, "encryption_key")); err != nil {
		t.Fatal("missing secret was not created")
	}
}

func TestSecretValidation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "admin")
	t.Setenv("GATEWAY_ADMIN_SECRET_FILE", path)
	if err := os.WriteFile(path, []byte("short"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := adminSecret(); err == nil {
		t.Fatal("short administrator password accepted")
	}
	want := strings.Repeat("a", 43)
	if err := os.WriteFile(path, []byte(want+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := adminSecret()
	if err != nil || got != want {
		t.Fatal("valid secret was not read")
	}
}

func TestShellQuotePreservesDirectoryAndBlocksCommands(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "injected")
	for _, value := range []string{"/srv/project with spaces", "/srv/o'brien", "", "/srv/a; touch " + marker, "/srv/$(touch " + marker + ")", "/srv/`touch " + marker + "`", "/srv/'\n touch " + marker + "\n'"} {
		// Execute the actual shell boundary used by SSH with only our own fixtures.
		out, err := exec.Command("sh", "-c", "printf '%s' "+shellQuote(value)).Output()
		if err != nil || string(out) != value {
			t.Fatalf("quoted value changed: %q, %v", out, err)
		}
		if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("directory executed as a command")
		}
	}
}

type noToken struct{ t *testing.T }

func (n noToken) AccessToken(context.Context) (string, error) {
	n.t.Error("admin path requested upstream authentication")
	return "", errors.New("unexpected upstream request")
}

func TestAPIAuthenticationHealthAndAdminIsolation(t *testing.T) {
	s, err := store.Open(t.TempDir(), make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	k, key, err := s.CreateKey(context.Background(), "Mastra")
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	h := apiHandler(s, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(204) }))
	for _, tc := range []struct {
		method, path, auth string
		status             int
	}{{"GET", "/healthz", "", 200}, {"POST", "/healthz", "", 401}, {"GET", "/v1/models", "", 401}, {"GET", "/v1/models", "Bearer wrong", 401}, {"GET", "/v1/models", key, 401}, {"GET", "/v1/models", "Bearer " + key, 204}} {
		r := httptest.NewRequest(tc.method, tc.path, nil)
		r.Header.Set("Authorization", tc.auth)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != tc.status {
			t.Fatalf("%s %s status %d, want %d", tc.method, tc.path, w.Code, tc.status)
		}
		if w.Code == 401 && (!strings.Contains(w.Body.String(), `"code":"invalid_api_key"`) || w.Header().Get("Content-Type") != "application/json") {
			t.Fatal("auth rejection is not OpenAI compatible")
		}
	}
	if calls != 1 {
		t.Fatalf("unauthenticated request reached inference: %d", calls)
	}
	isolation := apiHandler(s, inference.New(noToken{t}, nil))
	for _, path := range []string{"/", "/login", "/keys", "/revoke", "/disconnect", "/internal/identity", "/internal/import"} {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.Header.Set("Authorization", "Bearer "+key)
		w := httptest.NewRecorder()
		isolation.ServeHTTP(w, r)
		if w.Code != 404 {
			t.Fatalf("API exposed admin path %s: %d", path, w.Code)
		}
	}
	if err := s.RevokeKey(context.Background(), k.ID); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	r.Header.Set("Authorization", "Bearer "+key)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 401 || calls != 1 {
		t.Fatal("revoked key remained authorized")
	}
}

func TestLoginRejectsSSHOptionInjectionBeforeExecution(t *testing.T) {
	for _, args := range [][]string{{"--ssh", "-oProxyCommand=bad", "--directory", "/srv/project"}, {"--ssh", "host\ncommand", "--directory", "/srv/project"}, {"--ssh", "host", "--directory", "relative"}, {"extra"}} {
		if err := loginCommand(args); err == nil {
			t.Fatalf("unsafe arguments accepted: %v", args)
		}
	}
}

func TestLocalAdminDoesNotLeakSecretAcrossRedirect(t *testing.T) {
	secret := strings.Repeat("s", 43)
	path := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(path, []byte(secret), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GATEWAY_ADMIN_SECRET_FILE", path)
	called := false
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true }))
	defer target.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+secret {
			t.Error("missing admin authorization")
		}
		http.Redirect(w, r, target.URL, 307)
	}))
	defer server.Close()
	t.Setenv("GATEWAY_ADMIN_ADDR", strings.TrimPrefix(server.URL, "http://"))
	if _, err := localAdmin("GET", "/internal/identity", nil); err == nil || strings.Contains(err.Error(), secret) {
		t.Fatal("redirect did not fail safely")
	}
	if called {
		t.Fatal("administrator secret sent to redirected endpoint")
	}
}
