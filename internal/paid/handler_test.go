// Exercise paid routing, credential isolation, uploads, streams and failure paths.
package paid

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestPaidRequestsUseOnlyPlatformCredential(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer platform-secret" || r.Header.Get("OpenAI-Organization") != "" || r.Header.Get("OpenAI-Project") != "" || r.Header.Get("Cookie") != "" || r.Header.Get("X-Api-Key") != "" {
			t.Error("caller headers or credentials reached OpenAI")
		}
		if r.URL.RawQuery != "" {
			t.Error("query string reached OpenAI")
		}
		if r.ContentLength <= 0 || r.GetBody != nil {
			t.Error("paid upload lost its length or became replayable")
		}
		w.Header().Set("X-Request-Id", "paid-request-id")
		switch r.URL.Path {
		case "/embeddings":
			var request map[string]any
			if json.NewDecoder(r.Body).Decode(&request) != nil || request["model"] != "text-embedding-3-small" {
				t.Error("embedding request changed")
			}
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"object":"list","data":[{"object":"embedding","embedding":[0.1,0.2],"index":0}],"model":"text-embedding-3-small"}`)
		case "/audio/transcriptions":
			if err := r.ParseMultipartForm(maxAudioRequestBytes); err != nil {
				t.Error("multipart upload changed")
			}
			file, _, err := r.FormFile("file")
			if err != nil {
				t.Error("transcription file missing")
				return
			}
			defer file.Close()
			data, _ := io.ReadAll(file)
			if string(data) != "fixture audio" || r.FormValue("model") != "gpt-4o-transcribe" {
				t.Error("transcription payload changed")
			}
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"text":"fixture transcript"}`)
		case "/audio/speech":
			var request map[string]any
			if json.NewDecoder(r.Body).Decode(&request) != nil || request["input"] != "Say hello" {
				t.Error("speech request changed")
			}
			w.Header().Set("Content-Type", "audio/mpeg")
			w.Header().Set("Content-Disposition", `attachment; filename="speech.mp3"`)
			io.WriteString(w, "fixture speech")
		default:
			t.Error("wrong paid path")
		}
	}))
	defer upstream.Close()
	handler := NewWithBaseURL("platform-secret", upstream.Client(), upstream.URL)
	call := func(path, contentType string, body io.Reader) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(http.MethodPost, path, body)
		r.Header.Set("Authorization", "Bearer gateway-client-key")
		r.Header.Set("OpenAI-Organization", "caller-org")
		r.Header.Set("OpenAI-Project", "caller-project")
		r.Header.Set("X-Api-Key", "caller-key")
		r.Header.Set("Cookie", "session=caller")
		r.Header.Set("Content-Type", contentType)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != 200 || w.Header().Get("X-Request-Id") != "paid-request-id" {
			t.Fatalf("paid route returned HTTP %d without upstream request ID", w.Code)
		}
		return w
	}
	embeddings := call("/v1/embeddings", "application/json", strings.NewReader(`{"model":"text-embedding-3-small","input":"hello"}`))
	if !strings.Contains(embeddings.Body.String(), `"embedding":[0.1,0.2]`) {
		t.Fatal("embedding result changed")
	}
	var audio bytes.Buffer
	form := multipart.NewWriter(&audio)
	file, _ := form.CreateFormFile("file", "voice.wav")
	io.WriteString(file, "fixture audio")
	form.WriteField("model", "gpt-4o-transcribe")
	form.Close()
	transcription := call("/v1/audio/transcriptions", form.FormDataContentType(), &audio)
	if transcription.Body.String() != `{"text":"fixture transcript"}` {
		t.Fatal("transcription result changed")
	}
	speech := call("/v1/audio/speech", "application/json", strings.NewReader(`{"model":"gpt-4o-mini-tts","voice":"alloy","input":"Say hello"}`))
	if speech.Body.String() != "fixture speech" || speech.Header().Get("Content-Type") != "audio/mpeg" || speech.Header().Get("Content-Disposition") == "" {
		t.Fatal("speech bytes or media headers changed")
	}
	if calls.Load() != 3 {
		t.Fatal("unexpected upstream call count")
	}
}

func TestPaidRejectionsDoNotReachUpstream(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	defer upstream.Close()
	handler := NewWithBaseURL("platform-secret", upstream.Client(), upstream.URL)
	for _, tc := range []struct {
		method, path, contentType, body string
		status                          int
	}{
		{http.MethodGet, "/v1/embeddings", "", "", 405},
		{http.MethodPost, "/v1/embeddings?api_key=leak", "application/json", `{}`, 400},
		{http.MethodPost, "/v1/embeddings", "text/plain", `{}`, 415},
		{http.MethodPost, "/v1/embeddings", "application/json", `[1,2]`, 400},
		{http.MethodPost, "/v1/audio/transcriptions", "multipart/form-data; boundary=bad", "garbage", 400},
		{http.MethodPost, "/v1/audio/speech", "application/json", strings.Repeat("x", maxJSONBytes+1), 413},
		{http.MethodPost, "/v1/unknown", "application/json", `{}`, 404},
	} {
		r := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
		r.Header.Set("Content-Type", tc.contentType)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != tc.status {
			t.Errorf("%s returned HTTP %d, want %d", tc.path, w.Code, tc.status)
		}
	}
	missing := NewWithBaseURL("", upstream.Client(), upstream.URL)
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/embeddings", strings.NewReader(`{}`))
	r.Header.Set("Content-Type", "application/json")
	missing.ServeHTTP(w, r)
	if w.Code != 503 || !strings.Contains(w.Body.String(), "openai_paid_unavailable") || calls.Load() != 0 {
		t.Fatal("missing paid key was not rejected locally")
	}
}

