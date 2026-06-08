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
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/lifecyclehooks"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Compile-time interface compliance check.
var _ LifecycleHookExecutor = (*HTTPExecutor)(nil)

// maxRetryAttempts is the fixed maximum number of attempts for on_error=retry.
// After all attempts are exhausted, the executor falls back to "log" behavior.
const maxRetryAttempts = 3

// defaultTimeoutSeconds is the per-attempt HTTP timeout when the action does
// not specify one (or specifies 0). The M1 validator caps TimeoutSeconds at 30.
const defaultTimeoutSeconds = 10

// HTTPExecutor implements LifecycleHookExecutor by rendering the action
// template with the variable guard, resolving the execution identity (project
// SA) to an access token, and executing the HTTP request with timeout, retry,
// and audit.
//
// Security invariants:
//   - Initial-URL connections to loopback (127.0.0.0/8, ::1) and link-local
//     (169.254.0.0/16, fe80::/10) addresses are blocked at the dialer level
//     (SSRF protection). The dialer resolves the hostname, selects the first
//     non-blocked IP, and dials THAT SPECIFIC IP — never the original
//     hostname. This closes the DNS-rebinding TOCTOU window: the TCP
//     connection is made only to a validated, non-blocked IP.
//     RFC1918 addresses (10/8, 172.16/12, 192.168/16) are intentionally
//     ALLOWED for internal service registries.
//   - All redirects are blocked (SSRF protection via redirect).
//   - SA tokens are attached ONLY for action.Type == "http"; webhooks send
//     unauthenticated (the URL carries its own token).
//   - SA tokens/auth headers NEVER come from hook variables — they are injected
//     directly by the executor after rendering.
//   - Response bodies are NEVER recorded in the audit log.
//   - Rendered Authorization header values are NEVER recorded in the audit log.
type HTTPExecutor struct {
	store       store.Store
	tokenGen    GCPTokenGenerator
	auditLogger AuditLogger
	log         *slog.Logger

	// newHTTPClient creates the http.Client used for hook requests.
	// Defaults to newSSRFSafeClient. Tests may override this to inject
	// a client that allows loopback connections for httptest servers.
	newHTTPClient func() *http.Client

	// client is the lazily-initialized, shared http.Client reused across all
	// executions. http.Transport maintains an internal connection pool, so a
	// single client must be reused to enable connection reuse and avoid
	// socket/file-descriptor exhaustion under load.
	client     *http.Client
	clientOnce sync.Once
}

// httpClient lazily initializes and returns the shared http.Client. It is
// safe for concurrent use.
func (e *HTTPExecutor) httpClient() *http.Client {
	e.clientOnce.Do(func() {
		if e.newHTTPClient != nil {
			e.client = e.newHTTPClient()
		} else {
			e.client = newSSRFSafeClient()
		}
	})
	return e.client
}

// NewHTTPExecutor creates a new HTTPExecutor.
func NewHTTPExecutor(s store.Store, tokenGen GCPTokenGenerator, auditLogger AuditLogger, log *slog.Logger) *HTTPExecutor {
	if log == nil {
		log = slog.Default()
	}
	return &HTTPExecutor{
		store:         s,
		tokenGen:      tokenGen,
		auditLogger:   auditLogger,
		log:           log,
		newHTTPClient: newSSRFSafeClient,
	}
}

