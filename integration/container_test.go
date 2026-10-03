// The opt-in container contract exercises packaging without contacting OpenAI.
package integration

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestContainerOnboarding(t *testing.T) {
	if os.Getenv("DOCKER_CONTRACT") != "1" {
		t.Skip("set DOCKER_CONTRACT=1 after building openaisub-gateway:local")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	// Never include command output in failures: some commands return credentials.
	docker := func(args ...string) ([]byte, error) {
		return exec.CommandContext(ctx, "docker", args...).Output()
	}
	mustDocker := func(label string, args ...string) []byte {
		t.Helper()
		output, err := docker(args...)
		if err != nil {
			t.Fatalf("%s failed: %T", label, err)
		}
		return output
	}
	var random [8]byte
	if _, err := rand.Read(random[:]); err != nil {
		t.Fatal("cannot generate isolated container names")
	}
	name := "gateway-contract-" + hex.EncodeToString(random[:])
	secondName := name + "-second"
	volume := name + "-data"
	const image = "openaisub-gateway:local"
	secretDir := t.TempDir()
	mustDocker("create data volume", "volume", "create", volume)
	t.Cleanup(func() {
		cleanupCtx, stop := context.WithTimeout(context.Background(), 30*time.Second)
		defer stop()
		_ = exec.CommandContext(cleanupCtx, "docker", "rm", "-f", name, secondName).Run()
		if err := exec.CommandContext(cleanupCtx, "docker", "volume", "rm", "-f", volume).Run(); err != nil {
			t.Error("container contract data volume cleanup failed")
		}
	})
	mustDocker("initialize secrets", "run", "--rm", "--user", "0", "--mount", "type=bind,src="+secretDir+",dst=/bootstrap", image,
		"init", "--secrets-dir", "/bootstrap", "--owner", "10001")
	for _, file := range []string{"admin_secret", "encryption_key"} {
		info, err := os.Stat(filepath.Join(secretDir, file))
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatal("initialization must create owner-only secret files")
		}
	}
	// Separate file mounts match Compose secrets and avoid parent-directory access.
	runtimeArgs := []string{"--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges:true",
		"--mount", "type=volume,src=" + volume + ",dst=/data",
		"--mount", "type=bind,src=" + filepath.Join(secretDir, "admin_secret") + ",dst=/run/secrets/admin_secret,readonly",
		"--mount", "type=bind,src=" + filepath.Join(secretDir, "encryption_key") + ",dst=/run/secrets/encryption_key,readonly"}
	args := append([]string{"run", "--detach", "--name", name, "--publish", "127.0.0.1::8081"}, runtimeArgs...)
	args = append(args, image)
	mustDocker("start gateway", args...)

	var inspected []struct {
		Config          struct{ User string }
		HostConfig      struct{ ReadonlyRootfs bool }
		NetworkSettings struct {
			Ports map[string][]struct{ HostIp, HostPort string }
		}
	}
	if err := json.Unmarshal(mustDocker("inspect container", "inspect", name), &inspected); err != nil || len(inspected) != 1 {
		t.Fatal("cannot inspect running container")
	}
	state := inspected[0]
	if state.Config.User != "10001:10001" || !state.HostConfig.ReadonlyRootfs {
		t.Fatal("container must use UID 10001 and a read-only root filesystem")
	}
	bindings := state.NetworkSettings.Ports["8081/tcp"]
	if len(bindings) != 1 || bindings[0].HostIp != "127.0.0.1" || bindings[0].HostPort == "" {
		t.Fatal("administration must bind only to host loopback")
	}
	for port, bound := range state.NetworkSettings.Ports {
		if port != "8081/tcp" && len(bound) != 0 {
			t.Fatal("only administration may publish a host port")
		}
	}
	base := "http://127.0.0.1:" + bindings[0].HostPort
	client := &http.Client{Timeout: time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	waitReady := func() {
		t.Helper()
		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) && ctx.Err() == nil {
			response, err := client.Get(base + "/")
			if err == nil {
				response.Body.Close()
				if response.StatusCode == http.StatusOK {
					return
				}
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatal("container dashboard did not become ready")
	}
	waitReady()
	password := strings.TrimSpace(string(mustDocker("read administrator password", "exec", name, "/gateway", "admin", "password")))
	if len(password) < 32 {
		t.Fatal("administrator password is too short")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/login", strings.NewReader(url.Values{"password": {password}}.Encode()))
	if err != nil {
		t.Fatal("cannot create login request")
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Origin", base)
	response, err := client.Do(request)
	if err != nil {
		t.Fatal("container administrator login failed")
	}
	response.Body.Close()
	var session *http.Cookie
	for _, cookie := range response.Cookies() {
		if cookie.Name == "gateway_session" {
			session = cookie
		}
	}
	if response.StatusCode != http.StatusSeeOther || session == nil || !session.HttpOnly || session.SameSite != http.SameSiteStrictMode {
		t.Fatal("login must establish a protected administrator session")
	}
	request, _ = http.NewRequestWithContext(ctx, http.MethodGet, base+"/", nil)
	request.AddCookie(session)
	response, err = client.Do(request)
	if err != nil {
		t.Fatal("authenticated dashboard request failed")
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 64<<10))
	response.Body.Close()
	if err != nil || response.StatusCode != http.StatusOK || !strings.Contains(string(body), "Client API keys") || !strings.Contains(string(body), "Not connected") {
		t.Fatal("dashboard must support setup without OpenAI credentials")
	}
	identity := func() string {
		t.Helper()
		var value struct {
			HostID string `json:"host_id"`
		}
		if err := json.Unmarshal(mustDocker("read host identity", "exec", name, "/gateway", "auth", "identity"), &value); err != nil || value.HostID == "" {
			t.Fatal("container must expose a stable host identity")
		}
		return value.HostID
	}
	before := identity()
	secondArgs := append([]string{"run", "--rm", "--name", secondName}, runtimeArgs...)
	secondArgs = append(secondArgs, image)
	_, err = docker(secondArgs...)
	var exitError *exec.ExitError
	if !errors.As(err, &exitError) || !strings.Contains(string(exitError.Stderr), "another gateway process owns this data directory") {
		t.Fatal("a second server must reject the active data volume promptly")
	}
	mustDocker("restart gateway", "restart", "--time", "5", name)
	waitReady()
	if identity() != before {
		t.Fatal("host identity changed after container restart")
	}
}
