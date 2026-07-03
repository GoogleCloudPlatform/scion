package slack

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
)

const (
	linkingCodeExpiry   = 15 * time.Minute
	linkingPollInterval = 10 * time.Second
	linkingCodeCharset  = "ABCDEFGHJKMNPQRSTUVWXYZ23456789"
	linkingCodeLength   = 6
)

// RegistrationHandler manages the hub-verified code-based registration flow
// for Slack users.
type RegistrationHandler struct {
	store      Store
	hubURL     string
	hmacKey    string
	brokerID   string
	httpClient *http.Client
	log        *slog.Logger

	mu      sync.Mutex
	pending map[string]*pendingSlackReg // slackUserID -> pending registration
}

type pendingSlackReg struct {
	Code          string
	SlackUserID   string
	SlackUsername string
	ChannelID     string
	ExpiresAt     time.Time
	pollCancel    context.CancelFunc
}

type slackLinkRequest struct {
	Code        string `json:"code"`
	SlackUserID string `json:"slackUserId"`
}

type identityLinkStatusResponse struct {
	Status string            `json:"status"`
	User   *identityLinkUser `json:"user,omitempty"`
}

type identityLinkUser struct {
	ID    string `json:"id"`
	Email string `json:"email"`
}

// NewRegistrationHandler creates a new RegistrationHandler.
func NewRegistrationHandler(store Store, hubURL, hmacKey, brokerID string, log *slog.Logger) *RegistrationHandler {
	if log == nil {
		log = slog.Default()
	}
	return &RegistrationHandler{
		store:      store,
		hubURL:     hubURL,
		hmacKey:    hmacKey,
		brokerID:   brokerID,
		httpClient: &http.Client{Timeout: 15 * time.Second},
		log:        log,
		pending:    make(map[string]*pendingSlackReg),
	}
}

// StartRegistration initiates the registration flow for a Slack user.
func (h *RegistrationHandler) StartRegistration(ctx context.Context, slackUserID, slackUsername, channelID string) (string, error) {
	existing, err := h.store.GetUserMapping(ctx, slackUserID)
	if err != nil {
		return "", fmt.Errorf("check user mapping: %w", err)
	}
	if existing != nil {
		return "", fmt.Errorf("already registered as %s", existing.ScionEmail)
	}

	code, err := generateLinkingCode()
	if err != nil {
		return "", fmt.Errorf("generate linking code: %w", err)
	}

	if err := h.registerCodeWithHub(ctx, code, slackUserID); err != nil {
		return "", fmt.Errorf("register code with hub: %w", err)
	}

	h.mu.Lock()
	h.cleanExpiredLocked()
	if old, ok := h.pending[slackUserID]; ok && old.pollCancel != nil {
		old.pollCancel()
	}

	pollCtx, pollCancel := context.WithCancel(context.Background())
	reg := &pendingSlackReg{
		Code:          code,
		SlackUserID:   slackUserID,
		SlackUsername: slackUsername,
		ChannelID:     channelID,
		ExpiresAt:     time.Now().Add(linkingCodeExpiry),
		pollCancel:    pollCancel,
	}
	h.pending[slackUserID] = reg
	h.mu.Unlock()

	go h.pollForConfirmation(pollCtx, reg)

	hubLink := fmt.Sprintf("%s/profile/slack?code=%s&user_name=%s",
		strings.TrimRight(h.hubURL, "/"), code, slackUsername)
	return hubLink, nil
}

// Stop cancels all active polling goroutines.
func (h *RegistrationHandler) Stop() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for id, reg := range h.pending {
		if reg.pollCancel != nil {
			reg.pollCancel()
		}
		delete(h.pending, id)
	}
}

// Unregister removes a user's Slack-to-Scion mapping.
func (h *RegistrationHandler) Unregister(ctx context.Context, slackUserID string) error {
	existing, err := h.store.GetUserMapping(ctx, slackUserID)
	if err != nil {
		return fmt.Errorf("check user mapping: %w", err)
	}
	if existing == nil {
		return fmt.Errorf("not registered")
	}
	return h.store.DeleteUserMapping(ctx, slackUserID)
}

func (h *RegistrationHandler) pollForConfirmation(ctx context.Context, reg *pendingSlackReg) {
	ticker := time.NewTicker(linkingPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case t := <-ticker.C:
			if t.After(reg.ExpiresAt) {
				h.mu.Lock()
				if cur, ok := h.pending[reg.SlackUserID]; ok && cur.Code == reg.Code {
					delete(h.pending, reg.SlackUserID)
				}
				h.mu.Unlock()
				return
			}

			checkCtx, checkCancel := context.WithTimeout(ctx, 10*time.Second)
			statusResp, err := h.checkLinkingStatus(checkCtx, reg.SlackUserID)
			checkCancel()

			if err != nil {
				h.log.Debug("Poll check failed", "error", err, "slack_user_id", reg.SlackUserID)
				continue
			}

			if statusResp.Status == "confirmed" && statusResp.User != nil {
				h.completeRegistration(reg, statusResp)
				return
			}
		}
	}
}

func (h *RegistrationHandler) completeRegistration(reg *pendingSlackReg, statusResp *identityLinkStatusResponse) {
	if statusResp.User == nil {
		h.log.Error("Linking status confirmed but missing user info", "slack_user_id", reg.SlackUserID)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	mapping := &SlackUserMapping{
		SlackUserID:   reg.SlackUserID,
		SlackUsername: reg.SlackUsername,
		ScionUserID:   statusResp.User.ID,
		ScionEmail:    statusResp.User.Email,
		LinkedAt:      time.Now(),
	}

	if err := h.store.CreateUserMapping(ctx, mapping); err != nil {
		h.log.Error("Failed to save user mapping", "error", err, "slack_user_id", reg.SlackUserID)
		return
	}

	h.mu.Lock()
	if reg.pollCancel != nil {
		reg.pollCancel()
	}
	delete(h.pending, reg.SlackUserID)
	h.mu.Unlock()

	h.log.Info("User registered via hub linking",
		"slack_user_id", reg.SlackUserID,
		"scion_email", statusResp.User.Email,
		"scion_user_id", statusResp.User.ID,
	)
}

func (h *RegistrationHandler) registerCodeWithHub(ctx context.Context, code, slackUserID string) error {
	body, err := json.Marshal(slackLinkRequest{
		Code:        code,
		SlackUserID: slackUserID,
	})
	if err != nil {
		return fmt.Errorf("marshal slack link request: %w", err)
	}

	url := h.hubURL + "/api/v1/slack/link"
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create slack link request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if err := h.signRequest(req); err != nil {
		return fmt.Errorf("sign slack link request: %w", err)
	}

	resp, err := h.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("slack link request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("slack link endpoint returned status %d", resp.StatusCode)
	}

	return nil
}

func (h *RegistrationHandler) checkLinkingStatus(ctx context.Context, slackUserID string) (*identityLinkStatusResponse, error) {
	url := fmt.Sprintf("%s/api/v1/slack/link/status?slack_user_id=%s",
		h.hubURL, slackUserID)
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