// Execute performs the HTTP/webhook action defined in the hook for the given
// agent and trigger. It resolves the execution identity, renders the action
// template, executes the request with the configured timeout and retry policy,
// and records an audit event for each attempt.
//
// Execute MUST NOT panic. Errors are returned but never propagated to the
// transition path (the evaluator isolates this).
func (e *HTTPExecutor) Execute(ctx context.Context, hook *store.LifecycleHook, agent *store.Agent, trigger string) error {
	if hook.Action == nil {
		return fmt.Errorf("hook %s has no action defined", hook.ID)
	}

	action := hook.Action

	// -----------------------------------------------------------------------
	// 1. Resolve execution identity -> SA email -> access token
	// -----------------------------------------------------------------------
	saEmail, bearerToken, err := e.resolveIdentityAndToken(ctx, hook, action)
	if err != nil {
		e.recordAudit(ctx, hook, agent, trigger, saEmail, action, 0, 0, 1, err)
		return fmt.Errorf("resolve execution identity: %w", err)
	}

	// -----------------------------------------------------------------------
	// 2. Build render variables and render the action template
	// -----------------------------------------------------------------------
	vars := e.buildRenderVars(ctx, hook, agent, trigger, saEmail)
	rendered := lifecyclehooks.RenderAction(action, vars)

	// -----------------------------------------------------------------------
	// 3. Determine retry policy
	// -----------------------------------------------------------------------
	onError := rendered.OnError
	if onError == "" {
		onError = store.LifecycleHookOnErrorLog
	}

	attempts := 1
	if onError == store.LifecycleHookOnErrorRetry {
		attempts = maxRetryAttempts
	}

	// -----------------------------------------------------------------------
	// 4. Use the shared SSRF-safe HTTP client (connection pool reused across
	//    attempts AND across executions).
	// -----------------------------------------------------------------------
	client := e.httpClient()

	// -----------------------------------------------------------------------
	// 5. Execute with timeout + retry
	// -----------------------------------------------------------------------
	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		statusCode, latency, attemptErr := e.doHTTPRequest(ctx, client, rendered, bearerToken, action.Type)

		success := attemptErr == nil && statusCode >= 200 && statusCode < 300

		// Record audit for every attempt.
		e.recordAudit(ctx, hook, agent, trigger, saEmail, action, statusCode, latency, attempt, attemptErr)

		if success {
			return nil
		}

		lastErr = attemptErr
		if lastErr == nil {
			lastErr = fmt.Errorf("HTTP %d", statusCode)
		}

		// 4xx responses are non-retryable — record and return immediately.
		if statusCode >= 400 && statusCode < 500 {
			e.log.Warn("Lifecycle hook execution failed with non-retryable 4xx",
				"hook_id", hook.ID,
				"hook_name", hook.Name,
				"trigger", trigger,
				"agent_id", agent.ID,
				"attempt", attempt,
				"status_code", statusCode,
				"error", lastErr,
			)
			return fmt.Errorf("hook %s: non-retryable HTTP %d: %w", hook.ID, statusCode, lastErr)
		}

		e.log.Warn("Lifecycle hook execution attempt failed",
			"hook_id", hook.ID,
			"hook_name", hook.Name,
			"trigger", trigger,
			"agent_id", agent.ID,
			"attempt", attempt,
			"max_attempts", attempts,
			"status_code", statusCode,
			"error", lastErr,
		)

		// Backoff before retry (unless this was the last attempt). Use a
		// time.Timer (not time.After) so the timer is stopped on context
		// cancellation, avoiding a leaked runtime timer per cancelled request.
		if attempt < attempts {
			backoff := time.Duration(1<<uint(attempt-1)) * 500 * time.Millisecond
			timer := time.NewTimer(backoff)
			select {
			case <-timer.C:
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			}
		}
	}

	// All attempts exhausted -> fall back to log behavior (return the error
	// but never block the transition).
	return fmt.Errorf("hook %s: all %d attempts failed, last error: %w", hook.ID, attempts, lastErr)
}

// resolveIdentityAndToken resolves the execution identity to a SA email and
// obtains a bearer token via the existing GCP token generator. For webhook
// actions, no token is generated (the URL carries its own auth).
func (e *HTTPExecutor) resolveIdentityAndToken(ctx context.Context, hook *store.LifecycleHook, action *store.LifecycleHookAction) (saEmail string, bearerToken string, err error) {
	if hook.ExecutionIdentity == "" {
		// No execution identity configured. For webhooks this is fine
		// (unauthenticated). For http type, this is an error.
		if action.Type == store.LifecycleHookActionHTTP {
			return "", "", fmt.Errorf("hook %s: http action requires an execution identity", hook.ID)
		}
		return "", "", nil
	}

	// Resolve managed-SA record ID -> SA email via the store.
	sa, err := e.store.GetGCPServiceAccount(ctx, hook.ExecutionIdentity)
	if err != nil {
		return "", "", fmt.Errorf("resolve SA record %s: %w", hook.ExecutionIdentity, err)
	}
	saEmail = sa.Email

	// For webhook actions, do NOT attach the SA token.
	if action.Type == store.LifecycleHookActionWebhook {
		return saEmail, "", nil
	}

	// For http actions, obtain an access token by impersonation.
	if e.tokenGen == nil {
		return saEmail, "", fmt.Errorf("GCP token generator not configured; cannot impersonate %s", saEmail)
	}

	token, err := e.tokenGen.GenerateAccessToken(ctx, saEmail, []string{"https://www.googleapis.com/auth/cloud-platform"})
	if err != nil {
		return saEmail, "", fmt.Errorf("generate access token for %s: %w", saEmail, err)
	}

	return saEmail, token.AccessToken, nil
}

