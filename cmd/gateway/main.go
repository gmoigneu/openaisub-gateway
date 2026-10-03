// Gateway runs private inference and owner administration, or local sign-in setup.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/gmoigneu/openaisub-gateway/internal/admin"
	"github.com/gmoigneu/openaisub-gateway/internal/inference"
	"github.com/gmoigneu/openaisub-gateway/internal/openaiauth"
	"github.com/gmoigneu/openaisub-gateway/internal/store"
	"golang.org/x/sys/unix"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "gateway:", err)
		os.Exit(1)
	}
}
func env(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}
func run(args []string) error {
	if len(args) == 0 || args[0] == "serve" {
		return serve()
	}
	switch args[0] {
	case "init":
		return initialize(args[1:])
	case "auth":
		return authCommand(args[1:])
	case "admin":
		if len(args) == 2 && args[1] == "password" {
			v, err := adminSecret()
			if err != nil {
				return err
			}
			fmt.Println(v)
			return nil
		}
	}
	return errors.New("usage: gateway serve | init | auth login | auth identity | auth import | admin password")
}
func readSecret(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read secret file %s: %w", path, err)
	}
	return strings.TrimSpace(string(b)), nil
}
func adminSecret() (string, error) {
	v, err := readSecret(env("GATEWAY_ADMIN_SECRET_FILE", "/run/secrets/admin_secret"))
	if err != nil {
		return "", err
	}
	if len(v) < 32 {
		return "", errors.New("administrator secret must be at least 32 characters")
	}
	return v, nil
}
func initialize(args []string) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	dir := fs.String("secrets-dir", "./secrets", "secret directory")
	owner := fs.Int("owner", -1, "numeric owner for new secret files")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("unexpected init argument")
	}
	if *owner < -1 {
		return errors.New("owner must be a nonnegative UID")
	}
	if *owner >= 0 && os.Geteuid() != 0 {
		return errors.New("--owner requires root")
	}
	if err := os.MkdirAll(*dir, 0700); err != nil {
		return err
	}
	for _, name := range []string{"admin_secret", "encryption_key"} {
		path := filepath.Join(*dir, name)
		b := make([]byte, 32)
		if _, err := rand.Read(b); err != nil {
			return err
		}
		value := base64.RawURLEncoding.EncodeToString(b) + "\n"
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return err
		}
		_, writeErr := f.WriteString(value)
		if writeErr == nil && *owner >= 0 {
			writeErr = f.Chown(*owner, *owner)
		}
		closeErr := f.Close()
		if writeErr != nil {
			return writeErr
		}
		if closeErr != nil {
			return closeErr
		}
	}
	fmt.Println("Secret files are ready. Existing secrets were preserved.")
	return nil
}
func serve() error {
	secret, err := adminSecret()
	if err != nil {
		return err
	}
	encoded, err := readSecret(env("GATEWAY_ENCRYPTION_KEY_FILE", "/run/secrets/encryption_key"))
	if err != nil {
		return err
	}
	key, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || len(key) != 32 {
		return errors.New("encryption secret must encode 32 bytes as unpadded base64url")
	}
	dir := env("GATEWAY_DATA_DIR", "/data")
	if err = os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	lock, err := os.OpenFile(filepath.Join(dir, "serve.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err = unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return errors.New("another gateway process owns this data directory")
	}
	defer unix.Flock(int(lock.Fd()), unix.LOCK_UN)
	s, err := store.Open(dir, key)
	if err != nil {
		return err
	}
	defer s.Close()
	if _, err = s.HostID(context.Background()); err != nil {
		return err
	}
	manager := openaiauth.NewManager(s, nil)
	handler := inference.New(manager, nil)
	api := &http.Server{Addr: env("GATEWAY_API_ADDR", ":8080"), Handler: apiHandler(s, handler), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 90 * time.Second, MaxHeaderBytes: 32 << 10, ErrorLog: log.New(io.Discard, "", 0)}
	owner := &http.Server{Addr: env("GATEWAY_ADMIN_ADDR", ":8081"), Handler: admin.New(s, manager, secret), ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10, ErrorLog: log.New(io.Discard, "", 0)}
	apiListener, err := net.Listen("tcp", api.Addr)
	if err != nil {
		return err
	}
	defer apiListener.Close()
	adminListener, err := net.Listen("tcp", owner.Addr)
	if err != nil {
		return err
	}
	defer adminListener.Close()
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	api.BaseContext = func(net.Listener) context.Context { return ctx }
	owner.BaseContext = api.BaseContext
	failures := make(chan error, 2)
	go func() { failures <- api.Serve(apiListener) }()
	go func() { failures <- owner.Serve(adminListener) }()
	fmt.Fprintln(os.Stderr, "Gateway ready. Inference and administration use separate listeners.")
	select {
	case <-ctx.Done():
	case err = <-failures:
		stop()
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_ = api.Shutdown(shutdown)
	_ = owner.Shutdown(shutdown)
	_ = api.Close()
	_ = owner.Close()
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return errors.New("gateway listener failed")
	}
	return nil
}
func apiHandler(s *store.Store, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" && r.Method == http.MethodGet {
			w.WriteHeader(200)
			_, _ = io.WriteString(w, "ok\n")
			return
		}
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "Bearer ") || !s.Authenticate(r.Context(), strings.TrimPrefix(auth, "Bearer ")) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(401)
			_, _ = io.WriteString(w, `{"error":{"message":"Invalid gateway API key","type":"authentication_error","code":"invalid_api_key"}}`)
			return
		}
		next.ServeHTTP(w, r)
	})
}
func authCommand(args []string) error {
	if len(args) == 0 {
		return errors.New("auth requires login, identity or import")
	}
	if args[0] == "login" {
		return loginCommand(args[1:])
	}
	if len(args) != 1 {
		return errors.New("unexpected auth argument")
	}
	switch args[0] {
	case "identity":
		b, err := localAdmin("GET", "/internal/identity", nil)
		if err != nil {
			return err
		}
		_, err = os.Stdout.Write(b)
		return err
	case "import":
		b, err := io.ReadAll(io.LimitReader(os.Stdin, (128<<10)+1))
		if err != nil || len(b) > 128<<10 {
			return errors.New("credential input too large or unreadable")
		}
		_, err = localAdmin("POST", "/internal/import", b)
		return err
	}
	return errors.New("unknown auth command")
}
func localAdmin(method, path string, body []byte) ([]byte, error) {
	secret, err := adminSecret()
	if err != nil {
		return nil, err
	}
	addr := env("GATEWAY_ADMIN_ADDR", ":8081")
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, "http://"+net.JoinHostPort(host, port)+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+secret)
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 25 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return nil, errors.New("cannot reach running gateway administration listener")
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 128<<10))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("administration request failed: HTTP %d", resp.StatusCode)
	}
	return b, nil
}
func loginCommand(args []string) error {
	fs := flag.NewFlagSet("auth login", flag.ContinueOnError)
	sshHost := fs.String("ssh", "", "SSH destination for remote Docker")
	dir := fs.String("directory", ".", "Compose project directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("unexpected login argument")
	}
	if *sshHost != "" && (!filepath.IsAbs(*dir) || strings.HasPrefix(*sshHost, "-") || strings.ContainsAny(*sshHost, "\r\n")) {
		return errors.New("remote login requires an SSH host and an absolute Compose directory")
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	transfer := func(action string, input []byte) ([]byte, error) {
		var cmd *exec.Cmd
		if *sshHost != "" {
			command := "cd " + shellQuote(*dir) + " && docker compose exec -T gateway /gateway auth " + action
			cmd = exec.CommandContext(ctx, "ssh", "--", *sshHost, command)
		} else {
			cmd = exec.CommandContext(ctx, "docker", "compose", "exec", "-T", "gateway", "/gateway", "auth", action)
			cmd.Dir = *dir
		}
		cmd.Stdin = bytes.NewReader(input)
		cmd.Stderr = os.Stderr
		return cmd.Output()
	}
	b, err := transfer("identity", nil)
	if err != nil {
		return errors.New("cannot read gateway identity; check Docker, SSH and the Compose directory")
	}
	var identity struct {
		HostID   string `json:"host_id"`
		ClientID string `json:"client_id"`
		Subject  string `json:"subject"`
	}
	if err = json.Unmarshal(b, &identity); err != nil || identity.HostID == "" {
		return errors.New("invalid gateway identity")
	}
	credentials, err := openaiauth.Login(ctx, openaiauth.LoginOptions{HostID: identity.HostID, ClientID: identity.ClientID, Subject: identity.Subject, OnAuthorization: func(url string) {
		fmt.Fprintln(os.Stderr, "Continue with ChatGPT in your browser:", url)
		var cmd *exec.Cmd
		if runtime.GOOS == "darwin" {
			cmd = exec.Command("open", url)
		} else {
			cmd = exec.Command("xdg-open", url)
		}
		if cmd.Start() == nil {
			go func() { _ = cmd.Wait() }()
		}
	}})
	if err != nil {
		return err
	}
	b, err = json.Marshal(credentials)
	if err != nil {
		return err
	}
	if _, err = transfer("import", b); err != nil {
		return errors.New("could not save login to gateway; run sign-in again")
	}
	fmt.Println("OpenAI connected. The gateway will renew this session.")
	return nil
}
func shellQuote(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'" }
