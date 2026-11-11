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

package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	scionrt "github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/wsprotocol"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// brokerBodyMarker is placed in broker answer bodies; the tests check that
// it appears in the hub log and in no client response.
const brokerBodyMarker = "MARKER-broker-body-7f3a"

// TestBrokerStatusError_ErrorOmitsBody: Error() names the status, and the
// broker's code when it is well formed, but never any of the body: a JSON
// envelope, a plain-text page, an envelope with an empty message, or a
// malformed code.
func TestBrokerStatusError_ErrorOmitsBody(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{"json envelope", `{"error":{"code":"runtime_error","message":"` + brokerBodyMarker + `"}}`, "runtime broker returned error 500 (runtime_error)"},
		{"plain text", "<html>" + brokerBodyMarker + "</html>", "runtime broker returned error 500"},
		{"empty message", `{"error":{"code":"runtime_error","message":""},"x":"` + brokerBodyMarker + `"}`, "runtime broker returned error 500 (runtime_error)"},
		{"malformed code", `{"error":{"code":"` + brokerBodyMarker + `"}}`, "runtime broker returned error 500"},
		{"empty body", "", "runtime broker returned error 500"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			se := &brokerStatusError{StatusCode: http.StatusInternalServerError, Body: tc.body}
			assert.Equal(t, tc.want, se.Error())
			wrapped := fmt.Errorf("dispatch: %w", se)
			assert.NotContains(t, wrapped.Error(), brokerBodyMarker)
		})
	}
}

// TestBrokerStatusError_MessageNeverRawBody: brokerErrorMessage relays the
// JSON envelope's message, and otherwise the fixed generic text, never the
// raw body.
func TestBrokerStatusError_MessageNeverRawBody(t *testing.T) {
	se := &brokerStatusError{StatusCode: http.StatusConflict, Body: `{"error":{"code":"conflict","message":"agent is ambiguous"}}`}
	assert.Equal(t, "agent is ambiguous", se.brokerErrorMessage())

	for _, body := range []string{
		"<html>" + brokerBodyMarker + "</html>",
		`{"error":{"code":"conflict","message":""},"x":"` + brokerBodyMarker + `"}`,
		"",
	} {
		se := &brokerStatusError{StatusCode: http.StatusNotFound, Body: body}
		assert.Equal(t, brokerErrorGenericMessage, se.brokerErrorMessage(), "body %q", body)
	}
}

// TestNewBrokerStatusError_LogsCappedBody: building the error logs one warn
// line with the broker id, status, code and full body length, and the body
// itself only up to the cap: a marker inside the cap reaches the log, a
// marker past it does not, and no part of the body reaches Error(). A JSON
// envelope's code is logged; a body that is not JSON logs an empty code.
func TestNewBrokerStatusError_LogsCappedBody(t *testing.T) {
	const tailMarker = "TAILMARKER-past-the-cap"
	for _, tc := range []struct {
		name     string
		body     string
		wantCode string
	}{
		// The padding and the tail sit inside the JSON message, so the body
		// stays a valid envelope and its code is logged.
		{"json envelope", `{"error":{"code":"runtime_error","message":"` + brokerBodyMarker +
			strings.Repeat("x", maxBrokerBodyLogBytes) + tailMarker + `"}}`, "code=runtime_error"},
		// A body that is not JSON carries no code.
		{"not json", "<html>" + brokerBodyMarker + strings.Repeat("x", maxBrokerBodyLogBytes) + tailMarker,
			`code=""`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			buf := captureSlog(t)
			se := newBrokerStatusError("broker-abc", http.StatusBadGateway, []byte(tc.body), "7")

			assert.Equal(t, http.StatusBadGateway, se.StatusCode)
			assert.Equal(t, tc.body, se.Body, "the error keeps the full body for code and details parsing")
			assert.Equal(t, "7", se.RetryAfter)
			assert.NotContains(t, se.Error(), brokerBodyMarker)

			logged := buf.String()
			assert.Contains(t, logged, "level=WARN")
			assert.Contains(t, logged, "runtime broker returned an error")
			assert.Contains(t, logged, "broker_id=broker-abc")
			assert.Contains(t, logged, "status=502")
			assert.Contains(t, logged, tc.wantCode)
			assert.Contains(t, logged, fmt.Sprintf("body_len=%d", len(tc.body)))
			assert.Contains(t, logged, brokerBodyMarker, "the body inside the cap reaches the hub log")
			assert.NotContains(t, logged, tailMarker, "a marker past the cap must not reach the log")
		})
	}
}

// TestNewBrokerStatusError_LogNeutralisesControlCharacters: a body that
// tries to forge a log line (newline, carriage return, ESC) is logged as a
// single line with those characters replaced.
func TestNewBrokerStatusError_LogNeutralisesControlCharacters(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	body := "first\nlevel=ERROR msg=forged\r\x1b[31mred\x00"
	_ = newBrokerStatusError("broker-abc", http.StatusInternalServerError, []byte(body), "")

	logged := strings.TrimRight(buf.String(), "\n")
	assert.NotContains(t, logged, "\n", "the body must not split the log line")
	assert.NotContains(t, logged, "\r")
	assert.NotContains(t, logged, "\x1b")
	assert.NotContains(t, logged, "\x00")
	assert.NotContains(t, logged, "\\n", "the newline is replaced, not escaped")
	assert.Contains(t, logged, "level=ERROR msg=forged", "the readable text survives inside the body value")
	assert.Contains(t, logged, "body_len=")
}

