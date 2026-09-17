// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package bridge

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/google/uuid"
)

// mapV0MethodToV1 maps A2A v0.3 JSON-RPC method names (used by Gemini Enterprise,
// Vertex AI Agent Engine, and Python a2a-sdk v0.3) to A2A v1.0 method names
// expected by a2a-go/v2.
func mapV0MethodToV1(method string) (string, bool) {
	switch method {
	case "message/send":
		return "SendMessage", true
	case "message/stream":
		return "SendStreamingMessage", true
	case "tasks/get":
		return "GetTask", true
	case "tasks/list":
		return "ListTasks", true
	case "tasks/cancel":
		return "CancelTask", true
	case "tasks/resubscribe":
		return "SubscribeToTask", true
	case "tasks/pushNotification/get", "tasks/pushNotificationConfig/get":
		return "GetTaskPushNotificationConfig", true
	case "tasks/pushNotification/set", "tasks/pushNotificationConfig/set":
		return "CreateTaskPushNotificationConfig", true
	case "tasks/pushNotificationConfig/list":
		return "ListTaskPushNotificationConfigs", true
	case "tasks/pushNotificationConfig/delete":
		return "DeleteTaskPushNotificationConfig", true
	default:
		return method, false
	}
}

// normalizeV0RequestJSON checks if the incoming JSON-RPC request uses an A2A v0.3
// method name (e.g. "message/send"). If so, it rewrites the method field to the
// corresponding A2A v1.0 method name and returns isV0=true so that the response
// can also be normalized back to A2A v0.3 format.
func normalizeV0RequestJSON(body []byte) ([]byte, bool) {
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return body, false
	}
	method, ok := raw["method"].(string)
	if !ok {
		return body, false
	}
	v1Method, isV0 := mapV0MethodToV1(method)
	if !isV0 {
		return body, false
	}
	raw["method"] = v1Method

	if params, ok := raw["params"].(map[string]any); ok {
		if msg, ok := params["message"].(map[string]any); ok {
			if mid, _ := msg["messageId"].(string); mid == "" {
				if smid, _ := msg["message_id"].(string); smid != "" {
					msg["messageId"] = smid
				} else {
					msg["messageId"] = uuid.NewString()
				}
			}
			if tid, _ := msg["taskId"].(string); tid == "" {
				if stid, _ := msg["task_id"].(string); stid != "" {
					msg["taskId"] = stid
				} else if pid, _ := params["id"].(string); pid != "" {
					msg["taskId"] = pid
				} else if ptid, _ := params["taskId"].(string); ptid != "" {
					msg["taskId"] = ptid
				}
			}
			if cid, _ := msg["contextId"].(string); cid == "" {
				if scid, _ := msg["context_id"].(string); scid != "" {
					msg["contextId"] = scid
				} else if pcid, _ := params["contextId"].(string); pcid != "" {
					msg["contextId"] = pcid
				}
			}
		}
	}

	updated, err := json.Marshal(raw)
	if err != nil {
		return body, false
	}
	return updated, true
}

func mapV1StateToV0(state string) string {
	switch state {
	case "TASK_STATE_SUBMITTED":
		return "submitted"
	case "TASK_STATE_WORKING":
		return "working"
	case "TASK_STATE_INPUT_REQUIRED":
		return "input-required"
	case "TASK_STATE_COMPLETED":
		return "completed"
	case "TASK_STATE_CANCELED":
		return "canceled"
	case "TASK_STATE_FAILED":
		return "failed"
	case "TASK_STATE_REJECTED":
		return "rejected"
	case "TASK_STATE_AUTH_REQUIRED":
		return "auth-required"
	default:
		return state
	}
}

func mapV1RoleToV0(role string) string {
	switch role {
	case "ROLE_USER":
		return "user"
	case "ROLE_AGENT":
		return "agent"
	default:
		return role
	}
}

