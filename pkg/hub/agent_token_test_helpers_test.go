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

package hub

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// issueAgentTokenForTest issues a token for the stored agent record the way
// a dispatcher mint site does: authorize, sign for the agent's current run,
// record the credential.
func (s *Server) issueAgentTokenForTest(ctx context.Context, agent *store.Agent) (string, error) {
	grant, err := s.AuthorizeAgentToken(ctx, agent)
	if err != nil {
		return "", err
	}
	return signAndRecordAgentToken(ctx, s, s.store, grant, agent.RunID)
}

// generateRecordedAgentToken signs an agent JWT with custom scopes and
// ancestry, records its credential in s.store, and fails t on any error.
//
// Mechanical migration from the unrecorded 4-arg AgentTokenService.GenerateAgentToken:
//
//	before: tok, err := srv.GetAgentTokenService().GenerateAgentToken(agentID, projectID, scopes, ancestry)
//	        require.NoError(t, err)
//	after:  tok := srv.generateRecordedAgentToken(t, agentID, projectID, scopes, ancestry)
//
// For tests without a *Server (e.g. AuthConfig middleware unit tests) or tests
// using a custom *AgentTokenService (e.g. an expired-token service), use the
// package-level generateRecordedAgentToken with newMemAgentCredentialStore() or
// srv.store.
func (s *Server) generateRecordedAgentToken(t testing.TB, agentID, projectID string, scopes []AgentTokenScope, ancestry []string) string {
	t.Helper()
	s.mu.RLock()
	svc := s.agentTokenService
	s.mu.RUnlock()
	return generateRecordedAgentToken(t, svc, s.store, agentID, projectID, scopes, ancestry)
}

// generateRecordedAgentToken signs an agent JWT via svc, records its credential
// in credStore, and fails t on any error. Use this in tests that build
// AuthConfig directly without a *Server (pairing with
// newMemAgentCredentialStore) or that use a custom *AgentTokenService.
//
// Example for AuthConfig middleware unit tests:
//
//	credStore := newMemAgentCredentialStore()
//	cfg := AuthConfig{
//	    AgentTokenSvc:   agentTokenSvc,
//	    CredentialStore: credStore,
//	}
//	tok := generateRecordedAgentToken(t, agentTokenSvc, credStore, agentID, projectID, scopes, ancestry)
func generateRecordedAgentToken(t testing.TB, svc *AgentTokenService, credStore store.AgentCredentialStore, agentID, projectID string, scopes []AgentTokenScope, ancestry []string) string {
	t.Helper()
	if svc == nil {
		t.Fatal("generateRecordedAgentToken: agent token service is nil")
	}
	if credStore == nil {
		t.Fatal("generateRecordedAgentToken: credential store is nil")
	}
	tok, cred, err := svc.SignAgentToken(AgentTokenGrant{
		AgentID:   agentID,
		ProjectID: projectID,
		Scopes:    scopes,
		Ancestry:  ancestry,
	}, "")
	if err != nil {
		t.Fatalf("generateRecordedAgentToken: sign token: %v", err)
	}
	if err := credStore.CreateAgentCredential(context.Background(), cred); err != nil {
		t.Fatalf("generateRecordedAgentToken: record credential: %v", err)
	}
	return tok
}

// memAgentCredentialStore is a thread-safe in-memory store.AgentCredentialStore
// for middleware unit tests that construct AuthConfig directly without a SQLite
// or Postgres store.
type memAgentCredentialStore struct {
	mu     sync.RWMutex
	byID   map[string]*store.AgentCredential
	byHash map[string]*store.AgentCredential
	nextID int
}

var _ store.AgentCredentialStore = (*memAgentCredentialStore)(nil)

func newMemAgentCredentialStore() *memAgentCredentialStore {
	return &memAgentCredentialStore{
		byID:   make(map[string]*store.AgentCredential),
		byHash: make(map[string]*store.AgentCredential),
	}
}

