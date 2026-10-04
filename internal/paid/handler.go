// Package paid forwards selected OpenAI Platform API calls without retaining content.
package paid

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	maxJSONBytes         = 8 << 20
	maxAudioRequestBytes = 26 << 20
	maxResponseBytes     = 64 << 20
	maxErrorBytes        = 1 << 20
	streamBufferBytes    = 32 << 10
)

// Handler accepts gateway-authenticated clients and substitutes the paid API key.
type Handler struct {
	key     string
	client  *http.Client
	baseURL string
}

func New(key string, client *http.Client) *Handler {
	return NewWithBaseURL(key, client, "https://api.openai.com/v1")
}

// NewWithBaseURL permits controlled upstream fixtures. Production uses New.
func NewWithBaseURL(key string, client *http.Client, baseURL string) *Handler {
	if client == nil {
		client = &http.Client{Transport: &http.Transport{
			Proxy:               http.ProxyFromEnvironment,
			DialContext:         (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 30 * time.Second,
			MaxResponseHeaderBytes: 64 << 10, IdleConnTimeout: 90 * time.Second,
			ForceAttemptHTTP2: true,
		}}
	}
	copyClient := *client
	copyClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Handler{key: key, client: &copyClient, baseURL: strings.TrimRight(baseURL, "/")}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	var limit int64
	var multipartBody bool
	switch r.URL.Path {
	case "/v1/embeddings", "/v1/audio/speech":
		limit = maxJSONBytes
	case "/v1/audio/transcriptions":
		limit, multipartBody = maxAudioRequestBytes, true
	default:
		apiError(w, 404, "not_found", "Unsupported paid API route.")
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		apiError(w, 405, "method_not_allowed", "Use POST.")
		return
	}
	if r.URL.RawQuery != "" {
		apiError(w, 400, "unsupported_parameter", "Query parameters are not supported.")
		return
	}
	if h.key == "" {
		apiError(w, 503, "openai_paid_unavailable", "The paid OpenAI API key is not configured.")
		return
	}
	mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || (multipartBody && (mediaType != "multipart/form-data" || params["boundary"] == "")) || (!multipartBody && mediaType != "application/json") {
		apiError(w, 415, "unsupported_media_type", "Use JSON for embeddings and speech, or multipart form data for transcription.")
		return
	}
	if r.ContentLength > limit {
		apiError(w, 413, "request_too_large", "Request exceeds the route size limit.")
		return
	}
	controller := http.NewResponseController(w)
	_ = controller.SetReadDeadline(time.Now().Add(30 * time.Second))
	body, err := readBounded(r.Body, limit)
	_ = controller.SetReadDeadline(time.Time{})
	if err != nil {
		apiError(w, 413, "request_too_large", "Request exceeds the route size limit or could not be read.")
		return
	}
	if multipartBody {
		err = validateMultipart(body, params["boundary"])
	} else {
		var request map[string]any
		err = json.Unmarshal(body, &request)
		if err == nil && request == nil {
			err = errors.New("expected JSON object")
		}
	}
	if err != nil {
		apiError(w, 400, "invalid_request_error", "Request body is malformed.")
		return
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, h.baseURL+r.URL.Path[3:], struct{ io.Reader }{bytes.NewReader(body)})
	if err != nil {
		apiError(w, 502, "upstream_unavailable", "Could not prepare the OpenAI request.")
		return
	}
	// Keep a known length for multipart uploads without giving Transport a replay body.
	req.ContentLength = int64(len(body))
	req.Header.Set("Authorization", "Bearer "+h.key)
	if multipartBody {
		req.Header.Set("Content-Type", r.Header.Get("Content-Type"))
	} else {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := h.client.Do(req)
	if err != nil {
		apiError(w, 502, "upstream_unavailable", "Could not reach OpenAI. The request was not retried.")
		return
	}
	defer resp.Body.Close()
	copySafeHeaders(w.Header(), resp.Header)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		responseBody, err := readBounded(resp.Body, maxErrorBytes)
		if err != nil {
			apiError(w, 502, "invalid_upstream_response", "OpenAI error exceeds the response limit.")
			return
		}
		w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(responseBody)
		return
	}
	if resp.ContentLength > maxResponseBytes {
		apiError(w, 502, "invalid_upstream_response", "OpenAI response exceeds the response limit.")
		return
	}
	contentType := resp.Header.Get("Content-Type")
	if contentType != "" {
		w.Header().Set("Content-Type", contentType)
	}
	if disposition := resp.Header.Get("Content-Disposition"); disposition != "" {
		w.Header().Set("Content-Disposition", disposition)
	}
	stream := r.URL.Path == "/v1/audio/speech" || strings.HasPrefix(contentType, "text/event-stream")
	if !stream {
		responseBody, err := readBounded(resp.Body, maxResponseBytes)
		if err != nil {
			apiError(w, 502, "invalid_upstream_response", "OpenAI response is incomplete or too large.")
			return
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(responseBody)
		return
	}
	if resp.ContentLength >= 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(resp.ContentLength, 10))
	}
	w.WriteHeader(resp.StatusCode)
	if err := streamBounded(w, resp.Body, maxResponseBytes); err != nil {
		// net/http closes the client stream instead of reporting partial audio as complete.
		panic(http.ErrAbortHandler)
	}
}

func validateMultipart(body []byte, boundary string) error {
	reader := multipart.NewReader(bytes.NewReader(body), boundary)
	var file, model bool
	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if part.FormName() == "file" {
			file = true
		}
		if part.FormName() == "model" {
			model = true
		}
		if _, err := io.Copy(io.Discard, part); err != nil {
			return err
		}
	}
	if !file || !model {
		return errors.New("transcription requires file and model")
	}
	return nil
}

func streamBounded(w http.ResponseWriter, source io.Reader, limit int64) error {
	buf := make([]byte, streamBufferBytes)
	flusher, _ := w.(http.Flusher)
	var written int64
	for {
		n, err := source.Read(buf)
		if int64(n)+written > limit {
			return errors.New("response limit exceeded")
		}
		if n > 0 {
			count, writeErr := w.Write(buf[:n])
			written += int64(count)
			if writeErr != nil {
				return writeErr
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func readBounded(r io.Reader, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err == nil && int64(len(data)) > limit {
		err = errors.New("body limit exceeded")
	}
	return data, err
}

func copySafeHeaders(dst, src http.Header) {
	for name, values := range src {
		lower := strings.ToLower(name)
		if lower == "x-request-id" || lower == "retry-after" || strings.HasPrefix(lower, "x-ratelimit-") {
			dst[name] = append([]string(nil), values...)
		}
	}
}

func apiError(w http.ResponseWriter, status int, code, message string) {
	typ := "invalid_request_error"
	if status >= 500 {
		typ = "server_error"
	}
	body, _ := json.Marshal(map[string]any{"error": map[string]any{"type": typ, "code": code, "message": message}})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}
