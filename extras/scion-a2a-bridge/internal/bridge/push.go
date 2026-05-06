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
}

var errRedirectBlocked = errors.New("push notification redirects are not allowed")

func isPrivateIP(ip net.IP) bool {
	privateRanges := []net.IPNet{
		{IP: net.IPv4(10, 0, 0, 0), Mask: net.CIDRMask(8, 32)},
		{IP: net.IPv4(172, 16, 0, 0), Mask: net.CIDRMask(12, 32)},
		{IP: net.IPv4(192, 168, 0, 0), Mask: net.CIDRMask(16, 32)},
		{IP: net.IPv4(127, 0, 0, 0), Mask: net.CIDRMask(8, 32)},
		{IP: net.IPv4(169, 254, 0, 0), Mask: net.CIDRMask(16, 32)},
		{IP: net.IPv4(169, 254, 169, 254), Mask: net.CIDRMask(32, 32)},
	}
	for _, cidr := range privateRanges {
		if cidr.Contains(ip) {
			return true
		}
	}
	if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() {
		return true
	}
	return false
}

// NewPushDispatcher creates a new push notification dispatcher.
func NewPushDispatcher(store *state.Store, cfg *Config, log *slog.Logger) *PushDispatcher {
	return &PushDispatcher{
		store:  store,
		config: cfg,
		log:    log,
		client: &http.Client{
			Timeout: 10 * time.Second,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return errRedirectBlocked
			},
		},
		resolveIP: net.LookupIP,
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

	for _, cfg := range configs {
		go pd.sendWithRetry(cfg, event)
	}
}

func (pd *PushDispatcher) sendWithRetry(cfg state.PushNotificationConfig, event StreamEvent) {
	maxRetries := pd.config.Timeouts.PushRetryMax

	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			backoff := time.Duration(1<<uint(attempt-1)) * 2 * time.Second
			time.Sleep(backoff)
		}

		if err := pd.send(cfg, event); err != nil {
			pd.log.Warn("push notification failed",
				"url", cfg.URL,
				"config_id", cfg.ID,
				"attempt", attempt+1,
				"error", err,
			)
			continue
		}
		return
	}

	pd.log.Error("push notification exhausted retries, removing config",
		"id", cfg.ID, "url", cfg.URL)
	pd.store.DeletePushConfig(cfg.ID)
}

func (pd *PushDispatcher) send(cfg state.PushNotificationConfig, event StreamEvent) error {
	parsed, err := url.Parse(cfg.URL)
	if err != nil {
		return fmt.Errorf("parse push URL: %w", err)
	}
	host := parsed.Hostname()
	ips, err := pd.resolveIP(host)
	if err != nil {
		return fmt.Errorf("resolve push host %q: %w", host, err)
	}
	for _, ip := range ips {
		if isPrivateIP(ip) {
			return fmt.Errorf("push URL host %q resolves to private IP %s", host, ip)
		}
	}

	body, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("marshal event: %w", err)
	}

	req, err := http.NewRequest(http.MethodPost, cfg.URL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/a2a+json")

	if cfg.AuthScheme != "" && cfg.AuthCredentials != "" {
		req.Header.Set("Authorization", fmt.Sprintf("%s %s", cfg.AuthScheme, cfg.AuthCredentials))
	} else if cfg.Token != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.Token)
	}

	resp, err := pd.client.Do(req)
	if err != nil {
		return fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return fmt.Errorf("webhook returned status %d", resp.StatusCode)
	}

	return nil
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
