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

package integration_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"
	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"

	"github.com/GoogleCloudPlatform/scion/extras/scion-a2a-bridge/internal/bridge"
	bridgestate "github.com/GoogleCloudPlatform/scion/extras/scion-a2a-bridge/internal/state"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
	"github.com/GoogleCloudPlatform/scion/pkg/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/plugin/grpcbroker"
	"github.com/GoogleCloudPlatform/scion/pkg/plugin/refbroker"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
	brokerv1 "github.com/GoogleCloudPlatform/scion/proto/broker/v1"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

const (
	testGoogleClientID = "ge-integration-client.apps.googleusercontent.com"
	testHubSigningKey  = "ge-integration-hub-signing-key-32-bytes-minimum"
	testHubTokenTTL    = 3 * time.Second
)

type rewritePinnedGoogleTransport struct {
	target *url.URL
	base   http.RoundTripper
}

func (r rewritePinnedGoogleTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	clone := request.Clone(request.Context())
	clone.URL.Scheme = r.target.Scheme
	clone.URL.Host = r.target.Host
	clone.Host = r.target.Host
	return r.base.RoundTrip(clone)
}

type fakeGoogleProcess struct {
	mu     sync.RWMutex
	keys   []*rsa.PrivateKey
	active int
	counts map[string]int
}

func newFakeGoogleProcess(t *testing.T) *fakeGoogleProcess {
	t.Helper()
	keys := make([]*rsa.PrivateKey, 2)
	for index := range keys {
		key, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatal(err)
		}
		keys[index] = key
	}
	return &fakeGoogleProcess{keys: keys, counts: make(map[string]int)}
}

func (f *fakeGoogleProcess) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	f.mu.Lock()
	f.counts[request.URL.Path]++
	f.mu.Unlock()

	switch request.URL.Path {
	case "/oauth2/v3/certs":
		f.mu.RLock()
		index := f.active
		key := f.keys[index]
		f.mu.RUnlock()
		response.Header().Set("Cache-Control", "public, max-age=3600")
		writeTestJSON(response, http.StatusOK, jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{
			Key: &key.PublicKey, KeyID: fmt.Sprintf("key-%d", index), Algorithm: string(jose.RS256), Use: "sig",
		}}})
	case "/__test/mint":
		f.mint(response, request)
	case "/__test/rotate":
		f.mu.Lock()
		f.active = (f.active + 1) % len(f.keys)
		f.mu.Unlock()
		writeTestJSON(response, http.StatusOK, map[string]string{"outcome": "rotated"})
	case "/__test/stats":
		f.mu.RLock()
		defer f.mu.RUnlock()
		writeTestJSON(response, http.StatusOK, f.counts)
	default:
		http.NotFound(response, request)
	}
}

