// Package inference adapts subscription Responses streams without retaining content.
package inference

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	maxRequestBytes  = 8 << 20
	maxResponseBytes = 32 << 20
	maxEventBytes    = 16 << 20
	toolNamespace    = "gateway"
)

type TokenSource interface {
	AccessToken(context.Context) (string, error)
}

// Handler accepts already authenticated gateway clients. It never forwards their credentials.
type Handler struct {
	source          TokenSource
	client          *http.Client
	baseURL         string
	mu              sync.Mutex
	catalog         []byte
	catalogIdentity [32]byte
	catalogUntil    time.Time
}

func New(source TokenSource, client *http.Client) *Handler {
	return NewWithBaseURL(source, client, "https://api.openai.com/v1")
}

// NewWithBaseURL permits controlled upstream fixtures. Production uses New.
func NewWithBaseURL(source TokenSource, client *http.Client, baseURL string) *Handler {
	if client == nil {
		client = &http.Client{Transport: &http.Transport{
			Proxy:               http.ProxyFromEnvironment,
			DialContext:         (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 30 * time.Second,
			MaxResponseHeaderBytes: 64 << 10, IdleConnTimeout: 90 * time.Second,
			ForceAttemptHTTP2: true,
		}}
	}
	// Redirects could move the bearer token or replay an inference request.
	copyClient := *client
	copyClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Handler{source: source, client: &copyClient, baseURL: strings.TrimRight(baseURL, "/")}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.URL.RawQuery != "" {
		apiError(w, 400, "unsupported_parameter", "Query parameters are not supported.", "")
		return
	}
	switch r.URL.Path {
	case "/v1/models":
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			apiError(w, 405, "method_not_allowed", "Use GET.", "")
			return
		}
		h.models(w, r)
	case "/v1/responses":
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			apiError(w, 405, "method_not_allowed", "Use POST.", "")
			return
		}
		h.responses(w, r)
	default:
		apiError(w, 404, "not_found", "Only /v1/models and /v1/responses are supported.", "")
	}
}

func (h *Handler) token(w http.ResponseWriter, r *http.Request) (string, bool) {
	token, err := h.source.AccessToken(r.Context())
	if err != nil || token == "" {
		apiError(w, 503, "openai_unavailable", "OpenAI authentication is unavailable. Check the gateway dashboard.", "")
		return "", false
	}
	return token, true
}

func (h *Handler) upstream(ctx context.Context, method, path, token string, body []byte) (*http.Response, error) {
	// A reader without GetBody also prevents transport-level POST replays.
	req, err := http.NewRequestWithContext(ctx, method, h.baseURL+path, struct{ io.Reader }{bytes.NewReader(body)})
	if err != nil {
		return nil, err
	}
	if method == http.MethodGet {
		req.Body = nil
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "text/event-stream")
	}
	return h.client.Do(req)
}

func (h *Handler) models(w http.ResponseWriter, r *http.Request) {
	token, ok := h.token(w, r)
	if !ok {
		return
	}
	identity := sha256.Sum256([]byte(token))
	h.mu.Lock()
	if identity == h.catalogIdentity && time.Now().Before(h.catalogUntil) {
		body := h.catalog
		h.mu.Unlock()
		writeJSON(w, 200, body)
		return
	}
	h.mu.Unlock()
	resp, err := h.upstream(r.Context(), "GET", "/models", token, nil)
	if err != nil {
		apiError(w, 502, "upstream_unavailable", "Could not reach OpenAI.", "")
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		forwardError(w, resp)
		return
	}
	body, err := readBounded(resp.Body, maxResponseBytes)
	var catalog struct {
		Models *[]struct {
			Slug       string `json:"slug"`
			Visibility string `json:"visibility"`
		} `json:"models"`
	}
	if err != nil || json.Unmarshal(body, &catalog) != nil || catalog.Models == nil {
		apiError(w, 502, "invalid_upstream_response", "OpenAI returned an invalid model catalog.", "")
		return
	}
	data := make([]map[string]any, 0, len(*catalog.Models))
	for _, model := range *catalog.Models {
		if model.Visibility == "list" && model.Slug != "" {
			data = append(data, map[string]any{"id": model.Slug, "object": "model", "created": 0, "owned_by": "openai"})
		}
	}
	body, _ = json.Marshal(map[string]any{"object": "list", "data": data})
	h.mu.Lock()
	h.catalog = body
	h.catalogIdentity = identity
	h.catalogUntil = time.Now().Add(time.Minute)
	h.mu.Unlock()
	copyHeaders(w.Header(), resp.Header)
	writeJSON(w, 200, body)
}

func (h *Handler) responses(w http.ResponseWriter, r *http.Request) {
	// Bound slow request uploads without imposing a lifetime on generated streams.
	controller := http.NewResponseController(w)
	_ = controller.SetReadDeadline(time.Now().Add(30 * time.Second))
	body, err := readBounded(r.Body, maxRequestBytes)
	_ = controller.SetReadDeadline(time.Time{})
	if err != nil {
		apiError(w, 413, "request_too_large", "Request exceeds the 8 MiB limit.", "")
		return
	}
	request, stream, err := normalize(body)
	if err != nil {
		var invalid *requestError
		if errors.As(err, &invalid) {
			apiError(w, 400, "unsupported_parameter", invalid.message, invalid.param)
		} else {
			apiError(w, 400, "invalid_request_error", "Expected one JSON object.", "")
		}
		return
	}
	token, ok := h.token(w, r)
	if !ok {
		return
	}
	resp, err := h.upstream(r.Context(), "POST", "/responses", token, request)
	if err != nil {
		apiError(w, 502, "upstream_unavailable", "Could not reach OpenAI. The request was not retried.", "")
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		forwardError(w, resp)
		return
	}
	copyHeaders(w.Header(), resp.Header)
	var bodyReader io.Reader = resp.Body
	contentType := strings.TrimSpace(resp.Header.Get("Content-Type"))
	if contentType == "" {
		bodyReader, err = validatedEventStream(resp.Body)
	} else if !strings.HasPrefix(strings.ToLower(contentType), "text/event-stream") {
		err = errors.New("unexpected content type")
	}
	if err != nil {
		apiError(w, 502, "invalid_upstream_response", "OpenAI did not return an event stream.", "")
		return
	}
	h.consume(w, r, bodyReader, stream)
}

func copyHeaders(dst, src http.Header) {
	for name, values := range src {
		lower := strings.ToLower(name)
		if lower == "x-request-id" || lower == "retry-after" || strings.HasPrefix(lower, "x-ratelimit-") {
			dst[name] = append([]string(nil), values...)
		}
	}
}

func forwardError(w http.ResponseWriter, resp *http.Response) {
	body, err := readBounded(resp.Body, maxResponseBytes)
	if err != nil {
		apiError(w, 502, "invalid_upstream_response", "OpenAI error exceeds the response limit.", "")
		return
	}
	copyHeaders(w.Header(), resp.Header)
	w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(body)
}

func readBounded(r io.Reader, limit int64) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err == nil && int64(len(body)) > limit {
		err = errors.New("body limit exceeded")
	}
	return body, err
}

func apiError(w http.ResponseWriter, status int, code, message, param string) {
	typ := "invalid_request_error"
	if status >= 500 {
		typ = "server_error"
	}
	body, _ := json.Marshal(map[string]any{"error": map[string]any{"type": typ, "code": code, "message": message, "param": param}})
	writeJSON(w, status, body)
}

func writeJSON(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}