// buildRenderVars constructs the variable map for action rendering with
// correct trust classification. Trusted variables come from hub-controlled
// data only; untrusted variables come from agent/LLM-derived data.
//
// CRITICAL: agent/LLM-derived data MUST NEVER be placed into trusted variable
// names. The SA token/auth MUST NEVER come from any hook variable.
func (e *HTTPExecutor) buildRenderVars(ctx context.Context, hook *store.LifecycleHook, agent *store.Agent, trigger, saEmail string) lifecyclehooks.RenderVars {
	vars := lifecyclehooks.RenderVars{
		// TRUSTED: hub-controlled data only
		"HOOK_ID":   hook.ID,
		"HOOK_NAME": hook.Name,
		"TRIGGER":   trigger,
		"AGENT_ID":  agent.ID,
		"SA_EMAIL":  saEmail,
	}

	// Project metadata (hub-controlled).
	if agent.ProjectID != "" {
		vars["PROJECT_ID"] = agent.ProjectID
		if project, err := e.store.GetProject(ctx, agent.ProjectID); err == nil {
			vars["PROJECT_NAME"] = project.Name
		}
	}

	// Agent slug (hub-controlled identity).
	if agent.Slug != "" {
		vars["AGENT_SLUG"] = agent.Slug
	}

	// UNTRUSTED: agent/LLM-derived data. These are correctly classified by
	// the varguard (lifecyclehooks.ClassifyVar) and will be encoded at render
	// time. We NEVER place these values under trusted variable names.
	if agent.Name != "" {
		vars["AGENT_NAME"] = agent.Name
	}
	if agent.TaskSummary != "" {
		vars["TASK_SUMMARY"] = agent.TaskSummary
	}
	if agent.Phase != "" {
		vars["AGENT_STATUS"] = agent.Phase
	}
	if agent.Message != "" {
		vars["ERROR_MSG"] = agent.Message
	}

	return vars
}

// ssrfResolver abstracts DNS resolution for the SSRF-safe dialer.
// Production uses net.DefaultResolver; tests can inject a fake to control
// which IPs a hostname resolves to without real DNS.
type ssrfResolver interface {
	LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error)
}

// ssrfDialer abstracts the raw TCP dial for the SSRF-safe dialer.
// Production uses a net.Dialer; tests can inject a fake to verify which
// IP:port pairs are actually dialed.
type ssrfDialer interface {
	DialContext(ctx context.Context, network, addr string) (net.Conn, error)
}

// defaultSSRFResolver wraps net.DefaultResolver to satisfy ssrfResolver.
type defaultSSRFResolver struct{}

func (defaultSSRFResolver) LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error) {
	return net.DefaultResolver.LookupIPAddr(ctx, host)
}

// newSSRFSafeClient creates an http.Client with SSRF-safe transport and
// redirect blocking. The transport uses a DialContext that resolves the
// hostname, selects the first non-blocked IP, and dials THAT SPECIFIC IP —
// never the original hostname. This closes the DNS-rebinding TOCTOU window
// (the dialed IP is always the one we validated). TLS SNI and the HTTP
// Host header are unaffected because they come from req.URL.Host, not the
// dial address.
//
// The client blocks ALL redirects. No redundant http.Client.Timeout —
// the per-attempt context deadline is the single timeout mechanism.
func newSSRFSafeClient() *http.Client {
	return newSSRFSafeClientWith(defaultSSRFResolver{}, &net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
	})
}

// newSSRFSafeClientWith creates an SSRF-safe http.Client using the provided
// resolver and dialer. This is the injectable constructor used by tests.
func newSSRFSafeClientWith(resolver ssrfResolver, dialer ssrfDialer) *http.Client {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			// Resolve the address to check the actual IP before connecting.
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, fmt.Errorf("SSRF protection: invalid address %q: %w", addr, err)
			}
			ips, err := resolver.LookupIPAddr(ctx, host)
			if err != nil {
				return nil, fmt.Errorf("SSRF protection: DNS lookup failed for %q: %w", host, err)
			}

			// Select the first non-blocked IP and dial it directly.
			// This guarantees we connect to exactly the IP we validated,
			// closing the DNS-rebinding TOCTOU window.
			for _, ipAddr := range ips {
				if !isBlockedSSRFTarget(ipAddr.IP) {
					return dialer.DialContext(ctx, network, net.JoinHostPort(ipAddr.IP.String(), port))
				}
			}

			// Every resolved IP is blocked — refuse without dialing.
			return nil, fmt.Errorf("SSRF protection: all resolved IPs for %q are blocked (loopback/link-local)", host)
		},
	}
	return &http.Client{
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return fmt.Errorf("redirects are blocked for lifecycle hook requests (SSRF protection)")
		},
	}
}

