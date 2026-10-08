package discord

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// countingTransport is a test RoundTripper that counts calls and delegates
// to a base transport. Used to verify that IAP-wrapped transports are
// actually used on all HTTP paths.
type countingTransport struct {
	base  http.RoundTripper
	calls atomic.Int64
}

func (ct *countingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	ct.calls.Add(1)
	if ct.base != nil {
		return ct.base.RoundTrip(req)
	}
	return http.DefaultTransport.RoundTrip(req)
}

func TestNewHTTPHubClient_TransportInheritance(t *testing.T) {
	t.Run("longHTTPClient inherits transport from httpClient", func(t *testing.T) {
		// Create a custom transport that records calls.
		transport := &countingTransport{}

		httpClient := &http.Client{
			Timeout:   15 * time.Second,
			Transport: transport,
		}

		client := NewHTTPHubClient("http://example.com", "key", "broker-1", httpClient)
		hc := client.(*httpHubClient)

		// Verify the longHTTPClient has the same transport.
		assert.Same(t, transport, hc.longHTTPClient.Transport,
			"longHTTPClient should inherit the Transport from httpClient")

		// Verify the longHTTPClient has no global timeout.
		assert.Zero(t, hc.longHTTPClient.Timeout,
			"longHTTPClient should have no global timeout")

		// Verify the main httpClient retains its timeout.
		assert.Equal(t, 15*time.Second, hc.httpClient.Timeout,
			"httpClient should keep its original timeout")
	})

	t.Run("nil httpClient gets default with nil transport", func(t *testing.T) {
		client := NewHTTPHubClient("http://example.com", "key", "broker-1", nil)
		hc := client.(*httpHubClient)

		// Default httpClient should have 15s timeout.
		assert.Equal(t, 15*time.Second, hc.httpClient.Timeout)

		// longHTTPClient should have nil transport (uses http.DefaultTransport).
		assert.Nil(t, hc.longHTTPClient.Transport,
			"longHTTPClient transport should be nil when httpClient has nil transport")

		// longHTTPClient should have no timeout.
		assert.Zero(t, hc.longHTTPClient.Timeout)
	})
}

func TestHTTPHubClient_IAPTransport_UsedOnAllPaths(t *testing.T) {
	// Set up a fake Hub API server.
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/projects":
			json.NewEncoder(w).Encode(hubProjectsResponse{
				Projects: []hubProject{{ID: "p1", Name: "test", Slug: "test"}},
			})
		case "/api/v1/projects/p1/agents":
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(hubCreateAgentResponse{
				Agent: struct {
					Slug string `json:"slug"`
					Name string `json:"name"`
				}{Slug: "agent-1", Name: "Agent 1"},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer hub.Close()

	transport := &countingTransport{base: hub.Client().Transport}
	httpClient := &http.Client{
		Timeout:   15 * time.Second,
		Transport: transport,
	}

	client := NewHTTPHubClient(hub.URL, "", "", httpClient)
	ctx := context.Background()

	// Test ListProjectsForUser (uses httpClient).
	_, err := client.ListProjectsForUser(ctx, luPrincipal)
	require.NoError(t, err)
	assert.Equal(t, int64(1), transport.calls.Load(),
		"ListProjectsForUser should use the custom transport")

	// Test CreateAgent (uses longHTTPClient).
	_, err = client.CreateAgent(ctx, "p1", CreateAgentRequest{
		Name:     "Agent 1",
		Template: "default",
	}, "")
	require.NoError(t, err)
	assert.Equal(t, int64(2), transport.calls.Load(),
		"CreateAgent should also use the custom transport via longHTTPClient")
}

func TestHTTPHubClient_PlainClient_WhenNoIAP(t *testing.T) {
	// Set up a fake Hub API server.
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(hubProjectsResponse{
			Projects: []hubProject{{ID: "p1", Name: "test", Slug: "test"}},
		})
	}))
	defer hub.Close()

	// Create hub client with nil httpClient (no IAP).
	client := NewHTTPHubClient(hub.URL, "", "", nil)
	ctx := context.Background()

	// Should work without IAP transport.
	projects, err := client.ListProjectsForUser(ctx, luPrincipal)
	require.NoError(t, err)
	assert.Len(t, projects, 1)
	assert.Equal(t, "test", projects[0].Slug)
}

// ---------------------------------------------------------------------------
// Secrets API methods
// ---------------------------------------------------------------------------

