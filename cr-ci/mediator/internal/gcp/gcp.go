// Package gcp holds the two Google Cloud building blocks the mediator needs
// besides Cloud Run: an access-token source backed by the metadata server
// (the service's own identity) and a Secret Manager reader. Both use plain
// REST so the module stays dependency free.
package gcp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultMetadataURL is the metadata server origin on Cloud Run.
	DefaultMetadataURL = "http://metadata.google.internal"
	// DefaultSecretManagerURL is the Secret Manager REST origin.
	DefaultSecretManagerURL = "https://secretmanager.googleapis.com"

	tokenRefreshMargin = 2 * time.Minute
	maxBody            = 1 << 20
	errBodySize        = 300
)

// TokenSource returns an OAuth2 access token for Google APIs.
type TokenSource interface {
	Token(ctx context.Context) (string, error)
}

// MetadataTokenSource fetches the default service account's access token from
// the metadata server and caches it until shortly before it expires.
type MetadataTokenSource struct {
	BaseURL string       // default DefaultMetadataURL
	HTTP    *http.Client // default: 10 s timeout
	Now     func() time.Time

	mu      sync.Mutex
	token   string
	expires time.Time
}

// Token implements TokenSource.
func (m *MetadataTokenSource) Token(ctx context.Context) (string, error) {
	now := time.Now
	if m.Now != nil {
		now = m.Now
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.token != "" && now().Add(tokenRefreshMargin).Before(m.expires) {
		return m.token, nil
	}
	base := m.BaseURL
	if base == "" {
		base = DefaultMetadataURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		strings.TrimSuffix(base, "/")+"/computeMetadata/v1/instance/service-accounts/default/token", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Metadata-Flavor", "Google")
	var out struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	if err := doJSON(httpClient(m.HTTP, 10*time.Second), req, "metadata token", &out); err != nil {
		return "", err
	}
	if out.AccessToken == "" || out.ExpiresIn <= 0 {
		return "", errors.New("metadata token: response is missing access_token or expires_in")
	}
	m.token = out.AccessToken
	m.expires = now().Add(time.Duration(out.ExpiresIn) * time.Second)
	return m.token, nil
}

// SecretManager reads the latest version of a secret.
type SecretManager struct {
	BaseURL string // default DefaultSecretManagerURL
	HTTP    *http.Client
	Tokens  TokenSource
	Project string
}

// Access returns the payload of the latest version of secret name. The
// payload is a secret: callers must never log it.
func (s *SecretManager) Access(ctx context.Context, name string) ([]byte, error) {
	tok, err := s.Tokens.Token(ctx)
	if err != nil {
		return nil, err
	}
	base := s.BaseURL
	if base == "" {
		base = DefaultSecretManagerURL
	}
	u := strings.TrimSuffix(base, "/") + "/v1/projects/" + url.PathEscape(s.Project) +
		"/secrets/" + url.PathEscape(name) + "/versions/latest:access"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	var out struct {
		Payload struct {
			Data       string `json:"data"`
			DataCrc32c string `json:"dataCrc32c"` // int64 encoded as a JSON string
		} `json:"payload"`
	}
	what := "secret " + name
	if err := doJSON(httpClient(s.HTTP, 30*time.Second), req, what, &out); err != nil {
		return nil, err
	}
	data, err := base64.StdEncoding.DecodeString(out.Payload.Data)
	if err != nil {
		return nil, fmt.Errorf("%s: payload is not base64", what)
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("%s: latest version is empty", what)
	}
	if c := out.Payload.DataCrc32c; c != "" {
		want, err := strconv.ParseInt(c, 10, 64)
		if err != nil || int64(crc32.Checksum(data, crc32.MakeTable(crc32.Castagnoli))) != want {
			return nil, fmt.Errorf("%s: payload checksum mismatch", what)
		}
	}
	return data, nil
}

func httpClient(c *http.Client, timeout time.Duration) *http.Client {
	if c != nil {
		return c
	}
	return &http.Client{Timeout: timeout}
}

// doJSON sends req and decodes a 2xx JSON response into out. Error messages
// carry the status and a truncated error body, never a success body.
func doJSON(c *http.Client, req *http.Request, what string, out any) error {
	resp, err := c.Do(req)
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, errBodySize))
		return fmt.Errorf("%s: HTTP %d: %s", what, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBody)).Decode(out); err != nil {
		return fmt.Errorf("%s: decoding response failed", what)
	}
	return nil
}
