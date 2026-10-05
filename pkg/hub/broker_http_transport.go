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
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agentkeys"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

type brokerRequestSigner interface {
	Sign(context.Context, *http.Request, string) error
}

type hmacBrokerSigner struct {
	store store.Store
}

func (s *hmacBrokerSigner) Sign(ctx context.Context, req *http.Request, brokerID string) error {
	secret, err := s.store.GetBrokerSecret(ctx, brokerID)
	if err != nil {
		return fmt.Errorf("failed to get broker secret: %w", err)
	}
	if secret.Status != store.BrokerSecretStatusActive {
		return fmt.Errorf("broker secret is %s", secret.Status)
	}
	if !secret.ExpiresAt.IsZero() && time.Now().After(secret.ExpiresAt) {
		return fmt.Errorf("broker secret has expired")
	}

	auth := &apiclient.HMACAuth{
		BrokerID:  brokerID,
		SecretKey: secret.SecretKey,
	}
	return auth.ApplyAuth(req)
}

// brokerHTTPTransport centralizes broker HTTP dispatch and response handling.
// Optional signing is injected through brokerRequestSigner.
type brokerHTTPTransport struct {
	client *http.Client
	// keysClient is a redirect-refusing variant of client, sharing the same
	// connection pool. Keys dispatch must never follow an HTTP redirect
	// replay (.design/agent-keys-contract.md §4.3, agentkeys.
	// ClassifyDispatchError's "no automatic replay" rule): a 3xx response is
	// treated as the final response, not a cue to resend the request body to
	// a different URL.
	keysClient *http.Client
	debug      bool
	signer     brokerRequestSigner
}

func newBrokerHTTPTransport(debug bool, signer brokerRequestSigner) *brokerHTTPTransport {
	transport := otelhttp.NewTransport(http.DefaultTransport)
	return &brokerHTTPTransport{
		client: &http.Client{
			Transport: transport,
			Timeout:   120 * time.Second,
		},
		keysClient: &http.Client{
			Transport: transport,
			Timeout:   120 * time.Second,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		debug:  debug,
		signer: signer,
	}
}

func (t *brokerHTTPTransport) doRequest(ctx context.Context, brokerID, method, endpoint string, body []byte) (*http.Response, error) {
	if endpoint == "" || !strings.Contains(endpoint, "://") {
		return nil, fmt.Errorf("runtime broker %q has no HTTP endpoint configured (control channel may be required): %w", brokerID, errStartRequestNotSent)
	}

	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}

	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w (%w)", err, errStartRequestNotSent)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	if t.signer != nil {
		if err := t.signer.Sign(ctx, req, brokerID); err != nil {
			if t.debug {
				slog.Warn("Failed to sign request", "brokerID", brokerID, "error", err)
			}
			return nil, fmt.Errorf("failed to sign request: %w (%w)", err, errStartRequestNotSent)
		}
	}

	if t.debug {
		slog.Debug("Outgoing request to broker", "method", method, "endpoint", endpoint)
	}
	return t.client.Do(req)
}

func (t *brokerHTTPTransport) decodeResponse(resp *http.Response, out interface{}) error {
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("failed to decode response: %w", err)
	}
	return nil
}

func (t *brokerHTTPTransport) decodeResponseWithSnippet(resp *http.Response, out interface{}) error {
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read response body: %w", err)
	}
	if err := json.Unmarshal(respBody, out); err != nil {
		bodySnippet := string(respBody)
		if len(bodySnippet) > 256 {
			bodySnippet = bodySnippet[:256] + "...(truncated)"
		}
		return fmt.Errorf("failed to decode response: %w (body=%q)", err, bodySnippet)
	}
	return nil
}

// maxBrokerErrorBodyBytes caps how much of a broker's error response body is
// read into a brokerStatusError (ptone/scion#1841). Broker error bodies are
// small JSON envelopes ({"error":{"code","message","details"}}) that
// isBrokerAgentNotFound / brokerErrorMessage / brokerErrorDetails decode, so
// 64KiB leaves two orders of magnitude of headroom for legitimate bodies
// while bounding what a misbehaving or compromised broker can make the hub
// buffer (and then carry in error text) per failed request. A body over the
// cap is truncated; JSON decoding of it then fails and callers fall back to
// the status code, which is the safe behaviour for an oversized error.
const maxBrokerErrorBodyBytes = 64 << 10