// isBlockedSSRFTarget checks whether an IP address should be blocked for SSRF
// protection. Per architect decision, ONLY loopback (127.0.0.0/8, ::1) and
// link-local (169.254.0.0/16, fe80::/10) unicast+multicast are blocked.
// RFC1918 (10/8, 172.16/12, 192.168/16) is intentionally ALLOWED because
// internal service registries (Consul, internal catalogs) are a supported
// use case. The check handles IPv4-mapped-IPv6 variants via Go's net.IP
// normalization.
func isBlockedSSRFTarget(ip net.IP) bool {
	if ip == nil {
		return false
	}
	return ip.IsLoopback() ||
		ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() ||
		// Unspecified addresses (0.0.0.0, ::) route to loopback on many
		// platforms and would otherwise bypass the loopback block.
		ip.IsUnspecified()
}

// doHTTPRequest executes a single HTTP request with the per-action timeout.
// It returns the HTTP status code, latency, and any error.
//
// Security:
//   - The provided client has SSRF-safe transport.
//   - Redirects are blocked to prevent SSRF via redirect to internal addresses.
//   - The bearer token is injected directly (NOT via hook variables).
//   - Response body is consumed and discarded (never stored).
func (e *HTTPExecutor) doHTTPRequest(ctx context.Context, client *http.Client, action *store.LifecycleHookAction, bearerToken string, actionType string) (statusCode int, latency time.Duration, err error) {
	timeout := time.Duration(action.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = time.Duration(defaultTimeoutSeconds) * time.Second
	}

	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Use nil body when action.Body is empty (instead of strings.NewReader("")).
	var body io.Reader
	if action.Body != "" {
		body = strings.NewReader(action.Body)
	}

	// Build the request.
	req, err := http.NewRequestWithContext(reqCtx, action.Method, action.URL, body)
	if err != nil {
		return 0, 0, fmt.Errorf("build request: %w", err)
	}

	// Apply rendered headers.
	for name, value := range action.Headers {
		req.Header.Set(name, value)
	}

	// Inject bearer token for http actions ONLY (never for webhooks).
	// The token is injected directly — it NEVER comes from hook variables.
	if actionType == store.LifecycleHookActionHTTP && bearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+bearerToken)
	}

	start := time.Now()
	resp, err := client.Do(req)
	latency = time.Since(start)

	if err != nil {
		return 0, latency, fmt.Errorf("HTTP request failed: %w", err)
	}

	// Consume and discard the response body. Never store it.
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	return resp.StatusCode, latency, nil
}

// recordAudit records an execution audit event. It extracts only safe metadata
// (method, host, hook id, status, latency) — NEVER response bodies or auth
// header values.
func (e *HTTPExecutor) recordAudit(
	ctx context.Context,
	hook *store.LifecycleHook,
	agent *store.Agent,
	trigger string,
	saEmail string,
	action *store.LifecycleHookAction,
	statusCode int,
	latency time.Duration,
	attempt int,
	execErr error,
) {
	// Extract host from URL for audit (avoid logging full URL which may
	// contain path-based tokens in webhook URLs).
	host := ""
	if action != nil && action.URL != "" {
		if u, err := url.Parse(action.URL); err == nil {
			host = u.Host
		}
	}

	method := ""
	actionType := ""
	if action != nil {
		method = action.Method
		actionType = action.Type
	}

	executionIdentity := saEmail
	if executionIdentity == "" && hook.ExecutionIdentity != "" {
		executionIdentity = hook.ExecutionIdentity // fall back to record ID
	}

	failReason := ""
	success := execErr == nil && statusCode >= 200 && statusCode < 300
	if execErr != nil {
		failReason = execErr.Error()
	} else if statusCode > 0 && (statusCode < 200 || statusCode >= 300) {
		failReason = fmt.Sprintf("HTTP %d", statusCode)
		success = false
	}

	event := &LifecycleHookExecutionEvent{
		EventType:         LifecycleHookExecEventExecute,
		HookID:            hook.ID,
		HookName:          hook.Name,
		Trigger:           trigger,
		AgentID:           agent.ID,
		ExecutionIdentity: executionIdentity,
		ActionType:        actionType,
		Method:            method,
		Host:              host,
		Success:           success,
		HTTPStatusCode:    statusCode,
		FailReason:        failReason,
		LatencyMs:         latency.Milliseconds(),
		Attempt:           attempt,
		Timestamp:         time.Now(),
	}

	LogLifecycleHookExecutionEvent(ctx, e.auditLogger, event)
}
