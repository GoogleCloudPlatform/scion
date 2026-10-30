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

//go:build !no_sqlite

package hub

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/runtimebroker"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// End-to-end delivery of an attachment (ptone/scion#4596, section 8.9), in
// process: an agent uploads a file and sends it to an agent on a broker;
// the hub's HTTP dispatcher issues the capability and sends the message to
// a real runtimebroker.Server over HTTP; the broker redeems the capability
// against the hub's full handler chain with its HMAC credential, verifies
// the file and writes it with the real inbox script, run in a local shell
// against a temporary agent home, before the text is delivered.
//
// Limits: the agent manager is a stand-in that runs the message's
// pre-delivery work and records the text (the real manager's ordering is
// pkg/agent TestInboxWriter_WritesBeforeText); the human's view of the
// attachment is covered by the package 1 and 2 read tests.

// enableBrokerHMAC turns on broker HMAC authentication on f's server and
// returns the secret of broker brokerID. It runs before any request uses
// the handler chain: Handler() builds the chain from these fields on each
// call.
func enableBrokerHMAC(t *testing.T, f *sendFixture, brokerID string) []byte {
	t.Helper()
	if f.srv.brokerAuthService == nil {
		svc := NewBrokerAuthService(DefaultBrokerAuthConfig(), f.st)
		t.Cleanup(svc.Close)
		f.srv.brokerAuthService = svc
		f.srv.authConfig.BrokerAuthSvc = svc
	}
	return seedBrokerSecret(t, f.st, brokerID)
}

// e2eHomeRuntime runs the broker's exec commands in a local shell with HOME
// set to the agent home, after unwrapping ExecAsUserCmd, and records the
// order of the writes and the text delivery.
type e2eHomeRuntime struct {
	runtime.Runtime
	home string
	mu   sync.Mutex
	log  []string
}

func (r *e2eHomeRuntime) Name() string { return "docker" }

func (r *e2eHomeRuntime) ExecWithStdin(ctx context.Context, _ string, cmd []string, stdin io.Reader) (string, error) {
	data, err := io.ReadAll(stdin)
	if err != nil {
		return "", err
	}
	r.note("exec")
	argv := cmd
	if len(cmd) == 6 && cmd[0] == "sh" && cmd[3] == "exec-as-user" {
		argv = []string{"/bin/sh", "-c", cmd[5]}
	}
	c := exec.CommandContext(ctx, argv[0], argv[1:]...)
	c.Env = append(os.Environ(), "HOME="+r.home)
	c.Stdin = bytes.NewReader(data)
	out, err := c.CombinedOutput()
	return string(out), err
}

func (r *e2eHomeRuntime) note(s string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.log = append(r.log, s)
}

func (r *e2eHomeRuntime) events() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.log...)
}

// e2eMessageManager stands in for the broker's agent manager: Message runs
// the message's pre-delivery work against the runtime, then records the
// text it would deliver.
type e2eMessageManager struct {
	agent.Manager
	rt    *e2eHomeRuntime
	mu    sync.Mutex
	texts []string
}

func (m *e2eMessageManager) List(context.Context, map[string]string) ([]api.AgentInfo, error) {
	return nil, nil
}

func (m *e2eMessageManager) Message(ctx context.Context, _, _, text string, _ bool) error {
	extra := ""
	if pd := agent.PreDeliveryFromContext(ctx); pd != nil {
		extra = pd.BeforeText(ctx, m.rt, "agent-container")
	}
	m.rt.note("text")
	m.mu.Lock()
	defer m.mu.Unlock()
	m.texts = append(m.texts, text+extra)
	return nil
}

func (m *e2eMessageManager) delivered() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.texts...)
}

