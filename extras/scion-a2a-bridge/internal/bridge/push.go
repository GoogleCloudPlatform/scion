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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"

	"github.com/GoogleCloudPlatform/scion/extras/scion-a2a-bridge/internal/state"
)

// PushDispatcher delivers webhook notifications for task state changes.
type PushDispatcher struct {
	store       *state.Store
	config      *Config
	log         *slog.Logger
	client      *http.Client
	resolveIP   func(host string) ([]net.IP, error)
	sem         chan struct{}
	shutdownCtx context.Context
	wg          sync.WaitGroup
}

var errRedirectBlocked = errors.New("push notification redirects are not allowed")

// ErrSSRFBlocked is returned when a push notification URL resolves to a private or reserved IP.
var ErrSSRFBlocked = errors.New("push notification URL rejected")

// ValidatePushURL is a best-effort pre-check that the given URL does not resolve
// to a private or reserved IP address. The real enforcement happens at connect
// time via ssrfSafeDialer's Control function, which catches DNS rebinding.
func ValidatePushURL(pushURL string) error {
	parsed, err := url.Parse(pushURL)
	if err != nil {
		return fmt.Errorf("parse push URL: %w", err)
	}
	host := parsed.Hostname()
	ips, err := net.LookupIP(host)
	if err != nil {
		return fmt.Errorf("%w: cannot resolve host", ErrSSRFBlocked)
	}
	for _, ip := range ips {
		if v4 := ip.To4(); v4 != nil {
			ip = v4
		}
		if isPrivateIP(ip) {
			return fmt.Errorf("%w", ErrSSRFBlocked)
		}
	}
	return nil
}

var reservedRanges []net.IPNet

func init() {
	static := []net.IPNet{
		{IP: net.IPv4(169, 254, 0, 0), Mask: net.CIDRMask(16, 32)},
		{IP: net.IPv4(100, 64, 0, 0), Mask: net.CIDRMask(10, 32)},
		{IP: net.IPv4(224, 0, 0, 0), Mask: net.CIDRMask(4, 32)},
		{IP: net.IPv4(255, 255, 255, 255), Mask: net.CIDRMask(32, 32)},
		{IP: net.IPv4(192, 0, 0, 0), Mask: net.CIDRMask(24, 32)},   // IETF Protocol Assignments
		{IP: net.IPv4(192, 0, 2, 0), Mask: net.CIDRMask(24, 32)},   // TEST-NET-1
		{IP: net.IPv4(198, 51, 100, 0), Mask: net.CIDRMask(24, 32)}, // TEST-NET-2
		{IP: net.IPv4(203, 0, 113, 0), Mask: net.CIDRMask(24, 32)},  // TEST-NET-3
		{IP: net.IPv4(198, 18, 0, 0), Mask: net.CIDRMask(15, 32)},   // Benchmarking
	}
	for _, cidrStr := range []string{"fec0::/10", "ff00::/8", "2001:db8::/32"} {
		_, cidr, _ := net.ParseCIDR(cidrStr)
		if cidr != nil {
			static = append(static, *cidr)
		}
	}
	reservedRanges = static
}

func isPrivateIP(ip net.IP) bool {
	if ip.IsPrivate() {
		return true
	}
	if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() {
		return true
	}
	for _, cidr := range reservedRanges {
		if cidr.Contains(ip) {
			return true
		}
	}
	return false
}

// ssrfSafeDialer returns a DialContext that checks resolved IPs at connection time,
// preventing DNS rebinding attacks where DNS returns a public IP at validation
// but a private IP at connection time.
func ssrfSafeDialer() func(ctx context.Context, network, addr string) (net.Conn, error) {
	dialer := &net.Dialer{
		Timeout: 10 * time.Second,
		Control: func(network, address string, c syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return fmt.Errorf("%w: invalid address", ErrSSRFBlocked)
			}
			ip := net.ParseIP(host)
			if ip == nil {
				return fmt.Errorf("%w: cannot parse IP", ErrSSRFBlocked)
			}
			if v4 := ip.To4(); v4 != nil {
				ip = v4
			}
			if isPrivateIP(ip) {
				return fmt.Errorf("%w: resolved to private/reserved IP %s", ErrSSRFBlocked, ip)
			}
			return nil
		},
	}
	return dialer.DialContext
}

const maxPushConcurrency = 50

// NewSSRFSafeClient creates an HTTP client that checks resolved IPs at connection time.
func NewSSRFSafeClient() *http.Client {
	return &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			DialContext: ssrfSafeDialer(),
		},
		// Redirects must stay blocked: the Authorization header (token/credentials)
		// would be forwarded to the redirect target, leaking secrets.
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return errRedirectBlocked
		},
	}
}

// NewPushDispatcher creates a new push notification dispatcher.
func NewPushDispatcher(store *state.Store, cfg *Config, log *slog.Logger, shutdownCtx context.Context) *PushDispatcher {
	return &PushDispatcher{
		store:       store,
		config:      cfg,
		log:         log,
		client:      NewSSRFSafeClient(),
		resolveIP:   net.LookupIP,
		sem:         make(chan struct{}, maxPushConcurrency),
		shutdownCtx: shutdownCtx,
	}
}

// Dispatch sends a stream event to all registered push notification webhooks for a task.
// Returns immediately after spawning per-config goroutines; use pd.Wait() for shutdown drainage.
func (pd *PushDispatcher) Dispatch(ctx context.Context, taskID string, event StreamEvent) {
	select {
	case <-pd.shutdownCtx.Done():
		return
	default:
	}

	configs, err := pd.store.GetPushConfigsByTask(taskID)
	if err != nil {
		pd.log.Error("failed to get push configs", "task_id", taskID, "error", err)
		return
	}
	if len(configs) == 0 {
		return
	}

	pd.log.Debug("dispatching push notifications", "task_id", taskID, "config_count", len(configs))

	for _, cfg := range configs {
		select {
		case pd.sem <- struct{}{}:
		case <-pd.shutdownCtx.Done():
			return
		}
		pd.wg.Add(1)
		go func(c state.PushNotificationConfig) {
			defer pd.wg.Done()
			defer func() { <-pd.sem }()
			pd.sendWithRetry(c, event)
		}(cfg)
	}
}

