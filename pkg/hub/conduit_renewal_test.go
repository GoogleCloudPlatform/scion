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
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/grant"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
)

// fakeRenewalGrants is a conduitRenewalGrants test double.
type fakeRenewalGrants struct {
	mu        sync.Mutex
	claims    conduitRecheckClaims
	verifyErr error
	mintErr   error
	until     time.Time
	verified  []conduitRenewalBinding
	minted    []conduitRenewalMint
}

func (f *fakeRenewalGrants) VerifyForRecheck(_ context.Context, latest []byte, b conduitRenewalBinding) (conduitRecheckClaims, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.verified = append(f.verified, b)
	return f.claims, f.verifyErr
}

func (f *fakeRenewalGrants) Mint(_ context.Context, in conduitRenewalMint) ([]byte, time.Time, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.minted = append(f.minted, in)
	if f.mintErr != nil {
		return nil, time.Time{}, f.mintErr
	}
	return []byte("renewal-for-" + in.OpenJTI), f.until, nil
}

func streamRecheck(t *testing.T, c *relay.HubClient, items ...relay.HubStreamRecheckItem) []relay.HubStreamRecheckResult {
	t.Helper()
	var resp relay.HubStreamRecheckResponse
	require.NoError(t, c.Call(context.Background(), relay.HubStreamRecheckPath, relay.HubStreamRecheckRequest{Items: items}, &resp))
	require.Len(t, resp.Results, len(items))
	return resp.Results
}

// TestRelayStreamRecheck (design §3.9, §3.5): the hub verifies the latest
// grant, decides with the in-process rules for the principal the grant
// names, and returns the minted renewal unchanged, or refuses (4401), or
// is unavailable.
func TestRelayStreamRecheck(t *testing.T) {
	f, cf := newUserRelayOps(t)
	adm := f.mustAdmit(t, bearer(ownerJWT(t, cf)), "user-s1")
	until := time.Now().Add(8 * time.Hour).Truncate(time.Second)
	allowed := conduitRecheckClaims{
		UserID: cf.owner.ID(), AgentID: cf.agent.ID, ProjectID: cf.agent.ProjectID,
		Kind: grant.StreamKindTCP, Params: map[string]string{grant.ParamHost: "127.0.0.1", grant.ParamPort: "3000"},
	}
	item := relay.HubStreamRecheckItem{
		UserSessionID: "user-s1", AdmissionToken: adm.AdmissionToken,
		TargetSessionID: "agent-s1", TargetStreamID: 7, OpenJTI: "jti-1",
		LatestGrant: []byte("latest-grant-bytes"), Trigger: "sweep",
	}

	t.Run("no renewal source: unavailable", func(t *testing.T) {
		res := streamRecheck(t, f.hub, item)
		assert.False(t, res[0].Renew)
		assert.False(t, res[0].Close, "nothing closes when the hub cannot evaluate")
	})

	g := &fakeRenewalGrants{claims: allowed, until: until}
	cf.srv.setConduitRenewalGrants(g)

	t.Run("renewed: the minted grant is returned unchanged", func(t *testing.T) {
		res := streamRecheck(t, f.hub, item)
		require.True(t, res[0].Renew, "%+v", res[0])
		assert.Equal(t, []byte("renewal-for-jti-1"), res[0].RenewalGrant)
		assert.True(t, until.Equal(res[0].AuthzUntil))
		g.mu.Lock()
		defer g.mu.Unlock()
		require.NotEmpty(t, g.minted)
		m := g.minted[len(g.minted)-1]
		assert.Equal(t, []byte("latest-grant-bytes"), m.LatestGrant, "the latest grant reaches the mint unchanged")
		assert.Equal(t, conduitRenewalMint{
			UserID: cf.owner.ID(), UserSessionID: "user-s1", TargetSessionID: "agent-s1", TargetStreamID: 7,
			OpenJTI: "jti-1", Kind: grant.StreamKindTCP, Params: allowed.Params, LatestGrant: []byte("latest-grant-bytes"),
		}, m)
		assert.Equal(t, conduitRenewalBinding{TargetSessionID: "agent-s1", TargetStreamID: 7, OpenJTI: "jti-1"}, g.verified[len(g.verified)-1])
	})

	t.Run("grant does not verify (inputs do not match its claims): 4401", func(t *testing.T) {
		g.verifyErr = fmt.Errorf("%w: stream id mismatch", errConduitRenewalRefused)
		defer func() { g.verifyErr = nil }()
		res := streamRecheck(t, f.hub, item)
		assert.True(t, res[0].Close)
		assert.Equal(t, uint32(4401), res[0].CloseCode)
	})

	t.Run("grant names another principal: 4401", func(t *testing.T) {
		other := allowed
		other.UserID = cf.stranger.ID()
		g.claims = other
		defer func() { g.claims = allowed }()
		res := streamRecheck(t, f.hub, item)
		assert.True(t, res[0].Close)
		assert.Equal(t, uint32(4401), res[0].CloseCode)
	})

	t.Run("permission no longer held: 4401", func(t *testing.T) {
		denied := allowed
		denied.Params = map[string]string{grant.ParamHost: "127.0.0.1", grant.ParamPort: "9999"}
		g.claims = denied
		defer func() { g.claims = allowed }()
		res := streamRecheck(t, f.hub, item)
		assert.True(t, res[0].Close, "a port no longer authorized")
		assert.Equal(t, uint32(4401), res[0].CloseCode)
	})

	t.Run("verify cannot evaluate: unavailable", func(t *testing.T) {
		g.verifyErr = errConduitRenewalUnavailable
		defer func() { g.verifyErr = nil }()
		res := streamRecheck(t, f.hub, item)
		assert.False(t, res[0].Renew)
		assert.False(t, res[0].Close)
	})

	t.Run("a bad user session token: 4401", func(t *testing.T) {
		bad := item
		bad.AdmissionToken = tamper(adm.AdmissionToken)
		res := streamRecheck(t, f.hub, bad)
		assert.True(t, res[0].Close)
	})
}
