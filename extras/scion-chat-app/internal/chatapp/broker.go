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

package chatapp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/plugin"
	goplugin "github.com/hashicorp/go-plugin"
)

const (
	// dedupTTL is how long a message fingerprint is remembered for deduplication.
	dedupTTL = 5 * time.Minute
)

// MessageHandler is called when a message is received from the Hub via the broker plugin.
type MessageHandler func(ctx context.Context, topic string, msg *messages.StructuredMessage) error

// BrokerServer implements the MessageBrokerPluginInterface and serves it via go-plugin RPC.
type BrokerServer struct {
	handler       MessageHandler
	hostCallbacks plugin.HostCallbacks
	log           *slog.Logger

	mu            sync.RWMutex
	subscriptions map[string]bool
	configured    bool
	channelName   string

	sentIDs   map[string]time.Time
	sentIDsMu sync.Mutex
}

// Compile-time interface checks.
var _ plugin.MessageBrokerPluginInterface = (*BrokerServer)(nil)
var _ plugin.HostCallbacksAware = (*BrokerServer)(nil)

// NewBrokerServer creates a new broker plugin server.
func NewBrokerServer(handler MessageHandler, log *slog.Logger) *BrokerServer {
	return &BrokerServer{
		handler:       handler,
		log:           log,
		subscriptions: make(map[string]bool),
		sentIDs:       make(map[string]time.Time),
	}
}

// SetHandler replaces the message handler after construction, allowing
// deferred wiring (e.g. to a notification relay created later).
func (b *BrokerServer) SetHandler(handler MessageHandler) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.handler = handler
}

// Configure is called by the Hub plugin manager during initialization.
func (b *BrokerServer) Configure(config map[string]string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.configured = true
	if b.channelName == "" {
		b.channelName = "gchat"
	}
	if v, ok := config["plugin_name"]; ok && v != "" {
		b.channelName = v
	}
	b.log.Info("broker plugin configured", "config_keys", len(config))
	return nil
}

// Publish receives a message from the Hub and routes it to the handler.
func (b *BrokerServer) Publish(ctx context.Context, topic string, msg *messages.StructuredMessage) error {
	if msg == nil {
		return nil
	}

	// Dedup check: compute SHA256 fingerprint and skip if seen within TTL.
	dedupKey := msgDedupKey(msg)
	if dedupKey != "" {
		b.sentIDsMu.Lock()
		if t, ok := b.sentIDs[dedupKey]; ok && time.Since(t) < dedupTTL {
			b.sentIDsMu.Unlock()
			b.log.Debug("skipping duplicate message",
				"topic", topic,
				"sender", msg.Sender,
				"dedup_key", dedupKey,
			)
			return nil
		}
		b.sentIDs[dedupKey] = time.Now()
		b.pruneSentIDsLocked()
		b.sentIDsMu.Unlock()
	}

	b.log.Debug("received message via broker",
		"topic", topic,
		"sender", msg.Sender,
		"type", msg.Type,
	)
	b.mu.RLock()
	h := b.handler
	b.mu.RUnlock()
	if h != nil {
		return h(ctx, topic, msg)
	}
	return nil
}

// msgDedupKey returns a stable SHA256 fingerprint for a message, used to detect
// duplicate deliveries of the same logical message.
func msgDedupKey(msg *messages.StructuredMessage) string {
	if msg == nil || msg.Msg == "" {
		return ""
	}
	h := sha256.New()
	h.Write([]byte(msg.Sender))
	h.Write([]byte("|"))
	h.Write([]byte(msg.Recipient))
	h.Write([]byte("|"))
	h.Write([]byte(msg.Timestamp))
	h.Write([]byte("|"))
	h.Write([]byte(msg.Type))
	h.Write([]byte("|"))
	h.Write([]byte(msg.Msg))
	return hex.EncodeToString(h.Sum(nil)[:16])
}

// pruneSentIDsLocked removes dedup entries older than dedupTTL.
// Must be called with b.sentIDsMu held.
func (b *BrokerServer) pruneSentIDsLocked() {
	now := time.Now()
	for k, t := range b.sentIDs {
		if now.Sub(t) > dedupTTL {
			delete(b.sentIDs, k)
		}
	}
}

// ChannelName returns the configured channel name in a thread-safe manner.
func (b *BrokerServer) ChannelName() string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.channelName
}