// Wait blocks until all in-flight dispatches complete.
func (pd *PushDispatcher) Wait() {
	pd.wg.Wait()
}

func (pd *PushDispatcher) sendWithRetry(cfg state.PushNotificationConfig, event StreamEvent) {
	maxRetries := pd.config.Timeouts.PushRetryMax
	if maxRetries == 0 {
		maxRetries = 3
	}
	var lastStatusCode int

	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			shift := uint(attempt - 1)
			if shift > 8 {
				shift = 8 // cap at ~512s to prevent overflow at high push_retry_max
			}
			backoff := time.Duration(1<<shift) * 2 * time.Second
			select {
			case <-time.After(backoff):
			case <-pd.shutdownCtx.Done():
				return
			}
		}

		statusCode, err := pd.send(cfg, event)
		if err != nil {
			lastStatusCode = statusCode
			pd.log.Warn("push notification failed",
				"url", cfg.URL,
				"config_id", cfg.ID,
				"attempt", attempt+1,
				"status_code", statusCode,
				"error", err,
			)
			// Permanent client errors: remove config immediately.
			// 5xx is retried (not treated as permanent). Since Dispatch is async,
			// retries don't block the broker — they only consume a semaphore slot.
			if statusCode == 410 || (statusCode >= 400 && statusCode < 500 && statusCode != 408 && statusCode != 429) {
				pd.log.Error("push notification returned permanent client error, removing config",
					"id", cfg.ID, "url", cfg.URL, "status_code", statusCode)
				if err := pd.store.DeletePushConfig(cfg.ID); err != nil {
					pd.log.Error("failed to delete push config after permanent error", "id", cfg.ID, "error", err)
				}
				return
			}
			continue
		}
		return
	}

	pd.log.Error("push notification exhausted retries",
		"id", cfg.ID, "url", cfg.URL, "last_status_code", lastStatusCode)
}

// SECURITY TODO(v2): Add X-A2A-Signature HMAC over request body for webhook authentication.
// IMPORTANT: must land before public deployment. Currently the bearer token is the only
// authentication; webhook receivers have no way to verify the bridge's identity, and a
// leaked token is a full takeover.
func (pd *PushDispatcher) send(cfg state.PushNotificationConfig, event StreamEvent) (int, error) {
	body, err := json.Marshal(event)
	if err != nil {
		return 0, fmt.Errorf("marshal event: %w", err)
	}

	req, err := http.NewRequestWithContext(pd.shutdownCtx, http.MethodPost, cfg.URL, bytes.NewReader(body))
	if err != nil {
		return 0, fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/a2a+json")

	if cfg.AuthScheme != "" && cfg.AuthCredentials != "" {
		req.Header.Set("Authorization", fmt.Sprintf("%s %s", cfg.AuthScheme, cfg.AuthCredentials))
	} else if cfg.Token != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.Token)
	}

	resp, err := pd.client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("send request: %w", err)
	}
	defer func() {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		resp.Body.Close()
	}()

	if resp.StatusCode >= 400 {
		return resp.StatusCode, fmt.Errorf("webhook returned status %d", resp.StatusCode)
	}

	return resp.StatusCode, nil
}

const maxPushConfigsPerTask = 10

// SetPushNotificationConfig registers a webhook for task updates.
func (b *Bridge) SetPushNotificationConfig(ctx context.Context, taskID, pushURL, token, authScheme, authCredentials string) (*state.PushNotificationConfig, error) {
	if err := ValidatePushURL(pushURL); err != nil {
		return nil, fmt.Errorf("invalid push URL: %w", err)
	}

	task, err := b.store.GetTask(taskID)
	if err != nil {
		return nil, fmt.Errorf("get task: %w", err)
	}
	if task == nil {
		return nil, fmt.Errorf("task not found: %s", taskID)
	}

	existing, err := b.store.GetPushConfigsByTask(taskID)
	if err != nil {
		return nil, fmt.Errorf("check existing configs: %w", err)
	}
	if len(existing) >= maxPushConfigsPerTask {
		return nil, fmt.Errorf("maximum push notification configs (%d) reached for task", maxPushConfigsPerTask)
	}

	cfg := &state.PushNotificationConfig{
		ID:              uuid.New().String(),
		TaskID:          taskID,
		URL:             pushURL,
		Token:           token,
		AuthScheme:      authScheme,
		AuthCredentials: authCredentials,
		CreatedAt:       time.Now(),
	}

	if err := b.store.SetPushConfig(cfg); err != nil {
		return nil, fmt.Errorf("set push config: %w", err)
	}

	b.log.Info("push notification config set", "id", cfg.ID, "task_id", taskID, "url", pushURL)
	return cfg, nil
}

// GetPushNotificationConfig returns all push configs for a task.
func (b *Bridge) GetPushNotificationConfig(ctx context.Context, taskID string) ([]state.PushNotificationConfig, error) {
	return b.store.GetPushConfigsByTask(taskID)
}

// DeletePushNotificationConfig removes a push notification configuration,
// verifying it belongs to the specified task.
func (b *Bridge) DeletePushNotificationConfig(ctx context.Context, taskID, id string) error {
	return b.store.DeletePushConfigForTask(taskID, id)
}
