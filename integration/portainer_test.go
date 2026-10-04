// Exercise the Portainer stack through the shipped Caddy routes without OpenAI.
package integration

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

type portainerFixture struct {
	t         *testing.T
	ctx       context.Context
	dir, name string
	env       []string
	client    *http.Client
}

func (f *portainerFixture) docker(args ...string) []byte {
	f.t.Helper()
	cmd := exec.CommandContext(f.ctx, "docker", args...)
	cmd.Env = f.env
	output, err := cmd.Output()
	if err != nil {
		// Commands and response bodies can contain generated test credentials.
		f.t.Fatalf("Portainer fixture Docker operation failed: %T", err)
	}
	return output
}

func (f *portainerFixture) compose(args ...string) []byte {
	f.t.Helper()
	flags := []string{"compose", "-p", f.name, "-f", filepath.Join(f.dir, "compose.portainer.yaml")}
	return f.docker(append(flags, args...)...)
}

func (f *portainerFixture) request(method, endpoint, key, body string) (*http.Response, []byte) {
	f.t.Helper()
	req, err := http.NewRequestWithContext(f.ctx, method, endpoint, strings.NewReader(body))
	if err != nil {
		f.t.Fatal("cannot build fixture request")
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := f.client.Do(req)
	if err != nil {
		f.t.Fatal("Portainer fixture HTTP request failed")
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 128<<10))
	resp.Body.Close()
	if err != nil {
		f.t.Fatal("cannot read fixture response")
	}
	return resp, data
}

func (f *portainerFixture) wait(endpoint string, status int) {
	f.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) && f.ctx.Err() == nil {
		resp, err := f.client.Get(endpoint)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == status {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	f.t.Fatal("Portainer fixture listener did not become ready")
}

func (f *portainerFixture) adminBase() string {
	f.t.Helper()
	var bindings map[string][]struct {
		HostIP   string `json:"HostIp"`
		HostPort string
	}
	if err := json.Unmarshal(f.docker("inspect", "--format", "{{json .NetworkSettings.Ports}}", f.name), &bindings); err != nil {
		f.t.Fatal("cannot inspect Portainer port bindings")
	}
	admin := bindings["8081/tcp"]
	if len(admin) != 1 || admin[0].HostIP != "127.0.0.1" || admin[0].HostPort == "" {
		f.t.Fatal("Portainer must publish only loopback administration, never inference")
	}
	for port, bound := range bindings {
		if port != "8081/tcp" && len(bound) != 0 {
			f.t.Fatal("Portainer must not publish inference")
		}
	}
	return "http://127.0.0.1:" + admin[0].HostPort
}

func (f *portainerFixture) form(base, path string, values url.Values, status int) []byte {
	f.t.Helper()
	req, err := http.NewRequestWithContext(f.ctx, http.MethodPost, base+path, strings.NewReader(values.Encode()))
	if err != nil {
		f.t.Fatal("cannot build administrator form")
	}
	req.Header.Set("Origin", base)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := f.client.Do(req)
	if err != nil {
		f.t.Fatal("administrator form request failed")
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 128<<10))
	if err != nil || resp.StatusCode != status {
		f.t.Fatalf("administrator form returned HTTP %d, want %d", resp.StatusCode, status)
	}
	return body
}

func fixtureField(t *testing.T, body []byte, expression string) string {
	t.Helper()
	match := regexp.MustCompile(expression).FindSubmatch(body)
	if len(match) != 2 {
		t.Fatal("expected administrator field is absent")
	}
	return string(match[1])
}

func (f *portainerFixture) login(base string) string {
	f.t.Helper()
	password := strings.TrimSpace(string(f.docker("exec", f.name, "/gateway", "admin", "password")))
	f.form(base, "/login", url.Values{"password": {password}}, http.StatusSeeOther)
	resp, body := f.request(http.MethodGet, base+"/", "", "")
	if resp.StatusCode != http.StatusOK {
		f.t.Fatal("administrator dashboard unavailable")
	}
	return fixtureField(f.t, body, `name="csrf" value="([^"]+)"`)
}

func (f *portainerFixture) startCaddy(status int) string {
	f.t.Helper()
	caddy, err := os.ReadFile(filepath.Join("..", "examples", "Caddyfile"))
	if err != nil {
		f.t.Fatal(err)
	}
	// The operator's Caddy owns certificates; test identical routing on loopback HTTP.
	caddy = []byte(strings.Replace(string(caddy), "gateway.example.com", "http://:8080", 1))
	if err := os.WriteFile(filepath.Join(f.dir, "Caddyfile"), caddy, 0600); err != nil {
		f.t.Fatal(err)
	}
	f.docker("run", "-d", "--name", f.name+"-caddy", "--network", f.name, "--publish", "127.0.0.1::8080",
		"--volume", filepath.Join(f.dir, "Caddyfile")+":/etc/caddy/Caddyfile:ro", "caddy:2-alpine")
	proxy := "http://" + strings.TrimSpace(string(f.docker("port", f.name+"-caddy", "8080")))
	f.wait(proxy+"/v1/models", status)
	return proxy
}

