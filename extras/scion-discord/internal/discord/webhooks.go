package discord

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"

	"github.com/bwmarrin/discordgo"
)

const (
	// webhookName is the name used for all Scion-managed channel webhooks.
	// One webhook per channel is created lazily and reused for all agent messages.
	webhookName = "Scion Agent Relay"
)

// WebhookManager manages per-channel Discord webhooks used to send messages
// with per-agent identity (custom username and avatar). It lazily creates
// one webhook per channel, caches them in memory, and auto-recreates if
// a webhook is deleted externally.
type WebhookManager struct {
	session *discordgo.Session
	log     *slog.Logger

	mu    sync.RWMutex
	cache map[string]*discordgo.Webhook // channelID -> webhook
}

// NewWebhookManager creates a new WebhookManager.
func NewWebhookManager(session *discordgo.Session, log *slog.Logger) *WebhookManager {
	if log == nil {
		log = slog.Default()
	}
	return &WebhookManager{
		session: session,
		log:     log,
		cache:   make(map[string]*discordgo.Webhook),
	}
}

// getOrCreateWebhook returns the cached webhook for a channel, or discovers/
// creates one. The lifecycle is:
//  1. Check in-memory cache (fast path, read lock)
//  2. Query Discord for existing channel webhooks owned by us
//  3. Create a new webhook if none found
func (wm *WebhookManager) getOrCreateWebhook(channelID string) (*discordgo.Webhook, error) {
	// Fast path: check cache.
	wm.mu.RLock()
	if wh, ok := wm.cache[channelID]; ok {
		wm.mu.RUnlock()
		return wh, nil
	}
	wm.mu.RUnlock()

	// Slow path: look for existing webhook or create one.
	wm.mu.Lock()
	defer wm.mu.Unlock()

	// Double-check after acquiring write lock.
	if wh, ok := wm.cache[channelID]; ok {
		return wh, nil
	}

	// Check existing channel webhooks for one we own.
	webhooks, err := wm.session.ChannelWebhooks(channelID)
	if err != nil {
		return nil, fmt.Errorf("list channel webhooks: %w", err)
	}

	botUserID := ""
	if wm.session.State != nil && wm.session.State.User != nil {
		botUserID = wm.session.State.User.ID
	}

	for _, wh := range webhooks {
		if wh.Name == webhookName && wh.User != nil && wh.User.ID == botUserID {
			wm.cache[channelID] = wh
			wm.log.Debug("Reusing existing webhook",
				"channel_id", channelID,
				"webhook_id", wh.ID)
			return wh, nil
		}
	}

	// No existing webhook — create one.
	wh, err := wm.session.WebhookCreate(channelID, webhookName, "")
	if err != nil {
		return nil, fmt.Errorf("create webhook: %w", err)
	}

	wm.cache[channelID] = wh
	wm.log.Info("Created webhook for channel",
		"channel_id", channelID,
		"webhook_id", wh.ID)
	return wh, nil
}

// invalidate removes a cached webhook for a channel, forcing re-discovery
// on the next send.
func (wm *WebhookManager) invalidate(channelID string) {
	wm.mu.Lock()
	delete(wm.cache, channelID)
	wm.mu.Unlock()
}

// SendAsAgent sends a message via webhook with the agent's identity (name + avatar).
// If the webhook has been deleted externally (404/Unknown Webhook), the cache
// entry is invalidated and a new webhook is created for a retry.
func (wm *WebhookManager) SendAsAgent(channelID, agentSlug, content string, embeds []*discordgo.MessageEmbed, components []discordgo.MessageComponent, files []*discordgo.File) (*discordgo.Message, error) {
	wh, err := wm.getOrCreateWebhook(channelID)
	if err != nil {
		return nil, fmt.Errorf("get webhook for channel %s: %w", channelID, err)
	}

	params := &discordgo.WebhookParams{
		Content:    content,
		Username:   agentSlug,
		AvatarURL:  agentIconURL(agentSlug),
		Embeds:     embeds,
		Components: components,
		Files:      files,
	}

	// wait=true so discordgo returns the created Message object.
	msg, err := wm.session.WebhookExecute(wh.ID, wh.Token, true, params)
	if err != nil {
		// Check for 404 / Unknown Webhook — the webhook was deleted externally.
		if isWebhookNotFound(err) {
			wm.log.Warn("Webhook gone (deleted externally), recreating",
				"channel_id", channelID,
				"webhook_id", wh.ID)
			wm.invalidate(channelID)

			// Retry once with a fresh webhook.
			wh2, err2 := wm.getOrCreateWebhook(channelID)
			if err2 != nil {
				return nil, fmt.Errorf("recreate webhook after 404: %w", err2)
			}
			resetFileReaders(files)
			msg, err = wm.session.WebhookExecute(wh2.ID, wh2.Token, true, params)
			if err != nil {
				return nil, fmt.Errorf("webhook send after recreate: %w", err)
			}
			return msg, nil
		}
		return nil, fmt.Errorf("webhook execute: %w", err)
	}

	return msg, nil
}

