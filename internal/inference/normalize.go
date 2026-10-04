// Request conversion is limited to the documented subscription differences.
package inference

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

type requestError struct{ param, message string }

func (e *requestError) Error() string     { return e.message }
func invalid(param, message string) error { return &requestError{param, message} }

func normalize(body []byte) ([]byte, bool, error) {
	var request map[string]any
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(&request); err != nil || request == nil {
		return nil, false, errors.New("invalid JSON")
	}
	if dec.Decode(new(any)) != io.EOF {
		return nil, false, errors.New("trailing JSON")
	}
	for _, key := range []string{"background", "conversation", "max_output_tokens", "max_tool_calls", "metadata", "moderation", "multi_agent", "prompt", "prompt_cache_retention", "safety_identifier", "temperature", "top_logprobs", "top_p", "truncation", "user", "previous_response_id"} {
		if _, ok := request[key]; ok {
			return nil, false, invalid(key, key+" is unavailable for subscription inference. Send complete history in input; omit unsupported options.")
		}
	}
	if value, ok := request["store"]; ok && value != false {
		return nil, false, invalid("store", "Subscription inference requires store:false.")
	}
	stream := false
	if value, ok := request["stream"]; ok {
		var valid bool
		stream, valid = value.(bool)
		if !valid {
			return nil, false, invalid("stream", "stream must be a boolean.")
		}
	}
	if model, ok := request["model"].(string); !ok || model == "" {
		return nil, false, invalid("model", "Specify a model from /v1/models.")
	}
	if text, ok := request["input"].(string); ok {
		request["input"] = []any{map[string]any{"role": "user", "content": text}}
	}
	input, ok := request["input"].([]any)
	if !ok {
		return nil, false, invalid("input", "input must be text or a full-history array.")
	}
	for _, value := range input {
		item, ok := value.(map[string]any)
		if !ok {
			return nil, false, invalid("input", "Each input item must be an object.")
		}
		switch item["type"] {
		case "item_reference":
			return nil, false, invalid("input", "Stored item references are unavailable. Send complete history.")
		case "function_call":
			if ns, exists := item["namespace"]; exists && ns != nil && ns != toolNamespace {
				return nil, false, invalid("input", "Only gateway function tools are supported.")
			}
			item["namespace"] = toolNamespace
		case "additional_tools":
			return nil, false, invalid("input", "Define function tools in tools; additional_tools is unsupported.")
		}
		if item["role"] == "system" {
			return nil, false, invalid("input", "Use instructions or developer messages instead of system messages.")
		}
	}
	if raw, exists := request["tools"]; exists {
		tools, ok := raw.([]any)
		if !ok {
			return nil, false, invalid("tools", "tools must be an array of function definitions.")
		}
		names := map[string]bool{}
		for _, raw := range tools {
			tool, ok := raw.(map[string]any)
			if !ok || tool["type"] != "function" {
				return nil, false, invalid("tools", "Only client-executed function tools are supported.")
			}
			name, ok := tool["name"].(string)
			if !ok || name == "" || names[name] {
				return nil, false, invalid("tools", "Function names must be nonempty and unique.")
			}
			names[name] = true
			if tool["defer_loading"] == true {
				return nil, false, invalid("tools", "Deferred tools require unsupported tool search.")
			}
		}
		if len(tools) > 0 {
			request["tools"] = []any{map[string]any{"type": "namespace", "name": toolNamespace, "description": "Functions executed by the calling application.", "tools": tools}}
		}
	}
	request["store"] = false
	request["stream"] = true
	result, err := json.Marshal(request)
	return result, stream, err
}

// Only API-owned call objects are converted. User text, arguments and schemas stay opaque.
func externalCall(item map[string]any) {
	if item["type"] == "function_call" && item["namespace"] == toolNamespace {
		delete(item, "namespace")
	}
}

func externalResponse(response map[string]any) {
	if output, ok := response["output"].([]any); ok {
		for _, raw := range output {
			if item, ok := raw.(map[string]any); ok {
				externalCall(item)
			}
		}
	}
	if tools, ok := response["tools"].([]any); ok {
		flat := make([]any, 0, len(tools))
		for _, raw := range tools {
			tool, ok := raw.(map[string]any)
			if ok && tool["type"] == "namespace" && tool["name"] == toolNamespace {
				if children, ok := tool["tools"].([]any); ok {
					flat = append(flat, children...)
					continue
				}
			}
			flat = append(flat, raw)
		}
		response["tools"] = flat
	}
}