func (f *fakeGoogleProcess) mint(response http.ResponseWriter, request *http.Request) {
	f.mu.RLock()
	index := f.active
	key := f.keys[index]
	f.mu.RUnlock()

	ttl, err := time.ParseDuration(defaultString(request.URL.Query().Get("ttl"), "30s"))
	if err != nil {
		http.Error(response, "invalid ttl", http.StatusBadRequest)
		return
	}
	now := time.Now().UTC()
	claims := struct {
		jwt.Claims
		Email         string `json:"email"`
		EmailVerified bool   `json:"email_verified"`
		Name          string `json:"name"`
		HostedDomain  string `json:"hd,omitempty"`
		Nonce         string `json:"nonce"`
	}{
		Claims: jwt.Claims{
			Issuer:   "https://accounts.google.com",
			Subject:  defaultString(request.URL.Query().Get("sub"), "stable-google-subject"),
			Audience: jwt.Audience{defaultString(request.URL.Query().Get("audience"), testGoogleClientID)},
			IssuedAt: jwt.NewNumericDate(now),
			Expiry:   jwt.NewNumericDate(now.Add(ttl)),
		},
		Email:         defaultString(request.URL.Query().Get("email"), "ge-user@gmail.com"),
		EmailVerified: true,
		Name:          "GE Integration User",
		Nonce:         strconv.FormatInt(time.Now().UnixNano(), 10),
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", fmt.Sprintf("key-%d", index)))
	if err != nil {
		http.Error(response, "signer", http.StatusInternalServerError)
		return
	}
	token, err := jwt.Signed(signer).Claims(claims).Serialize()
	if err != nil {
		http.Error(response, "serialize", http.StatusInternalServerError)
		return
	}
	writeTestJSON(response, http.StatusOK, map[string]string{"token": token})
}

type hubProcessStats struct {
	Exchanges       int                   `json:"exchanges"`
	Messages        int                   `json:"messages"`
	LastUserID      string                `json:"last_user_id,omitempty"`
	LastMessageAuth string                `json:"-"`
	LastExchange    *hub.ExchangeResponse `json:"-"`
}

func serveHubProcess(t *testing.T, address string) {
	t.Helper()
	fakeGoogleURL, err := url.Parse(os.Getenv("SCION_TEST_FAKE_GOOGLE_URL"))
	if err != nil || fakeGoogleURL.Host == "" {
		t.Fatalf("parse fake Google URL: %v", err)
	}
	http.DefaultTransport = rewritePinnedGoogleTransport{target: fakeGoogleURL, base: http.DefaultTransport}

	databasePath := os.Getenv("SCION_TEST_HUB_DATABASE")
	dsn := "file:" + databasePath + "?_journal_mode=WAL&_busy_timeout=5000"
	client, err := entc.OpenSQLite(dsn, entc.PoolConfig{MaxOpenConns: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if err := entc.AutoMigrate(context.Background(), client); err != nil {
		t.Fatal(err)
	}
	store := entadapter.NewCompositeStore(client)
	cfg := hub.DefaultServerConfig()
	cfg.CORSEnabled = false
	cfg.UserAccessMode = "open"
	cfg.UserTokenConfig.SigningKey = []byte(testHubSigningKey)
	cfg.AgentTokenConfig.SigningKey = []byte(testHubSigningKey + "-agent")
	cfg.GEGoogleExchange = hub.GEGoogleExchangeConfig{
		Enabled: true, AllowedClientIDs: []string{testGoogleClientID}, TokenTTL: testHubTokenTTL,
	}
	hubServer, err := hub.New(cfg, store)
	if err != nil {
		t.Fatal(err)
	}
	tokenService, err := hub.NewUserTokenService(hub.UserTokenConfig{SigningKey: []byte(testHubSigningKey)})
	if err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	stats := hubProcessStats{}
	productionHandler := hubServer.Handler()
	handler := http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch {
		case request.URL.Path == "/api/v1/auth/integrations/google/exchange":
			recorder := httptest.NewRecorder()
			productionHandler.ServeHTTP(recorder, request)
			var exchange hub.ExchangeResponse
			if recorder.Code == http.StatusOK {
				if err := json.Unmarshal(recorder.Body.Bytes(), &exchange); err != nil {
					t.Errorf("decode captured exchange response: %v", err)
				}
			}
			mu.Lock()
			stats.Exchanges++
			stats.LastExchange = &exchange
			mu.Unlock()
			for key, values := range recorder.Header() {
				response.Header()[key] = append([]string(nil), values...)
			}
			response.WriteHeader(recorder.Code)
			_, _ = recorder.Body.WriteTo(response)
		case request.Method == http.MethodGet && request.URL.Path == "/api/v1/agents":
			if request.Header.Get("Authorization") != "Bearer unused-admin-token" && !validHubBearer(request, tokenService) {
				http.Error(response, "unauthorized", http.StatusUnauthorized)
				return
			}
			projectID := defaultString(request.URL.Query().Get("project_id"), "proj1")
			writeTestJSON(response, http.StatusOK, map[string]any{
				"agents":     []map[string]any{{"id": "agent-001", "name": "agent1", "slug": "agent1", "projectId": projectID, "status": "running"}},
				"totalCount": 1,
			})
		case request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/message"):
			token := strings.TrimPrefix(request.Header.Get("Authorization"), "Bearer ")
			claims, err := tokenService.ValidateUserToken(token)
			if err != nil {
				http.Error(response, "unauthorized", http.StatusUnauthorized)
				return
			}
			_, _ = io.Copy(io.Discard, request.Body)
			mu.Lock()
			stats.Messages++
			stats.LastUserID = claims.UserID
			stats.LastMessageAuth = request.Header.Get("Authorization")
			mu.Unlock()
			writeTestJSON(response, http.StatusOK, map[string]string{"conversationId": "conv-001", "messageId": "msg-001"})
		case request.URL.Path == "/__test/stats":
			mu.Lock()
			copy := stats
			mu.Unlock()
			writeTestJSON(response, http.StatusOK, copy)
		case request.URL.Path == "/__test/captured-bearer":
			mu.Lock()
			bearer := stats.LastMessageAuth
			mu.Unlock()
			writeTestJSON(response, http.StatusOK, map[string]string{"bearer": strings.TrimPrefix(bearer, "Bearer ")})
		case request.URL.Path == "/__test/last-exchange":
			mu.Lock()
			exchange := stats.LastExchange
			mu.Unlock()
			if exchange == nil {
				http.Error(response, "no exchange captured", http.StatusNotFound)
				return
			}
			writeTestJSON(response, http.StatusOK, exchange)
		default:
			http.NotFound(response, request)
		}
	})
	serveHTTPProcess(t, address, handler)
}

func validHubBearer(request *http.Request, service *hub.UserTokenService) bool {
	token := strings.TrimPrefix(request.Header.Get("Authorization"), "Bearer ")
	_, err := service.ValidateUserToken(token)
	return token != "" && err == nil
}

type authBridgeProcess struct {
	validator *bridge.GEExchangeValidator
	replica   string
	requests  atomic.Int64
}

func (p *authBridgeProcess) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	switch request.URL.Path {
	case "/request":
		p.requests.Add(1)
		credential := strings.TrimPrefix(request.Header.Get("Authorization"), "Bearer ")
		identity, err := p.validator.Validate(request.Context(), credential)
		if err != nil {
			http.Error(response, "unauthorized: "+err.Error(), http.StatusUnauthorized)
			return
		}
		writeTestJSON(response, http.StatusOK, map[string]string{"replica": p.replica, "user_id": identity.UserID})
	case "/__test/invalidate":
		p.validator.InvalidateCache()
		writeTestJSON(response, http.StatusOK, map[string]string{"outcome": "invalidated"})
	case "/__test/stats":
		writeTestJSON(response, http.StatusOK, map[string]int64{"requests": p.requests.Load()})
	default:
		http.NotFound(response, request)
	}
}

func serveAuthBridgeProcess(t *testing.T, address, replica string) {
	t.Helper()
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	validator := bridge.NewGEExchangeValidator(os.Getenv("SCION_TEST_HUB_URL"), bridge.GEExchangeConfig{
		CredentialType: "id_token", CacheTTL: 10 * time.Second,
	}, logger)
	serveHTTPProcess(t, address, &authBridgeProcess{validator: validator, replica: replica})
}

