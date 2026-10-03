// Check the shipped Compose files and optional host access without using OpenAI.
package integration

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestContainerComposeHostPort(t *testing.T) {
	if os.Getenv("DOCKER_CONTRACT") != "1" {
		t.Skip("set DOCKER_CONTRACT=1 after building openaisub-gateway:local")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	dir := t.TempDir()
	for _, file := range []string{"compose.yaml", "compose.host.yaml"} {
		data, err := os.ReadFile(filepath.Join("..", file))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, file), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	// Only the test administrator port differs, to avoid fixed-port collisions.
	if err := os.WriteFile(filepath.Join(dir, "test.yaml"), []byte("services:\n  gateway:\n    ports: !override\n      - '127.0.0.1::8081'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	project := "gateway-host-" + strings.ToLower(rand.Text())
	compose := func(host bool, port string, args ...string) []byte {
		t.Helper()
		flags := []string{"compose", "-p", project, "-f", filepath.Join(dir, "compose.yaml")}
		// Inspect the shipped configuration unchanged; isolate ports only at runtime.
		if args[0] != "config" {
			flags = append(flags, "-f", filepath.Join(dir, "test.yaml"))
		}
		if host {
			flags = append(flags, "-f", filepath.Join(dir, "compose.host.yaml"))
		}
		cmd := exec.CommandContext(ctx, "docker", append(flags, args...)...)
		cmd.Env = append(os.Environ(), "GATEWAY_HOST_PORT="+port)
		output, err := cmd.Output()
		if err != nil {
			t.Fatalf("Compose command failed: %T", err)
		}
		return output
	}
	t.Cleanup(func() {
		cleanupCtx, stop := context.WithTimeout(context.Background(), 30*time.Second)
		defer stop()
		if err := exec.CommandContext(cleanupCtx, "docker", "compose", "-p", project, "-f", filepath.Join(dir, "compose.yaml"), "down", "--volumes", "--timeout", "5").Run(); err != nil {
			t.Error("host-port test cleanup failed")
		}
	})
	for _, tc := range []struct {
		name, port, want string
		host             bool
	}{{name: "private default"}, {name: "host default", host: true, want: "8080"}, {name: "custom host port", host: true, port: "18080", want: "18080"}} {
		t.Run(tc.name, func(t *testing.T) {
			var config struct {
				Services map[string]struct {
					Ports []struct {
						Target    int
						Published string
						HostIP    string `json:"host_ip"`
					}
				}
			}
			if err := json.Unmarshal(compose(tc.host, tc.port, "config", "--format", "json"), &config); err != nil {
				t.Fatal(err)
			}
			wantPorts := 1
			if tc.host {
				wantPorts++
			}
			if len(config.Services["gateway"].Ports) != wantPorts {
				t.Fatal("unexpected published ports")
			}
			found := false
			for _, binding := range config.Services["gateway"].Ports {
				if binding.HostIP != "127.0.0.1" {
					t.Fatal("published ports must bind to loopback")
				}
				if binding.Target == 8080 {
					found = true
					if binding.Published != tc.want {
						t.Fatal("unexpected inference host port")
					}
				} else if binding.Target != 8081 || binding.Published != "8081" {
					t.Fatal("administrator port must remain unchanged")
				}
			}
			if found != tc.host {
				t.Fatal("inference publication must require the override")
			}
		})
	}
	secrets := filepath.Join(dir, "secrets")
	if err := os.Mkdir(secrets, 0700); err != nil {
		t.Fatal(err)
	}
	if err := exec.CommandContext(ctx, "docker", "run", "--rm", "--user", "0", "--volume", secrets+":/bootstrap", "openaisub-gateway:local", "init", "--secrets-dir", "/bootstrap", "--owner", "10001").Run(); err != nil {
		t.Fatal("host-port test secret initialization failed")
	}
	compose(true, "0", "up", "-d", "--no-build")
	address := strings.TrimSpace(string(compose(true, "0", "port", "gateway", "8080")))
	if !strings.HasPrefix(address, "127.0.0.1:") {
		t.Fatal("inference must be published only on loopback")
	}
	client := &http.Client{Timeout: time.Second}
	waitReady := func(url string) {
		t.Helper()
		deadline := time.Now().Add(15 * time.Second)
		for {
			response, err := client.Get(url)
			if err == nil {
				response.Body.Close()
				if response.StatusCode == http.StatusOK {
					return
				}
			}
			if time.Now().After(deadline) {
				t.Fatal("published listener did not become ready")
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	waitReady("http://" + address + "/healthz")
	identity := string(compose(true, "0", "exec", "-T", "gateway", "/gateway", "auth", "identity"))
	response, err := client.Get("http://" + address + "/v1/models")
	if err != nil {
		t.Fatal("cannot reach the published models endpoint")
	}
	response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatal("host access must still require a client key")
	}
	compose(false, "", "up", "-d", "--no-build")
	adminAddress := strings.TrimSpace(string(compose(false, "", "port", "gateway", "8081")))
	waitReady("http://" + adminAddress + "/")
	if string(compose(false, "", "exec", "-T", "gateway", "/gateway", "auth", "identity")) != identity {
		t.Fatal("disabling host access must preserve gateway identity")
	}
	id := strings.TrimSpace(string(compose(false, "", "ps", "-q", "gateway")))
	output, err := exec.CommandContext(ctx, "docker", "inspect", "--format", "{{json .HostConfig.PortBindings}}", id).Output()
	if err != nil {
		t.Fatal("cannot inspect the private gateway")
	}
	var bindings map[string]json.RawMessage
	if err := json.Unmarshal(output, &bindings); err != nil {
		t.Fatal(err)
	}
	if _, found := bindings["8080/tcp"]; found {
		t.Fatal("removing the override must remove the host inference binding")
	}
}
