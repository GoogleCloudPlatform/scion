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

package relay

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/api/idtoken"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

type staticTokenSource struct{ tok string }

func (s staticTokenSource) Token() (string, error) { return s.tok, nil }

// newOIDCModeAuth returns an oidc-mode authenticator for selfID whose ID
// tokens always verify as this node's service account.
func newOIDCModeAuth(t *testing.T, selfID, token string, _ *string, mod func(*ConduitPeerAuthOptions)) PeerAuth {
	t.Helper()
	o := ConduitPeerAuthOptions{
		Mode: config.ConduitPeerAuthOIDC, SelfID: selfID, SharedSecret: "shared-signing-secret-0123456789ab",
		OwnServiceAccount: "hub@p.iam.gserviceaccount.com",
		TokenSource:       staticTokenSource{tok: token},
		Validate: func(_ context.Context, _, aud string) (*idtoken.Payload, error) {
			return &idtoken.Payload{Audience: aud, Claims: map[string]any{"email": "hub@p.iam.gserviceaccount.com", "email_verified": true}}, nil
		},
	}
	if mod != nil {
		mod(&o)
	}
	a, mode, err := NewConduitPeerAuth(o)
	require.NoError(t, err)
	require.Equal(t, config.ConduitPeerAuthOIDC, mode)
	return a
}

// TestConduitPeerAuth_DefaultComputeSAWarning: an OIDC allow-list that
// falls back to a Compute Engine default service account logs one WARN per
// process; an explicit list or a dedicated account logs none.
func TestConduitPeerAuth_DefaultComputeSAWarning(t *testing.T) {
	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(orig); conduitDefaultSAWarnOnce = sync.Once{} })

	const computeSA = "123456789-compute@developer.gserviceaccount.com"
	tests := []struct {
		name      string
		own       string
		explicit  []string
		wantWarns int
	}{
		{name: "dedicated own SA", own: "hub@p.iam.gserviceaccount.com"},
		{name: "explicit list with a compute own SA", own: computeSA, explicit: []string{"hub@p.iam.gserviceaccount.com"}},
		{name: "fallback to a compute SA", own: computeSA, wantWarns: 1},
		{name: "fallback to a compute SA, upper case", own: strings.ToUpper(computeSA), wantWarns: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			buf.Reset()
			conduitDefaultSAWarnOnce = sync.Once{}
			for range 2 { // the second build in the same process is silent
				newOIDCModeAuth(t, "hub-a", "own", nil, func(o *ConduitPeerAuthOptions) {
					o.OwnServiceAccount, o.ServiceAccounts = tt.own, tt.explicit
				})
			}
			got := strings.Count(buf.String(), "Compute Engine default service account")
			assert.Equal(t, tt.wantWarns, got, buf.String())
			if tt.wantWarns > 0 {
				assert.Contains(t, buf.String(), "peer_service_accounts")
			}
		})
	}
}