// TestBrokerBodyForLog: the renderer caps at maxBrokerBodyLogBytes on a
// rune boundary and replaces invalid UTF-8, control and other
// non-printable runes with U+FFFD.
func TestBrokerBodyForLog(t *testing.T) {
	assert.Equal(t, "plain text", brokerBodyForLog("plain text"))
	assert.Equal(t, "a\uFFFDb\uFFFDc\uFFFDd\uFFFDe", brokerBodyForLog("a\nb\x1bc\xffd\u200be"))

	long := strings.Repeat("\u00e9", maxBrokerBodyLogBytes) // 2 bytes per rune
	got := brokerBodyForLog(long)
	assert.LessOrEqual(t, len(got), maxBrokerBodyLogBytes)
	assert.True(t, utf8.ValidString(got))
	assert.NotContains(t, got, "\uFFFD", "the cut lands on a rune boundary")
}

// TestBrokerStatusError_LogValueOmitsBody: logging the error itself gives
// its text, status, code and body length, but not the body, which
// newBrokerStatusError has already logged once.
func TestBrokerStatusError_LogValueOmitsBody(t *testing.T) {
	buf := captureSlog(t)
	se := &brokerStatusError{StatusCode: http.StatusInternalServerError,
		Body: `{"error":{"code":"runtime_error","message":"` + brokerBodyMarker + `"}}`}

	slog.Error("dispatch failed", "error", se)

	logged := buf.String()
	assert.Contains(t, logged, "error.status=500")
	assert.Contains(t, logged, "error.code=runtime_error")
	assert.Contains(t, logged, fmt.Sprintf("error.body_len=%d", len(se.Body)))
	assert.Contains(t, logged, "runtime broker returned error 500 (runtime_error)")
	assert.NotContains(t, logged, brokerBodyMarker, "the body is not logged again")
}

// TestNewBrokerStatusError_NotFoundLogsAtDebug: a 404 answer is logged at
// debug level, any other status at warn.
func TestNewBrokerStatusError_NotFoundLogsAtDebug(t *testing.T) {
	buf := captureSlog(t)
	_ = newBrokerStatusError("broker-abc", http.StatusNotFound, []byte(`{"error":{"code":"agent_not_found"}}`), "")
	logged := buf.String()
	assert.Contains(t, logged, "level=DEBUG")
	assert.NotContains(t, logged, "level=WARN")
}

// TestRenderBrokerText_OutputWithinCap: text made only of bytes the
// renderer replaces (each grows to the 3-byte U+FFFD) still renders to at
// most the cap, for both the log cap and the client message cap.
func TestRenderBrokerText_OutputWithinCap(t *testing.T) {
	for _, maxBytes := range []int{maxBrokerBodyLogBytes, maxBrokerMessageBytes} {
		for _, in := range []string{
			strings.Repeat("\x01", maxBytes),
			strings.Repeat("\xff", maxBytes*2),
			strings.Repeat("a\n", maxBytes),
		} {
			got := renderBrokerText(in, maxBytes)
			assert.LessOrEqual(t, len(got), maxBytes, "cap %d", maxBytes)
			assert.True(t, utf8.ValidString(got))
		}
	}
	assert.LessOrEqual(t, len(brokerBodyForLog(strings.Repeat("\x00", 3*maxBrokerBodyLogBytes))), maxBrokerBodyLogBytes)
}

// TestBrokerHTTPError_BodyNotInErrorText: the HTTP transport's error keeps
// the status and body for callers but its text carries none of the body.
func TestBrokerHTTPError_BodyNotInErrorText(t *testing.T) {
	_ = captureSlog(t)
	resp := &http.Response{
		StatusCode: http.StatusInternalServerError,
		Body:       io.NopCloser(strings.NewReader(brokerBodyMarker)),
		Header:     http.Header{},
	}
	err := brokerHTTPError("broker-1", resp)
	var se *brokerStatusError
	require.True(t, errors.As(err, &se))
	assert.Equal(t, brokerBodyMarker, se.Body)
	assert.NotContains(t, err.Error(), brokerBodyMarker)
}

// TestIsHashMismatchError_MatchesBrokerBody: the hash-mismatch repair still
// recognises a broker answer whose body names the mismatch, now that the
// body is not part of Error(), and still routes on the hydration prefix.
func TestIsHashMismatchError_MatchesBrokerBody(t *testing.T) {
	se := &brokerStatusError{StatusCode: http.StatusInternalServerError,
		Body: `{"error":{"code":"runtime_error","message":"Failed to hydrate template: hash mismatch for file a.txt"}}`}
	err := fmt.Errorf("dispatch: %w", se)
	assert.True(t, isHashMismatchError(err))
	assert.Contains(t, brokerErrorMatchText(err), "Failed to hydrate template:")

	assert.True(t, isHashMismatchError(errors.New("Failed to hydrate harness-config: hash mismatch for file config.yaml")),
		"plain error text still matches")
	assert.False(t, isHashMismatchError(&brokerStatusError{StatusCode: 500, Body: "other failure"}))
	assert.False(t, isHashMismatchError(nil))
}