// Subscribe registers a topic pattern for receiving messages.
func (b *BrokerServer) Subscribe(pattern string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.subscriptions[pattern] = true
	b.log.Info("subscribed to pattern", "pattern", pattern)
	return nil
}

// Unsubscribe removes a topic pattern subscription.
func (b *BrokerServer) Unsubscribe(pattern string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.subscriptions, pattern)
	b.log.Info("unsubscribed from pattern", "pattern", pattern)
	return nil
}

// Close gracefully shuts down the broker plugin.
func (b *BrokerServer) Close() error {
	b.log.Info("broker plugin closing")
	return nil
}

// GetInfo returns plugin metadata.
func (b *BrokerServer) GetInfo() (*plugin.PluginInfo, error) {
	return &plugin.PluginInfo{
		Name:         "scion-chat-app",
		Version:      "1.0.0",
		ChannelID:    b.ChannelName(),
		Capabilities: []string{"chat-bridge", "notification-relay"},
	}, nil
}

// HealthCheck returns the plugin's health status.
func (b *BrokerServer) HealthCheck() (*plugin.HealthStatus, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	status := "healthy"
	msg := "chat app broker plugin operational"
	if !b.configured {
		status = "degraded"
		msg = "not yet configured by hub"
	}

	return &plugin.HealthStatus{
		Status:  status,
		Message: msg,
	}, nil
}

// SetHostCallbacks is called by the go-plugin framework to provide the reverse channel.
func (b *BrokerServer) SetHostCallbacks(hc plugin.HostCallbacks) {
	b.mu.Lock()
	b.hostCallbacks = hc
	subs := make([]string, 0, len(b.subscriptions))
	for p := range b.subscriptions {
		subs = append(subs, p)
	}
	b.mu.Unlock()

	b.log.Info("host callbacks connected")

	go func() {
		for _, pattern := range subs {
			// Retry loop since the host forwarder may not have its underlying implementation set immediately.
			for i := 0; i < 10; i++ {
				err := hc.RequestSubscription(pattern)
				if err == nil {
					b.log.Info("subscribed to deferred pattern", "pattern", pattern)
					break
				}

				if err.Error() == "host callbacks not yet available" {
					b.log.Debug("host callbacks not ready yet, retrying...", "pattern", pattern, "attempt", i+1)
					time.Sleep(time.Second)
				} else {
					b.log.Error("failed to request deferred subscription", "pattern", pattern, "error", err)
					break
				}
			}
		}
	}()
}

// HostCallbacks returns the host callbacks interface (for requesting subscriptions).
func (b *BrokerServer) HostCallbacks() plugin.HostCallbacks {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.hostCallbacks
}

// RequestSubscription asks the Hub to subscribe this plugin to a topic pattern.
func (b *BrokerServer) RequestSubscription(pattern string) error {
	b.mu.Lock()
	b.subscriptions[pattern] = true
	b.mu.Unlock()

	hc := b.HostCallbacks()
	if hc == nil {
		return fmt.Errorf("host callbacks not available")
	}
	return hc.RequestSubscription(pattern)
}

// CancelSubscription asks the Hub to cancel a subscription.
func (b *BrokerServer) CancelSubscription(pattern string) error {
	hc := b.HostCallbacks()
	if hc == nil {
		return fmt.Errorf("host callbacks not available")
	}
	return hc.CancelSubscription(pattern)
}

// Serve starts the go-plugin RPC server on the given address.
// The Hub's plugin manager connects to this server as a self-managed plugin.
//
// We use goplugin.RPCServer directly instead of goplugin.Serve() because
// Serve() is designed for plugin binaries launched by a parent process — it
// checks for the magic cookie env var and calls os.Exit(1) when it is absent.
// As a self-managed plugin we own our own process lifecycle, so we just need
// to speak the go-plugin net/rpc protocol on a listener we control.
func (b *BrokerServer) Serve(listenAddr string) (*PluginServer, error) {
	listener, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return nil, fmt.Errorf("listening on %s: %w", listenAddr, err)
	}

	pluginMap := map[string]goplugin.Plugin{
		plugin.BrokerPluginName: &plugin.BrokerPlugin{
			Impl: b,
		},
	}

	server := &PluginServer{
		listener: listener,
		broker:   b,
		log:      b.log,
	}

	// Create dummy stdout/stderr readers for the go-plugin stream protocol.
	// Self-managed plugins don't pipe stdio to the host, so these are
	// never-closing readers that the stream copiers will block on harmlessly.
	stdoutR, _ := io.Pipe()
	stderrR, _ := io.Pipe()

	doneCh := make(chan struct{})
	rpcServer := &goplugin.RPCServer{
		Plugins: pluginMap,
		Stdout:  stdoutR,
		Stderr:  stderrR,
		DoneCh:  doneCh,
	}

	go rpcServer.Serve(listener)

	b.log.Info("broker plugin RPC server started", "address", listenAddr)
	server.addr = listener.Addr().String()

	return server, nil
}

