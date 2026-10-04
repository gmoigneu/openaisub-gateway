// Public deployment checks reject ambiguous credentials and bound unauthenticated uploads.
package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gmoigneu/openaisub-gateway/internal/store"
)

func TestInferenceRejectsInvalidAndAmbiguousCredentials(t *testing.T) {
	s, err := store.Open(t.TempDir(), make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	k, key, err := s.CreateKey(context.Background(), "public client")
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	h := apiHandler(s, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusNoContent)
	}))
	routes := []struct{ method, path string }{{"GET", "/v1/models"}, {"POST", "/v1/responses"}, {"POST", "/v1/embeddings"}, {"POST", "/v1/audio/transcriptions"}, {"POST", "/v1/audio/speech"}}
	for _, route := range routes {
		for _, auth := range [][]string{nil, {"Bearer wrong"}, {"Basic " + key}, {"Bearer "}, {"Bearer " + strings.Repeat("a", 43)}, {"Bearer " + key, "Bearer wrong"}, {"Bearer wrong", "Bearer " + key}, {"Bearer " + key, "Bearer " + key}, {"Bearer " + key + ", Bearer wrong"}} {
			r := httptest.NewRequest(route.method, route.path, nil)
			for _, value := range auth {
				r.Header.Add("Authorization", value)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != http.StatusUnauthorized || calls != 0 {
				t.Fatal("invalid credentials reached inference")
			}
		}
		r := httptest.NewRequest(route.method, route.path+"?api_key="+key, nil)
		r.AddCookie(&http.Cookie{Name: "api_key", Value: key})
		r.Header.Set("X-Api-Key", key)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusUnauthorized || calls != 0 {
			t.Fatal("alternative credential location bypassed bearer authentication")
		}
	}
	request := func(route struct{ method, path string }) int {
		r := httptest.NewRequest(route.method, route.path, nil)
		r.Header.Set("Authorization", "Bearer "+key)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	for _, route := range routes {
		if request(route) != http.StatusNoContent {
			t.Fatal("valid key was rejected")
		}
	}
	if calls != len(routes) {
		t.Fatal("valid requests missed the inference handler")
	}
	if err := s.RevokeKey(context.Background(), k.ID); err != nil {
		t.Fatal(err)
	}
	for _, route := range routes {
		if request(route) != http.StatusUnauthorized || calls != len(routes) {
			t.Fatal("revoked key reached inference")
		}
	}
}

func TestUnauthenticatedSlowBodyHasReadDeadline(t *testing.T) {
	s, err := store.Open(t.TempDir(), make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	server := newAPIServer("127.0.0.1:0", apiHandler(s, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("unauthenticated upload reached inference")
	})))
	if server.ReadTimeout <= 0 || server.ReadTimeout > 30*time.Second || server.WriteTimeout != 0 {
		t.Fatal("API must bound uploads without imposing a total stream lifetime")
	}
	server.ReadTimeout = 100 * time.Millisecond
	listener, err := net.Listen("tcp", server.Addr)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { defer close(done); _ = server.Serve(listener) }()
	defer func() { _ = server.Close(); <-done }()
	conn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	// net/http drains small unread request bodies before writing an auth rejection.
	_, err = io.WriteString(conn, "POST /v1/responses HTTP/1.1\r\nHost: gateway\r\nContent-Length: 128\r\n\r\nx")
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("slow unauthenticated upload prevented rejection: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatal("slow upload did not receive an auth rejection")
	}
}

func TestPaidKeyFileAndInferenceRouteSplit(t *testing.T) {
	t.Setenv("GATEWAY_OPENAI_API_KEY_FILE", "")
	if key, err := paidAPIKey(); err != nil || key != "" {
		t.Fatal("unconfigured paid key should leave subscription routes available")
	}
	path := filepath.Join(t.TempDir(), "openai_api_key")
	if err := os.WriteFile(path, []byte("platform-test-key\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GATEWAY_OPENAI_API_KEY_FILE", path)
	if key, err := paidAPIKey(); err != nil || key != "platform-test-key" {
		t.Fatal("paid key file was not loaded")
	}
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := paidAPIKey(); err == nil {
		t.Fatal("empty paid key file was accepted")
	}
	if err := os.WriteFile(path, []byte("first\nsecond"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := paidAPIKey(); err == nil {
		t.Fatal("multiline paid key file was accepted")
	}
	var subscription, platform int
	routes := inferenceRoutes(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { subscription++ }), http.HandlerFunc(func(http.ResponseWriter, *http.Request) { platform++ }))
	for _, path := range []string{"/v1/models", "/v1/responses"} {
		routes.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", path, nil))
	}
	for _, path := range []string{"/v1/embeddings", "/v1/audio/transcriptions", "/v1/audio/speech"} {
		routes.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", path, nil))
	}
	if subscription != 2 || platform != 3 {
		t.Fatal("paid route crossed the subscription credential boundary")
	}
}

func TestContainerLoginRejectsUnsafeTargets(t *testing.T) {
	for _, name := range []string{"", "-i", "a b", "a;b", "a\nb", "$(id)", "a'b", "/gateway"} {
		if err := loginCommand([]string{"--container", name}); err == nil {
			t.Fatal("unsafe container target accepted")
		}
	}
	if err := loginCommand([]string{"--container", "gateway", "--directory", "/srv"}); err == nil {
		t.Fatal("ambiguous container and Compose targets accepted")
	}
	if err := loginCommand([]string{"--ssh", "-oProxyCommand=bad", "--container", "gateway"}); err == nil {
		t.Fatal("SSH option accepted as a destination")
	}
}

func TestLoginTransferCommandsPreserveStdinAndTarget(t *testing.T) {
	dir := t.TempDir()
	// A controlled Docker substitute observes the actual shell boundary and stdin.
	fixture := "#!/bin/sh\nprintf '%s\\n' \"$@\"\ncat\n"
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(fixture), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	for _, remote := range []bool{false, true} {
		for _, target := range []string{"openaisub-gateway", "abc123", ""} {
			for _, action := range []string{"identity", "register", "import"} {
				t.Run(fmt.Sprintf("remote=%t/%s/%s", remote, target, action), func(t *testing.T) {
					sshHost := ""
					if remote {
						sshHost = "owner@example.com"
					}
					cmd := loginTransferCommand(context.Background(), sshHost, dir, target, action)
					if remote {
						if !reflect.DeepEqual(cmd.Args[1:3], []string{"--", sshHost}) {
							t.Fatal("remote destination changed")
						}
						cmd = exec.Command("sh", "-c", cmd.Args[3])
					}
					payload := "fixture-credential-payload"
					if strings.Contains(strings.Join(cmd.Args, " "), payload) {
						t.Fatal("credentials were included in process arguments")
					}
					cmd.Stdin = strings.NewReader(payload)
					out, err := cmd.Output()
					want := "exec\n-i\n" + target + "\n/gateway\nauth\n" + action + "\n" + payload
					if target == "" {
						want = "compose\nexec\n-T\ngateway\n/gateway\nauth\n" + action + "\n" + payload
					}
					if err != nil || string(out) != want {
						t.Fatal("transfer changed target, arguments or input")
					}
				})
			}
		}
	}
}