// TestRelayableStreamCloseReason: fixed protocol tokens pass, free text is
// replaced by "" and logged capped and rendered.
func TestRelayableStreamCloseReason(t *testing.T) {
	buf := captureSlog(t)
	assert.Equal(t, wsprotocol.CloseReasonSessionEnded, relayableStreamCloseReason("runtime_broker", 4410, wsprotocol.CloseReasonSessionEnded))
	assert.Equal(t, "", relayableStreamCloseReason("runtime_broker", 4410, ""))
	assert.Empty(t, buf.String(), "a fixed or empty reason logs nothing")

	assert.Equal(t, "", relayableStreamCloseReason("runtime_broker", 4410, "exec failed: "+brokerBodyMarker))
	logged := buf.String()
	assert.Contains(t, logged, "stream close reason replaced")
	assert.Contains(t, logged, "source=runtime_broker")
	assert.Contains(t, logged, "close_code=4410")
	assert.Contains(t, logged, brokerBodyMarker)
}

// TestPTYLeafCloseCode_FreeTextReasonReplaced: a conduit close with free
// text keeps its code mapping but loses the reason.
func TestPTYLeafCloseCode_FreeTextReasonReplaced(t *testing.T) {
	_ = captureSlog(t)
	code, reason := ptyLeafCloseCode(4503, "admission timed out at "+brokerBodyMarker)
	assert.Equal(t, 4503, code)
	assert.Equal(t, "", reason)

	code, reason = ptyLeafCloseCode(4400, "decode error: "+brokerBodyMarker)
	assert.Equal(t, wsprotocol.ClosePTYInternalError, code)
	assert.Equal(t, "", reason)

	code, reason = ptyLeafCloseCode(4503, "relay_restart")
	assert.Equal(t, 4503, code)
	assert.Equal(t, "relay_restart", reason)
}

// TestDispatchCreateErrorResponse_BrokerBodyNotRelayed: the shared create,
// start and restart error switch keeps its status and code but carries none
// of a broker body: not on the generic 502 (JSON or plain body), and not on
// the 404 relay when the body has no JSON message.
func TestDispatchCreateErrorResponse_BrokerBodyNotRelayed(t *testing.T) {
	_ = captureSlog(t)
	for _, tc := range []struct {
		name        string
		status      int
		body        string
		wantStatus  int
		wantCode    string
		wantMessage string
	}{
		{"502 json", http.StatusInternalServerError, `{"error":{"code":"runtime_error","message":"` + brokerBodyMarker + `"}}`,
			http.StatusBadGateway, ErrCodeRuntimeError, "Failed to dispatch to runtime broker: dispatch: runtime broker returned error 500 (runtime_error)"},
		{"502 plain", http.StatusInternalServerError, "<html>" + brokerBodyMarker + "</html>",
			http.StatusBadGateway, ErrCodeRuntimeError, "Failed to dispatch to runtime broker: dispatch: runtime broker returned error 500"},
		{"404 plain", http.StatusNotFound, "<html>" + brokerBodyMarker + "</html>",
			http.StatusNotFound, ErrCodeNotFound, "Failed to dispatch to runtime broker: " + brokerErrorGenericMessage},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			dispatchCreateErrorResponse(w, fmt.Errorf("dispatch: %w", &brokerStatusError{StatusCode: tc.status, Body: tc.body}), "")

			require.Equal(t, tc.wantStatus, w.Code, w.Body.String())
			assert.NotContains(t, w.Body.String(), brokerBodyMarker)
			var resp ErrorResponse
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
			assert.Equal(t, tc.wantCode, resp.Error.Code)
			assert.Equal(t, tc.wantMessage, resp.Error.Message)
		})
	}
}

// TestPTYCloseCode_FreeTextBrokerReasonReplaced: a broker StreamClose whose
// reason is free text reaches the browser with its code and an empty
// reason; the text stays out of the close frame.
func TestPTYCloseCode_FreeTextBrokerReasonReplaced(t *testing.T) {
	f := startCloseCodeSession(t)
	raw := []byte(`{"type":"` + wsprotocol.TypeStreamClose + `","streamId":"` + f.open.StreamID +
		`","reason":"exec failed at ` + brokerBodyMarker + `","code":4410}`)
	require.NoError(t, f.broker.WriteMessage(websocket.TextMessage, raw))

	_, ce := readBrowserUntilClose(t, f.browser)
	require.Equal(t, wsprotocol.ClosePTYSessionGone, ce.Code)
	assert.Equal(t, "", ce.Text)
	_ = f.waitDone(t)
}

// TestPTYCloseCode_FixedBrokerReasonKept: a broker StreamClose with a fixed
// protocol reason keeps it.
func TestPTYCloseCode_FixedBrokerReasonKept(t *testing.T) {
	f := startCloseCodeSession(t)
	raw := []byte(`{"type":"` + wsprotocol.TypeStreamClose + `","streamId":"` + f.open.StreamID +
		`","reason":"` + wsprotocol.CloseReasonSessionEnded + `","code":4410}`)
	require.NoError(t, f.broker.WriteMessage(websocket.TextMessage, raw))

	_, ce := readBrowserUntilClose(t, f.browser)
	require.Equal(t, wsprotocol.ClosePTYSessionGone, ce.Code)
	assert.Equal(t, wsprotocol.CloseReasonSessionEnded, ce.Text)
	_ = f.waitDone(t)
}

