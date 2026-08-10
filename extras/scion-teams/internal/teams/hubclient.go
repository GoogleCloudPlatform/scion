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

package teams

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
)

// HubClient delivers inbound messages to the Scion Hub.
type HubClient struct {
	hubURL     string
	hmacKey    string
	brokerID   string
	httpClient *http.Client
}

// NewHubClient creates a new HubClient for delivering messages to the hub.
func NewHubClient(hubURL, hmacKey, brokerID string) *HubClient {
	return &HubClient{
		hubURL:     hubURL,
		hmacKey:    hmacKey,
		brokerID:   brokerID,
		httpClient: &http.Client{Timeout: 15 * time.Second},
	}
}

// inboundPayload is the JSON body POSTed to the hub's inbound endpoint.
type inboundPayload struct {
	Topic   string                    `json:"topic"`
	Message *messages.StructuredMessage `json:"message"`
}

// DeliverInbound sends a structured message to the hub's inbound endpoint.
func (c *HubClient) DeliverInbound(ctx context.Context, topic string, msg *messages.StructuredMessage) error {
	payload := inboundPayload{
		Topic:   topic,
		Message: msg,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal inbound payload: %w", err)
	}

	url := c.hubURL + "/api/v1/broker/inbound"

	slog.Debug("Delivering inbound message to hub",
		"url", url,
		"topic", topic,
		"sender", msg.Sender,
		"broker_id", c.brokerID,
	)

	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create inbound request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	if err := c.signRequest(req); err != nil {
		return fmt.Errorf("sign request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("inbound delivery failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("hub returned status %d: %s", resp.StatusCode, string(respBody))
	}

	return nil
}

// callbackPayload is the JSON body POSTed to the hub's callback endpoint.
type callbackPayload struct {
	Data map[string]interface{} `json:"data"`
}

// DeliverCallback sends callback data to the hub's callback endpoint.
func (c *HubClient) DeliverCallback(ctx context.Context, data map[string]interface{}) error {
	payload := callbackPayload{Data: data}

	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal callback payload: %w", err)
	}

	url := c.hubURL + "/api/v1/broker/callback"

	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create callback request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	if err := c.signRequest(req); err != nil {
		return fmt.Errorf("sign request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("callback delivery failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("hub callback returned status %d: %s", resp.StatusCode, string(respBody))
	}

	return nil
}

// signRequest adds HMAC authentication headers to the request.
func (c *HubClient) signRequest(req *http.Request) error {
	if c.brokerID == "" || c.hmacKey == "" {
		return nil
	}

	secretKey, err := base64.StdEncoding.DecodeString(c.hmacKey)
	if err != nil {
		// Try URL-safe base64.
		secretKey, err = base64.URLEncoding.DecodeString(c.hmacKey)
		if err != nil {
			return fmt.Errorf("decode HMAC key: %w", err)
		}
	}

	auth := &apiclient.HMACAuth{
		BrokerID:  c.brokerID,
		SecretKey: secretKey,
	}
	return auth.ApplyAuth(req)
}
