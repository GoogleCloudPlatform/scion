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
	store     *state.Store
	config    *Config
	log       *slog.Logger
	client    *http.Client
	resolveIP func(host string) ([]net.IP, error)
	sem       chan struct{}
}

var errRedirectBlocked = errors.New("push notification redirects are not allowed")

// ErrSSRFBlocked is returned when a push notification URL resolves to a private or reserved IP.
var ErrSSRFBlocked = errors.New("push notification URL rejected")

// ValidatePushURL checks that the given URL does not resolve to a private or reserved IP address.
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
		if isPrivateIP(ip) {
			return fmt.Errorf("%w", ErrSSRFBlocked)
		}
	}
	return nil
}

func isPrivateIP(ip net.IP) bool {
	// Use Go's built-in IsPrivate (covers RFC1918 IPv4 + IPv6 ULA fc00::/7).
	if ip.IsPrivate() {
		return true
	}
	if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() {
		return true
	}

	// Additional reserved ranges not covered by IsPrivate.
	reservedRanges := []net.IPNet{
		// IPv4 link-local / cloud metadata.
		{IP: net.IPv4(169, 254, 0, 0), Mask: net.CIDRMask(16, 32)},
		// IPv4 CGNAT (Carrier-Grade NAT).
		{IP: net.IPv4(100, 64, 0, 0), Mask: net.CIDRMask(10, 32)},
		// IPv4 multicast.
		{IP: net.IPv4(224, 0, 0, 0), Mask: net.CIDRMask(4, 32)},
		// IPv4 broadcast.
		{IP: net.IPv4(255, 255, 255, 255), Mask: net.CIDRMask(32, 32)},
	}

	// IPv6 site-local (deprecated but still seen).
	_, ipv6SiteLocal, _ := net.ParseCIDR("fec0::/10")
	if ipv6SiteLocal != nil {
		reservedRanges = append(reservedRanges, *ipv6SiteLocal)
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
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return errRedirectBlocked
		},
	}
}

// NewPushDispatcher creates a new push notification dispatcher.
func NewPushDispatcher(store *state.Store, cfg *Config, log *slog.Logger) *PushDispatcher {
	return &PushDispatcher{
		store:     store,
		config:    cfg,
		log:       log,
		client:    NewSSRFSafeClient(),
		resolveIP: net.LookupIP,
		sem:       make(chan struct{}, maxPushConcurrency),
	}
}

// Dispatch sends a stream event to all registered push notification webhooks for a task.
func (pd *PushDispatcher) Dispatch(ctx context.Context, taskID string, event StreamEvent) {
	configs, err := pd.store.GetPushConfigsByTask(taskID)
	if err != nil {
		pd.log.Error("failed to get push configs", "task_id", taskID, "error", err)
		return
	}
	if len(configs) == 0 {
		return
	}

	pd.log.Debug("dispatching push notifications", "task_id", taskID, "config_count", len(configs))

	var wg sync.WaitGroup
	for _, cfg := range configs {
		wg.Add(1)
		go func(c state.PushNotificationConfig) {
			defer wg.Done()
			pd.sem <- struct{}{}
			defer func() { <-pd.sem }()
			pd.sendWithRetry(c, event)
		}(cfg)
	}
	wg.Wait()
}

func (pd *PushDispatcher) sendWithRetry(cfg state.PushNotificationConfig, event StreamEvent) {
	maxRetries := pd.config.Timeouts.PushRetryMax
	var lastStatusCode int

	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			backoff := time.Duration(1<<uint(attempt-1)) * 2 * time.Second
			time.Sleep(backoff)
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
			if statusCode == 410 || (statusCode >= 400 && statusCode < 500 && statusCode != 408 && statusCode != 429) {
				pd.log.Error("push notification returned permanent client error, removing config",
					"id", cfg.ID, "url", cfg.URL, "status_code", statusCode)
				pd.store.DeletePushConfig(cfg.ID)
				return
			}
			continue
		}
		return
	}

	pd.log.Error("push notification exhausted retries",
		"id", cfg.ID, "url", cfg.URL, "last_status_code", lastStatusCode)
}

func (pd *PushDispatcher) send(cfg state.PushNotificationConfig, event StreamEvent) (int, error) {
	body, err := json.Marshal(event)
	if err != nil {
		return 0, fmt.Errorf("marshal event: %w", err)
	}

	req, err := http.NewRequest(http.MethodPost, cfg.URL, bytes.NewReader(body))
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
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return resp.StatusCode, fmt.Errorf("webhook returned status %d", resp.StatusCode)
	}

	return resp.StatusCode, nil
}

// SetPushNotificationConfig registers a webhook for task updates.
func (b *Bridge) SetPushNotificationConfig(ctx context.Context, taskID, url, token, authScheme, authCredentials string) (*state.PushNotificationConfig, error) {
	task, err := b.store.GetTask(taskID)
	if err != nil {
		return nil, fmt.Errorf("get task: %w", err)
	}
	if task == nil {
		return nil, fmt.Errorf("task not found: %s", taskID)
	}

	cfg := &state.PushNotificationConfig{
		ID:              uuid.New().String(),
		TaskID:          taskID,
		URL:             url,
		Token:           token,
		AuthScheme:      authScheme,
		AuthCredentials: authCredentials,
		CreatedAt:       time.Now(),
	}

	if err := b.store.SetPushConfig(cfg); err != nil {
		return nil, fmt.Errorf("set push config: %w", err)
	}

	b.log.Info("push notification config set", "id", cfg.ID, "task_id", taskID, "url", url)
	return cfg, nil
}

// GetPushNotificationConfig returns all push configs for a task.
func (b *Bridge) GetPushNotificationConfig(ctx context.Context, taskID string) ([]state.PushNotificationConfig, error) {
	return b.store.GetPushConfigsByTask(taskID)
}

// DeletePushNotificationConfig removes a push notification configuration.
func (b *Bridge) DeletePushNotificationConfig(ctx context.Context, id string) error {
	return b.store.DeletePushConfig(id)
}