// TestDecodeResponseWithSnippet_BodyNotInError: a broker success answer that
// cannot be decoded gives an error without any of the body; the body
// reaches the hub log.
func TestDecodeResponseWithSnippet_BodyNotInError(t *testing.T) {
	logs := captureSlog(t)
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader("not json " + brokerBodyMarker)),
		Header:     http.Header{},
	}
	var out map[string]any
	err := (&brokerHTTPTransport{}).decodeResponseWithSnippet("broker-1", resp, &out)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to decode response")
	assert.NotContains(t, err.Error(), brokerBodyMarker)
	assert.Contains(t, logs.String(), brokerBodyMarker, "the body stays in the hub log")
	assert.Contains(t, logs.String(), "broker_id=broker-1")
}

// TestRepairHashMismatch_RoutesOnBrokerBody: the repair still routes on the
// hydration prefix in the broker's body (here: template), now that the body
// is not part of Error().
func TestRepairHashMismatch_RoutesOnBrokerBody(t *testing.T) {
	d := NewHTTPAgentDispatcherWithClient(nil, nil, false, slog.Default())
	harnessCalled, templateCalled := false, false
	d.SetHarnessConfigRepairer(func(context.Context, HarnessConfigRepairRef) error {
		harnessCalled = true
		return nil
	})
	d.SetTemplateRepairer(func(context.Context, TemplateRepairRef) error {
		templateCalled = true
		return nil
	})
	agent := &store.Agent{
		Slug:      "a",
		ProjectID: "proj-1",
		Template:  "tpl",
		AppliedConfig: &store.AgentAppliedConfig{
			HarnessConfig: "claude",
			TemplateID:    "tpl-id-1",
		},
	}
	dispatchErr := fmt.Errorf("dispatch: %w", &brokerStatusError{StatusCode: http.StatusInternalServerError,
		Body: `{"error":{"code":"runtime_error","message":"Failed to hydrate template: hash mismatch for file a.txt"}}`})

	require.NoError(t, d.repairHashMismatch(context.Background(), agent, dispatchErr))
	assert.True(t, templateCalled, "the template named in the broker's body is repaired")
	assert.False(t, harnessCalled, "the harness config is not repaired for a template mismatch")
}

// TestBrokerStatusError_MessageCappedAndFiltered: a broker's JSON error
// message is relayed capped at maxBrokerMessageBytes on a rune boundary,
// with newlines, ESC and other control runes replaced.
func TestBrokerStatusError_MessageCappedAndFiltered(t *testing.T) {
	const tailMarker = "TAILMARKER-past-the-message-cap"
	msg := "line1\nline2\x1b[31mred\x00" + strings.Repeat("\u00e9", maxBrokerMessageBytes) + tailMarker
	raw, err := json.Marshal(map[string]any{"error": map[string]any{"code": "conflict", "message": msg}})
	require.NoError(t, err)
	se := &brokerStatusError{StatusCode: http.StatusConflict, Body: string(raw)}

	got := se.brokerErrorMessage()

	assert.True(t, strings.HasPrefix(got, "line1\uFFFDline2\uFFFD[31mred\uFFFD"), "got %q", got[:min(len(got), 40)])
	assert.NotContains(t, got, "\n")
	assert.NotContains(t, got, "\x1b")
	assert.NotContains(t, got, "\x00")
	assert.NotContains(t, got, tailMarker, "text past the cap is not relayed")
	assert.LessOrEqual(t, len(got), maxBrokerMessageBytes)
	assert.True(t, utf8.ValidString(got))
}

// TestDispatchCreateErrorResponse_JSONMessageControlRunesRemoved: on the
// 404 relay of a broker's JSON error message, a newline and an ESC in the
// message do not reach the client response.
func TestDispatchCreateErrorResponse_JSONMessageControlRunesRemoved(t *testing.T) {
	_ = captureSlog(t)
	raw, err := json.Marshal(map[string]any{"error": map[string]any{
		"code": "agent_not_found", "message": "no such agent\nlevel=ERROR forged\x1b[2J",
	}})
	require.NoError(t, err)
	w := httptest.NewRecorder()
	dispatchCreateErrorResponse(w, &brokerStatusError{StatusCode: http.StatusNotFound, Body: string(raw)}, "")

	require.Equal(t, http.StatusNotFound, w.Code, w.Body.String())
	var resp ErrorResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, ErrCodeNotFound, resp.Error.Code)
	assert.NotContains(t, resp.Error.Message, "\n", "the newline must not reach the client")
	assert.NotContains(t, resp.Error.Message, "\x1b", "the ESC must not reach the client")
	assert.Contains(t, resp.Error.Message, "no such agent")
}