func TestContainerPortainerPublicAuthentication(t *testing.T) {
	if os.Getenv("DOCKER_CONTRACT") != "1" {
		t.Skip("set DOCKER_CONTRACT=1 after building openaisub-gateway:local")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	jar, _ := cookiejar.New(nil)
	f := &portainerFixture{t: t, ctx: ctx, dir: t.TempDir(), name: "gateway-portainer-" + strings.ToLower(rand.Text()),
		client: &http.Client{Jar: jar, Timeout: 2 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	secrets := filepath.Join(f.dir, "secrets")
	if err := os.Mkdir(secrets, 0700); err != nil {
		t.Fatal(err)
	}
	f.env = append(os.Environ(), "GATEWAY_IMAGE=openaisub-gateway:local", "GATEWAY_CONTAINER_NAME="+f.name,
		"GATEWAY_ADMIN_PORT=0", "GATEWAY_SECRETS_DIR="+secrets, "GATEWAY_DATA_VOLUME="+f.name+"-data", "GATEWAY_PROXY_NETWORK="+f.name)
	stack, err := os.ReadFile(filepath.Join("..", "compose.portainer.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.dir, "compose.portainer.yaml"), stack, 0600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 30*time.Second)
		defer stop()
		_ = exec.CommandContext(cleanup, "docker", "rm", "-fv", f.name+"-caddy", f.name).Run()
		if exec.CommandContext(cleanup, "docker", "volume", "rm", "-f", f.name+"-data").Run() != nil {
			t.Error("Portainer fixture volume cleanup failed")
		}
		if exec.CommandContext(cleanup, "docker", "network", "rm", f.name).Run() != nil {
			t.Error("Portainer fixture network cleanup failed")
		}
	})
	f.docker("network", "create", f.name)
	f.docker("run", "--rm", "--user", "0", "--volume", secrets+":/bootstrap", "openaisub-gateway:local", "init", "--secrets-dir", "/bootstrap", "--owner", "10001")
	f.compose("up", "-d", "--no-build", "--pull", "never")
	base := f.adminBase()
	f.wait(base+"/", http.StatusOK)
	proxy := f.startCaddy(http.StatusUnauthorized)
	csrf := f.login(base)
	body := f.form(base, "/keys", url.Values{"csrf": {csrf}, "name": {"Portainer contract"}}, http.StatusOK)
	key := fixtureField(t, body, `<code>(osk_[A-Za-z0-9_-]{43})</code>`)
	id := fixtureField(t, body, `name="id" value="([^"]+)"`)
	for _, tc := range []struct {
		name, method, path, key, body string
		status                        int
	}{
		{"missing models key", "GET", "/v1/models", "", "", 401},
		{"invalid models key", "GET", "/v1/models", "invalid", "", 401},
		{"missing responses key", "POST", "/v1/responses", "", `{}`, 401},
		{"invalid responses key", "POST", "/v1/responses", "invalid", `{}`, 401},
		{"valid models key", "GET", "/v1/models", key, "", 503},
		{"valid responses key", "POST", "/v1/responses", key, `{"model":"fixture","input":"fixture"}`, 503},
	} {
		t.Run(tc.name, func(t *testing.T) {
			subtest := *f
			subtest.t = t
			resp, _ := subtest.request(tc.method, proxy+tc.path, tc.key, tc.body)
			if resp.StatusCode != tc.status {
				t.Fatalf("HTTP %d, want %d", resp.StatusCode, tc.status)
			}
		})
	}
	for _, path := range []string{"/", "/healthz", "/login", "/keys", "/revoke", "/disconnect", "/internal/identity", "/internal/import", "/internal/registration"} {
		for _, credential := range []string{"", key} {
			resp, _ := f.request(http.MethodGet, proxy+path, credential, "")
			if resp.StatusCode != http.StatusNotFound {
				t.Fatalf("public path %s returned HTTP %d, want 404", path, resp.StatusCode)
			}
		}
	}
	identity := string(f.docker("exec", f.name, "/gateway", "auth", "identity"))
	f.compose("up", "-d", "--no-build", "--pull", "never", "--force-recreate")
	base = f.adminBase()
	f.wait(base+"/", http.StatusOK)
	if string(f.docker("exec", f.name, "/gateway", "auth", "identity")) != identity {
		t.Fatal("Portainer recreation changed gateway identity")
	}
	// A saved key still reaches gateway authentication after the container is replaced.
	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, _ := f.request(http.MethodGet, proxy+"/v1/models", key, "")
		if resp.StatusCode == http.StatusServiceUnavailable {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("saved key returned HTTP %d after recreation", resp.StatusCode)
		}
		time.Sleep(100 * time.Millisecond)
	}
	csrf = f.login(base)
	f.form(base, "/revoke", url.Values{"csrf": {csrf}, "id": {id}}, http.StatusSeeOther)
	for _, endpoint := range []struct{ method, path, body string }{{"GET", "/v1/models", ""}, {"POST", "/v1/responses", `{}`}} {
		resp, _ := f.request(endpoint.method, proxy+endpoint.path, key, endpoint.body)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("revoked key returned HTTP %d", resp.StatusCode)
		}
	}
}

func TestContainerCaddyStreamingAndCancellation(t *testing.T) {
	if os.Getenv("DOCKER_CONTRACT") != "1" {
		t.Skip("set DOCKER_CONTRACT=1 to exercise the Caddy routing example")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	f := &portainerFixture{t: t, ctx: ctx, dir: t.TempDir(), name: "gateway-caddy-" + strings.ToLower(rand.Text()),
		env: os.Environ(), client: &http.Client{Timeout: 2 * time.Second}}
	// The fixture holds a stream open until cancellation and counts POST attempts.
	const fixture = `const http = require('node:http');
let requests = 0, closed = 0;
http.createServer((req, res) => {
  if (req.url === '/v1/models') {
    res.setHeader('Content-Type', 'application/json');
    res.end(JSON.stringify({requests, closed}));
    return;
  }
  if (req.headers.authorization !== 'Bearer fixture-client-key') {
    res.writeHead(401).end();
    return;
  }
  requests++;
  req.resume();
  req.on('end', () => {
    if (req.headers['x-fixture-fail']) {
      req.socket.destroy();
      return;
    }
    res.writeHead(200, {'Content-Type': 'text/event-stream'});
    res.write('data: {"type":"fixture.started"}\n\n');
    const timer = setInterval(() => res.write(': heartbeat\n\n'), 10000);
    res.on('close', () => { clearInterval(timer); closed++; });
  });
}).listen(8080, '0.0.0.0');
`
	fixturePath := filepath.Join(f.dir, "fixture.cjs")
	if err := os.WriteFile(fixturePath, []byte(fixture), 0644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 30*time.Second)
		defer stop()
		_ = exec.CommandContext(cleanup, "docker", "rm", "-fv", f.name+"-caddy", f.name).Run()
		if exec.CommandContext(cleanup, "docker", "network", "rm", f.name).Run() != nil {
			t.Error("Caddy fixture network cleanup failed")
		}
	})
	f.docker("network", "create", f.name)
	f.docker("run", "-d", "--name", f.name, "--network", f.name, "--network-alias", "openaisub-gateway",
		"--volume", fixturePath+":/fixture.cjs:ro", "node:24-bookworm-slim", "node", "/fixture.cjs")
	proxy := f.startCaddy(http.StatusOK)
	streamCtx, stopStream := context.WithCancel(ctx)
	defer stopStream()
	req, _ := http.NewRequestWithContext(streamCtx, http.MethodPost, proxy+"/v1/responses", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer fixture-client-key")
	resp, err := f.client.Do(req)
	if err != nil {
		t.Fatal("Caddy did not flush the first streaming response promptly")
	}
	line, err := bufio.NewReader(resp.Body).ReadString('\n')
	if err != nil || resp.StatusCode != http.StatusOK || line != "data: {\"type\":\"fixture.started\"}\n" {
		resp.Body.Close()
		t.Fatal("Caddy did not forward the first event before the stream completed")
	}
	stopStream()
	resp.Body.Close()
	state := func() (int, int) {
		t.Helper()
		_, data := f.request(http.MethodGet, proxy+"/v1/models", "", "")
		var result struct{ Requests, Closed int }
		if json.Unmarshal(data, &result) != nil {
			t.Fatal("invalid Caddy fixture state")
		}
		return result.Requests, result.Closed
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		requests, closed := state()
		if requests == 1 && closed == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Caddy did not cancel its upstream after the client disconnected")
		}
		time.Sleep(50 * time.Millisecond)
	}
	req, _ = http.NewRequestWithContext(ctx, http.MethodPost, proxy+"/v1/responses", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer fixture-client-key")
	req.Header.Set("X-Fixture-Fail", "1")
	resp, err = f.client.Do(req)
	if err != nil {
		t.Fatal("Caddy did not return the upstream transport failure")
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("Caddy returned HTTP %d for a failed upstream, want 502", resp.StatusCode)
	}
	requests, _ := state()
	if requests != 2 {
		t.Fatalf("Caddy replayed an inference POST: %d total attempts, want 2", requests)
	}
}
