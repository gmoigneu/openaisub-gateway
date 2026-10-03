package store

import (
	"bytes"
	"context"
	"errors"
	"github.com/gmoigneu/openaisub-gateway/internal/openaiauth"
	"os"
	"path/filepath"
	"testing"
)

func TestCredentialsSurviveRestartAndRejectStaleWrites(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	key := bytes.Repeat([]byte{7}, 32)
	s, err := Open(dir, key)
	if err != nil {
		t.Fatal(err)
	}
	host, err := s.HostID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	c := openaiauth.Credentials{ClientID: "oaiapp_test", HostID: host, Subject: "owner", AccessToken: "very-secret-bearer", RefreshToken: "rotating-secret"}
	if err = s.SaveCredentials(ctx, c); err != nil {
		t.Fatal(err)
	}
	first, err := s.LoadCredentials(ctx)
	if err != nil {
		t.Fatal(err)
	}
	second := first
	first.AccessToken = "next-token"
	if err = s.SaveCredentials(ctx, first); err != nil {
		t.Fatal(err)
	}
	if err = s.SaveCredentials(ctx, second); !errors.Is(err, openaiauth.ErrConflict) {
		t.Fatalf("stale write: %v", err)
	}
	if err = s.ClearCredentials(ctx, second.Revision); !errors.Is(err, openaiauth.ErrConflict) {
		t.Fatalf("stale clear: %v", err)
	}
	s.Close()
	raw, err := os.ReadFile(filepath.Join(dir, "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte(c.AccessToken)) || bytes.Contains(raw, []byte(c.RefreshToken)) {
		t.Fatal("plaintext credentials persisted")
	}
	s, err = Open(dir, key)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	same, _ := s.HostID(ctx)
	if same != host {
		t.Fatal("host changed after restart")
	}
	c, err = s.LoadCredentials(ctx)
	if err != nil || c.AccessToken != "next-token" {
		t.Fatalf("restart: %+v %v", c, err)
	}
	if err = s.ClearCredentials(ctx, c.Revision); err != nil {
		t.Fatal(err)
	}
	c, _ = s.LoadCredentials(ctx)
	if c.AccessToken != "" || c.RefreshToken != "" || c.ClientID != "oaiapp_test" {
		t.Fatal("clear did not preserve registration only")
	}
}
func TestKeysAndWrongEncryptionKey(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := Open(dir, bytes.Repeat([]byte{1}, 32))
	if err != nil {
		t.Fatal(err)
	}
	k, token, err := s.CreateKey(ctx, "Mastra")
	if err != nil {
		t.Fatal(err)
	}
	if !s.Authenticate(ctx, token) || s.Authenticate(ctx, "wrong") {
		t.Fatal("key authentication")
	}
	if err = s.RevokeKey(ctx, k.ID); err != nil {
		t.Fatal(err)
	}
	if s.Authenticate(ctx, token) {
		t.Fatal("revoked key accepted")
	}
	keys, _ := s.Keys(ctx)
	if len(keys) != 1 || !keys[0].Revoked {
		t.Fatal("key status")
	}
	if err = s.SaveCredentials(ctx, openaiauth.Credentials{AccessToken: "secret"}); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if wrong, err := Open(dir, bytes.Repeat([]byte{2}, 32)); err == nil {
		wrong.Close()
		t.Fatal("wrong encryption key accepted")
	}
}