func serveFullBridgeProcess(t *testing.T, address, replica string) {
	t.Helper()
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	database := filepath.Join(os.Getenv("SCION_TEST_BRIDGE_DIR"), replica+".db")
	stateStore, err := bridgestate.NewSQLite(database)
	if err != nil {
		t.Fatal(err)
	}
	defer stateStore.Close()
	hubURL := os.Getenv("SCION_TEST_HUB_URL")
	adminClient, err := hubclient.New(hubURL, hubclient.WithBearerToken("unused-admin-token"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := &bridge.Config{
		Bridge: bridge.BridgeConfig{ExternalURL: "http://" + address, MaxSubscribers: 8},
		Hub:    bridge.HubConfig{Endpoint: hubURL, User: "integration@example.invalid"},
		Auth: bridge.AuthConfig{Scheme: "geGoogle", GEExchange: bridge.GEExchangeConfig{
			CredentialType: "id_token", CacheTTL: 10 * time.Second,
		}},
		Projects: []bridge.ProjectConfig{{Slug: "proj1", ExposedAgents: []string{"agent1"}}},
		Timeouts: bridge.TimeoutConfig{SendMessage: 2 * time.Second, SSEKeepalive: 100 * time.Millisecond},
	}
	b := bridge.New(stateStore, adminClient, nil, cfg, nil, logger)
	defer b.Shutdown()
	executor := bridge.NewScionExecutor(b, logger)
	routeAuth := bridge.RouteKeyAuthenticator()
	innerStore := taskstore.NewInMemory(&taskstore.InMemoryStoreConfig{Authenticator: routeAuth})
	scopedStore := bridge.NewScopedTaskStore(innerStore)
	sdkHandler := a2asrv.NewHandler(executor,
		a2asrv.WithLogger(logger),
		a2asrv.WithCapabilityChecks(&a2a.AgentCapabilities{Streaming: true}),
		a2asrv.WithAgentInactivityTimeout(2*time.Second),
		a2asrv.WithTaskStore(scopedStore),
	)
	b.SetSDKRequestHandler(sdkHandler)
	server := bridge.NewServer(b, cfg, nil, logger, a2asrv.NewJSONRPCHandler(sdkHandler))
	serveHTTPProcess(t, address, server.Handler())
}

// serveHABridgeProcess composes the same production HTTP auth, SDK executor,
// durable PostgreSQL stores, and BrokerService h2c routing used by standalone
// bridge deployments. Only its Hub and Google identity providers are local
// deterministic processes; no store helper is exposed to the test client.
func serveHABridgeProcess(t *testing.T, address, replica string) {
	t.Helper()
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Fatal("TEST_DATABASE_URL is required for ha-bridge")
	}
	stateStore, err := bridgestate.NewPostgres(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer stateStore.Close()
	sdkStore, err := bridge.NewPostgresTaskStoreWithDB(stateStore.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer sdkStore.Close()

	hubURL := os.Getenv("SCION_TEST_HUB_URL")
	adminClient, err := hubclient.New(hubURL, hubclient.WithBearerToken("unused-admin-token"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := &bridge.Config{
		Bridge: bridge.BridgeConfig{ExternalURL: "http://" + address, MaxSubscribers: 16},
		Hub:    bridge.HubConfig{Endpoint: hubURL, User: "integration@example.invalid"},
		Auth: bridge.AuthConfig{Scheme: "geGoogle", GEExchange: bridge.GEExchangeConfig{
			CredentialType: "id_token", CacheTTL: 10 * time.Second,
		}},
		Projects: []bridge.ProjectConfig{{Slug: "proj1", ExposedAgents: []string{"agent1"}}},
		Timeouts: bridge.TimeoutConfig{SendMessage: 2 * time.Second, SSEKeepalive: 50 * time.Millisecond},
	}
	b := bridge.New(stateStore, adminClient, nil, cfg, nil, logger.With("replica", replica))
	defer b.Shutdown()
	b.SetSDKTaskStore(sdkStore)
	barrierStore := bridge.NewBarrierTaskStore(sdkStore)
	b.SetBarrierStore(barrierStore)

	brokerServer := bridge.NewBrokerServer(nil, logger.With("component", "broker"), context.Background())
	brokerServer.SetHandler(b.HandleBrokerMessage)
	b.SetBroker(brokerServer)

	executor := bridge.NewScionExecutor(b, logger.With("component", "executor"))
	sdkHandler := a2asrv.NewHandler(executor,
		a2asrv.WithLogger(logger),
		a2asrv.WithCapabilityChecks(&a2a.AgentCapabilities{Streaming: true}),
		a2asrv.WithAgentInactivityTimeout(2*time.Second),
		a2asrv.WithTaskStore(barrierStore),
	)
	b.SetSDKRequestHandler(sdkHandler)
	durableHandler := bridge.NewDurableRequestHandler(sdkHandler, sdkStore, stateStore, nil)
	a2aServer := bridge.NewServer(b, cfg, nil, logger, a2asrv.NewJSONRPCHandler(
		durableHandler, a2asrv.WithTransportKeepAlive(50*time.Millisecond)))

	validator, err := grpcbroker.NewGoogleIDTokenValidator(grpcbroker.GoogleIDTokenValidatorConfig{
		Audience:           os.Getenv("SCION_TEST_CONTROL_AUDIENCE"),
		AuthorizedSubjects: []string{"hub-sa@hub-project.iam.gserviceaccount.com"},
		JWKSURL:            os.Getenv("SCION_TEST_FAKE_GOOGLE_URL") + "/oauth2/v3/certs",
	})
	if err != nil {
		t.Fatal(err)
	}
	grpcServer := grpc.NewServer(
		grpc.UnaryInterceptor(grpcbroker.UnaryAuthInterceptor(validator)),
		grpc.StreamInterceptor(grpcbroker.StreamAuthInterceptor(validator)),
	)
	brokerv1.RegisterBrokerServiceServer(grpcServer, grpcbroker.NewServer(brokerServer))
	defer grpcServer.Stop()

	// A restarted replica reaps only expired leases. Tests set the timeout low
	// enough to exercise the crash boundary without test-only database mutation.
	if _, err := sdkStore.ReapStaleTasks(context.Background(), 2*time.Second); err != nil {
		t.Fatalf("startup reap: %v", err)
	}

	httpHandler := a2aServer.Handler()
	muxed := h2c.NewHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor == 2 && strings.HasPrefix(r.Header.Get("Content-Type"), "application/grpc") {
			grpcServer.ServeHTTP(w, r)
			return
		}
		if r.URL.Path == "/__test/janitor-cycle" {
			ctx := r.Context()
			reaped, err := sdkStore.ReapStaleTasks(ctx, 2*time.Second)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			b.RunSweep(ctx)
			writeTestJSON(w, http.StatusOK, map[string]any{
				"status":      "ok",
				"replica":     replica,
				"reapedCount": len(reaped),
				"timestamp":   time.Now().UTC().Format(time.RFC3339Nano),
			})
			return
		}
		httpHandler.ServeHTTP(w, r)
	}), &http2.Server{})
	serveHTTPProcess(t, address, muxed)
}

func serveControlGRPCProcess(t *testing.T, address string) {
	t.Helper()
	validator, err := grpcbroker.NewGoogleIDTokenValidator(grpcbroker.GoogleIDTokenValidatorConfig{
		Audience:           os.Getenv("SCION_TEST_CONTROL_AUDIENCE"),
		AuthorizedSubjects: []string{"hub-sa@hub-project.iam.gserviceaccount.com"},
		JWKSURL:            os.Getenv("SCION_TEST_FAKE_GOOGLE_URL") + "/oauth2/v3/certs",
	})
	if err != nil {
		t.Fatal(err)
	}
	listener := inheritedHelperListener(t, address)
	broker := refbroker.New(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	defer broker.Close()
	server := grpc.NewServer(
		grpc.UnaryInterceptor(grpcbroker.UnaryAuthInterceptor(validator)),
		grpc.StreamInterceptor(grpcbroker.StreamAuthInterceptor(validator)),
	)
	brokerv1.RegisterBrokerServiceServer(server, grpcbroker.NewServer(broker))
	if err := server.Serve(listener); err != nil {
		t.Fatal(err)
	}
}

func writeTestJSON(response http.ResponseWriter, status int, value any) {
	response.Header().Set("Content-Type", "application/json")
	response.WriteHeader(status)
	_ = json.NewEncoder(response).Encode(value)
}

func defaultString(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func fetchMintedToken(t *testing.T, fakeGoogleURL string, values url.Values) string {
	t.Helper()
	response, err := http.Get(fakeGoogleURL + "/__test/mint?" + values.Encode())
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var payload map[string]string
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if payload["token"] == "" {
		t.Fatal("fake Google returned empty token")
	}
	return payload["token"]
}

func getJSON[T any](t *testing.T, endpoint string) T {
	t.Helper()
	response, err := http.Get(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("GET %s: status %d: %s", endpoint, response.StatusCode, body)
	}
	var result T
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	return result
}

func postBearer(t *testing.T, endpoint, bearer, body string) (int, []byte) {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, endpoint, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+bearer)
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, data
}

func startAuthTopology(t *testing.T, redactor *credentialRedactor) (*processTopology, *testProcess, *testProcess, *testProcess, *testProcess) {
	t.Helper()
	topology := newProcessTopology(t, redactor)
	fakeGoogle := topology.start(t, processSpec{Name: "fake-google", Mode: "fake-google", ReplicaID: "fake-google"})
	hubDatabase := filepath.Join(t.TempDir(), "hub.db")
	hubProcess := topology.start(t, processSpec{Name: "hub", Mode: "hub", ReplicaID: "hub", Env: map[string]string{
		"SCION_TEST_FAKE_GOOGLE_URL": fakeGoogle.URL(), "SCION_TEST_HUB_DATABASE": hubDatabase,
	}})
	bridge1 := topology.start(t, processSpec{Name: "bridge-1", Mode: "auth-bridge", ReplicaID: "bridge-1", Env: map[string]string{"SCION_TEST_HUB_URL": hubProcess.URL()}})
	bridge2 := topology.start(t, processSpec{Name: "bridge-2", Mode: "auth-bridge", ReplicaID: "bridge-2", Env: map[string]string{"SCION_TEST_HUB_URL": hubProcess.URL()}})
	return topology, fakeGoogle, hubProcess, bridge1, bridge2
}

func TestColdReplicaAndRotation(t *testing.T) {
	topology, fakeGoogle, hubProcess, bridge1, bridge2 := startAuthTopology(t, nil)
	alternator := topology.start(t, processSpec{Name: "alternator", Mode: "alternator", ReplicaID: "alternator", Env: map[string]string{
		"SCION_TEST_BACKEND_1": bridge1.URL(), "SCION_TEST_BACKEND_2": bridge2.URL(),
	}})
	token := fetchMintedToken(t, fakeGoogle.URL(), nil)

	var firstUserID string
	for index := 0; index < 4; index++ {
		status, body := postBearer(t, alternator.URL()+"/request", token, "")
		if status != http.StatusOK {
			t.Fatalf("request %d status = %d: %s\nlogs:\n%s", index, status, body, topology.logs.String())
		}
		var result map[string]string
		if err := json.Unmarshal(body, &result); err != nil {
			t.Fatal(err)
		}
		if firstUserID == "" {
			firstUserID = result["user_id"]
		} else if result["user_id"] != firstUserID {
			t.Fatalf("replicas resolved different users: %q != %q", result["user_id"], firstUserID)
		}
	}
	stats := getJSON[hubProcessStats](t, hubProcess.URL()+"/__test/stats")
	if stats.Exchanges != 2 {
		t.Fatalf("Hub exchanges = %d, want one cold miss per replica", stats.Exchanges)
	}

	// A new credential under the same signing key misses both independent
	// caches and resolves through the same durable subject binding.
	rotated := fetchMintedToken(t, fakeGoogle.URL(), nil)
	for _, endpoint := range []string{bridge1.URL(), bridge2.URL()} {
		status, body := postBearer(t, endpoint+"/request", rotated, "")
		if status != http.StatusOK {
			t.Fatalf("rotated token status = %d: %s", status, body)
		}
	}
	stats = getJSON[hubProcessStats](t, hubProcess.URL()+"/__test/stats")
	if stats.Exchanges != 4 {
		t.Fatalf("Hub exchanges after token rotation = %d, want 4", stats.Exchanges)
	}

	for _, endpoint := range []string{bridge1.URL(), bridge2.URL()} {
		if status, body := postBearer(t, endpoint+"/__test/invalidate", "", ""); status != http.StatusOK {
			t.Fatalf("invalidate status = %d: %s", status, body)
		}
	}
	status, body := postBearer(t, bridge1.URL()+"/request", rotated, "")
	if status != http.StatusOK {
		t.Fatalf("cache re-prime status = %d: %s", status, body)
	}
	// Observe the exact response used to prime the bridge cache and wait from
	// the minted JWT's integer-second exp boundary, not from arbitrary setup.
	exchange := getJSON[hub.ExchangeResponse](t, hubProcess.URL()+"/__test/last-exchange")
	parsedToken, err := jwt.ParseSigned(exchange.AccessToken, []jose.SignatureAlgorithm{jose.HS256})
	if err != nil {
		t.Fatalf("parse captured Hub JWT: %v", err)
	}
	var claims hub.UserTokenClaims
	if err := parsedToken.Claims([]byte(testHubSigningKey), &claims); err != nil {
		t.Fatalf("verify captured Hub JWT: %v", err)
	}
	if claims.Expiry == nil {
		t.Fatal("captured Hub JWT has no exp claim")
	}
	jwtExpiry := claims.Expiry.Time()
	responseExpiry, err := time.Parse(time.RFC3339, exchange.ExpiresAt)
	if err != nil {
		t.Fatalf("parse response expiresAt: %v", err)
	}
	upstreamExpiry, err := time.Parse(time.RFC3339, exchange.UpstreamExpiresAt)
	if err != nil {
		t.Fatalf("parse response upstreamExpiresAt: %v", err)
	}
	if !jwtExpiry.Equal(responseExpiry) {
		t.Fatalf("JWT exp %s != response expiresAt %s", jwtExpiry, responseExpiry)
	}
	if responseExpiry.After(upstreamExpiry) {
		t.Fatalf("Hub expiry %s exceeds upstream expiry %s", responseExpiry, upstreamExpiry)
	}
	remaining := time.Until(jwtExpiry)
	if remaining <= 0 {
		t.Fatalf("captured Hub JWT already expired at observation boundary: %s", jwtExpiry)
	}
	time.Sleep(remaining + 200*time.Millisecond)
	status, body = postBearer(t, bridge1.URL()+"/request", rotated, "")
	if status != http.StatusOK {
		t.Fatalf("post-Hub-expiry re-exchange status = %d: %s", status, body)
	}
	stats = getJSON[hubProcessStats](t, hubProcess.URL()+"/__test/stats")
	if stats.Exchanges < 6 {
		t.Fatalf("Hub expiry did not force re-exchange; exchanges = %d", stats.Exchanges)
	}

	// Rotate the Google signing key and replace the Hub process over the same
	// durable identity store. The replacement's cold JWKS cache fetches the new
	// key while the original process is no longer serving.
	var databasePath string
	for _, entry := range hubProcess.cmd.Env {
		if strings.HasPrefix(entry, "SCION_TEST_HUB_DATABASE=") {
			databasePath = strings.TrimPrefix(entry, "SCION_TEST_HUB_DATABASE=")
		}
	}
	topology.stopProcess(t, hubProcess)
	response, err := http.Post(fakeGoogle.URL()+"/__test/rotate", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	hub2 := topology.start(t, processSpec{Name: "hub-restarted", Mode: "hub", ReplicaID: "hub-restarted", Env: map[string]string{
		"SCION_TEST_FAKE_GOOGLE_URL": fakeGoogle.URL(), "SCION_TEST_HUB_DATABASE": databasePath,
	}})
	coldReplica := topology.start(t, processSpec{Name: "bridge-cold", Mode: "auth-bridge", ReplicaID: "bridge-cold", Env: map[string]string{"SCION_TEST_HUB_URL": hub2.URL()}})
	changedEmail := fetchMintedToken(t, fakeGoogle.URL(), url.Values{"email": {"renamed-ge-user@gmail.com"}})
	status, body = postBearer(t, coldReplica.URL()+"/request", changedEmail, "")
	if status != http.StatusOK {
		t.Fatalf("changed-email request status = %d: %s", status, body)
	}
	var changed map[string]string
	if err := json.Unmarshal(body, &changed); err != nil {
		t.Fatal(err)
	}
	if changed["user_id"] != firstUserID {
		t.Fatalf("stable subject relinked after email change: got %q, want %q", changed["user_id"], firstUserID)
	}

	// The successful changed-email request above is also the cold-replica,
	// post-restart durability assertion; it could only return the original ID
	// by loading the stable-subject binding from SQLite.
}

func TestGEEnvelopeCompatibility(t *testing.T) {
	topology := newProcessTopology(t, nil)
	fakeGoogle := topology.start(t, processSpec{Name: "fake-google", Mode: "fake-google", ReplicaID: "fake-google"})
	hubProcess := topology.start(t, processSpec{Name: "hub", Mode: "hub", ReplicaID: "hub", Env: map[string]string{
		"SCION_TEST_FAKE_GOOGLE_URL": fakeGoogle.URL(), "SCION_TEST_HUB_DATABASE": filepath.Join(t.TempDir(), "hub.db"),
	}})
	bridgeDir := t.TempDir()
	bridge1 := topology.start(t, processSpec{Name: "bridge-1", Mode: "full-bridge", ReplicaID: "bridge-1", Env: map[string]string{
		"SCION_TEST_HUB_URL": hubProcess.URL(), "SCION_TEST_BRIDGE_DIR": bridgeDir,
	}})
	bridge2 := topology.start(t, processSpec{Name: "bridge-2", Mode: "full-bridge", ReplicaID: "bridge-2", Env: map[string]string{
		"SCION_TEST_HUB_URL": hubProcess.URL(), "SCION_TEST_BRIDGE_DIR": bridgeDir,
	}})
	alternator := topology.start(t, processSpec{Name: "alternator", Mode: "alternator", ReplicaID: "alternator", Env: map[string]string{
		"SCION_TEST_BACKEND_1": bridge1.URL(), "SCION_TEST_BACKEND_2": bridge2.URL(),
	}})

	for _, path := range []string{"/.well-known/agent.json", "/projects/proj1/agents/agent1/.well-known/agent-card.json"} {
		response, err := http.Get(alternator.URL() + path)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if response.StatusCode != http.StatusOK || !json.Valid(body) {
			t.Fatalf("discovery %s status=%d body=%s", path, response.StatusCode, body)
		}
	}

	token := fetchMintedToken(t, fakeGoogle.URL(), nil)
	payload := `{"jsonrpc":"2.0","id":"ge-process-1","method":"SendMessage","params":{"message":{"messageId":"message-001","role":"ROLE_USER","parts":[{"text":"synthetic hello"}]}}}`
	status, body := postBearer(t, alternator.URL()+"/projects/proj1/agents/agent1", token, payload)
	if status != http.StatusOK {
		t.Fatalf("direct JSON-RPC status = %d: %s", status, body)
	}
	var envelope map[string]any
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope["jsonrpc"] != "2.0" || envelope["error"] != nil {
		t.Fatalf("unexpected JSON-RPC envelope: %s", body)
	}
	stats := getJSON[hubProcessStats](t, hubProcess.URL()+"/__test/stats")
	if stats.Messages != 1 || stats.Exchanges != 1 || stats.LastUserID == "" {
		t.Fatalf("Hub receipt counters = %+v; want one exchange and one authenticated message", stats)
	}
}

type fixedTokenSource struct{ token string }

func (s *fixedTokenSource) Token() (string, error) { return s.token, nil }
func (s *fixedTokenSource) SetToken(token string, _ time.Time) {
	s.token = token
}
func (*fixedTokenSource) Expiry() time.Time { return time.Now().Add(time.Hour) }

type serverlessOnlyCredentials struct{ token string }

func (c serverlessOnlyCredentials) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return map[string]string{"x-serverless-authorization": "Bearer " + c.token}, nil
}
func (serverlessOnlyCredentials) RequireTransportSecurity() bool { return false }

func invokeEveryUnary(client brokerv1.BrokerServiceClient) []error {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, errConfigure := client.Configure(ctx, &brokerv1.ConfigureRequest{Config: map[string]string{"hub_url": "http://127.0.0.1"}})
	_, errPublish := client.Publish(ctx, &brokerv1.PublishRequest{Topic: "integration.test", Message: &brokerv1.StructuredMessage{
		Version: 1, Sender: "user:test", Recipient: "agent:test", Msg: "hello", Type: messages.TypeInstruction,
	}})
	_, errSubscribe := client.Subscribe(ctx, &brokerv1.SubscribeRequest{Pattern: "integration.>"})
	_, errUnsubscribe := client.Unsubscribe(ctx, &brokerv1.UnsubscribeRequest{Pattern: "integration.>"})
	_, errHealth := client.HealthCheck(ctx, &brokerv1.HealthCheckRequest{})
	_, errInfo := client.GetInfo(ctx, &brokerv1.GetInfoRequest{})
	return []error{errConfigure, errPublish, errSubscribe, errUnsubscribe, errHealth, errInfo}
}

func TestControlPlanePrincipalIsolation(t *testing.T) {
	const audience = "https://bridge-control.example.invalid"
	topology := newProcessTopology(t, nil)
	fakeGoogle := topology.start(t, processSpec{Name: "fake-google", Mode: "fake-google", ReplicaID: "fake-google"})
	control := topology.start(t, processSpec{Name: "bridge-control", Mode: "grpc-control", ReplicaID: "bridge-control", Env: map[string]string{
		"SCION_TEST_FAKE_GOOGLE_URL": fakeGoogle.URL(), "SCION_TEST_CONTROL_AUDIENCE": audience,
	}})
	hubToken := fetchMintedToken(t, fakeGoogle.URL(), url.Values{
		"audience": {audience}, "email": {"hub-sa@hub-project.iam.gserviceaccount.com"}, "sub": {"hub-service"},
	})
	geToken := fetchMintedToken(t, fakeGoogle.URL(), url.Values{
		"audience": {audience}, "email": {"ge-invoker@ge-project.iam.gserviceaccount.com"}, "sub": {"ge-service"},
	})

	dial := func(perRPCCredentials credentials.PerRPCCredentials) *grpc.ClientConn {
		t.Helper()
		options := []grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}
		if perRPCCredentials != nil {
			options = append(options, grpc.WithPerRPCCredentials(perRPCCredentials))
		}
		connection, err := grpc.NewClient(strings.TrimPrefix(control.URL(), "http://"), options...)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = connection.Close() })
		return connection
	}

	dual := grpcbroker.NewTokenSourceCredentials(&fixedTokenSource{hubToken}, false, grpcbroker.WithCloudRunHeader())
	for index, err := range invokeEveryUnary(brokerv1.NewBrokerServiceClient(dial(dual))) {
		if err != nil {
			t.Fatalf("Hub principal unary method %d failed: %v", index, err)
		}
	}

	for index, err := range invokeEveryUnary(brokerv1.NewBrokerServiceClient(dial(grpcbroker.NewTokenSourceCredentials(&fixedTokenSource{geToken}, false)))) {
		if status.Code(err) != codes.PermissionDenied {
			t.Fatalf("GE Authorization unary method %d code = %s, want PermissionDenied (err=%v)", index, status.Code(err), err)
		}
	}
	for index, err := range invokeEveryUnary(brokerv1.NewBrokerServiceClient(dial(serverlessOnlyCredentials{token: geToken}))) {
		if status.Code(err) != codes.Unauthenticated {
			t.Fatalf("platform-only unary method %d code = %s, want Unauthenticated (err=%v)", index, status.Code(err), err)
		}
	}
}