func TestHTTPHubClient_ListSecrets(t *testing.T) {
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "GET", r.Method)
		assert.Equal(t, "/api/v1/secrets", r.URL.Path)
		assert.Equal(t, "project", r.URL.Query().Get("scope"))
		assert.Equal(t, "proj-1", r.URL.Query().Get("scopeId"))

		json.NewEncoder(w).Encode(hubListSecretsResponse{
			Secrets: []SecretInfo{
				{Key: "API_KEY", Type: "environment", Scope: "project"},
				{Key: "DB_PASS", Type: "environment", Scope: "project", Description: "Database password"},
			},
		})
	}))
	defer hub.Close()

	client := NewHTTPHubClient(hub.URL, "", "", nil)
	secrets, err := client.ListSecrets(context.Background(), "project", "proj-1", "")
	require.NoError(t, err)
	assert.Len(t, secrets, 2)
	assert.Equal(t, "API_KEY", secrets[0].Key)
	assert.Equal(t, "DB_PASS", secrets[1].Key)
	assert.Equal(t, "Database password", secrets[1].Description)
}

func TestHTTPHubClient_ListSecrets_Error(t *testing.T) {
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer hub.Close()

	client := NewHTTPHubClient(hub.URL, "", "", nil)
	_, err := client.ListSecrets(context.Background(), "project", "proj-1", "")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "status 500")
}

func TestHTTPHubClient_GetSecret(t *testing.T) {
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "GET", r.Method)
		assert.Equal(t, "/api/v1/secrets/MY_KEY", r.URL.Path)
		assert.Equal(t, "project", r.URL.Query().Get("scope"))

		json.NewEncoder(w).Encode(SecretInfo{
			Key:     "MY_KEY",
			Type:    "environment",
			Scope:   "project",
			Version: 3,
			Updated: "2026-01-01T00:00:00Z",
		})
	}))
	defer hub.Close()

	client := NewHTTPHubClient(hub.URL, "", "", nil)
	info, err := client.GetSecret(context.Background(), "MY_KEY", "project", "proj-1", "")
	require.NoError(t, err)
	assert.Equal(t, "MY_KEY", info.Key)
	assert.Equal(t, 3, info.Version)
}

func TestHTTPHubClient_GetSecret_NotFound(t *testing.T) {
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer hub.Close()

	client := NewHTTPHubClient(hub.URL, "", "", nil)
	_, err := client.GetSecret(context.Background(), "MISSING", "project", "proj-1", "")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestHTTPHubClient_SetSecret(t *testing.T) {
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "PUT", r.Method)
		assert.Equal(t, "/api/v1/secrets/NEW_KEY", r.URL.Path)
		assert.Equal(t, "user:alice@example.com", r.Header.Get("X-Scion-On-Behalf-Of"))
		assert.Equal(t, "x-scion-on-behalf-of", r.Header.Get("X-Scion-Signed-Headers"))

		var payload setSecretPayload
		err := json.NewDecoder(r.Body).Decode(&payload)
		require.NoError(t, err)
		assert.Equal(t, "my-secret-value", payload.Value)
		assert.Equal(t, "raw", payload.Encoding)
		assert.Equal(t, "project", payload.Scope)
		assert.Equal(t, "proj-1", payload.ScopeID)

		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]interface{}{"created": true})
	}))
	defer hub.Close()

	client := NewHTTPHubClient(hub.URL, "", "", nil)
	err := client.SetSecret(context.Background(), "NEW_KEY", "my-secret-value", "project", "proj-1", "user:alice@example.com")
	assert.NoError(t, err)
}

func TestHTTPHubClient_SetSecret_Error(t *testing.T) {
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		json.NewEncoder(w).Encode(map[string]string{"error": "forbidden", "message": "no access"})
	}))
	defer hub.Close()

	client := NewHTTPHubClient(hub.URL, "", "", nil)
	err := client.SetSecret(context.Background(), "KEY", "val", "project", "p1", "user:bob@example.com")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "status 403")
}

func TestHTTPHubClient_DeleteSecret(t *testing.T) {
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "DELETE", r.Method)
		assert.Equal(t, "/api/v1/secrets/OLD_KEY", r.URL.Path)
		assert.Equal(t, "project", r.URL.Query().Get("scope"))
		assert.Equal(t, "proj-1", r.URL.Query().Get("scopeId"))
		assert.Equal(t, "user:alice@example.com", r.Header.Get("X-Scion-On-Behalf-Of"))

		w.WriteHeader(http.StatusNoContent)
	}))
	defer hub.Close()

	client := NewHTTPHubClient(hub.URL, "", "", nil)
	err := client.DeleteSecret(context.Background(), "OLD_KEY", "project", "proj-1", "user:alice@example.com")
	assert.NoError(t, err)
}

