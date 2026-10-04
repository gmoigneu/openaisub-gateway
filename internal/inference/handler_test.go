// HTTP fixtures verify the gateway contract, not live subscription eligibility.
package inference

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type tokenFunc func(context.Context) (string, error)

func (f tokenFunc) AccessToken(ctx context.Context) (string, error) { return f(ctx) }

func fixture(t *testing.T, upstream http.HandlerFunc) *Handler {
	t.Helper()
	server := httptest.NewServer(upstream)
	t.Cleanup(server.Close)
	return NewWithBaseURL(tokenFunc(func(context.Context) (string, error) { return "openai-secret", nil }), server.Client(), server.URL)
}

func call(h http.Handler, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer gateway-client-secret")
	r.Header.Set("OpenAI-Organization", "other-account")
	r.Header.Set("Cookie", "private=cookie")
	r.Header.Set("X-Forwarded-Host", "attacker.invalid")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func emit(w http.ResponseWriter, typ string, response map[string]any) {
	w.Header().Set("Content-Type", "text/event-stream")
	data, _ := json.Marshal(map[string]any{"type": typ, "response": response})
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", typ, data)
}

func TestNormalizeAndBufferedCompletion(t *testing.T) {
	h := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" || r.Header.Get("Authorization") != "Bearer openai-secret" {
			t.Errorf("upstream routing/auth: %s", r.URL.Path)
		}
		for _, key := range []string{"OpenAI-Organization", "Cookie", "X-Forwarded-Host"} {
			if r.Header.Get(key) != "" {
				t.Errorf("forwarded caller header %s", key)
			}
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body["store"] != false || body["stream"] != true {
			t.Errorf("missing subscription flags: %v", body)
		}
		input := body["input"].([]any)
		if input[0].(map[string]any)["content"] != "hello" {
			t.Error("scalar input lost")
		}
		if body["text"].(map[string]any)["format"].(map[string]any)["type"] != "json_schema" {
			t.Error("structured output lost")
		}
		w.Header().Set("X-Request-ID", "request-1")
		w.Header().Set("Set-Cookie", "upstream-secret")
		emit(w, "response.completed", map[string]any{"id": "response-1", "object": "response", "status": "completed", "output": []any{}, "usage": map[string]any{"total_tokens": 7}})
	})
	w := call(h, `{"model":"dynamic-model","input":"hello","text":{"format":{"type":"json_schema","name":"answer","schema":{"type":"object"}}}}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"status":"completed"`) || !strings.Contains(w.Body.String(), `"total_tokens":7`) {
		t.Fatalf("response %d %s", w.Code, w.Body)
	}
	if w.Header().Get("X-Request-ID") != "request-1" || w.Header().Get("Set-Cookie") != "" {
		t.Fatal("unsafe response headers")
	}
}

func TestFunctionRoundTrip(t *testing.T) {
	var count atomic.Int32
	h := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		namespace := body["tools"].([]any)[0].(map[string]any)
		if namespace["type"] != "namespace" || namespace["name"] != toolNamespace || namespace["tools"].([]any)[0].(map[string]any)["name"] != "weather" {
			t.Error("function namespace incorrect")
		}
		if count.Add(1) == 1 {
			emit(w, "response.completed", map[string]any{"status": "completed", "tools": body["tools"], "output": []any{map[string]any{"type": "function_call", "namespace": toolNamespace, "name": "weather", "call_id": "call-7", "arguments": `{"city":"Paris"}`}}})
			return
		}
		items := body["input"].([]any)
		toolCall := items[1].(map[string]any)
		result := items[2].(map[string]any)
		if toolCall["namespace"] != toolNamespace || toolCall["name"] != "weather" || toolCall["call_id"] != "call-7" || result["call_id"] != "call-7" || result["output"] != "sunny" {
			t.Error("tool history changed")
		}
		emit(w, "response.completed", map[string]any{"status": "completed", "output": []any{}})
	})
	tools := `[{"type":"function","name":"weather","parameters":{"type":"object"}}]`
	w := call(h, `{"model":"m","input":"weather","tools":`+tools+`}`)
	if w.Code != 200 || strings.Contains(w.Body.String(), `"namespace"`) {
		t.Fatalf("namespace leaked: %s", w.Body)
	}
	var response map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &response)
	items := []any{map[string]any{"role": "user", "content": "weather"}, response["output"].([]any)[0], map[string]any{"type": "function_call_output", "call_id": "call-7", "output": "sunny"}}
	input, _ := json.Marshal(items)
	w = call(h, `{"model":"m","input":`+string(input)+`,"tools":`+tools+`}`)
	if w.Code != 200 || count.Load() != 2 {
		t.Fatalf("tool followup %d %s", w.Code, w.Body)
	}
}

