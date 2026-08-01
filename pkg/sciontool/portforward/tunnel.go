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

package portforward

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	wire "github.com/GoogleCloudPlatform/scion/pkg/portforward"
	scionhub "github.com/GoogleCloudPlatform/scion/pkg/sciontool/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/log"
	"github.com/gorilla/websocket"
)

const (
	reconnectDelay       = 5 * time.Second
	localRequestTimeout  = 60 * time.Second
	maxLocalResponseBody = 64 << 20
)

type Manager struct {
	client  *scionhub.Client
	http    *http.Client
	writeMu sync.Mutex
}

func NewManager(client *scionhub.Client) *Manager {
	return &Manager{
		client: client,
		http: &http.Client{
			Timeout: localRequestTimeout,
		},
	}
}

func (m *Manager) Run(ctx context.Context) {
	if m == nil || m.client == nil || !m.client.IsConfigured() {
		return
	}
	for {
		err := m.runOnce(ctx)
		if err != nil {
			if ctx.Err() != nil {
				log.Info("Port-forward tunnel disconnected: context cancelled")
			} else {
				log.Error("Port-forward tunnel disconnected: %v", err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(reconnectDelay):
		}
	}
}

func (m *Manager) runOnce(ctx context.Context) error {
	endpoint, err := tunnelURL(m.client.HubURL(), m.client.AgentID())
	if err != nil {
		return err
	}
	header := http.Header{}
	header.Set("X-Scion-Agent-Token", m.client.AuthToken())
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, endpoint, header)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	// Close the connection when the context is cancelled to unblock ReadJSON.
	go func() {
		<-ctx.Done()
		_ = conn.Close()
	}()
	log.Info("Port-forward tunnel connected")
	for {
		var msg wire.Message
		if err := conn.ReadJSON(&msg); err != nil {
			return err
		}
		if msg.Type != wire.MessageTypeRequest || msg.Request == nil {
			continue
		}
		go m.handleRequest(conn, msg.Request)
	}
}

func (m *Manager) handleRequest(conn *websocket.Conn, req *wire.Request) {
	resp := m.doLocalRequest(req)
	msgType := wire.MessageTypeResponse
	if resp.Error != "" {
		msgType = wire.MessageTypeError
	}
	m.writeMu.Lock()
	defer m.writeMu.Unlock()
	_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if err := conn.WriteJSON(wire.Message{Type: msgType, Response: resp}); err != nil {
		log.Error("Failed to write port-forward response: %v", err)
	}
}

func isLoopbackHost(host string) bool {
	if strings.ToLower(strings.TrimSpace(host)) == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (m *Manager) doLocalRequest(req *wire.Request) *wire.Response {
	if !isLoopbackHost(req.Host) {
		return &wire.Response{StreamID: req.StreamID, Error: "unauthorized host: only loopback addresses are allowed"}
	}
	target := url.URL{
		Scheme:   "http",
		Host:     fmt.Sprintf("%s:%d", req.Host, req.Port),
		Path:     req.Path,
		RawQuery: req.Query,
	}
	httpReq, err := http.NewRequest(req.Method, target.String(), bytes.NewReader(req.Body))
	if err != nil {
		return &wire.Response{StreamID: req.StreamID, Error: err.Error()}
	}
	httpReq.Header = cloneForwardHeaders(req.Header)
	httpReq.Host = target.Host

	resp, err := m.http.Do(httpReq)
	if err != nil {
		return &wire.Response{StreamID: req.StreamID, Error: err.Error()}
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxLocalResponseBody+1))
	if err != nil {
		return &wire.Response{StreamID: req.StreamID, Error: err.Error()}
	}
	if len(body) > maxLocalResponseBody {
		return &wire.Response{StreamID: req.StreamID, Error: "local response body too large"}
	}
	log.Debug("Local request forwarded: port=%d method=%s path=%s status=%d", req.Port, req.Method, req.Path, resp.StatusCode)
	return &wire.Response{
		StreamID: req.StreamID,
		Status:   resp.StatusCode,
		Header:   cloneForwardHeaders(resp.Header),
		Body:     body,
	}
}

func tunnelURL(hubURL, agentID string) (string, error) {
	u, err := url.Parse(strings.TrimSuffix(hubURL, "/") + "/api/v1/agents/" + url.PathEscape(agentID) + "/ports/tunnel")
	if err != nil {
		return "", err
	}
	switch u.Scheme {
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	default:
		return "", fmt.Errorf("unsupported hub URL scheme %q", u.Scheme)
	}
	return u.String(), nil
}

func cloneForwardHeaders(in http.Header) http.Header {
	out := make(http.Header, len(in))
	for k, vals := range in {
		if hopByHopHeader(k) || sensitiveHeader(k) {
			continue
		}
		out[k] = append([]string(nil), vals...)
	}
	return out
}

func hopByHopHeader(k string) bool {
	switch strings.ToLower(k) {
	case "connection", "keep-alive", "proxy-authenticate", "proxy-authorization", "te", "trailer", "transfer-encoding", "upgrade":
		return true
	default:
		return false
	}
}

func sensitiveHeader(k string) bool {
	switch strings.ToLower(k) {
	case "authorization", "cookie", "x-scion-agent-token", "x-scion-broker-id", "x-scion-broker-hmac":
		return true
	default:
		return false
	}
}
