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
	t.Run("uses registerURL for link when set", func(t *testing.T) {
		h := NewRegistrationHandler(nil, nil, "http://localhost:8080", "https://public.example.com", "", "", nil, testLogger())
		assert.Equal(t, "https://public.example.com", h.registrationBaseURL())
	})

	t.Run("falls back to hubURL when registerURL is empty", func(t *testing.T) {
		h := NewRegistrationHandler(nil, nil, "http://localhost:8080", "", "", "", nil, testLogger())
		assert.Equal(t, "http://localhost:8080", h.registrationBaseURL())
	})

	t.Run("registerURL with trailing slash is trimmed in link", func(t *testing.T) {
		h := NewRegistrationHandler(nil, nil, "http://localhost:8080", "https://public.example.com/", "", "", nil, testLogger())
		link := formatRegistrationLink(h.registrationBaseURL(), "ABC123", "testuser")
		assert.Equal(t, "https://public.example.com/profile/discord?code=ABC123&user_name=testuser", link)
	})

	t.Run("hubURL with trailing slash is trimmed in link", func(t *testing.T) {
		h := NewRegistrationHandler(nil, nil, "http://localhost:8080/", "", "", "", nil, testLogger())
		link := formatRegistrationLink(h.registrationBaseURL(), "ABC123", "testuser")
		assert.Equal(t, "http://localhost:8080/profile/discord?code=ABC123&user_name=testuser", link)
	})

	t.Run("special characters in username are URL-escaped", func(t *testing.T) {
		link := formatRegistrationLink("https://example.com", "ABC123", "user name&foo")
		assert.Equal(t, "https://example.com/profile/discord?code=ABC123&user_name=user+name%26foo", link)
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