func TestRejectedOptionsNeverReachUpstream(t *testing.T) {
	h := fixture(t, func(http.ResponseWriter, *http.Request) { t.Error("invalid request reached upstream") })
	for _, fragment := range []string{`"store":true`, `"stream":"yes"`, `"temperature":0.2`, `"max_output_tokens":10`, `"previous_response_id":"r"`, `"background":false`, `"metadata":{}`, `"tools":[{"type":"web_search"}]`, `"tools":[{"type":"namespace","name":"custom","tools":[]}]`, `"tools":[{"type":"function","name":"f","defer_loading":true}]`} {
		t.Run(fragment, func(t *testing.T) {
			w := call(h, `{"model":"m","input":"x",`+fragment+`}`)
			if w.Code != 400 {
				t.Fatalf("%d %s", w.Code, w.Body)
			}
		})
	}
	for _, body := range []string{`null`, `[]`, `{} {}`, `{"model":"m","input":[{"role":"system","content":"x"}]}`, `{"model":"m","input":[{"type":"item_reference","id":"r"}]}`, `{"model":"m","input":null}`} {
		if w := call(h, body); w.Code != 400 {
			t.Fatalf("%s: %d", body, w.Code)
		}
	}
	w := call(h, strings.Repeat(" ", maxRequestBytes+1))
	if w.Code != 413 {
		t.Fatalf("oversize: %d", w.Code)
	}
}

func TestUpstreamErrorsAndRedirectsAreNotRetried(t *testing.T) {
	for _, status := range []int{307, 401, 429, 503} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var calls atomic.Int32
			h := fixture(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Location", "/responses")
				w.Header().Set("Retry-After", "60")
				w.Header().Set("Content-Type", "text/plain")
				w.WriteHeader(status)
				fmt.Fprint(w, "unusual upstream envelope")
			})
			w := call(h, `{"model":"m","input":"x"}`)
			if w.Code != status || w.Body.String() != "unusual upstream envelope" || w.Header().Get("Retry-After") != "60" || calls.Load() != 1 {
				t.Fatalf("%d %s, requests=%d", w.Code, w.Body, calls.Load())
			}
		})
	}
}

func TestTerminalStatusesAndInterruptedStreams(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, typ := range []string{"response.completed", "response.failed", "response.incomplete", "truncated", "partial", "malformed"} {
			t.Run(fmt.Sprintf("%v/%s", stream, typ), func(t *testing.T) {
				h := fixture(t, func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "text/event-stream")
					switch typ {
					case "truncated":
						fmt.Fprint(w, "data: {\"type\":\"response.created\"}\n\n")
					case "partial":
						fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}")
					case "malformed":
						fmt.Fprint(w, "data: garbage\n\n")
					default:
						emit(w, typ, map[string]any{"status": strings.TrimPrefix(typ, "response."), "error": map[string]any{"code": "subscription_sharing_usage_limit_exceeded"}})
					}
				})
				w := call(h, fmt.Sprintf(`{"model":"m","input":"x","stream":%v}`, stream))
				bad := typ == "truncated" || typ == "partial" || typ == "malformed"
				if bad {
					if !strings.Contains(w.Body.String(), "upstream_stream_interrupted") || (!stream && w.Code != 502) {
						t.Fatalf("false success: %d %s", w.Code, w.Body)
					}
				} else if w.Code != 200 || !strings.Contains(w.Body.String(), `"status":"`+strings.TrimPrefix(typ, "response.")+`"`) {
					t.Fatalf("terminal status lost: %d %s", w.Code, w.Body)
				}
			})
		}
	}
}

func TestSSEMultilineAndOutputToolConversion(t *testing.T) {
	h := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, ": heartbeat\r\nevent: response.output_item.added\r\nid: 7\r\ndata: {\"type\":\"response.output_item.added\",\r\ndata: \"item\":{\"type\":\"function_call\",\"namespace\":\"gateway\",\"name\":\"f\",\"call_id\":\"c\"}}\r\n\r\n")
		emit(w, "response.completed", map[string]any{"status": "completed", "output": []any{}})
	})
	w := call(h, `{"model":"m","input":"x","stream":true}`)
	if w.Code != 200 || strings.Contains(w.Body.String(), `"namespace"`) || !strings.Contains(w.Body.String(), "id: 7\n") || !strings.Contains(w.Body.String(), `"call_id":"c"`) || !w.Flushed {
		t.Fatalf("invalid SSE: %d %s", w.Code, w.Body)
	}
}

