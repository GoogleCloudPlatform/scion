package slack

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRegistrationHandler_TrailingSlashAndQueryEscaping(t *testing.T) {
	var (
		mu             sync.Mutex
		registeredBody slackLinkRequest
		registerURI    string
		statusURI      string
		registerHMAC   string
		statusHMAC     string
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/slack/link":
			registerURI = r.RequestURI
			registerHMAC = r.Header.Get("X-Scion-Signature")
			raw, err := io.ReadAll(r.Body)
			require.NoError(t, err)
			require.NoError(t, json.Unmarshal(raw, &registeredBody))
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"status":"registered"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/slack/link/status":
			statusURI = r.RequestURI
			statusHMAC = r.Header.Get("X-Scion-Signature")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":"pending"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	store := newTestStore(t)
	hmacKey := base64.StdEncoding.EncodeToString([]byte("test-hmac-key-32-bytes-long!!!!!"))
	handler := NewRegistrationHandler(store, srv.URL+"///", hmacKey, "broker-slack-test", nil)
	t.Cleanup(handler.Stop)

	slackUserID := "U123&extra=1+2"
	slackUsername := "alice+dev&foo=bar"

	hubLink, err := handler.StartRegistration(context.Background(), slackUserID, slackUsername, "C123")
	require.NoError(t, err)

	// Verify StartRegistration link trims trailing slashes and escapes query params.
	assert.False(t, strings.Contains(strings.TrimPrefix(hubLink, "http://"), "//"), "hubLink should not contain double slashes: %s", hubLink)
	parsedLink, err := url.Parse(hubLink)
	require.NoError(t, err)
	assert.Equal(t, "/profile/slack", parsedLink.Path)
	assert.Len(t, parsedLink.Query().Get("code"), linkingCodeLength)
	assert.Equal(t, slackUsername, parsedLink.Query().Get("user_name"))
	assert.Contains(t, parsedLink.RawQuery, "user_name="+url.QueryEscape(slackUsername))

	// Verify POST /api/v1/slack/link hit exact path without double slashes.
	mu.Lock()
	gotRegisterURI := registerURI
	gotRegisterBody := registeredBody
	gotRegisterHMAC := registerHMAC
	mu.Unlock()
	assert.Equal(t, "/api/v1/slack/link", gotRegisterURI)
	assert.Equal(t, slackUserID, gotRegisterBody.SlackUserID)
	assert.Equal(t, parsedLink.Query().Get("code"), gotRegisterBody.Code)
	assert.NotEmpty(t, gotRegisterHMAC)

	// Verify GET /api/v1/slack/link/status trims trailing slash and escapes slack_user_id.
	statusResp, err := handler.checkLinkingStatus(context.Background(), slackUserID)
	require.NoError(t, err)
	assert.Equal(t, "pending", statusResp.Status)

	mu.Lock()
	gotStatusURI := statusURI
	gotStatusHMAC := statusHMAC
	mu.Unlock()
	assert.Equal(t, "/api/v1/slack/link/status?slack_user_id="+url.QueryEscape(slackUserID), gotStatusURI)
	assert.NotEmpty(t, gotStatusHMAC)
}