func cloneAgentCredential(c *store.AgentCredential) *store.AgentCredential {
	if c == nil {
		return nil
	}
	cp := *c
	if c.RevokedAt != nil {
		ts := *c.RevokedAt
		cp.RevokedAt = &ts
	}
	if c.RevokedBy != nil {
		s := *c.RevokedBy
		cp.RevokedBy = &s
	}
	if c.RevokeReason != nil {
		s := *c.RevokeReason
		cp.RevokeReason = &s
	}
	if c.LastSeenAt != nil {
		ts := *c.LastSeenAt
		cp.LastSeenAt = &ts
	}
	return &cp
}

func (m *memAgentCredentialStore) CreateAgentCredential(_ context.Context, cred *store.AgentCredential) error {
	if cred == nil {
		return fmt.Errorf("agent credential is nil")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.byHash[cred.TokenJTIHash]; exists {
		return store.ErrAlreadyExists
	}
	if cred.ID == "" {
		m.nextID++
		cred.ID = fmt.Sprintf("mem-cred-%d", m.nextID)
	}
	cp := cloneAgentCredential(cred)
	m.byID[cp.ID] = cp
	m.byHash[cp.TokenJTIHash] = cp
	return nil
}

func (m *memAgentCredentialStore) GetAgentCredentialByJTIHash(_ context.Context, jtiHash string) (*store.AgentCredential, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	c, ok := m.byHash[jtiHash]
	if !ok {
		return nil, store.ErrNotFound
	}
	return cloneAgentCredential(c), nil
}

func (m *memAgentCredentialStore) GetAgentCredentialByID(_ context.Context, id string) (*store.AgentCredential, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	c, ok := m.byID[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	return cloneAgentCredential(c), nil
}

func (m *memAgentCredentialStore) RevokeAgentCredential(_ context.Context, id string, revokedBy string, reason string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.byID[id]
	if !ok {
		return store.ErrNotFound
	}
	now := time.Now().UTC()
	by := revokedBy
	rsn := reason
	c.RevokedAt = &now
	c.RevokedBy = &by
	c.RevokeReason = &rsn
	return nil
}

func (m *memAgentCredentialStore) RevokeAgentCredentialsByAgent(_ context.Context, agentID string, revokedBy string, reason string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now().UTC()
	n := 0
	for _, c := range m.byID {
		if c.AgentID == agentID && c.RevokedAt == nil {
			t := now
			by := revokedBy
			rsn := reason
			c.RevokedAt = &t
			c.RevokedBy = &by
			c.RevokeReason = &rsn
			n++
		}
	}
	return n, nil
}

func (m *memAgentCredentialStore) UpdateAgentCredentialLastSeen(_ context.Context, id string, lastSeen time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.byID[id]
	if !ok {
		return store.ErrNotFound
	}
	ts := lastSeen
	c.LastSeenAt = &ts
	return nil
}

func (m *memAgentCredentialStore) PurgeExpiredAgentCredentials(_ context.Context, cutoff time.Time) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for id, c := range m.byID {
		if c.ExpiresAt.Before(cutoff) {
			delete(m.byID, id)
			delete(m.byHash, c.TokenJTIHash)
			n++
		}
	}
	return n, nil
}

func (m *memAgentCredentialStore) HasAgentCredentialForRun(_ context.Context, agentID, runID string) (bool, error) {
	if runID == "" {
		return false, nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, c := range m.byID {
		if c.AgentID == agentID && c.RunID == runID {
			return true, nil
		}
	}
	return false, nil
}

func (m *memAgentCredentialStore) DeleteAgentCredentialsByProject(_ context.Context, projectID string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for id, c := range m.byID {
		if c.ProjectID == projectID {
			delete(m.byID, id)
			delete(m.byHash, c.TokenJTIHash)
			n++
		}
	}
	return n, nil
}