func TestCombinedStartupMatrix(t *testing.T) {
	validBridge := &bridge.Config{
		Bridge: bridge.BridgeConfig{ExternalURL: "https://bridge.example.invalid"},
		Hub:    bridge.HubConfig{Endpoint: "https://hub.example.invalid", User: "integration@example.invalid"},
		Auth: bridge.AuthConfig{Scheme: "geGoogle", GEExchange: bridge.GEExchangeConfig{
			CredentialType: "id_token", CacheTTL: time.Minute,
		}},
	}
	if err := bridge.ValidateConfig(validBridge); err != nil {
		t.Fatalf("valid GE bridge config: %v", err)
	}
	for name, mutate := range map[string]func(*bridge.Config){
		"missing hub endpoint": func(cfg *bridge.Config) { cfg.Hub.Endpoint = "" },
		"missing hub user":     func(cfg *bridge.Config) { cfg.Hub.User = "" },
		"missing external url": func(cfg *bridge.Config) { cfg.Bridge.ExternalURL = "" },
		"unsupported scheme":   func(cfg *bridge.Config) { cfg.Auth.Scheme = "invalid" },
		"invalid provider url": func(cfg *bridge.Config) { cfg.Bridge.Provider.URL = "://bad" },
		"negative uat ttl":     func(cfg *bridge.Config) { cfg.Auth.UATCacheTTL = -time.Second },
		"oversized uat ttl":    func(cfg *bridge.Config) { cfg.Auth.UATCacheTTL = 301 * time.Second },
	} {
		t.Run(name, func(t *testing.T) {
			copy := *validBridge
			mutate(&copy)
			if err := bridge.ValidateConfig(&copy); err == nil {
				t.Fatal("invalid bridge config passed startup validation")
			}
		})
	}

	for name, cfg := range map[string]grpcbroker.StandaloneServerConfig{
		"missing audience":       {AuthMode: grpcbroker.AuthModeGoogleIDToken, AuthorizedSubjects: []string{"hub@example.invalid"}, ListenAddress: ":50051"},
		"missing subjects":       {AuthMode: grpcbroker.AuthModeGoogleIDToken, Audience: "audience", ListenAddress: ":50051"},
		"missing auth remote":    {ListenAddress: "0.0.0.0:50051"},
		"local dev remote":       {AuthMode: grpcbroker.AuthModeLocalDev, ListenAddress: "0.0.0.0:50051"},
		"cert without key":       {AuthMode: grpcbroker.AuthModeGoogleIDToken, Audience: "audience", AuthorizedSubjects: []string{"hub@example.invalid"}, ListenAddress: ":50051", TLSCertFile: "cert.pem"},
		"client CA without pair": {AuthMode: grpcbroker.AuthModeGoogleIDToken, Audience: "audience", AuthorizedSubjects: []string{"hub@example.invalid"}, ListenAddress: ":50051", TLSClientCAFile: "ca.pem"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := grpcbroker.ValidateStandaloneServerConfig(cfg); err == nil {
				t.Fatal("invalid transport config passed startup validation")
			}
		})
	}

	// Cloud Run terminates TLS and forwards h2c; app authentication remains
	// mandatory on the wildcard listener.
	cloudRun := grpcbroker.StandaloneServerConfig{
		AuthMode: grpcbroker.AuthModeGoogleIDToken, Audience: "https://bridge.run.app",
		AuthorizedSubjects: []string{"hub-sa@project.iam.gserviceaccount.com"},
		ListenAddress:      ":50051", JWKSURL: "http://127.0.0.1/unused",
	}
	if err := grpcbroker.ValidateStandaloneServerConfig(cloudRun); err != nil {
		t.Fatalf("valid Cloud Run h2c config: %v", err)
	}
	if options, err := grpcbroker.BuildStandaloneServerOptions(cloudRun); err != nil {
		t.Fatalf("build Cloud Run server: %v", err)
	} else {
		grpc.NewServer(options...).Stop()
	}

	certFile, keyFile, caFile := writeMTLSFixture(t)
	kubernetes := cloudRun
	kubernetes.ListenAddress = "0.0.0.0:50051"
	kubernetes.TLSCertFile = certFile
	kubernetes.TLSKeyFile = keyFile
	kubernetes.TLSClientCAFile = caFile
	if err := grpcbroker.ValidateStandaloneServerConfig(kubernetes); err != nil {
		t.Fatalf("valid Kubernetes mTLS config: %v", err)
	}
	if options, err := grpcbroker.BuildStandaloneServerOptions(kubernetes); err != nil {
		t.Fatalf("build Kubernetes mTLS server: %v", err)
	} else {
		grpc.NewServer(options...).Stop()
	}
}