func TestHTTPHubClient_DeleteSecret_Error(t *testing.T) {
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{"error": "not_found", "message": "secret not found"})
	}))
	defer hub.Close()

	client := NewHTTPHubClient(hub.URL, "", "", nil)
	err := client.DeleteSecret(context.Background(), "MISSING", "project", "proj-1", "user:alice@example.com")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "status 404")
}

func TestHTTPHubClient_SetSecret_NoOnBehalfOf(t *testing.T) {
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// When onBehalfOf is empty, headers should not be set.
		assert.Empty(t, r.Header.Get("X-Scion-On-Behalf-Of"))
		assert.Empty(t, r.Header.Get("X-Scion-Signed-Headers"))
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]interface{}{"created": false})
	}))
	defer hub.Close()

	client := NewHTTPHubClient(hub.URL, "", "", nil)
	err := client.SetSecret(context.Background(), "KEY", "val", "project", "p1", "")
	assert.NoError(t, err)
}

// headerRecorder is a fake hub that records the linked-user headers of each
// request and serves minimal JSON bodies for the read endpoints.
type headerRecorder struct {
	mu    sync.Mutex
	calls []recordedHubCall
}

type recordedHubCall struct {
	Method        string
	Path          string
	RawQuery      string
	OnBehalfOf    string
	SignedHeaders string
}

func (hr *headerRecorder) snapshot() []recordedHubCall {
	hr.mu.Lock()
	defer hr.mu.Unlock()
	return append([]recordedHubCall(nil), hr.calls...)
}