func TestPaidUpstreamErrorAndRedirectAreNotRetried(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("X-Request-Id", "paid-error-id")
		if r.URL.Path == "/embeddings" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(429)
			io.WriteString(w, `{"error":{"code":"rate_limit_exceeded"}}`)
			return
		}
		w.Header().Set("Location", "/leak")
		w.WriteHeader(307)
	}))
	defer upstream.Close()
	handler := NewWithBaseURL("platform-secret", upstream.Client(), upstream.URL)
	for _, tc := range []struct {
		path   string
		status int
	}{{"/v1/embeddings", 429}, {"/v1/audio/speech", 307}} {
		r := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(`{}`))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != tc.status || w.Header().Get("X-Request-Id") != "paid-error-id" {
			t.Fatal("upstream status or request ID was lost")
		}
	}
	if calls.Load() != 2 {
		t.Fatal("paid POST was retried or redirected")
	}
}

func TestSpeechFlushesAndCancelsUpstream(t *testing.T) {
	closed := make(chan struct{}, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/mpeg")
		w.Write([]byte("first audio"))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		closed <- struct{}{}
	}))
	defer upstream.Close()
	handler := NewWithBaseURL("platform-secret", upstream.Client(), upstream.URL)
	proxy := httptest.NewServer(handler)
	defer proxy.Close()
	ctx, cancel := context.WithCancel(context.Background())
	r, _ := http.NewRequestWithContext(ctx, http.MethodPost, proxy.URL+"/v1/audio/speech", strings.NewReader(`{"input":"hello"}`))
	r.Header.Set("Content-Type", "application/json")
	resp, err := proxy.Client().Do(r)
	if err != nil {
		t.Fatal("first audio was not flushed")
	}
	buf := make([]byte, len("first audio"))
	if _, err := io.ReadFull(resp.Body, buf); err != nil || string(buf) != "first audio" {
		t.Fatal("first audio was not delivered before completion")
	}
	cancel()
	resp.Body.Close()
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("client cancellation did not reach OpenAI")
	}
}

func TestStreamBoundedStopsAtLimit(t *testing.T) {
	w := httptest.NewRecorder()
	if err := streamBounded(w, strings.NewReader("123456789"), 8); err == nil || w.Body.Len() > 8 {
		t.Fatal("audio response exceeded limit")
	}
}

func TestInterruptedSpeechDoesNotComplete(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/mpeg")
		w.Header().Set("Content-Length", "20")
		w.Write([]byte("short"))
	}))
	defer upstream.Close()
	proxy := httptest.NewServer(NewWithBaseURL("platform-secret", upstream.Client(), upstream.URL))
	defer proxy.Close()
	r, _ := http.NewRequest(http.MethodPost, proxy.URL+"/v1/audio/speech", strings.NewReader(`{}`))
	r.Header.Set("Content-Type", "application/json")
	resp, err := proxy.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if _, err := io.ReadAll(resp.Body); err == nil {
		t.Fatal("interrupted speech looked complete to the client")
	}
}

func TestTranscriptionEventStreamFlushesAndCancels(t *testing.T) {
	closed := make(chan struct{}, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.FormValue("stream") != "true" {
			t.Error("transcription stream option was lost")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "event: transcript.text.delta\ndata: {\"delta\":\"hello\"}\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		closed <- struct{}{}
	}))
	defer upstream.Close()
	proxy := httptest.NewServer(NewWithBaseURL("platform-secret", upstream.Client(), upstream.URL))
	defer proxy.Close()
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	file, _ := form.CreateFormFile("file", "voice.wav")
	io.WriteString(file, "audio")
	form.WriteField("model", "gpt-4o-transcribe")
	form.WriteField("stream", "true")
	form.Close()
	ctx, cancel := context.WithCancel(context.Background())
	r, _ := http.NewRequestWithContext(ctx, http.MethodPost, proxy.URL+"/v1/audio/transcriptions", &body)
	r.Header.Set("Content-Type", form.FormDataContentType())
	resp, err := proxy.Client().Do(r)
	if err != nil {
		t.Fatal("first transcript event was not flushed")
	}
	buf := make([]byte, len("event: transcript.text.delta\n"))
	if _, err := io.ReadFull(resp.Body, buf); err != nil || string(buf) != "event: transcript.text.delta\n" {
		t.Fatal("transcription event was not delivered before completion")
	}
	cancel()
	resp.Body.Close()
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("transcription cancellation did not reach OpenAI")
	}
}