// convertV1ToV0JSONRPCResponse converts an A2A v1.0 JSON-RPC response payload
// into an A2A v0.3 compliant response payload (unwrapping StreamResponse envelopes
// like {"task": ...} and injecting required "kind" discriminators and v0.3 enum names).
func convertV1ToV0JSONRPCResponse(raw []byte) []byte {
	var resp map[string]any
	if err := json.Unmarshal(raw, &resp); err != nil {
		return raw
	}
	resVal, hasResult := resp["result"]
	if !hasResult || resVal == nil {
		return raw
	}
	if resMap, ok := resVal.(map[string]any); ok {
		if task, ok := resMap["task"].(map[string]any); ok && len(resMap) == 1 {
			task["kind"] = "task"
			resp["result"] = task
		} else if msg, ok := resMap["message"].(map[string]any); ok && len(resMap) == 1 {
			msg["kind"] = "message"
			resp["result"] = msg
		} else if su, ok := resMap["statusUpdate"].(map[string]any); ok && len(resMap) == 1 {
			su["kind"] = "status-update"
			if _, hasFinal := su["final"]; !hasFinal {
				isFinal := false
				if statusMap, ok := su["status"].(map[string]any); ok {
					if state, ok := statusMap["state"].(string); ok {
						switch mapV1StateToV0(state) {
						case "completed", "failed", "canceled", "rejected", "input-required":
							isFinal = true
						}
					}
				}
				su["final"] = isFinal
			}
			resp["result"] = su
		} else if au, ok := resMap["artifactUpdate"].(map[string]any); ok && len(resMap) == 1 {
			au["kind"] = "artifact-update"
			resp["result"] = au
		} else if _, hasID := resMap["id"]; hasID && resMap["status"] != nil {
			resMap["kind"] = "task"
		} else if _, hasMsgID := resMap["messageId"]; hasMsgID {
			resMap["kind"] = "message"
		}
	}

	enrichV0Kinds(resp["result"])

	out, err := json.Marshal(resp)
	if err != nil {
		return raw
	}
	return out
}

func enrichV0Kinds(v any) {
	switch val := v.(type) {
	case map[string]any:
		if role, ok := val["role"].(string); ok {
			val["role"] = mapV1RoleToV0(role)
		}
		if state, ok := val["state"].(string); ok {
			val["state"] = mapV1StateToV0(state)
		}
		if _, hasMsgID := val["messageId"]; hasMsgID && val["role"] != nil {
			if _, hasKind := val["kind"]; !hasKind {
				val["kind"] = "message"
			}
		}
		if parts, ok := val["parts"].([]any); ok {
			for _, p := range parts {
				if partMap, ok := p.(map[string]any); ok {
					if _, hasKind := partMap["kind"]; !hasKind {
						if _, hasText := partMap["text"]; hasText {
							partMap["kind"] = "text"
						} else if _, hasData := partMap["data"]; hasData {
							partMap["kind"] = "data"
						} else if _, hasRaw := partMap["raw"]; hasRaw {
							partMap["kind"] = "file"
						} else if _, hasURL := partMap["url"]; hasURL {
							partMap["kind"] = "file"
						}
					}
				}
			}
		}
		for _, child := range val {
			enrichV0Kinds(child)
		}
	case []any:
		for _, item := range val {
			enrichV0Kinds(item)
		}
	}
}

// v0CompatResponseWriter intercepts HTTP responses for A2A v0.3 requests and
// transforms JSON-RPC results (both standard JSON and SSE data lines) from v1.0
// to v0.3 schema.
type v0CompatResponseWriter struct {
	http.ResponseWriter
	statusCode int
	headerSent bool
	isSSE      bool
	buf        bytes.Buffer
}

func newV0CompatResponseWriter(w http.ResponseWriter) *v0CompatResponseWriter {
	return &v0CompatResponseWriter{
		ResponseWriter: w,
		statusCode:     http.StatusOK,
	}
}

func (w *v0CompatResponseWriter) WriteHeader(code int) {
	w.statusCode = code
	ct := w.Header().Get("Content-Type")
	if strings.HasPrefix(ct, "text/event-stream") {
		w.isSSE = true
		w.headerSent = true
		w.ResponseWriter.WriteHeader(code)
	}
}

func (w *v0CompatResponseWriter) Write(p []byte) (int, error) {
	if !w.headerSent {
		ct := w.Header().Get("Content-Type")
		if strings.HasPrefix(ct, "text/event-stream") {
			w.isSSE = true
			w.headerSent = true
			w.ResponseWriter.WriteHeader(w.statusCode)
		}
	}
	if w.isSSE {
		// Transform any "data: {...}" lines in the SSE chunk.
		lines := strings.Split(string(p), "\n")
		var out strings.Builder
		for i, line := range lines {
			if strings.HasPrefix(line, "data: ") {
				jsonStr := strings.TrimPrefix(line, "data: ")
				converted := convertV1ToV0JSONRPCResponse([]byte(jsonStr))
				out.WriteString("data: ")
				out.Write(converted)
			} else {
				out.WriteString(line)
			}
			if i < len(lines)-1 {
				out.WriteByte('\n')
			}
		}
		_, err := w.ResponseWriter.Write([]byte(out.String()))
		return len(p), err
	}
	return w.buf.Write(p)
}

func (w *v0CompatResponseWriter) Flush() {
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (w *v0CompatResponseWriter) finish() {
	if w.isSSE {
		return
	}
	converted := convertV1ToV0JSONRPCResponse(w.buf.Bytes())
	w.Header().Del("Content-Length")
	if !w.headerSent {
		w.ResponseWriter.WriteHeader(w.statusCode)
		w.headerSent = true
	}
	_, _ = w.ResponseWriter.Write(converted)
}
