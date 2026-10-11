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

	smpb "cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func newCopyForwardMockSMClient() *copyForwardMockSMClient {
	return &copyForwardMockSMClient{
		secrets:  make(map[string]*smpb.Secret),
		versions: make(map[string][]byte),
	}
}

// copyForwardMockSMClient is a minimal secret.SMClient fake for exercising
// the hub-startup signing-key copy-forward (ensureSigningKey,
// OIDCKeyManager.loadOrCreateKey) against a real *secret.GCPBackend, without
// any GCP network calls — per the ptone/scion#2152 constraint that tests run
// only against fakes/mocks.
type copyForwardMockSMClient struct {
	mu       sync.Mutex
	secrets  map[string]*smpb.Secret
	versions map[string][]byte
}

func (m *copyForwardMockSMClient) CreateSecret(_ context.Context, req *smpb.CreateSecretRequest) (*smpb.Secret, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	fullName := fmt.Sprintf("%s/secrets/%s", req.Parent, req.SecretId)
	if _, exists := m.secrets[fullName]; exists {
		return nil, status.Errorf(codes.AlreadyExists, "secret %s already exists", fullName)
	}
	sec := &smpb.Secret{Name: fullName, Labels: req.Secret.GetLabels()}
	m.secrets[fullName] = sec
	return sec, nil
}

func (m *copyForwardMockSMClient) AddSecretVersion(_ context.Context, req *smpb.AddSecretVersionRequest) (*smpb.SecretVersion, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.secrets[req.Parent]; !exists {
		return nil, status.Errorf(codes.NotFound, "secret %s not found", req.Parent)
	}
	m.versions[req.Parent] = req.Payload.Data
	return &smpb.SecretVersion{Name: req.Parent + "/versions/1"}, nil
}

func (m *copyForwardMockSMClient) AccessSecretVersion(_ context.Context, req *smpb.AccessSecretVersionRequest) (*smpb.AccessSecretVersionResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	name := req.Name
	for _, suffix := range []string{"/versions/latest", "/versions/1"} {
		if len(name) > len(suffix) && name[len(name)-len(suffix):] == suffix {
			name = name[:len(name)-len(suffix)]
			break
		}
	}
	data, exists := m.versions[name]
	if !exists {
		return nil, status.Errorf(codes.NotFound, "version not found for %s", req.Name)
	}
	return &smpb.AccessSecretVersionResponse{Name: req.Name, Payload: &smpb.SecretPayload{Data: data}}, nil
}

func (m *copyForwardMockSMClient) DeleteSecret(_ context.Context, req *smpb.DeleteSecretRequest) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.secrets[req.Name]; !exists {
		return status.Errorf(codes.NotFound, "secret %s not found", req.Name)
	}
	delete(m.secrets, req.Name)
	delete(m.versions, req.Name)
	return nil
}

func (m *copyForwardMockSMClient) GetSecret(_ context.Context, req *smpb.GetSecretRequest) (*smpb.Secret, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	sec, exists := m.secrets[req.Name]
	if !exists {
		return nil, status.Errorf(codes.NotFound, "secret %s not found", req.Name)
	}
	return sec, nil
}

func (m *copyForwardMockSMClient) Close() error { return nil }

func (m *copyForwardMockSMClient) seed(t *testing.T, projectID, smName, value string) {
	t.Helper()
	ctx := context.Background()
	fullName := fmt.Sprintf("projects/%s/secrets/%s", projectID, smName)
	if _, err := m.CreateSecret(ctx, &smpb.CreateSecretRequest{
		Parent: fmt.Sprintf("projects/%s", projectID), SecretId: smName, Secret: &smpb.Secret{},
	}); err != nil {
		t.Fatalf("failed to seed mock secret %s: %v", smName, err)
	}
	if _, err := m.AddSecretVersion(ctx, &smpb.AddSecretVersionRequest{
		Parent: fullName, Payload: &smpb.SecretPayload{Data: []byte(value)},
	}); err != nil {
		t.Fatalf("failed to seed mock secret version %s: %v", smName, err)
	}
}
