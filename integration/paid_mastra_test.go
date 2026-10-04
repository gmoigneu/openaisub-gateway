// Check the pinned OpenAI provider's paid request shapes against gateway fixtures.
package integration

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gmoigneu/openaisub-gateway/internal/paid"
)

func TestPaidProviderContract(t *testing.T) {
	if os.Getenv("MASTRA_CONTRACT") != "1" {
		t.Skip("set MASTRA_CONTRACT=1 after npm ci in examples/mastra")
	}
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer fixture-platform-key" {
			t.Error("paid fixture received the wrong credential")
		}
		switch r.URL.Path {
		case "/embeddings":
			var request map[string]any
			if json.NewDecoder(r.Body).Decode(&request) != nil || request["model"] != "text-embedding-3-small" {
				t.Error("provider embedding request changed")
			}
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"object":"list","data":[{"object":"embedding","index":0,"embedding":[0.1,0.2]}],"model":"text-embedding-3-small","usage":{"prompt_tokens":2,"total_tokens":2}}`)
		case "/audio/speech":
			var request map[string]any
			if json.NewDecoder(r.Body).Decode(&request) != nil || request["model"] != "gpt-4o-mini-tts" || request["voice"] != "alloy" {
				t.Error("provider speech request changed")
			}
			w.Header().Set("Content-Type", "audio/mpeg")
			w.Write([]byte("fixture mp3 bytes"))
		case "/audio/transcriptions":
			if err := r.ParseMultipartForm(26 << 20); err != nil || r.FormValue("model") != "gpt-4o-mini-transcribe" {
				t.Error("provider transcription form changed")
			}
			file, _, err := r.FormFile("file")
			if err != nil {
				t.Error("provider transcription file missing")
				return
			}
			defer file.Close()
			data, _ := io.ReadAll(file)
			if string(data) != "fixture mp3 bytes" {
				t.Error("speech bytes did not reach transcription")
			}
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"text":"Gateway check"}`)
		default:
			t.Error("provider used an unsupported paid route")
		}
	}))
	defer upstream.Close()
	proxy := paid.NewWithBaseURL("fixture-platform-key", upstream.Client(), upstream.URL)
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
	cmd := exec.CommandContext(ctx, "npm", "run", "smoke:paid", "--prefix", "../examples/mastra")
	cmd.Env = append(os.Environ(), "GATEWAY_BASE_URL="+gateway.URL+"/v1", "GATEWAY_API_KEY=fixture-client-key")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("paid provider contract: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), "PASS transcription") || calls.Load() != 3 {
		t.Fatal("paid provider did not complete all three gateway calls")
	}
}