func brokerHTTPError(resp *http.Response) error {
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxBrokerErrorBodyBytes))
	return &brokerStatusError{StatusCode: resp.StatusCode, Body: string(respBody), RetryAfter: resp.Header.Get("Retry-After")}
}

func (t *brokerHTTPTransport) CreateAgent(ctx context.Context, brokerID, brokerEndpoint string, req *RemoteCreateAgentRequest) (*RemoteAgentResponse, error) {
	endpoint := fmt.Sprintf("%s/api/v1/agents", strings.TrimSuffix(brokerEndpoint, "/"))
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}
	resp, err := t.doRequest(ctx, brokerID, http.MethodPost, endpoint, body)
	if err != nil {
		return nil, fmt.Errorf("failed to send request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		return nil, brokerHTTPError(resp)
	}
	var result RemoteAgentResponse
	if err := t.decodeResponse(resp, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func (t *brokerHTTPTransport) StartAgent(ctx context.Context, brokerID, brokerEndpoint, agentID, projectID, task, projectPath, projectSlug, harnessConfig, harnessConfigID, harnessConfigHash string, resolvedEnv map[string]string, resolvedSecrets []ResolvedSecret, inlineConfig *api.ScionConfig, sharedDirs []api.SharedDir, sharedWorkspace, resume bool, extras StartExtras) (*RemoteAgentResponse, error) {
	endpoint := fmt.Sprintf("%s/api/v1/agents/%s/start", strings.TrimSuffix(brokerEndpoint, "/"), url.PathEscape(agentID))
	if projectID != "" {
		endpoint += "?projectId=" + url.QueryEscape(projectID)
	}
	payload := map[string]interface{}{}
	if task != "" {
		payload["task"] = task
	}
	if projectPath != "" {
		payload["projectPath"] = projectPath
	}
	if projectSlug != "" {
		payload["projectSlug"] = projectSlug
	}
	if harnessConfig != "" {
		payload["harnessConfig"] = harnessConfig
	}
	if harnessConfigID != "" {
		payload["harnessConfigId"] = harnessConfigID
	}
	if harnessConfigHash != "" {
		payload["harnessConfigHash"] = harnessConfigHash
	}
	if len(resolvedEnv) > 0 {
		payload["resolvedEnv"] = resolvedEnv
	}
	if len(resolvedSecrets) > 0 {
		payload["resolvedSecrets"] = resolvedSecrets
	}
	if inlineConfig != nil {
		payload["inlineConfig"] = inlineConfig
	}
	if len(sharedDirs) > 0 {
		payload["sharedDirs"] = sharedDirs
	}
	if sharedWorkspace {
		payload["sharedWorkspace"] = true
	}
	if resume {
		payload["resume"] = true
	}
	// Carry the same dispatch-time metadata create sends, so the broker can
	// attach a working skill resolver and recreate the workspace on every
	// path that can reach ProvisionAgent, not just create.
	applyStartExtras(payload, extras)

	var body []byte
	if len(payload) > 0 {
		var err error
		body, err = json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal request: %w (%w)", err, errStartRequestNotSent)
		}
	}

	resp, err := t.doRequest(ctx, brokerID, http.MethodPost, endpoint, body)
	if err != nil {
		return nil, fmt.Errorf("failed to send request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		return nil, brokerHTTPError(resp)
	}

	var result RemoteAgentResponse
	if err := t.decodeResponseWithSnippet(resp, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func (t *brokerHTTPTransport) StopAgent(ctx context.Context, brokerID, brokerEndpoint, agentID, projectID string) error {
	endpoint := fmt.Sprintf("%s/api/v1/agents/%s/stop", strings.TrimSuffix(brokerEndpoint, "/"), url.PathEscape(agentID))
	if projectID != "" {
		endpoint += "?projectId=" + url.QueryEscape(projectID)
	}
	endpoint = withRecordedRuntimeURL(ctx, endpoint)
	resp, err := t.doRequest(ctx, brokerID, http.MethodPost, endpoint, nil)
	if err != nil {
		return fmt.Errorf("failed to send request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		return brokerHTTPError(resp)
	}
	return nil
}

func (t *brokerHTTPTransport) RestartAgent(ctx context.Context, brokerID, brokerEndpoint, agentID, projectID string, resolvedEnv map[string]string, extras StartExtras) (*RemoteAgentResponse, error) {
	endpoint := fmt.Sprintf("%s/api/v1/agents/%s/restart", strings.TrimSuffix(brokerEndpoint, "/"), url.PathEscape(agentID))
	if projectID != "" {
		endpoint += "?projectId=" + url.QueryEscape(projectID)
	}
	endpoint = withRecordedRuntimeURL(ctx, endpoint)
	payload := map[string]interface{}{}
	if len(resolvedEnv) > 0 {
		payload["resolvedEnv"] = resolvedEnv
	}
	// Carry the same dispatch-time metadata the create/start paths send, so
	// the broker can attach a working skill resolver when restart
	// (re-)provisions the agent.
	applyStartExtras(payload, extras)
	var body []byte
	if len(payload) > 0 {
		var err error
		body, err = json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal restart request: %w", err)
		}
	}
	resp, err := t.doRequest(ctx, brokerID, http.MethodPost, endpoint, body)
	if err != nil {
		return nil, fmt.Errorf("failed to send request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		return nil, brokerHTTPError(resp)
	}
	// The broker answers a restart with the entry it started (or found
	// still running), including its run ID. An empty or undecodable body
	// (an older broker) is not an error: the restart succeeded.
	var result RemoteAgentResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, nil
	}
	return &result, nil
}

// resetAuthBody builds the broker reset-auth request body. The transport
// token is included only when the hub minted one.
func resetAuthBody(token, transportToken string) map[string]string {
	body := map[string]string{"token": token}
	if transportToken != "" {
		body["transportToken"] = transportToken
	}
	return body
}

func (t *brokerHTTPTransport) ResetAuthAgent(ctx context.Context, brokerID, brokerEndpoint, agentID, projectID, token, transportToken string) error {
	endpoint := fmt.Sprintf("%s/api/v1/agents/%s/reset-auth", strings.TrimSuffix(brokerEndpoint, "/"), url.PathEscape(agentID))
	if projectID != "" {
		endpoint += "?projectId=" + url.QueryEscape(projectID)
	}
	endpoint = withRecordedRuntimeURL(ctx, endpoint)
	body, err := json.Marshal(resetAuthBody(token, transportToken))
	if err != nil {
		return fmt.Errorf("failed to marshal reset-auth request: %w", err)
	}
	resp, err := t.doRequest(ctx, brokerID, http.MethodPost, endpoint, body)
	if err != nil {
		return fmt.Errorf("failed to send request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		return brokerHTTPError(resp)
	}
	return nil
}

func (t *brokerHTTPTransport) DeleteAgent(ctx context.Context, brokerID, brokerEndpoint, agentID, projectID string, opts DeleteAgentOptions) error {
	endpoint := fmt.Sprintf("%s/api/v1/agents/%s?%s",
		strings.TrimSuffix(brokerEndpoint, "/"), url.PathEscape(agentID), deleteAgentQuery(ctx, projectID, opts))

	resp, err := t.doRequest(ctx, brokerID, http.MethodDelete, endpoint, nil)
	if err != nil {
		return fmt.Errorf("failed to send request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 && resp.StatusCode != http.StatusNotFound {
		return brokerHTTPError(resp)
	}
	return nil
}

func (t *brokerHTTPTransport) MessageAgent(ctx context.Context, brokerID, brokerEndpoint, agentID, projectID, message string, interrupt bool, structuredMsg *messages.StructuredMessage) error {
	endpoint := fmt.Sprintf("%s/api/v1/agents/%s/message", strings.TrimSuffix(brokerEndpoint, "/"), url.PathEscape(agentID))
	if projectID != "" {
		endpoint += "?projectId=" + url.QueryEscape(projectID)
	}
	endpoint = withRecordedRuntimeURL(ctx, endpoint)

	// Build the request body with structured message if available
	reqBody := map[string]interface{}{
		"interrupt": interrupt,
	}
	if projectID != "" {
		reqBody["project_id"] = projectID
	}
	if structuredMsg != nil {
		reqBody["structured_message"] = structuredMsg
		// Phase 9b(i): promote DeliveryText to the top-level wire field
		// so the broker can prefer it without parsing StructuredMessage.
		if structuredMsg.DeliveryText != "" {
			reqBody["delivery_text"] = structuredMsg.DeliveryText
		}
	} else {
		reqBody["message"] = message
	}
	// #1820: carry the persisted hub message ID so the broker can report a
	// buffered-delivery failure back against the right row.
	if msgID := dispatchMessageIDFromContext(ctx); msgID != "" {
		reqBody["message_id"] = msgID
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return fmt.Errorf("failed to marshal request: %w", err)
	}
	resp, err := t.doRequest(ctx, brokerID, http.MethodPost, endpoint, body)
	if err != nil {
		return fmt.Errorf("failed to send request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		return brokerHTTPError(resp)
	}
	return nil
}

// maxKeysResponseBodyBytes bounds how much of a broker's keys response body
// ExecuteKeys will read. agentkeys.BrokerResult is a small, fixed-shape JSON
// value (operation_id, outcome, an optional human-readable message that must
// never carry key content); this ceiling gives generous headroom over that
// shape's realistic worst case while bounding memory use against a
// misbehaving or compromised broker that returns an arbitrarily large
// response. A response that is truncated by this limit will simply fail to
// parse as a BrokerResult, which decodeBrokerKeysResponse already treats as
// an honest "outcome unknown" rather than a false success or a crash.
const maxKeysResponseBodyBytes = 64 * 1024

// ExecuteKeys dispatches a typed keys request to a runtime broker's dedicated
// keys route directly over HTTP. It is single-attempt (no retry, no redirect
// following via keysClient) and returns agentkeys.ErrNotDispatched only for
// failures proven to have occurred before any bytes reached the broker; see
// classifyKeysSendError.
func (t *brokerHTTPTransport) ExecuteKeys(ctx context.Context, brokerID, brokerEndpoint, agentSlug string, req agentkeys.BrokerRequest) (agentkeys.BrokerResult, error) {
	if brokerEndpoint == "" || !strings.Contains(brokerEndpoint, "://") {
		return agentkeys.BrokerResult{}, fmt.Errorf("%w: runtime broker %q has no HTTP endpoint configured (control channel may be required)", agentkeys.ErrNotDispatched, brokerID)
	}

	// The broker compares ExecuteBefore against its own UTC clock; a caller
	// that marshaled a non-UTC time.Time would encode its local zone offset
	// instead, per BrokerRequest.ExecuteBefore's doc comment.
	req.ExecuteBefore = req.ExecuteBefore.UTC()
	body, err := json.Marshal(req)
	if err != nil {
		// Proven before anything was sent: no request was ever constructed.
		return agentkeys.BrokerResult{}, fmt.Errorf("%w: failed to marshal keys request: %w", agentkeys.ErrNotDispatched, err)
	}

	path := strings.ReplaceAll(agentkeys.BrokerRoutePath, "{id}", url.PathEscape(agentSlug))
	endpoint := fmt.Sprintf("%s%s?%s=%s", strings.TrimSuffix(brokerEndpoint, "/"), path,
		agentkeys.BrokerProjectIDQueryParam, url.QueryEscape(req.ProjectID))
	endpoint = withRecordedRuntimeURL(ctx, endpoint)

	httpReq, err := http.NewRequestWithContext(ctx, agentkeys.BrokerRouteMethod, endpoint, bytes.NewReader(body))
	if err != nil {
		return agentkeys.BrokerResult{}, fmt.Errorf("%w: failed to create request: %w", agentkeys.ErrNotDispatched, err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if t.signer != nil {
		if err := t.signer.Sign(ctx, httpReq, brokerID); err != nil {
			if t.debug {
				slog.Warn("Failed to sign keys request", "brokerID", brokerID, "error", err)
			}
			// A signing failure (e.g. missing/expired broker secret) never
			// puts a byte on the wire.
			return agentkeys.BrokerResult{}, fmt.Errorf("%w: failed to sign request: %w", agentkeys.ErrNotDispatched, err)
		}
	}

	// http.NewRequestWithContext populates GetBody for a []byte-backed
	// reader so the net/http machinery can re-read the body for a transparent
	// resend. Over HTTP/2, http2shouldRetryRequest resends the request on a
	// new stream after a RST_STREAM(PROTOCOL_ERROR) whenever GetBody != nil
	// (golang/go#47635) — a replay the broker may already have started
	// executing, underneath this adapter's own single-attempt logic. Clearing
	// GetBody removes that retry path: HTTP/2 then only retries on
	// errClientConnUnusable, which is itself proven pre-send (the connection
	// was never usable), and the graceful-GOAWAY-then-resend case is instead
	// surfaced as an uncertain (keys_outcome_unknown) send error, which is the
	// honest answer per contract §4.3. Do not "fix" this by adding an
	// idempotency header instead: that would make HTTP/1.1 treat the request
	// as safely replayable too, which is the opposite of what this needs.
	//
	// This is set last, immediately before Do, after every other request
	// mutation (including signing): today's HMAC signer only reads and
	// restores Body and never touches GetBody, but setting this earlier would
	// silently stop protecting against a future signer that rebuilds the
	// request (e.g. via http.NewRequest) or otherwise repopulates GetBody.
	httpReq.GetBody = nil

	if t.debug {
		slog.Debug("Outgoing keys request to broker", "method", agentkeys.BrokerRouteMethod, "endpoint", endpoint)
	}

	// Record whether the transport ever handed this request a connection.
	// Until GotConn fires, net/http has not written a single byte of the
	// request (HTTP/1 calls it from getConn before writeLoop sees the
	// request; HTTP/2 calls it before cc.RoundTrip encodes any frame), so a
	// failure with no connection is proven pre-send. This covers the cases
	// a bare dial *net.OpError does not: a dial still pending when the
	// request context or the client timeout expires (net/http then returns
	// only the context error), and a TLS handshake failure.
	var gotConn atomic.Bool
	httpReq = httpReq.WithContext(httptrace.WithClientTrace(httpReq.Context(), &httptrace.ClientTrace{
		GotConn: func(httptrace.GotConnInfo) { gotConn.Store(true) },
	}))

	resp, err := t.keysClient.Do(httpReq)
	if err != nil {
		return agentkeys.BrokerResult{}, classifyKeysSendError(err, gotConn.Load())
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxKeysResponseBodyBytes))
	if err != nil {
		return agentkeys.BrokerResult{}, fmt.Errorf("keys: failed to read broker response: %w", err)
	}
	return decodeBrokerKeysResponse(resp.StatusCode, respBody, req.OperationID)
}

// classifyKeysSendError turns a transport-level send failure into
// agentkeys.ErrNotDispatched only when the failure provably occurred before
// any bytes reached the broker: either the transport never obtained a
// connection for the request (gotConn is false: a refused or still-pending
// dial, a DNS failure, a TLS handshake failure, or the request context
// expiring while waiting for a connection), or the error is itself a dial
// failure. Any other transport error (a timeout waiting for a response, a
// connection reset while reading one, a write failure on an established
// connection) does not prove the broker never received or began acting on
// the request, so it must not be reported as a definite non-dispatch: it is
// returned as a plain, unclassified error instead, which
// agentkeys.ClassifyDispatchError maps to OutcomeKeysOutcomeUnknown — the
// honest "may have run" outcome required by
// .design/agent-keys-contract.md §2.5/§4.3.
func classifyKeysSendError(err error, gotConn bool) error {
	var opErr *net.OpError
	if !gotConn || (errors.As(err, &opErr) && opErr.Op == "dial") {
		return fmt.Errorf("%w: %w", agentkeys.ErrNotDispatched, err)
	}
	return fmt.Errorf("keys: uncertain dispatch outcome: %w", err)
}

func (t *brokerHTTPTransport) CheckAgentPrompt(ctx context.Context, brokerID, brokerEndpoint, agentID, projectID string) (bool, error) {
	endpoint := fmt.Sprintf("%s/api/v1/agents/%s/has-prompt", strings.TrimSuffix(brokerEndpoint, "/"), url.PathEscape(agentID))
	if projectID != "" {
		endpoint += "?projectId=" + url.QueryEscape(projectID)
	}
	endpoint = withRecordedRuntimeURL(ctx, endpoint)
	resp, err := t.doRequest(ctx, brokerID, http.MethodPost, endpoint, nil)
	if err != nil {
		return false, fmt.Errorf("failed to send request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		return false, brokerHTTPError(resp)
	}
	var result HasPromptResponse
	if err := t.decodeResponse(resp, &result); err != nil {
		return false, err
	}
	return result.HasPrompt, nil
}

func (t *brokerHTTPTransport) CreateAgentWithGather(ctx context.Context, brokerID, brokerEndpoint string, req *RemoteCreateAgentRequest) (*RemoteAgentResponse, *RemoteEnvRequirementsResponse, error) {
	endpoint := fmt.Sprintf("%s/api/v1/agents", strings.TrimSuffix(brokerEndpoint, "/"))
	body, err := json.Marshal(req)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to marshal request: %w", err)
	}
	resp, err := t.doRequest(ctx, brokerID, http.MethodPost, endpoint, body)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to send request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		return nil, nil, brokerHTTPError(resp)
	}

	if resp.StatusCode == http.StatusAccepted {
		var envReqs RemoteEnvRequirementsResponse
		if err := t.decodeResponse(resp, &envReqs); err != nil {
			return nil, nil, fmt.Errorf("failed to decode env requirements: %w", err)
		}
		return nil, &envReqs, nil
	}

	var result RemoteAgentResponse
	if err := t.decodeResponse(resp, &result); err != nil {
		return nil, nil, err
	}
	return &result, nil, nil
}

func (t *brokerHTTPTransport) GetAgentLogs(ctx context.Context, brokerID, brokerEndpoint, agentID, projectID string, tail int) (string, error) {
	endpoint := fmt.Sprintf("%s/api/v1/agents/%s/logs", strings.TrimSuffix(brokerEndpoint, "/"), url.PathEscape(agentID))
	sep := "?"
	if tail > 0 {
		endpoint += fmt.Sprintf("?tail=%d", tail)
		sep = "&"
	}
	if projectID != "" {
		endpoint += sep + "projectId=" + url.QueryEscape(projectID)
	}
	endpoint = withRecordedRuntimeURL(ctx, endpoint)
	resp, err := t.doRequest(ctx, brokerID, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", fmt.Errorf("failed to send request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		return "", brokerHTTPError(resp)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read response: %w", err)
	}
	return string(body), nil
}

func (t *brokerHTTPTransport) ExecAgent(ctx context.Context, brokerID, brokerEndpoint, agentID, projectID string, command []string, timeout int) (string, int, error) {
	endpoint := fmt.Sprintf("%s/api/v1/agents/%s/exec", strings.TrimSuffix(brokerEndpoint, "/"), url.PathEscape(agentID))
	if projectID != "" {
		endpoint += "?projectId=" + url.QueryEscape(projectID)
	}
	endpoint = withRecordedRuntimeURL(ctx, endpoint)

	body, err := json.Marshal(map[string]interface{}{
		"command": command,
		"timeout": timeout,
	})
	if err != nil {
		return "", 0, fmt.Errorf("failed to marshal request: %w", err)
	}

	resp, err := t.doRequest(ctx, brokerID, http.MethodPost, endpoint, body)
	if err != nil {
		return "", 0, fmt.Errorf("failed to send request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		return "", 0, brokerHTTPError(resp)
	}

	var result struct {
		Output   string `json:"output"`
		ExitCode int    `json:"exitCode"`
	}
	if err := t.decodeResponse(resp, &result); err != nil {
		return "", 0, err
	}
	return result.Output, result.ExitCode, nil
}

// BrokerUnsupportedError indicates that a broker responded but does not support
// the requested endpoint (e.g. an old broker that hasn't been upgraded yet).
type BrokerUnsupportedError struct {
	StatusCode int
}

func (e *BrokerUnsupportedError) Error() string {
	return fmt.Sprintf("broker does not support this endpoint (HTTP %d)", e.StatusCode)
}

// BrokerImageStatusResponse is the response from a broker's /api/v1/images/status endpoint.
type BrokerImageStatusResponse struct {
	LocalShort *BrokerImageEntityState `json:"local_short,omitempty"`
	LocalLong  *BrokerImageEntityState `json:"local_long,omitempty"`
}

// BrokerImageEntityState describes the local state of a single image entity on a broker.
type BrokerImageEntityState struct {
	Exists bool   `json:"exists"`
	Hash   string `json:"hash,omitempty"`
}

func (t *brokerHTTPTransport) ImageStatus(ctx context.Context, brokerID, brokerEndpoint, shortImage, longImage string) (*BrokerImageStatusResponse, error) {
	query := url.Values{}
	if shortImage != "" {
		query.Set("short", shortImage)
	}
	if longImage != "" {
		query.Set("long", longImage)
	}
	endpoint := fmt.Sprintf("%s/api/v1/images/status?%s", strings.TrimSuffix(brokerEndpoint, "/"), query.Encode())

	resp, err := t.doRequest(ctx, brokerID, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotFound {
		return nil, &BrokerUnsupportedError{StatusCode: resp.StatusCode}
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("broker returned status %d", resp.StatusCode)
	}

	var result BrokerImageStatusResponse
	if err := t.decodeResponse(resp, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func (t *brokerHTTPTransport) PullImage(ctx context.Context, brokerID, brokerEndpoint, image string) error {
	endpoint := fmt.Sprintf("%s/api/v1/images/pull", strings.TrimSuffix(brokerEndpoint, "/"))
	body, err := json.Marshal(map[string]string{"image": image})
	if err != nil {
		return fmt.Errorf("failed to marshal request: %w", err)
	}
	resp, err := t.doRequest(ctx, brokerID, http.MethodPost, endpoint, body)
	if err != nil {
		return fmt.Errorf("failed to send request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		return brokerHTTPError(resp)
	}
	return nil
}

func (t *brokerHTTPTransport) DeleteImage(ctx context.Context, brokerID, brokerEndpoint, image string) error {
	endpoint := fmt.Sprintf("%s/api/v1/images/local", strings.TrimSuffix(brokerEndpoint, "/"))
	body, err := json.Marshal(map[string]string{"image": image})
	if err != nil {
		return fmt.Errorf("failed to marshal request: %w", err)
	}
	resp, err := t.doRequest(ctx, brokerID, http.MethodDelete, endpoint, body)
	if err != nil {
		return fmt.Errorf("failed to send request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		return brokerHTTPError(resp)
	}
	return nil
}

func (t *brokerHTTPTransport) CleanupProject(ctx context.Context, brokerID, brokerEndpoint, projectSlug, projectID string) error {
	endpoint := fmt.Sprintf("%s/api/v1/projects/%s", strings.TrimSuffix(brokerEndpoint, "/"), url.PathEscape(projectSlug))
	if projectID != "" {
		endpoint += "?project_id=" + url.QueryEscape(projectID)
	}
	resp, err := t.doRequest(ctx, brokerID, http.MethodDelete, endpoint, nil)
	if err != nil {
		return fmt.Errorf("failed to send request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 && resp.StatusCode != http.StatusNotFound {
		return brokerHTTPError(resp)
	}
	return nil
}
