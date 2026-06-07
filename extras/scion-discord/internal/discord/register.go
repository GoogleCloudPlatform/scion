package discord

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/bwmarrin/discordgo"
)

// RegistrationHandler manages the hub-verified code-based registration flow
// for Discord users.
type RegistrationHandler struct {
	store      Store
	session    *discordgo.Session
	hubURL     string
	hmacKey    string
	brokerID   string
	httpClient *http.Client
	log        *slog.Logger

	mu      sync.Mutex
	pending map[string]*pendingLinkReg // discordUserID -> pending registration
}

// pendingLinkReg holds state for an in-progress hub-based linking registration.
type pendingLinkReg struct {
	Code             string
	DiscordUserID    string
	DiscordUsername  string
	ChannelID        string
	InteractionToken string // for follow-up messages
	ExpiresAt        time.Time
	pollCancel       context.CancelFunc
}

// discordLinkRequest is the JSON body sent to the hub to register a linking code.
type discordLinkRequest struct {
	Code          string `json:"code"`
	DiscordUserID string `json:"discordUserId"`
}

// identityLinkStatusResponse is the JSON response from checking a linking status.
type identityLinkStatusResponse struct {
	Status string            `json:"status"` // "pending", "confirmed", "expired", "not_found"
	User   *identityLinkUser `json:"user,omitempty"`
}

// identityLinkUser holds user info returned by the hub when a linking code is confirmed.
type identityLinkUser struct {
	ID    string `json:"id"`
	Email string `json:"email"`
}

const (
	linkingCodeExpiry   = 15 * time.Minute
	linkingPollInterval = 10 * time.Second
	linkingCodeCharset  = "ABCDEFGHJKMNPQRSTUVWXYZ23456789"
	linkingCodeLength   = 6
)

// NewRegistrationHandler creates a new RegistrationHandler.
func NewRegistrationHandler(store Store, session *discordgo.Session, hubURL, hmacKey, brokerID string, log *slog.Logger) *RegistrationHandler {
	if log == nil {
		log = slog.Default()
	}
	return &RegistrationHandler{
		store:      store,
		session:    session,
		hubURL:     hubURL,
		hmacKey:    hmacKey,
		brokerID:   brokerID,
		httpClient: &http.Client{Timeout: 15 * time.Second},
		log:        log,
		pending:    make(map[string]*pendingLinkReg),
	}
}

// HandleRegister handles the /scion register command. It generates a short
// linking code, registers it with the hub, and sends the user a link button.
func (h *RegistrationHandler) HandleRegister(s *discordgo.Session, i *discordgo.InteractionCreate) {
	discordUserID := interactionUserID(i)
	if discordUserID == "" {
		h.followup(s, i, "Could not identify your user.")
		return
	}

	discordUsername := interactionUsername(i)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// Check if already registered.
	existing, err := h.store.GetUserMapping(ctx, discordUserID)
	if err != nil {
		h.log.Error("Failed to check user mapping", "error", err, "discord_user_id", discordUserID)
		h.followup(s, i, "Something went wrong. Please try again.")
		return
	}
	if existing != nil {
		h.followup(s, i, fmt.Sprintf(
			"You are already registered as **%s**. Use `/scion unregister` first.",
			existing.ScionEmail,
		))
		return
	}

	// Generate linking code.
	code, err := generateLinkingCode()
	if err != nil {
		h.log.Error("Failed to generate linking code", "error", err)
		h.followup(s, i, "Something went wrong. Please try again.")
		return
	}

	// Register code with the hub.
	guildID := ""
	if i.GuildID != "" {
		guildID = i.GuildID
	}
	if err := h.registerCodeWithHub(ctx, code, discordUserID, discordUsername, guildID); err != nil {
		h.log.Error("Failed to register linking code with hub", "error", err)
		h.followup(s, i, "Failed to start registration. Please try again later.")
		return
	}

	// Cancel any existing pending registration for this user.
	h.mu.Lock()
	h.cleanExpiredLocked()
	if old, ok := h.pending[discordUserID]; ok && old.pollCancel != nil {
		old.pollCancel()
	}

	pollCtx, pollCancel := context.WithCancel(context.Background())
	reg := &pendingLinkReg{
		Code:             code,
		DiscordUserID:    discordUserID,
		DiscordUsername:  discordUsername,
		ChannelID:        i.ChannelID,
		InteractionToken: i.Token,
		ExpiresAt:        time.Now().Add(linkingCodeExpiry),
		pollCancel:       pollCancel,
	}
	h.pending[discordUserID] = reg
	h.mu.Unlock()

	// Build the link URL.
	hubLink := fmt.Sprintf("%s/profile/discord?code=%s&user_name=%s",
		strings.TrimRight(h.hubURL, "/"), code, discordUsername)

	// Send follow-up with a URL button.
	_, err = s.FollowupMessageCreate(i.Interaction, true, &discordgo.WebhookParams{
		Content: "To link your Discord and Scion accounts, click the button below and sign in.\n\n(Link expires in 15 minutes.)",
		Components: []discordgo.MessageComponent{
			discordgo.ActionsRow{
				Components: []discordgo.MessageComponent{
					discordgo.Button{
						Label: "Link Account",
						Style: discordgo.LinkButton,
						URL:   hubLink,
					},
				},
			},
		},
	})
	if err != nil {
		h.log.Error("Failed to send registration message", "error", err)
	}

	// Start polling in the background.
	go h.pollForConfirmation(pollCtx, s, i.Interaction, reg)
}