func (hr *headerRecorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	hr.mu.Lock()
	hr.calls = append(hr.calls, recordedHubCall{
		Method:        r.Method,
		Path:          r.URL.Path,
		RawQuery:      r.URL.RawQuery,
		OnBehalfOf:    r.Header.Get("X-Scion-On-Behalf-Of"),
		SignedHeaders: r.Header.Get("X-Scion-Signed-Headers"),
	})
	hr.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/api/v1/projects":
		_ = json.NewEncoder(w).Encode(hubProjectsResponse{Projects: []hubProject{{ID: "p1", Slug: "proj-one"}}})
	case "/api/v1/projects/p1/agents":
		_ = json.NewEncoder(w).Encode(hubAgentsResponse{Agents: []hubAgent{{ID: "a1", Slug: "worker"}}})
	case "/api/v1/templates":
		_ = json.NewEncoder(w).Encode(hubTemplatesResponse{Templates: []hubTemplate{{Slug: "default", Name: "Default"}}})
	case "/api/v1/secrets":
		_ = json.NewEncoder(w).Encode(hubListSecretsResponse{Secrets: []SecretInfo{{Key: "API_KEY"}}})
	case "/api/v1/secrets/API_KEY":
		_ = json.NewEncoder(w).Encode(SecretInfo{Key: "API_KEY", Scope: "project"})
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func TestHTTPHubClient_ReadsSendLinkedUser(t *testing.T) {
	const principal = "user:alice@example.com"
	ctx := context.Background()

	tests := []struct {
		name      string
		call      func(c HubClient, onBehalfOf string) error
		wantPaths []string
	}{
		{
			name: "ListProjectsForUser",
			call: func(c HubClient, ob string) error {
				_, err := c.ListProjectsForUser(ctx, ob)
				return err
			},
			wantPaths: []string{"/api/v1/projects"},
		},
		{
			name: "ListAgents",
			call: func(c HubClient, ob string) error {
				_, err := c.ListAgents(ctx, "p1", ob)
				return err
			},
			wantPaths: []string{"/api/v1/projects/p1/agents"},
		},
		{
			name: "ListTemplates",
			call: func(c HubClient, ob string) error {
				_, err := c.ListTemplates(ctx, "p1", ob)
				return err
			},
			// Global and project-scoped template lists.
			wantPaths: []string{"/api/v1/templates", "/api/v1/templates"},
		},
		{
			name: "ListSecrets",
			call: func(c HubClient, ob string) error {
				_, err := c.ListSecrets(ctx, "project", "p1", ob)
				return err
			},
			wantPaths: []string{"/api/v1/secrets"},
		},
		{
			name: "GetSecret",
			call: func(c HubClient, ob string) error {
				_, err := c.GetSecret(ctx, "API_KEY", "project", "p1", ob)
				return err
			},
			wantPaths: []string{"/api/v1/secrets/API_KEY"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name+"/with_user", func(t *testing.T) {
			rec := &headerRecorder{}
			hub := httptest.NewServer(rec)
			defer hub.Close()

			require.NoError(t, tt.call(NewHTTPHubClient(hub.URL, "", "", nil), principal))

			calls := rec.snapshot()
			require.Len(t, calls, len(tt.wantPaths))
			for idx, c := range calls {
				assert.Equal(t, http.MethodGet, c.Method)
				assert.Equal(t, tt.wantPaths[idx], c.Path)
				assert.Equal(t, principal, c.OnBehalfOf)
				assert.Equal(t, "x-scion-on-behalf-of", c.SignedHeaders)
			}
		})

		t.Run(tt.name+"/without_user", func(t *testing.T) {
			rec := &headerRecorder{}
			hub := httptest.NewServer(rec)
			defer hub.Close()

			require.NoError(t, tt.call(NewHTTPHubClient(hub.URL, "", "", nil), ""))

			for _, c := range rec.snapshot() {
				assert.Empty(t, c.OnBehalfOf)
				assert.Empty(t, c.SignedHeaders)
			}
		})
	}
}

func TestHTTPHubClient_ListProjectsForUser_NoOwnerQuery(t *testing.T) {
	rec := &headerRecorder{}
	hub := httptest.NewServer(rec)
	defer hub.Close()

	projects, err := NewHTTPHubClient(hub.URL, "", "", nil).ListProjectsForUser(context.Background(), "user:alice@example.com")
	require.NoError(t, err)
	require.Len(t, projects, 1)
	assert.Equal(t, "p1", projects[0].ID)

	calls := rec.snapshot()
	require.Len(t, calls, 1)
	assert.Empty(t, calls[0].RawQuery, "projects are scoped by the linked user, not an ownerId filter")
}

// TestHTTPHubClient_CreateAgent_TypedErrors checks that every non-OK create
// agent response is a typed hub error carrying the status and code, and
// that the error text shown to users is unchanged.
func TestHTTPHubClient_CreateAgent_TypedErrors(t *testing.T) {
	cases := []struct {
		name     string
		status   int
		body     string
		wantCode string
		wantText string
	}{
		{
			name:     "not found",
			status:   http.StatusNotFound,
			body:     `{"error":{"code":"not_found","message":"template missing"}}`,
			wantCode: "not_found",
			wantText: "not found: template missing",
		},
		{
			name:     "not found without envelope",
			status:   http.StatusNotFound,
			body:     ``,
			wantText: "not found: Not Found",
		},
		{
			name:     "bad request",
			status:   http.StatusBadRequest,
			body:     `{"error":{"code":"validation_error","message":"name is invalid"}}`,
			wantCode: "validation_error",
			wantText: "validation error: name is invalid",
		},
		{
			name:     "server error",
			status:   http.StatusInternalServerError,
			body:     `{"error":{"code":"internal","message":"boom"}}`,
			wantCode: "internal",
			wantText: "create agent returned status 500: boom",
		},
		{
			name:     "server error without code",
			status:   http.StatusBadGateway,
			body:     `{"error":{"message":"upstream"}}`,
			wantText: "create agent returned status 502: Bad Gateway",
		},
		{
			name:     "forbidden",
			status:   http.StatusForbidden,
			body:     `{"error":{"code":"forbidden","message":"denied"}}`,
			wantCode: "forbidden",
			wantText: "create agent returned status 403: denied",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			client := NewHTTPHubClient(srv.URL, "", "", nil)
			_, err := client.CreateAgent(context.Background(), "p1", CreateAgentRequest{Name: "a"}, "user:a@example.com")
			require.Error(t, err)

			var he *HubError
			require.True(t, errors.As(err, &he), "want *HubError, got %T", err)
			assert.Equal(t, tc.status, he.StatusCode)
			assert.Equal(t, tc.wantCode, he.Code)
			assert.Equal(t, "create agent", he.Op)
			assert.Equal(t, tc.wantText, err.Error())
		})
	}
}