// PluginServer wraps the running plugin RPC server.
type PluginServer struct {
	listener net.Listener
	broker   *BrokerServer
	addr     string
	log      *slog.Logger
}

// Addr returns the address the server is listening on.
func (s *PluginServer) Addr() string {
	return s.addr
}

// Close shuts down the plugin server.
func (s *PluginServer) Close() error {
	if s.listener != nil {
		return s.listener.Close()
	}
	return nil
}

// --- Attachment helpers ---

const (
	// MaxAttachmentSize is the maximum file size for chat attachment uploads (25 MB).
	// Shared by all platform adapters (Google Chat, Discord, etc.).
	MaxAttachmentSize = 25 * 1024 * 1024

	// gchatAttachmentDir is the subdirectory under .attachments for Google Chat files.
	gchatAttachmentDir = "_gchat"
)

// ResolveOutboundAttachments converts agent-side attachment paths from a
// StructuredMessage into Attachment structs with resolved host-side paths.
// Paths that cannot be resolved or exceed the size limit are skipped with a
// logged warning.
func ResolveOutboundAttachments(log *slog.Logger, attachmentPaths []string, projectSlug, projectID string) []Attachment {
	if len(attachmentPaths) == 0 {
		return nil
	}

	var result []Attachment
	for _, agentPath := range attachmentPaths {
		if agentPath == "" {
			continue
		}

		hostPath := resolveAgentPath(agentPath, projectSlug, projectID)
		if hostPath == "" {
			log.Warn("cannot resolve attachment path, skipping",
				"agent_path", agentPath)
			continue
		}

		fi, err := os.Stat(hostPath)
		if err != nil {
			log.Warn("attachment file not found, skipping",
				"agent_path", agentPath,
				"host_path", hostPath,
				"error", err)
			continue
		}
		if fi.IsDir() {
			log.Warn("attachment path is a directory, skipping",
				"agent_path", agentPath,
				"host_path", hostPath)
			continue
		}
		if fi.Size() > MaxAttachmentSize {
			log.Warn("attachment file too large, skipping",
				"agent_path", agentPath,
				"size", fi.Size(),
				"max", MaxAttachmentSize)
			continue
		}

		result = append(result, Attachment{
			Filename: filepath.Base(hostPath),
			Path:     hostPath,
			Size:     fi.Size(),
		})
	}
	return result
}

// SizeLimitErrorCard returns a Card notifying the user that an attachment
// exceeds the size limit. When size is 0 (unknown), the message omits the
// specific file size.
func SizeLimitErrorCard(filename string, size int64) Card {
	var detail string
	if size > 0 {
		detail = fmt.Sprintf(
			"The file `%s` is %s, which exceeds the 25 MB attachment limit.\n\nPlease reduce the file size and try again.",
			filename,
			formatFileSize(size),
		)
	} else {
		detail = fmt.Sprintf(
			"The file `%s` exceeds the 25 MB attachment limit.\n\nPlease reduce the file size and try again.",
			filename,
		)
	}
	return Card{
		Header: CardHeader{
			Title:    "⚠️ Attachment Too Large",
			Subtitle: filename,
		},
		Sections: []CardSection{
			{
				Widgets: []Widget{
					{
						Type:    WidgetText,
						Content: detail,
					},
				},
			},
		},
	}
}

// resolveGChatAttachmentDir returns the host-side directory for storing
// Google Chat attachments in the shared scratchpad volume.
func resolveGChatAttachmentDir(projectSlug, projectID, conversationID string) (string, error) {
	if projectSlug == "" || projectID == "" {
		return "", fmt.Errorf("project slug or ID is empty")
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolving home directory: %w", err)
	}

	sharedDirBase := sharedDirHostPath(home, projectSlug, projectID, "scratchpad")
	return filepath.Join(sharedDirBase, ".attachments", gchatAttachmentDir, conversationID), nil
}