func writeMTLSFixture(t *testing.T) (string, string, string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "127.0.0.1"},
		NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour),
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment | x509.KeyUsageCertSign,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		IsCA:        true, BasicConstraintsValid: true, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	certFile := filepath.Join(directory, "server.pem")
	keyFile := filepath.Join(directory, "server-key.pem")
	caFile := filepath.Join(directory, "ca.pem")
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	if err := os.WriteFile(certFile, certPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, keyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(caFile, certPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	return certFile, keyFile, caFile
}

func TestCredentialRedaction(t *testing.T) {
	upstream := fetchSyntheticTokenSeed(t)
	redactor := newCredentialRedactor(upstream)
	topology := newProcessTopology(t, redactor)
	fakeGoogle := topology.start(t, processSpec{Name: "fake-google", Mode: "fake-google", ReplicaID: "fake-google"})
	hubProcess := topology.start(t, processSpec{Name: "hub", Mode: "hub", ReplicaID: "hub", Env: map[string]string{
		"SCION_TEST_FAKE_GOOGLE_URL": fakeGoogle.URL(), "SCION_TEST_HUB_DATABASE": filepath.Join(t.TempDir(), "hub.db"),
	}})
	bridge1 := topology.start(t, processSpec{Name: "bridge-1", Mode: "full-bridge", ReplicaID: "bridge-1", Env: map[string]string{
		"SCION_TEST_HUB_URL": hubProcess.URL(), "SCION_TEST_BRIDGE_DIR": t.TempDir(),
	}})
	token := fetchMintedToken(t, fakeGoogle.URL(), url.Values{"sub": {upstream}})
	redactor.add(token)
	payload := `{"jsonrpc":"2.0","id":"redaction-1","method":"SendMessage","params":{"message":{"messageId":"redaction-message","role":"ROLE_USER","parts":[{"text":"redaction check"}]}}}`
	status, body := postBearer(t, bridge1.URL()+"/projects/proj1/agents/agent1", token, payload)
	if status != http.StatusOK {
		t.Fatalf("redaction request status = %d: %s", status, body)
	}
	captured := getJSON[map[string]string](t, hubProcess.URL()+"/__test/captured-bearer")
	if captured["bearer"] == "" {
		t.Fatal("Hub did not capture the exchanged bearer used by the executor")
	}
	redactor.add(captured["bearer"])
	combined := topology.logs.String() + topology.observations.String()
	for _, credential := range []string{upstream, token, captured["bearer"]} {
		digest := sha256Bytes(credential)
		for _, forbidden := range []string{
			credential,
			hex.EncodeToString(digest),
			base64.StdEncoding.EncodeToString(digest),
			base64.RawURLEncoding.EncodeToString(digest),
		} {
			if strings.Contains(combined, forbidden) {
				t.Fatalf("combined process evidence contains credential material %q", forbidden)
			}
		}
	}
	for _, safe := range []string{`"replica_id"`, `"outcome"`} {
		if !strings.Contains(combined, safe) {
			t.Fatalf("combined observations lost safe field %s: %s", safe, combined)
		}
	}
}

func fetchSyntheticTokenSeed(t *testing.T) string {
	t.Helper()
	return "SYNTHETIC_UPSTREAM_CREDENTIAL_" + strconv.FormatInt(time.Now().UnixNano(), 10)
}

func sha256Bytes(value string) []byte {
	digest := sha256.Sum256([]byte(value))
	return digest[:]
}