// TestBrokerErrorMessage_ProjectShareTextsByteIdentical: every project
// storage refusal text the hub allowlists passes through brokerErrorMessage
// byte-identical (none is over the cap or holds a replaced rune), so the
// allowlist match keeps working; the longest of them is over 256 bytes and
// is still relayed whole by brokerProjectStorageRefusal.
func TestBrokerErrorMessage_ProjectShareTextsByteIdentical(t *testing.T) {
	texts := scionrt.ProjectShareClientTexts()
	require.NotEmpty(t, texts)
	longest := ""
	for _, text := range texts {
		raw, err := json.Marshal(map[string]any{"error": map[string]any{
			"code": projectStorageUnavailableErrorCode, "message": text,
		}})
		require.NoError(t, err)
		se := &brokerStatusError{StatusCode: http.StatusServiceUnavailable, Body: string(raw)}
		assert.Equal(t, text, se.brokerErrorMessage(), "allowlisted text must pass byte-identical")
		if len(text) > len(longest) {
			longest = text
		}
	}

	require.Greater(t, len(longest), 256, "the longest allowlisted text is why the cap is above 256 bytes")
	raw, err := json.Marshal(map[string]any{"error": map[string]any{
		"code": projectStorageUnavailableErrorCode, "message": longest,
	}})
	require.NoError(t, err)
	status, code, message, ok := brokerProjectStorageRefusal(
		&brokerStatusError{StatusCode: http.StatusServiceUnavailable, Body: string(raw)})
	require.True(t, ok)
	assert.Equal(t, http.StatusServiceUnavailable, status)
	assert.Equal(t, projectStorageUnavailableErrorCode, code)
	assert.Equal(t, longest, message, "the long allowlisted text is relayed whole")
}

// TestBrokerFailureDetails_Allowlist: the hub relays only the allowlisted
// details, as fresh string values: a template_error's cause
// "file_not_found" and a plain base-name file, a local_worktree_refused's
// bounded reason; every other key, shape or value is dropped.
func TestBrokerFailureDetails_Allowlist(t *testing.T) {
	envelope := func(code string, details any) error {
		raw, err := json.Marshal(map[string]any{"error": map[string]any{"code": code, "message": brokerBodyMarker, "details": details}})
		require.NoError(t, err)
		return fmt.Errorf("dispatch: %w", &brokerStatusError{StatusCode: http.StatusUnprocessableEntity, Body: string(raw)})
	}
	fileCase := func(file any) map[string]any {
		return map[string]any{"cause": "file_not_found", "file": file, "extra": brokerBodyMarker}
	}
	causeOnly := map[string]any{"cause": "file_not_found"}
	for _, tc := range []struct {
		name string
		err  error
		want map[string]any
	}{
		{"accepted file", envelope("template_error", fileCase("settings.json")), map[string]any{"cause": "file_not_found", "file": "settings.json"}},
		{"separator", envelope("template_error", fileCase("dir/settings.json")), causeOnly},
		{"backslash", envelope("template_error", fileCase(`dir\settings.json`)), causeOnly},
		{"dot dot", envelope("template_error", fileCase("..")), causeOnly},
		{"dot", envelope("template_error", fileCase(".")), causeOnly},
		{"control char", envelope("template_error", fileCase("a\nb.json")), causeOnly},
		{"bidi char", envelope("template_error", fileCase("a\u202eb.json")), causeOnly},
		{"zero-width char", envelope("template_error", fileCase("a\u200bb.json")), causeOnly},
		{"over-length", envelope("template_error", fileCase(strings.Repeat("a", 256))), causeOnly},
		{"non-string file", envelope("template_error", fileCase(42)), causeOnly},
		{"nested file", envelope("template_error", fileCase(map[string]any{"x": "y"})), causeOnly},
		{"other cause", envelope("template_error", map[string]any{"cause": "parse_error", "file": "a.json"}), nil},
		{"unknown code", envelope("something_else", map[string]any{"cause": "file_not_found", "file": "a.json"}), nil},
		{"accepted reason", envelope("local_worktree_refused", map[string]any{"reason": "point origin at git@example.com:acme/w.git", "other": "x"}),
			map[string]any{"reason": "point origin at git@example.com:acme/w.git"}},
		{"reason with userinfo url", envelope("local_worktree_refused", map[string]any{"reason": "origin https://user:pw@example.com/acme/w.git"}), nil},
		{"userinfo url across the cap", envelope("local_worktree_refused", map[string]any{"reason": strings.Repeat("r", 1015) + " https://user:secret@example.com/acme/w.git"}), nil},
		{"reason bounded to one line", envelope("local_worktree_refused", map[string]any{"reason": "line one\nline two\x1b[2J"}),
			map[string]any{"reason": "line oneline two[2J"}},
		{"non-string reason", envelope("local_worktree_refused", map[string]any{"reason": []string{"a"}}), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := brokerFailureDetails(tc.err)
			if tc.want == nil {
				assert.Nil(t, got)
				return
			}
			assert.Equal(t, tc.want, got)
			for _, v := range got {
				assert.NotContains(t, v, brokerBodyMarker)
			}
		})
	}
	long := brokerFailureDetails(envelope("local_worktree_refused", map[string]any{"reason": strings.Repeat("r", 2000)}))
	require.NotNil(t, long)
	assert.Equal(t, maxRelayedReasonRunes+3, len([]rune(long["reason"].(string))), "1024 runes plus \"...\"")
}

// testMintedRunID is a run id in the form the hub and broker mint
// (uuid.NewString()).
const testMintedRunID = "3f2b8c1e-9d4a-4e7b-a1c6-5d8e0f2a7b9c"

// testTokenShaped is a made-up value with the shape of an access token.
const testTokenShaped = "tok_4f9a2c7e1b8d3a6f5e0c9b2a7d4e"

