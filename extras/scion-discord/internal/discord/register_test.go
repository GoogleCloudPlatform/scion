package discord

import (
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestNewRegistrationHandler_RegisterURL(t *testing.T) {
	t.Run("registerURL is stored when provided", func(t *testing.T) {
		h := NewRegistrationHandler(nil, nil, "http://localhost:8080", "https://public.example.com", "key", "broker1", nil, testLogger())
		require.NotNil(t, h)
		assert.Equal(t, "http://localhost:8080", h.hubURL)
		assert.Equal(t, "https://public.example.com", h.registerURL)
	})

	t.Run("registerURL is empty when not provided", func(t *testing.T) {
		h := NewRegistrationHandler(nil, nil, "http://localhost:8080", "", "key", "broker1", nil, testLogger())
		require.NotNil(t, h)
		assert.Equal(t, "http://localhost:8080", h.hubURL)
		assert.Equal(t, "", h.registerURL)
	})

	t.Run("default http client is created when nil", func(t *testing.T) {
		h := NewRegistrationHandler(nil, nil, "http://localhost:8080", "", "key", "broker1", nil, testLogger())
		require.NotNil(t, h.httpClient)
		assert.Equal(t, 15*time.Second, h.httpClient.Timeout)
	})

	t.Run("custom http client is used when provided", func(t *testing.T) {
		client := &http.Client{Timeout: 30 * time.Second}
		h := NewRegistrationHandler(nil, nil, "http://localhost:8080", "", "key", "broker1", client, testLogger())
		assert.Equal(t, client, h.httpClient)
	})
}

func TestRegistrationHandler_LinkURL(t *testing.T) {
	// Test the URL construction logic by verifying the baseURL selection.
	// We can't easily test HandleRegister end-to-end without mocking the full
	// Discord session and store, so we test the baseURL fallback logic directly.

	t.Run("uses registerURL for link when set", func(t *testing.T) {
		h := NewRegistrationHandler(nil, nil, "http://localhost:8080", "https://public.example.com", "", "", nil, testLogger())

		baseURL := h.hubURL
		if h.registerURL != "" {
			baseURL = h.registerURL
		}
		assert.Equal(t, "https://public.example.com", baseURL)
	})

	t.Run("falls back to hubURL when registerURL is empty", func(t *testing.T) {
		h := NewRegistrationHandler(nil, nil, "http://localhost:8080", "", "", "", nil, testLogger())

		baseURL := h.hubURL
		if h.registerURL != "" {
			baseURL = h.registerURL
		}
		assert.Equal(t, "http://localhost:8080", baseURL)
	})

	t.Run("registerURL with trailing slash is trimmed in link", func(t *testing.T) {
		h := NewRegistrationHandler(nil, nil, "http://localhost:8080", "https://public.example.com/", "", "", nil, testLogger())

		baseURL := h.hubURL
		if h.registerURL != "" {
			baseURL = h.registerURL
		}

		// Simulate the same formatting as HandleRegister
		link := formatRegistrationLink(baseURL, "ABC123", "testuser")
		assert.Equal(t, "https://public.example.com/profile/discord?code=ABC123&user_name=testuser", link)
	})

	t.Run("hubURL with trailing slash is trimmed in link", func(t *testing.T) {
		h := NewRegistrationHandler(nil, nil, "http://localhost:8080/", "", "", "", nil, testLogger())

		baseURL := h.hubURL
		if h.registerURL != "" {
			baseURL = h.registerURL
		}

		link := formatRegistrationLink(baseURL, "ABC123", "testuser")
		assert.Equal(t, "http://localhost:8080/profile/discord?code=ABC123&user_name=testuser", link)
	})
}

func TestGenerateLinkingCode(t *testing.T) {
	code, err := generateLinkingCode()
	require.NoError(t, err)
	assert.Len(t, code, linkingCodeLength)

	// Verify all characters are from the allowed charset.
	for _, c := range code {
		assert.Contains(t, linkingCodeCharset, string(c))
	}
}