// resolveAgentPath translates agent-side container paths to host-side paths.
//
// Supported paths:
//   - /scion-volumes/<name>/<file> → shared dir host path
//   - /workspace/.scion-volumes/<name>/<file> → same as above
//   - Absolute paths starting with / that are already host paths → returned as-is
func resolveAgentPath(agentPath, projectSlug, projectID string) string {
	// Handle /scion-volumes/<name>/... paths.
	if strings.HasPrefix(agentPath, "/scion-volumes/") {
		return resolveSharedDirPath(agentPath, projectSlug, projectID)
	}

	// Handle /workspace/.scion-volumes/<name>/... paths.
	if strings.HasPrefix(agentPath, "/workspace/.scion-volumes/") {
		containerPath := "/scion-volumes/" + strings.TrimPrefix(agentPath, "/workspace/.scion-volumes/")
		return resolveSharedDirPath(containerPath, projectSlug, projectID)
	}

	// For absolute paths that aren't container paths, only allow paths
	// under known safe directories to prevent arbitrary file exfiltration.
	if strings.HasPrefix(agentPath, "/") {
		safePrefixes := []string{"/workspace/", "/scion-volumes/"}
		for _, prefix := range safePrefixes {
			if strings.HasPrefix(agentPath, prefix) {
				clean := filepath.ToSlash(filepath.Clean(agentPath))
				// Re-check prefix after Clean to prevent traversal (e.g. /workspace/../etc/passwd).
				if strings.HasPrefix(clean, prefix) {
					if _, err := os.Stat(clean); err == nil {
						return clean
					}
				}
				break
			}
		}
	}

	return ""
}

// resolveSharedDirPath converts a /scion-volumes/<name>/... path to the host-side path.
func resolveSharedDirPath(containerPath, projectSlug, projectID string) string {
	if projectSlug == "" || projectID == "" {
		return ""
	}

	trimmed := strings.TrimPrefix(containerPath, "/scion-volumes/")
	if trimmed == "" || trimmed == containerPath {
		return ""
	}

	parts := strings.SplitN(trimmed, "/", 2)
	sharedDirName := parts[0]
	if sharedDirName == "" || sharedDirName == "." || sharedDirName == ".." || strings.ContainsAny(sharedDirName, "/\\") {
		return ""
	}

	relPath := ""
	if len(parts) > 1 {
		relPath = filepath.Clean(parts[1])
		if strings.HasPrefix(relPath, "..") || filepath.IsAbs(relPath) {
			return ""
		}
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}

	base := sharedDirHostPath(home, projectSlug, projectID, sharedDirName)
	if relPath == "" || relPath == "." {
		return base
	}

	hostPath := filepath.Join(base, relPath)
	// Verify the resolved path doesn't escape the shared dir.
	if !strings.HasPrefix(hostPath, base+string(filepath.Separator)) {
		return ""
	}
	return hostPath
}

// sanitizePathComponent removes characters that are unsafe in file paths.
func sanitizePathComponent(s string) string {
	s = filepath.Base(s)
	s = strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' || r == '\x00' {
			return '_'
		}
		return r
	}, s)
	if s == "." || s == ".." || s == "" {
		return ""
	}
	return s
}

// formatFileSize returns a human-readable file size string.
func formatFileSize(bytes int64) string {
	const mb = 1024 * 1024
	if bytes >= mb {
		return fmt.Sprintf("%.1f MB", float64(bytes)/float64(mb))
	}
	return fmt.Sprintf("%.1f KB", float64(bytes)/1024.0)
}

// sharedDirHostPath computes the host-side directory path for a shared
// directory. This replicates the logic from pkg/config.SharedDirHostPath
// to avoid pulling in the full config package with its heavy transitive
// dependencies (ent, koanf, rclone, etc.).
//
// Path format: ~/.scion/project-configs/<slug>__<shortUUID>/shared-dirs/<name>
func sharedDirHostPath(home, slug, projectID, sharedDirName string) string {
	shortUUID := strings.ReplaceAll(projectID, "-", "")
	if len(shortUUID) > 8 {
		shortUUID = shortUUID[:8]
	}
	dirName := fmt.Sprintf("%s__%s", slug, shortUUID)
	return filepath.Join(home, ".scion", "project-configs", dirName, "shared-dirs", sharedDirName)
}