// TestBrokerFailureDetails_RunMismatchCurrentRun: a run_mismatch gives only
// its currentRunId, by exact key, and only in the minted form; any other
// value is dropped, never cut and relayed.
func TestBrokerFailureDetails_RunMismatchCurrentRun(t *testing.T) {
	envelope := func(details map[string]any) error {
		raw, err := json.Marshal(map[string]any{"error": map[string]any{"code": "run_mismatch", "message": "run_mismatch", "details": details}})
		require.NoError(t, err)
		return fmt.Errorf("dispatch delete: %w", &brokerStatusError{StatusCode: http.StatusNotFound, Body: string(raw)})
	}
	got := brokerFailureDetails(envelope(map[string]any{"runId": "run-a", "currentRunId": testMintedRunID, "extra": brokerBodyMarker}))
	assert.Equal(t, map[string]any{"currentRunId": testMintedRunID}, got)
	for name, current := range map[string]any{
		"upper case":   strings.ToUpper(testMintedRunID),
		"overlong":     testMintedRunID + "0",
		"newline":      testMintedRunID[:8] + "\n" + testMintedRunID[9:],
		"path":         "/var/run/" + testMintedRunID,
		"url":          "https://example.com/" + testMintedRunID,
		"token shape":  testTokenShaped,
		"empty":        "",
		"not a string": 42,
	} {
		t.Run(name, func(t *testing.T) {
			assert.Nil(t, brokerFailureDetails(envelope(map[string]any{"currentRunId": current})))
		})
	}
	assert.Nil(t, brokerFailureDetails(envelope(map[string]any{"runId": testMintedRunID})), "only currentRunId is relayed")
}

// TestBrokerRecordText: the recorded text is the status-and-code text, the
// broker's capped and rendered message, and an allowlisted current run,
// never the raw body. A message holding a URL with userinfo, a path
// separator or a token shape, or none at all, gives the generic text; a
// message that only repeats the code is left out.
func TestBrokerRecordText(t *testing.T) {
	envelope := func(status int, code, message string, details map[string]any) *brokerStatusError {
		raw, err := json.Marshal(map[string]any{"error": map[string]any{"code": code, "message": message, "details": details}})
		require.NoError(t, err)
		return &brokerStatusError{StatusCode: status, Body: string(raw)}
	}
	const filesRefused = "the agent already has files on this broker; refusing to recreate over them"
	runtimeErr := "runtime broker returned error 500 (runtime_error)"
	generic := ": " + brokerErrorGenericMessage
	// A prefix longer than the rendered message cap, so the token sits past
	// the cut and only a check on the whole raw message can see it.
	pastCap := strings.Repeat("a ", maxBrokerMessageBytes/2+1) + testTokenShaped
	require.NotContains(t, renderBrokerText(pastCap, maxBrokerMessageBytes), testTokenShaped[:4], "setup: the token sits past the cut")
	for _, tc := range []struct {
		name string
		se   *brokerStatusError
		want string
	}{
		{"message", envelope(http.StatusConflict, "move_target_not_fresh", filesRefused, map[string]any{"x": brokerBodyMarker}),
			"runtime broker returned error 409 (move_target_not_fresh): " + filesRefused},
		{"snake case words", envelope(http.StatusInternalServerError, "runtime_error", "harness_config_unusable_for_this_profile", nil),
			runtimeErr + ": harness_config_unusable_for_this_profile"},
		{"control runes rendered", envelope(http.StatusInternalServerError, "runtime_error", "line one\nline two", nil),
			runtimeErr + ": line one\uFFFDline two"},
		{"message repeats the code", envelope(http.StatusNotFound, "run_mismatch", "run_mismatch", nil),
			"runtime broker returned error 404 (run_mismatch)"},
		{"current run", envelope(http.StatusNotFound, "run_mismatch", "run_mismatch", map[string]any{"runId": "run-a", "currentRunId": testMintedRunID}),
			"runtime broker returned error 404 (run_mismatch) (current run " + testMintedRunID + ")"},
		{"current run not minted", envelope(http.StatusNotFound, "run_mismatch", "run_mismatch", map[string]any{"currentRunId": testTokenShaped}),
			"runtime broker returned error 404 (run_mismatch)"},
		{"path", envelope(http.StatusInternalServerError, "runtime_error", "mount of /home/u/.config/creds failed", nil), runtimeErr + generic},
		{"windows path", envelope(http.StatusInternalServerError, "runtime_error", `open C:\Users\u\creds failed`, nil), runtimeErr + generic},
		{"token shape", envelope(http.StatusInternalServerError, "runtime_error", "auth failed for "+testTokenShaped, nil), runtimeErr + generic},
		{"token shape past the cap", envelope(http.StatusInternalServerError, "runtime_error", pastCap, nil), runtimeErr + generic},
		{"userinfo url", envelope(http.StatusInternalServerError, "runtime_error", "clone of https://user:secret@example.com failed", nil), runtimeErr + generic},
		{"no message", envelope(http.StatusBadGateway, "runtime_error", "", nil), "runtime broker returned error 502 (runtime_error)" + generic},
		{"not json", &brokerStatusError{StatusCode: http.StatusBadGateway, Body: "<html>" + brokerBodyMarker + "</html>"}, "runtime broker returned error 502" + generic},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := brokerRecordText(tc.se)
			assert.Equal(t, tc.want, got)
			assert.NotContains(t, got, brokerBodyMarker)
			assert.NotContains(t, got, testTokenShaped)
			assert.NotContains(t, got, "{")
		})
	}

	// dispatchFailureText keeps the hub's wrapping text around it.
	wrapped := fmt.Errorf("dispatch start failed: %w", envelope(http.StatusConflict, "move_target_not_fresh", filesRefused, nil))
	assert.Equal(t, "dispatch start failed: runtime broker returned error 409 (move_target_not_fresh): "+filesRefused, dispatchFailureText(wrapped))
}