func TestAttachmentDelivery_EndToEnd(t *testing.T) {
	f := newSendFixture(t)
	f.enableBrokerDelivery(t)
	ctx := context.Background()
	content := "end to end bytes\n"
	msgID, call := f.sendWithFile(t, "report.txt", content)
	require.Len(t, call.StructuredMessage.AttachmentPlan, 1)
	path := call.StructuredMessage.AttachmentPlan[0].InboxPath
	require.Contains(t, call.StructuredMessage.DeliveryText+strings.Join(call.StructuredMessage.Attachments, "\n")+
		strings.Join(legacyAttachmentCopy(call.StructuredMessage).Attachments, "\n"), path,
		"the message the recipient gets names the inbox path")

	// The hub, with broker HMAC authentication, behind a real listener.
	key := enableBrokerHMAC(t, f, f.brokerID)
	hubTS := httptest.NewServer(f.srv.Handler())
	t.Cleanup(hubTS.Close)

	// The broker, its hub client authenticated with its HMAC secret.
	rt := &e2eHomeRuntime{home: t.TempDir()}
	mgr := &e2eMessageManager{rt: rt}
	cfg := runtimebroker.DefaultServerConfig()
	cfg.BrokerAuthEnabled = false // no hub keys here: serve the broker API without auth
	cfg.BrokerID = f.brokerID
	cfg.BrokerName = "send-broker"
	cfg.StateDir = t.TempDir()
	brokerSrv := runtimebroker.New(cfg, mgr, rt, config.RuntimeBuildRecord{})
	hc, err := hubclient.New(hubTS.URL, hubclient.WithHMACAuth(f.brokerID, key))
	require.NoError(t, err)
	brokerSrv.SetHubClient(hc)
	// The message request the broker receives, as sent over HTTP.
	var retentionMu sync.Mutex
	var retention interface{}
	brokerTS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/message") {
			body, _ := io.ReadAll(r.Body)
			var req map[string]interface{}
			if json.Unmarshal(body, &req) == nil {
				retentionMu.Lock()
				retention = req["attachment_retention_days"]
				retentionMu.Unlock()
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
		}
		brokerSrv.Handler().ServeHTTP(w, r)
	}))
	t.Cleanup(brokerTS.Close)
	b, err := f.st.GetRuntimeBroker(ctx, f.brokerID)
	require.NoError(t, err)
	b.Endpoint = brokerTS.URL
	require.NoError(t, f.st.UpdateRuntimeBroker(ctx, b))

	// The hub's HTTP dispatcher sends the message the send rendered.
	d := NewHTTPAgentDispatcherWithClient(f.st, NewHTTPRuntimeBrokerClient(), false, slog.Default())
	d.SetAttachmentIssuer(f.srv.attachmentDelivery())
	require.NoError(t, d.DispatchAgentMessage(withDispatchMessageID(ctx, msgID), f.target, call.Message, false, call.StructuredMessage))

	// The file is in the recipient's home at the path the message names,
	// with the sent bytes, written before the text was delivered.
	require.True(t, strings.HasPrefix(path, "~/"), path)
	got, err := os.ReadFile(filepath.Join(rt.home, strings.TrimPrefix(path, "~/")))
	require.NoError(t, err)
	want := sha256.Sum256([]byte(content))
	gotSum := sha256.Sum256(got)
	require.Equal(t, hex.EncodeToString(want[:]), hex.EncodeToString(gotSum[:]))
	texts := mgr.delivered()
	require.Len(t, texts, 1)
	require.NotContains(t, texts[0], "Attachment not delivered")
	events := rt.events()
	require.NotEmpty(t, events)
	require.Equal(t, "exec", events[0], "the write runs before the text: %v", events)
	require.Contains(t, events, "text")
	retentionMu.Lock()
	defer retentionMu.Unlock()
	require.EqualValues(t, DefaultAttachmentInboxRetentionDays, retention, "the broker receives the inbox retention in the request body")
}

// TestDeliveryRedemption_BrokerHMAC: redemption through the hub's broker
// HMAC authentication on the full handler chain. The issuing broker's
// signed request gets the file; another broker's signed request, and a
// request with a wrong signature, are refused without spending the
// capability.
func TestDeliveryRedemption_BrokerHMAC(t *testing.T) {
	f := newSendFixture(t)
	token, _ := fixtureToken(t, f)
	key := enableBrokerHMAC(t, f, f.brokerID)
	path := attachmentDeliveriesPrefix + token

	other := uuid.NewString()
	require.NoError(t, f.st.CreateRuntimeBroker(context.Background(), &store.RuntimeBroker{
		ID: other, Name: "other-broker", Slug: "other-broker", Status: store.BrokerStatusOnline,
	}))
	otherKey := seedBrokerSecret(t, f.st, other)
	wrong := doBrokerSignedRequest(t, f.srv, other, otherKey, "", http.MethodGet, path, nil)
	require.Equal(t, http.StatusNotFound, wrong.Code, "another broker's signed request")

	forged := doBrokerSignedRequest(t, f.srv, f.brokerID, []byte("not-the-broker-secret-0123456789ab"), "", http.MethodGet, path, nil)
	require.NotEqual(t, http.StatusOK, forged.Code, "a wrong signature")
	require.NotContains(t, forged.Body.String(), "file bytes")

	ok := doBrokerSignedRequest(t, f.srv, f.brokerID, key, "", http.MethodGet, path, nil)
	require.Equal(t, http.StatusOK, ok.Code, ok.Body.String())
	require.Equal(t, "file bytes", ok.Body.String())
}
