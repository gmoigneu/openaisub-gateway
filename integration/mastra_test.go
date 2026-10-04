// These fixtures verify actual Mastra serialization; they do not prove live account access.
package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gmoigneu/openaisub-gateway/internal/inference"
)

type fixtureToken struct{}

func (fixtureToken) AccessToken(context.Context) (string, error) {
	return "fixture-upstream-token", nil
}
func TestMastraContract(t *testing.T) {
	if os.Getenv("MASTRA_CONTRACT") != "1" {
		t.Skip("set MASTRA_CONTRACT=1 after npm ci in examples/mastra")
	}
	var calls, errorsSeen, toolsSeen atomic.Int32
	cancelled := make(chan struct{}, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/responses" || r.Header.Get("Authorization") != "Bearer fixture-upstream-token" {
			t.Error("invalid upstream routing or credential")
			http.Error(w, "invalid fixture routing", 400)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		var request map[string]any
		if json.Unmarshal(body, &request) != nil {
			t.Error("invalid JSON")
			return
		}
		if request["stream"] != true || request["store"] != false {
			t.Error("incorrect upstream stream/storage")
		}
		input, _ := json.Marshal(request["input"])
		marker := string(input)
		if strings.Contains(marker, `"role":"system"`) {
			t.Error("system instruction reached upstream")
		}
		if strings.Contains(marker, "CONTRACT_ERROR") {
			errorsSeen.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("x-request-id", "fixture-error")
			w.WriteHeader(429)
			fmt.Fprint(w, `{"error":{"message":"Fixture usage limit","type":"rate_limit_error","code":"subscription_sharing_usage_limit_exceeded"}}`)
			return
		}
		answer := "OK"
		tool := false
		if strings.Contains(marker, "CONTRACT_JSON") {
			text, _ := request["text"].(map[string]any)
			format, _ := text["format"].(map[string]any)
			if format["type"] != "json_schema" {
				t.Error("native JSON schema missing")
			}
			answer = `{"ok":true}`
		}
		if strings.Contains(marker, "CONTRACT_TOOL") {
			if strings.Contains(marker, "function_call_output") {
				if !strings.Contains(marker, "weather-call") || !strings.Contains(marker, "temperature") || !strings.Contains(marker, "21") {
					t.Error("tool results or call identity lost")
				}
				answer = "21"
				toolsSeen.Add(1)
			} else {
				tool = true
				serialized, _ := json.Marshal(request["tools"])
				if !strings.Contains(string(serialized), `"type":"namespace"`) {
					t.Error("tools were not namespaced")
				}
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		emit := func(event map[string]any) {
			data, _ := json.Marshal(event)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event["type"], data)
			w.(http.Flusher).Flush()
		}
		response := map[string]any{"id": "resp_fixture", "object": "response", "created_at": time.Now().Unix(), "status": "in_progress", "model": "fixture-model", "output": []any{}, "error": nil, "incomplete_details": nil, "usage": nil}
		emit(map[string]any{"type": "response.created", "response": response})
		var item map[string]any
		if tool {
			item = map[string]any{"id": "fc_fixture", "type": "function_call", "call_id": "weather-call", "name": "getWeather", "namespace": "gateway", "arguments": "", "status": "in_progress"}
			emit(map[string]any{"type": "response.output_item.added", "output_index": 0, "item": item})
			emit(map[string]any{"type": "response.function_call_arguments.delta", "item_id": "fc_fixture", "output_index": 0, "delta": `{"city":"Paris"}`})
			item["arguments"] = `{"city":"Paris"}`
			item["status"] = "completed"
			emit(map[string]any{"type": "response.function_call_arguments.done", "item_id": "fc_fixture", "output_index": 0, "arguments": item["arguments"]})
		} else {
			part := map[string]any{"type": "output_text", "text": "", "annotations": []any{}}
			item = map[string]any{"id": "msg_fixture", "type": "message", "role": "assistant", "status": "in_progress", "content": []any{}}
			emit(map[string]any{"type": "response.output_item.added", "output_index": 0, "item": item})
			emit(map[string]any{"type": "response.content_part.added", "item_id": "msg_fixture", "output_index": 0, "content_index": 0, "part": part})
			emit(map[string]any{"type": "response.output_text.delta", "item_id": "msg_fixture", "output_index": 0, "content_index": 0, "delta": answer, "logprobs": []any{}})
			if strings.Contains(marker, "CONTRACT_CANCEL") {
				select {
				case <-r.Context().Done():
					select {
					case cancelled <- struct{}{}:
					default:
					}
				case <-time.After(10 * time.Second):
					t.Error("cancellation did not reach upstream")
				}
				return
			}
			part["text"] = answer
			item["content"] = []any{part}
			item["status"] = "completed"
			emit(map[string]any{"type": "response.output_text.done", "item_id": "msg_fixture", "output_index": 0, "content_index": 0, "text": answer, "logprobs": []any{}})
			emit(map[string]any{"type": "response.content_part.done", "item_id": "msg_fixture", "output_index": 0, "content_index": 0, "part": part})
		}
		emit(map[string]any{"type": "response.output_item.done", "output_index": 0, "item": item})
		response["status"] = "completed"
		response["output"] = []any{item}
		response["usage"] = map[string]any{"input_tokens": 10, "output_tokens": 5, "total_tokens": 15, "input_tokens_details": map[string]any{"cached_tokens": 0}, "output_tokens_details": map[string]any{"reasoning_tokens": 0}}
		emit(map[string]any{"type": "response.completed", "response": response})
	}))
	defer upstream.Close()
	proxy := inference.NewWithBaseURL(fixtureToken{}, upstream.Client(), upstream.URL)
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fixture-client-key" {
			http.Error(w, "unauthorized", 401)
			return
		}
		proxy.ServeHTTP(w, r)
	}))
	defer gateway.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "npm", "run", "smoke", "--prefix", "../examples/mastra")
	cmd.Env = append(os.Environ(), "GATEWAY_BASE_URL="+gateway.URL+"/v1", "GATEWAY_API_KEY=fixture-client-key", "GATEWAY_MODEL=fixture-model", "MASTRA_FIXTURE=1")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Mastra contract: %v\n%s", err, output)
	}
	t.Log(string(output))
	if errorsSeen.Load() != 1 {
		t.Errorf("upstream error was retried: %d requests", errorsSeen.Load())
	}
	if toolsSeen.Load() != 1 {
		t.Errorf("tool round trips: %d", toolsSeen.Load())
	}
	if calls.Load() != 7 {
		t.Errorf("unexpected request count: %d", calls.Load())
	}
	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Error("upstream cancellation missing")
	}
}