func TestStreamingFlushAndCancellation(t *testing.T) {
	cancelled := make(chan struct{})
	release := make(chan struct{})
	h := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		defer close(cancelled)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"response.created\"}\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-release:
		}
	})
	server := httptest.NewServer(h)
	defer server.Close()
	defer close(release)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "POST", server.URL+"/v1/responses", strings.NewReader(`{"model":"m","input":"x","stream":true}`))
	client := &http.Client{Timeout: time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	buffer := make([]byte, 256)
	n, err := resp.Body.Read(buffer)
	if err != nil || !strings.Contains(string(buffer[:n]), "response.created") {
		t.Fatalf("event did not flush: %q %v", buffer[:n], err)
	}
	cancel()
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("upstream did not receive cancellation")
	}
}

func TestDynamicModelsCacheUsesTokenIdentity(t *testing.T) {
	var token atomic.Value
	token.Store("first")
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/models" {
			t.Error(r.URL.Path)
		}
		fmt.Fprintf(w, `{"models":[{"slug":%q,"visibility":"list"},{"slug":"hidden","visibility":"hide"}]}`, r.Header.Get("Authorization"))
	}))
	defer upstream.Close()
	h := NewWithBaseURL(tokenFunc(func(context.Context) (string, error) { return token.Load().(string), nil }), upstream.Client(), upstream.URL)
	for _, identity := range []string{"first", "first", "second"} {
		token.Store(identity)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", "/v1/models", nil))
		if w.Code != 200 || !strings.Contains(w.Body.String(), `"id":"Bearer `+identity+`"`) || strings.Contains(w.Body.String(), "hidden") {
			t.Fatalf("catalog: %d %s", w.Code, w.Body)
		}
	}
	if calls.Load() != 2 {
		t.Fatalf("catalog requests: %d", calls.Load())
	}
}

func TestStreamMemoryLimits(t *testing.T) {
	large := strings.Repeat("x", maxEventBytes)
	if err := readEvents(strings.NewReader("data: "+large+"\n\n"), true, func(string, []byte, []string) (bool, error) { t.Fatal("oversized event delivered"); return false, nil }); err == nil {
		t.Fatal("unbounded event")
	}
	// Small valid events exceed the buffered total without allocating a large source string.
	line := "data: {\"type\":\"response.output_text.delta\",\"delta\":\"" + strings.Repeat("x", 1024) + "\"}\n\n"
	reader := io.LimitReader(&repeatReader{value: []byte(line)}, maxResponseBytes+10000)
	if err := readEvents(reader, false, func(string, []byte, []string) (bool, error) { return false, nil }); err == nil || err == io.ErrUnexpectedEOF {
		t.Fatalf("buffer total not bounded: %v", err)
	}
}

func TestAuthenticationFailureDoesNotExposeSourceError(t *testing.T) {
	h := New(tokenFunc(func(context.Context) (string, error) {
		return "", errors.New("failed to renew private-refresh-token")
	}), nil)
	w := call(h, `{"model":"m","input":"x"}`)
	if w.Code != 503 || strings.Contains(w.Body.String(), "private-refresh-token") {
		t.Fatalf("unsafe authentication error: %d %s", w.Code, w.Body)
	}
}

func TestMalformedSuccessNeverBecomesCompletion(t *testing.T) {
	for _, content := range []string{
		`data: {"type":"response.completed","response":{"status":"failed"}}` + "\n\n",
		`data: {"type":"response.completed","response":{"status":"completed"}} garbage` + "\n\n",
		`data: {"type":"response.completed"}` + "\n\n",
	} {
		h := fixture(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, content)
		})
		w := call(h, `{"model":"m","input":"x"}`)
		if w.Code != 502 {
			t.Fatalf("accepted malformed terminal: %d %s", w.Code, w.Body)
		}
	}
}

type repeatReader struct {
	value  []byte
	offset int
}

func (r *repeatReader) Read(p []byte) (int, error) {
	n := copy(p, r.value[r.offset:])
	r.offset = (r.offset + n) % len(r.value)
	return n, nil
}
