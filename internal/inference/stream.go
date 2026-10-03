// SSE parsing bounds each event and waits for a real terminal response.
package inference

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

func (h *Handler) consume(w http.ResponseWriter, r *http.Request, body io.Reader, stream bool) {
	if stream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(200)
		_ = http.NewResponseController(w).Flush()
	}
	err := readEvents(body, stream, func(event string, data []byte, fields []string) (bool, error) {
		if bytes.Equal(data, []byte("[DONE]")) {
			return false, errors.New("stream ended without terminal response")
		}
		var payload map[string]any
		dec := json.NewDecoder(bytes.NewReader(data))
		dec.UseNumber()
		if dec.Decode(&payload) != nil || payload == nil {
			return false, errors.New("invalid stream event")
		}
		if dec.Decode(new(any)) != io.EOF {
			return false, errors.New("trailing stream data")
		}
		typ, _ := payload["type"].(string)
		if typ == "" {
			typ = event
		}
		if item, ok := payload["item"].(map[string]any); ok {
			externalCall(item)
		}
		response, _ := payload["response"].(map[string]any)
		if response != nil {
			externalResponse(response)
		}
		terminal := typ == "response.completed" || typ == "response.failed" || typ == "response.incomplete" || typ == "error"
		if terminal && typ != "error" && response == nil {
			return false, errors.New("terminal event missing response")
		}
		if terminal && typ != "error" && response["status"] != strings.TrimPrefix(typ, "response.") {
			return false, errors.New("terminal response status mismatch")
		}
		if stream {
			encoded, _ := json.Marshal(payload)
			for _, field := range fields {
				if _, err := fmt.Fprintln(w, field); err != nil {
					return false, err
				}
			}
			if _, err := fmt.Fprintf(w, "data: %s\n\n", encoded); err != nil {
				return false, err
			}
			if err := http.NewResponseController(w).Flush(); err != nil {
				return false, err
			}
		} else if terminal {
			if typ == "error" {
				encoded, _ := json.Marshal(map[string]any{"error": payload})
				writeJSON(w, 502, encoded)
			} else {
				encoded, _ := json.Marshal(response)
				writeJSON(w, 200, encoded)
			}
		}
		return terminal, nil
	})
	if err == nil || r.Context().Err() != nil {
		return
	}
	if stream {
		_, _ = io.WriteString(w, "event: error\ndata: {\"type\":\"error\",\"code\":\"upstream_stream_interrupted\",\"message\":\"OpenAI stream ended without a valid terminal response. The request was not retried.\"}\n\n")
		_ = http.NewResponseController(w).Flush()
	} else {
		apiError(w, 502, "upstream_stream_interrupted", "OpenAI stream ended without a valid terminal response or exceeded the response limit. The request was not retried.", "")
	}
}

func readEvents(reader io.Reader, stream bool, visit func(string, []byte, []string) (bool, error)) error {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 4096), maxEventBytes)
	var data bytes.Buffer
	var fields []string
	var event string
	var eventBytes, total int
	for scanner.Scan() {
		line := scanner.Text()
		eventBytes += len(line) + 1
		total += len(line) + 1
		if eventBytes > maxEventBytes || (!stream && total > maxResponseBytes) {
			return errors.New("stream limit exceeded")
		}
		if line == "" {
			if data.Len() > 0 {
				done, err := visit(event, bytes.TrimSuffix(data.Bytes(), []byte("\n")), fields)
				if err != nil {
					return err
				}
				if done {
					return nil
				}
			}
			data.Reset()
			fields = nil
			event = ""
			eventBytes = 0
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		name, value, _ := strings.Cut(line, ":")
		value = strings.TrimPrefix(value, " ")
		switch name {
		case "data":
			data.WriteString(value)
			data.WriteByte('\n')
		case "event":
			event = value
			fields = append(fields, line)
		case "id", "retry":
			fields = append(fields, line)
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	// An unfinished final frame is not a delivered SSE event.
	return io.ErrUnexpectedEOF
}