// SendAsAgentInThread sends a message via webhook with the agent's identity,
// targeting a specific thread. For forum channels, the webhook is created on
// the parent channel and executed with thread_id to post in the correct thread.
func (wm *WebhookManager) SendAsAgentInThread(parentChannelID, threadID, agentSlug, content string, embeds []*discordgo.MessageEmbed, components []discordgo.MessageComponent, files []*discordgo.File) (*discordgo.Message, error) {
	wh, err := wm.getOrCreateWebhook(parentChannelID)
	if err != nil {
		return nil, fmt.Errorf("get webhook for channel %s: %w", parentChannelID, err)
	}

	params := &discordgo.WebhookParams{
		Content:    content,
		Username:   agentSlug,
		AvatarURL:  agentIconURL(agentSlug),
		Embeds:     embeds,
		Components: components,
		Files:      files,
	}

	msg, err := wm.session.WebhookThreadExecute(wh.ID, wh.Token, true, threadID, params)
	if err != nil {
		if isWebhookNotFound(err) {
			wm.log.Warn("Webhook gone (deleted externally), recreating",
				"channel_id", parentChannelID,
				"webhook_id", wh.ID)
			wm.invalidate(parentChannelID)

			wh2, err2 := wm.getOrCreateWebhook(parentChannelID)
			if err2 != nil {
				return nil, fmt.Errorf("recreate webhook after 404: %w", err2)
			}
			resetFileReaders(files)
			msg, err = wm.session.WebhookThreadExecute(wh2.ID, wh2.Token, true, threadID, params)
			if err != nil {
				return nil, fmt.Errorf("webhook send after recreate: %w", err)
			}
			return msg, nil
		}
		return nil, fmt.Errorf("webhook thread execute: %w", err)
	}

	return msg, nil
}

// agentIconURL returns a deterministic avatar URL for an agent using RoboHash.
// Discord recommends webhook avatars be at least 128×128 pixels.
func agentIconURL(agentSlug string) string {
	return fmt.Sprintf("https://robohash.org/%s?set=set1&size=128x128", url.PathEscape(agentSlug))
}

// isWebhookNotFound checks whether a Discord API error indicates that the
// webhook no longer exists (HTTP 404 or Discord error code 10015 "Unknown Webhook").
func isWebhookNotFound(err error) bool {
	if err == nil {
		return false
	}

	// discordgo wraps REST errors as *discordgo.RESTError.
	var restErr *discordgo.RESTError
	if errors.As(err, &restErr) {
		if restErr.Response != nil && restErr.Response.StatusCode == http.StatusNotFound {
			return true
		}
		// Discord error code 10015 = Unknown Webhook.
		if restErr.Message != nil && restErr.Message.Code == 10015 {
			return true
		}
	}

	// Fallback: check error string for common patterns.
	s := err.Error()
	return strings.Contains(s, "10015") || strings.Contains(s, "Unknown Webhook")
}

func resetFileReaders(files []*discordgo.File) {
	for _, f := range files {
		if seeker, ok := f.Reader.(io.Seeker); ok {
			_, _ = seeker.Seek(0, io.SeekStart)
		}
	}
}

func isDiscordHTTPError(err error, statusCode int) bool {
	if err == nil {
		return false
	}
	var restErr *discordgo.RESTError
	if errors.As(err, &restErr) {
		return restErr.Response != nil && restErr.Response.StatusCode == statusCode
	}
	// Fallback: check for status code in error string.
	return strings.Contains(err.Error(), strconv.Itoa(statusCode))
}