// HandleUnregister handles the /scion unregister command.
func (h *RegistrationHandler) HandleUnregister(s *discordgo.Session, i *discordgo.InteractionCreate) {
	discordUserID := interactionUserID(i)
	if discordUserID == "" {
		h.followup(s, i, "Could not identify your user.")
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	existing, err := h.store.GetUserMapping(ctx, discordUserID)
	if err != nil {
		h.log.Error("Failed to check user mapping", "error", err, "discord_user_id", discordUserID)
		h.followup(s, i, "Something went wrong. Please try again.")
		return
	}
	if existing == nil {
		h.followup(s, i, "You don't have a linked Scion account. Use `/scion register` to link one.")
		return
	}

	if err := h.store.DeleteUserMapping(ctx, discordUserID); err != nil {
		h.log.Error("Failed to delete user mapping", "error", err, "discord_user_id", discordUserID)
		h.followup(s, i, "Failed to unlink your account. Please try again.")
		return
	}

	h.followup(s, i, "Your Discord account has been unlinked from Scion.")
	h.log.Info("User unregistered",
		"discord_user_id", discordUserID,
		"scion_email", existing.ScionEmail,
	)
}

// pollForConfirmation polls the hub for confirmation status in the background.
func (h *RegistrationHandler) pollForConfirmation(ctx context.Context, s *discordgo.Session, interaction *discordgo.Interaction, reg *pendingLinkReg) {
	ticker := time.NewTicker(linkingPollInterval)
	defer ticker.Stop()

	deadline := reg.ExpiresAt
	for {
		select {
		case <-ctx.Done():
			return
		case t := <-ticker.C:
			if t.After(deadline) {
				h.mu.Lock()
				if cur, ok := h.pending[reg.DiscordUserID]; ok && cur.Code == reg.Code {
					delete(h.pending, reg.DiscordUserID)
				}
				h.mu.Unlock()
				return
			}

			checkCtx, checkCancel := context.WithTimeout(ctx, 10*time.Second)
			statusResp, err := h.checkLinkingStatus(checkCtx, reg.DiscordUserID)
			checkCancel()

			if err != nil {
				h.log.Debug("Poll check failed", "error", err, "discord_user_id", reg.DiscordUserID)
				continue
			}

			if statusResp.Status == "confirmed" && statusResp.User != nil {
				h.completeRegistration(s, interaction, reg, statusResp)
				return
			}
		}
	}
}

// completeRegistration saves the user mapping and notifies the user.
func (h *RegistrationHandler) completeRegistration(s *discordgo.Session, interaction *discordgo.Interaction, reg *pendingLinkReg, statusResp *identityLinkStatusResponse) {
	if statusResp.User == nil {
		h.log.Error("Linking status confirmed but missing user info", "discord_user_id", reg.DiscordUserID)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	mapping := &DiscordUserMapping{
		DiscordUserID:   reg.DiscordUserID,
		DiscordUsername: reg.DiscordUsername,
		ScionUserID:     statusResp.User.ID,
		ScionEmail:      statusResp.User.Email,
		LinkedAt:        time.Now(),
	}

	if err := h.store.CreateUserMapping(ctx, mapping); err != nil {
		h.log.Error("Failed to save user mapping", "error", err, "discord_user_id", reg.DiscordUserID)
		return
	}

	h.mu.Lock()
	if reg.pollCancel != nil {
		reg.pollCancel()
	}
	delete(h.pending, reg.DiscordUserID)
	h.mu.Unlock()

	// Send follow-up via the interaction.
	_, err := s.FollowupMessageCreate(interaction, true, &discordgo.WebhookParams{
		Content: fmt.Sprintf("Linked! You are **%s**", statusResp.User.Email),
	})
	if err != nil {
		h.log.Error("Failed to send registration confirmation", "error", err)
	}

	h.log.Info("User registered via hub linking",
		"discord_user_id", reg.DiscordUserID,
		"scion_email", statusResp.User.Email,
		"scion_user_id", statusResp.User.ID,
	)
}

// registerCodeWithHub POSTs a linking code to the hub for registration.
func (h *RegistrationHandler) registerCodeWithHub(ctx context.Context, code, discordUserID, _, _ string) error {
	body, err := json.Marshal(discordLinkRequest{
		Code:          code,
		DiscordUserID: discordUserID,
	})
	if err != nil {
		return fmt.Errorf("marshal discord link request: %w", err)
	}

	url := h.hubURL + "/api/v1/discord/link"
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create discord link request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if err := h.signRequest(req); err != nil {
		return fmt.Errorf("sign discord link request: %w", err)
	}

	resp, err := h.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("discord link request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("discord link endpoint returned status %d", resp.StatusCode)
	}

	return nil
}

// checkLinkingStatus checks with the hub whether a linking code was confirmed.
func (h *RegistrationHandler) checkLinkingStatus(ctx context.Context, discordUserID string) (*identityLinkStatusResponse, error) {
	url := fmt.Sprintf("%s/api/v1/discord/link/status?discord_user_id=%s",
		h.hubURL, discordUserID)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("create identity link status request: %w", err)
	}
	if err := h.signRequest(req); err != nil {
		return nil, fmt.Errorf("sign identity link status request: %w", err)
	}

	resp, err := h.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("identity link status request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("identity link status endpoint returned status %d", resp.StatusCode)
	}

	var statusResp identityLinkStatusResponse
	if err := json.NewDecoder(resp.Body).Decode(&statusResp); err != nil {
		return nil, fmt.Errorf("decode identity link status response: %w", err)
	}
	return &statusResp, nil
}

// signRequest signs an HTTP request with HMAC broker credentials.
func (h *RegistrationHandler) signRequest(req *http.Request) error {
	if h.hmacKey == "" || h.brokerID == "" {
		return nil
	}
	secretKey, err := decodeBase64(h.hmacKey)
	if err != nil {
		return fmt.Errorf("decode HMAC key: %w", err)
	}
	auth := &apiclient.HMACAuth{
		BrokerID:  h.brokerID,
		SecretKey: secretKey,
	}
	return auth.ApplyAuth(req)
}

// followup sends a follow-up message to the interaction.
func (h *RegistrationHandler) followup(s *discordgo.Session, i *discordgo.InteractionCreate, content string) {
	_, err := s.FollowupMessageCreate(i.Interaction, true, &discordgo.WebhookParams{
		Content: content,
	})
	if err != nil {
		h.log.Error("Failed to send follow-up message", "error", err)
	}
}

func (h *RegistrationHandler) cleanExpiredLocked() {
	now := time.Now()
	for id, reg := range h.pending {
		if now.After(reg.ExpiresAt) {
			if reg.pollCancel != nil {
				reg.pollCancel()
			}
			delete(h.pending, id)
		}
	}
}

// interactionUsername extracts the Discord username from an interaction.
func interactionUsername(i *discordgo.InteractionCreate) string {
	if i.Member != nil && i.Member.User != nil {
		return i.Member.User.Username
	}
	if i.User != nil {
		return i.User.Username
	}
	return ""
}

// generateLinkingCode creates a 6-character alphanumeric code using a
// charset that avoids ambiguous characters (0/O, 1/I/L).
func generateLinkingCode() (string, error) {
	result := make([]byte, linkingCodeLength)
	for i := range result {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(linkingCodeCharset))))
		if err != nil {
			return "", fmt.Errorf("generate random char: %w", err)
		}
		result[i] = linkingCodeCharset[n.Int64()]
	}
	return string(result), nil
}

// decodeBase64 tries standard and URL-safe base64 decoding.
func decodeBase64(s string) ([]byte, error) {
	if b, err := base64.StdEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	if b, err := base64.URLEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	return nil, fmt.Errorf("invalid base64 encoding")
}