// TestBrokerRecordText_OneReadFeedsFilterAndRender: the message the relay
// filter judges and the message that is rendered come from one read of the
// body (brokerErrorRawMessage). A body that read fails on (a type error on a
// repeated message key) gives the generic text, and a body it succeeds on
// renders exactly what brokerErrorMessage renders.
func TestBrokerRecordText_OneReadFeedsFilterAndRender(t *testing.T) {
	runtimeErr := "runtime broker returned error 500 (runtime_error)"
	bad := &brokerStatusError{StatusCode: http.StatusInternalServerError,
		Body: `{"error":{"code":"runtime_error","message":"plain words","message":5}}`}
	_, ok := bad.brokerErrorRawMessage()
	require.False(t, ok, "setup: the one read fails on this body")
	assert.Equal(t, runtimeErr+": "+brokerErrorGenericMessage, brokerRecordText(bad))
	assert.Equal(t, brokerErrorGenericMessage, bad.brokerErrorMessage())

	good := &brokerStatusError{StatusCode: http.StatusInternalServerError,
		Body: `{"error":{"code":"runtime_error","message":"line one` + "\\t" + `line two"}}`}
	raw, ok := good.brokerErrorRawMessage()
	require.True(t, ok)
	assert.Equal(t, "line one\tline two", raw)
	assert.Equal(t, runtimeErr+": "+good.brokerErrorMessage(), brokerRecordText(good))
}

// TestBrokerRecordText_FilterJudgesRenderedText: whatever message text
// brokerRecordText appends is the rendered form of the one read, and it
// passes relayableBrokerMessage itself, for inputs the renderer changes
// (tabs, newlines, control and zero-width runes, invalid bytes, text past
// the cap).
func TestBrokerRecordText_FilterJudgesRenderedText(t *testing.T) {
	for name, msg := range map[string]string{
		"tab and newline": "first\tsecond\nthird",
		"control runes":   "a\x00b\x1bc",
		"zero-width":      "split​word",
		"invalid bytes":   "bad \xff\xfe bytes",
		"past the cap":    strings.Repeat("word ", maxBrokerMessageBytes),
		"token past tab":  "auth\t" + testTokenShaped,
	} {
		t.Run(name, func(t *testing.T) {
			raw, err := json.Marshal(map[string]any{"error": map[string]any{"code": "runtime_error", "message": msg}})
			require.NoError(t, err)
			se := &brokerStatusError{StatusCode: http.StatusInternalServerError, Body: string(raw)}
			prefix := se.Error() + ": "
			got := brokerRecordText(se)
			require.True(t, strings.HasPrefix(got, prefix), got)
			emitted := strings.TrimPrefix(got, prefix)
			assert.True(t, relayableBrokerMessage(emitted), "the emitted text passes the filter: %q", emitted)
			if emitted != brokerErrorGenericMessage {
				assert.Equal(t, renderBrokerText(msg, maxBrokerMessageBytes), emitted)
			}
			assert.NotContains(t, got, testTokenShaped)
		})
	}
}

// TestDispatchFailureText_NoMatchFallback: an error that wraps a broker
// answer but writes its own text (DeleteRunMismatchError) is recorded as
// that text, unchanged, with no body bytes in it.
func TestDispatchFailureText_NoMatchFallback(t *testing.T) {
	se := &brokerStatusError{StatusCode: http.StatusNotFound,
		Body: `{"error":{"code":"run_mismatch","message":"` + brokerBodyMarker + `","details":{"currentRunId":"` + testMintedRunID + `"}}}`}
	err := &DeleteRunMismatchError{RequestedRunID: "run-a", CurrentRunID: testMintedRunID, Err: se}
	var found *brokerStatusError
	require.True(t, errors.As(err, &found), "setup: the broker answer is in the chain")
	require.NotContains(t, err.Error(), se.Error(), "setup: the wrapper's text leaves out the broker answer's text")
	got := dispatchFailureText(err)
	assert.Equal(t, err.Error(), got)
	assert.NotContains(t, got, brokerBodyMarker)
}

