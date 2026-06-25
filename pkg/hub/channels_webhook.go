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
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
)

// WebhookChannel delivers notifications by POSTing the structured message
// as JSON to a configured URL.
type WebhookChannel struct {
	url     string
	method  string
	headers map[string]string
	client  *http.Client
}

// NewWebhookChannel creates a WebhookChannel from params.
// Supported params:
//   - url: the webhook endpoint (required)
//   - method: HTTP method (default "POST")
//   - headers: comma-separated key=value pairs (e.g., "Authorization=Bearer xxx,X-Custom=val")
func NewWebhookChannel(params map[string]string) *WebhookChannel {
	method := params["method"]
	if method == "" {
		method = "POST"
	}

	headers := make(map[string]string)
	if raw := params["headers"]; raw != "" {
		for _, pair := range strings.Split(raw, ",") {
			k, v, ok := strings.Cut(strings.TrimSpace(pair), "=")
			if ok {
				headers[strings.TrimSpace(k)] = strings.TrimSpace(v)
			}
		}
	}

	return &WebhookChannel{
		url:     params["url"],
		method:  method,
		headers: headers,
		client: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

func (w *WebhookChannel) Name() string { return "webhook" }

func (w *WebhookChannel) Validate() error {
	if w.url == "" {
		return fmt.Errorf("webhook channel requires a 'url' param")
	}
	if !strings.HasPrefix(w.url, "http://") && !strings.HasPrefix(w.url, "https://") {
		return fmt.Errorf("webhook url must start with http:// or https://")
	}
	return nil
}

func (w *WebhookChannel) Deliver(ctx context.Context, msg *messages.StructuredMessage) error {
	body, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("failed to marshal message: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, w.method, w.url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	for k, v := range w.headers {
		req.Header.Set(k, v)
	}

	resp, err := w.client.Do(req)
	if err != nil {
		return fmt.Errorf("webhook request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("webhook returned status %d", resp.StatusCode)
	}

	return nil
}