// TestDispatchCreateErrorResponse_RunMismatchCurrentRunOnly: the generic 502
// relays a run_mismatch's currentRunId alone, and only in the minted form
// (brokerFailureDetails). The broker's own run_mismatch is a 404, which
// answers not-found without details, so only another status reaches the 502.
func TestDispatchCreateErrorResponse_RunMismatchCurrentRunOnly(t *testing.T) {
	_ = captureSlog(t)
	body := func(current string) string {
		raw, err := json.Marshal(map[string]any{"error": map[string]any{"code": "run_mismatch", "message": "run_mismatch",
			"details": map[string]any{"runId": "run-a", "currentRunId": current, "extra": brokerBodyMarker}}})
		require.NoError(t, err)
		return string(raw)
	}
	for _, tc := range []struct {
		name        string
		status      int
		current     string
		wantStatus  int
		wantDetails map[string]any
	}{
		{"minted run", http.StatusConflict, testMintedRunID, http.StatusBadGateway, map[string]any{"currentRunId": testMintedRunID}},
		{"run not minted", http.StatusConflict, testTokenShaped, http.StatusBadGateway, nil},
		{"broker 404", http.StatusNotFound, testMintedRunID, http.StatusNotFound, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			dispatchCreateErrorResponse(w, &brokerStatusError{StatusCode: tc.status, Body: body(tc.current)}, "")
			require.Equal(t, tc.wantStatus, w.Code, w.Body.String())
			assert.NotContains(t, w.Body.String(), brokerBodyMarker)
			assert.NotContains(t, w.Body.String(), "run-a")
			assert.NotContains(t, w.Body.String(), testTokenShaped)
			var resp ErrorResponse
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
			assert.Equal(t, tc.wantDetails, resp.Error.Details)
		})
	}
}

// TestDispatchCreateErrorResponse_RelaysAllowlistedDetails: the generic 502
// carries the status-and-code text and the allowlisted details as
// structured error details. The texts are the ones cmd
// TestWrapHubError_CurrentHubText feeds to the CLI.
func TestDispatchCreateErrorResponse_RelaysAllowlistedDetails(t *testing.T) {
	_ = captureSlog(t)
	reason := "cannot create a local worktree for this agent: the project's checkout at /home/u/r has origin " +
		"https://example.com/acme/other.git, but the hub project's repository is https://example.com/acme/widgets.git; " +
		"point origin at the hub project's repository, or link this checkout as its own project"
	for _, tc := range []struct {
		name        string
		status      int
		body        string
		wantMessage string
		wantDetails map[string]any
	}{
		{"template file", http.StatusUnprocessableEntity,
			`{"error":{"code":"template_error","message":"` + brokerBodyMarker + `","details":{"cause":"file_not_found","file":"settings.json"}}}`,
			"Failed to dispatch to runtime broker: runtime broker returned error 422 (template_error)",
			map[string]any{"cause": "file_not_found", "file": "settings.json"}},
		{"worktree reason", http.StatusConflict,
			`{"error":{"code":"local_worktree_refused","message":"` + brokerBodyMarker + `","details":{"reason":"` + reason + `"}}}`,
			"Failed to dispatch to runtime broker: runtime broker returned error 409 (local_worktree_refused)",
			map[string]any{"reason": reason}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			dispatchCreateErrorResponse(w, &brokerStatusError{StatusCode: tc.status, Body: tc.body}, "")
			require.Equal(t, http.StatusBadGateway, w.Code, w.Body.String())
			assert.NotContains(t, w.Body.String(), brokerBodyMarker)
			var resp ErrorResponse
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
			assert.Equal(t, ErrCodeRuntimeError, resp.Error.Code)
			assert.Equal(t, tc.wantMessage, resp.Error.Message)
			assert.Equal(t, tc.wantDetails, resp.Error.Details)
		})
	}
}

// TestRelayableStreamCloseReason_InputOverflow: the broker's input-overflow
// close reason is a fixed protocol token and reaches the client close frame.
func TestRelayableStreamCloseReason_InputOverflow(t *testing.T) {
	_ = captureSlog(t)
	assert.Equal(t, wsprotocol.CloseReasonInputOverflow,
		relayableStreamCloseReason("runtime_broker", 1009, wsprotocol.CloseReasonInputOverflow))
}

// TestRelayableStreamCloseReason_TokenWithDetail: a "<token>: <detail>"
// reason whose token is exactly an allowlisted token relays the token only
// (split once on the first ": "); every other form relays "".
func TestRelayableStreamCloseReason_TokenWithDetail(t *testing.T) {
	for _, tc := range []struct {
		name, reason, want string
		wantDetailLog      bool
	}{
		{"bare token", "relay_restart", "relay_restart", false},
		{"token with detail", "relay_restart: drain deadline " + brokerBodyMarker, "relay_restart", true},
		{"detail containing the separator", "relay_restart: a: b " + brokerBodyMarker, "relay_restart", true},
		{"unknown token with detail", "not_a_token: " + brokerBodyMarker, "", false},
		{"token with empty detail", "relay_restart: ", "", false},
		{"text before the token", "x relay_restart: " + brokerBodyMarker, "", false},
		{"case differs", "Relay_Restart: " + brokerBodyMarker, "", false},
		{"drain deadline", conduit.CloseReasonDrainDeadline, "drain deadline", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			buf := captureSlog(t)
			got := relayableStreamCloseReason("conduit", 4503, tc.reason)
			assert.Equal(t, tc.want, got)
			assert.NotContains(t, got, brokerBodyMarker)
			if tc.wantDetailLog {
				logs := buf.String()
				assert.Contains(t, logs, "stream close reason detail dropped", "the dropped detail is logged")
				assert.Contains(t, logs, brokerBodyMarker, "the dropped detail goes to the hub log")
				assert.NotContains(t, logs, "stream close reason replaced", "a token with detail is not a replaced reason")
			}
		})
	}
}
