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
	"log/slog"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

func TestResolveSecrets(t *testing.T) {
	memStore := createTestStore(t)
	ctx := context.Background()

	// Create test secrets across multiple scopes
	userSecret := &store.Secret{
		ID:             "s1",
		Key:            "API_KEY",
		EncryptedValue: "user-api-key",
		SecretType:     store.SecretTypeEnvironment,
		Target:         "API_KEY",
		Scope:          store.ScopeUser,
		ScopeID:        "user-1",
	}
	projectSecret := &store.Secret{
		ID:             "s2",
		Key:            "DB_PASS",
		EncryptedValue: "project-db-pass",
		SecretType:     store.SecretTypeEnvironment,
		Target:         "DATABASE_PASSWORD",
		Scope:          store.ScopeProject,
		ScopeID:        "project-1",
	}
	// Project-level override of user API_KEY
	projectOverride := &store.Secret{
		ID:             "s3",
		Key:            "API_KEY",
		EncryptedValue: "project-api-key",
		SecretType:     store.SecretTypeEnvironment,
		Target:         "API_KEY",
		Scope:          store.ScopeProject,
		ScopeID:        "project-1",
	}
	fileSecret := &store.Secret{
		ID:             "s4",
		Key:            "TLS_CERT",
		EncryptedValue: "cert-data",
		SecretType:     store.SecretTypeFile,
		Target:         "/etc/ssl/cert.pem",
		Scope:          store.ScopeUser,
		ScopeID:        "user-1",
	}
	varSecret := &store.Secret{
		ID:             "s5",
		Key:            "CONFIG",
		EncryptedValue: `{"key":"val"}`,
		SecretType:     store.SecretTypeVariable,
		Target:         "config",
		Scope:          store.ScopeUser,
		ScopeID:        "user-1",
	}

	for _, s := range []*store.Secret{userSecret, projectSecret, projectOverride, fileSecret, varSecret} {
		if err := memStore.CreateSecret(ctx, s); err != nil {
			t.Fatalf("failed to create test secret %s: %v", s.Key, err)
		}
	}

	// Create dispatcher with local backend (reads work, writes are blocked)
	backend := secret.NewLocalBackend(memStore, "test-hub-id")
	mockClient := &mockRuntimeBrokerClient{}
	dispatcher := NewHTTPAgentDispatcherWithClient(memStore, mockClient, false, slog.Default())
	dispatcher.SetSecretBackend(backend)

	agent := &store.Agent{
		ID:      "agent-1",
		Name:    "test-agent",
		OwnerID: "user-1",
		ProjectID: "project-1",
	}

	resolved, err := dispatcher.resolveSecrets(ctx, agent)
	if err != nil {
		t.Fatalf("resolveSecrets failed: %v", err)
	}

	// Build a map for easier assertions
	byName := make(map[string]ResolvedSecret)
	for _, rs := range resolved {
		byName[rs.Name] = rs
	}

	// API_KEY should be overridden by project scope
	apiKey, ok := byName["API_KEY"]
	if !ok {
		t.Fatal("expected API_KEY in resolved secrets")
	}
	if apiKey.Value != "project-api-key" {
		t.Errorf("expected API_KEY value from project scope %q, got %q", "project-api-key", apiKey.Value)
	}
	if apiKey.Source != store.ScopeProject {
		t.Errorf("expected API_KEY source %q, got %q", store.ScopeProject, apiKey.Source)
	}

	// DB_PASS should come from project scope
	dbPass, ok := byName["DB_PASS"]
	if !ok {
		t.Fatal("expected DB_PASS in resolved secrets")
	}
	if dbPass.Value != "project-db-pass" {
		t.Errorf("expected DB_PASS value %q, got %q", "project-db-pass", dbPass.Value)
	}
	if dbPass.Target != "DATABASE_PASSWORD" {
		t.Errorf("expected DB_PASS target %q, got %q", "DATABASE_PASSWORD", dbPass.Target)
	}

	// TLS_CERT should be a file type from user scope
	cert, ok := byName["TLS_CERT"]
	if !ok {
		t.Fatal("expected TLS_CERT in resolved secrets")
	}
	if cert.Type != store.SecretTypeFile {
		t.Errorf("expected TLS_CERT type %q, got %q", store.SecretTypeFile, cert.Type)
	}
	if cert.Target != "/etc/ssl/cert.pem" {
		t.Errorf("expected TLS_CERT target %q, got %q", "/etc/ssl/cert.pem", cert.Target)
	}

	// CONFIG should be a variable type
	config, ok := byName["CONFIG"]
	if !ok {
		t.Fatal("expected CONFIG in resolved secrets")
	}
	if config.Type != store.SecretTypeVariable {
		t.Errorf("expected CONFIG type %q, got %q", store.SecretTypeVariable, config.Type)
	}

	// Total count: API_KEY, DB_PASS, TLS_CERT, CONFIG = 4
	if len(resolved) != 4 {
		t.Errorf("expected 4 resolved secrets, got %d", len(resolved))
	}
}

func TestResolveSecrets_WithBackend(t *testing.T) {
	memStore := createTestStore(t)
	ctx := context.Background()

	// Seed secrets directly through the store
	for _, s := range []*store.Secret{
		{
			ID:             "s1",
			Key:            "API_KEY",
			EncryptedValue: "user-api-key",
			SecretType:     store.SecretTypeEnvironment,
			Target:         "API_KEY",
			Scope:          store.ScopeUser,
			ScopeID:        "user-1",
		},
		{
			ID:             "s2",
			Key:            "API_KEY",
			EncryptedValue: "project-api-key",
			SecretType:     store.SecretTypeEnvironment,
			Target:         "API_KEY",
			Scope:          store.ScopeProject,
			ScopeID:        "project-1",
		},
		{
			ID:             "s3",
			Key:            "DB_PASS",
			EncryptedValue: "db-password",
			SecretType:     store.SecretTypeEnvironment,
			Target:         "DATABASE_PASSWORD",
			Scope:          store.ScopeProject,
			ScopeID:        "project-1",
		},
	} {
		if err := memStore.CreateSecret(ctx, s); err != nil {
			t.Fatalf("failed to seed secret %s: %v", s.Key, err)
		}
	}

	// Create dispatcher with local backend
	backend := secret.NewLocalBackend(memStore, "test-hub-id")
	mockClient := &mockRuntimeBrokerClient{}
	dispatcher := NewHTTPAgentDispatcherWithClient(memStore, mockClient, false, slog.Default())
	dispatcher.SetSecretBackend(backend)

	agent := &store.Agent{
		ID:      "agent-1",
		Name:    "test-agent",
		OwnerID: "user-1",
		ProjectID: "project-1",
	}

	resolved, err := dispatcher.resolveSecrets(ctx, agent)
	if err != nil {
		t.Fatalf("resolveSecrets with backend failed: %v", err)
	}

	byName := make(map[string]ResolvedSecret)
	for _, rs := range resolved {
		byName[rs.Name] = rs
	}

	// API_KEY should be overridden by project scope
	apiKey, ok := byName["API_KEY"]
	if !ok {
		t.Fatal("expected API_KEY in resolved secrets")
	}
	if apiKey.Value != "project-api-key" {
		t.Errorf("expected API_KEY value %q, got %q", "project-api-key", apiKey.Value)
	}
	if apiKey.Source != store.ScopeProject {
		t.Errorf("expected API_KEY source %q, got %q", store.ScopeProject, apiKey.Source)
	}

	// DB_PASS target should be preserved
	dbPass, ok := byName["DB_PASS"]
	if !ok {
		t.Fatal("expected DB_PASS in resolved secrets")
	}
	if dbPass.Target != "DATABASE_PASSWORD" {
		t.Errorf("expected DB_PASS target %q, got %q", "DATABASE_PASSWORD", dbPass.Target)
	}

	if len(resolved) != 2 {
		t.Errorf("expected 2 resolved secrets, got %d", len(resolved))
	}
}

func TestResolveSecrets_NoOwner(t *testing.T) {
	memStore := createTestStore(t)
	ctx := context.Background()

	backend := secret.NewLocalBackend(memStore, "test-hub-id")
	mockClient := &mockRuntimeBrokerClient{}
	dispatcher := NewHTTPAgentDispatcherWithClient(memStore, mockClient, false, slog.Default())
	dispatcher.SetSecretBackend(backend)

	agent := &store.Agent{
		ID:   "agent-1",
		Name: "test-agent",
	}

	resolved, err := dispatcher.resolveSecrets(ctx, agent)
	if err != nil {
		t.Fatalf("resolveSecrets failed: %v", err)
	}

	if len(resolved) != 0 {
		t.Errorf("expected 0 resolved secrets for agent with no owner, got %d", len(resolved))
	}
}

func TestResolveSecrets_HubScope(t *testing.T) {
	memStore := createTestStore(t)
	ctx := context.Background()

	// Create hub-scoped secrets
	hubSecret := &store.Secret{
		ID:             "sh1",
		Key:            "ORG_API_KEY",
		EncryptedValue: "hub-org-api-key",
		SecretType:     store.SecretTypeEnvironment,
		Target:         "ORG_API_KEY",
		Scope:          store.ScopeHub,
		ScopeID:        "test-hub-id",
	}
	// Create a hub secret that will be overridden by user scope
	hubOverridden := &store.Secret{
		ID:             "sh2",
		Key:            "API_KEY",
		EncryptedValue: "hub-default-api-key",
		SecretType:     store.SecretTypeEnvironment,
		Target:         "API_KEY",
		Scope:          store.ScopeHub,
		ScopeID:        "test-hub-id",
	}
	// Create user secret that overrides hub
	userSecret := &store.Secret{
		ID:             "su1",
		Key:            "API_KEY",
		EncryptedValue: "user-personal-api-key",
		SecretType:     store.SecretTypeEnvironment,
		Target:         "API_KEY",
		Scope:          store.ScopeUser,
		ScopeID:        "user-1",
	}
	// Create a hub secret overridden by project scope
	hubProjectOverridden := &store.Secret{
		ID:             "sh3",
		Key:            "DB_PASS",
		EncryptedValue: "hub-default-db-pass",
		SecretType:     store.SecretTypeEnvironment,
		Target:         "DB_PASS",
		Scope:          store.ScopeHub,
		ScopeID:        "test-hub-id",
	}
	projectSecret := &store.Secret{
		ID:             "sg1",
		Key:            "DB_PASS",
		EncryptedValue: "project-db-pass",
		SecretType:     store.SecretTypeEnvironment,
		Target:         "DB_PASS",
		Scope:          store.ScopeProject,
		ScopeID:        "project-1",
	}

	for _, s := range []*store.Secret{hubSecret, hubOverridden, userSecret, hubProjectOverridden, projectSecret} {
		if err := memStore.CreateSecret(ctx, s); err != nil {
			t.Fatalf("failed to create test secret %s (scope=%s): %v", s.Key, s.Scope, err)
		}
	}

	backend := secret.NewLocalBackend(memStore, "test-hub-id")
	mockClient := &mockRuntimeBrokerClient{}
	dispatcher := NewHTTPAgentDispatcherWithClient(memStore, mockClient, false, slog.Default())
	dispatcher.SetSecretBackend(backend)

	agent := &store.Agent{
		ID:      "agent-hub-1",
		Name:    "hub-test-agent",
		OwnerID: "user-1",
		ProjectID: "project-1",
	}

	resolved, err := dispatcher.resolveSecrets(ctx, agent)
	if err != nil {
		t.Fatalf("resolveSecrets failed: %v", err)
	}

	byName := make(map[string]ResolvedSecret)
	for _, rs := range resolved {
		byName[rs.Name] = rs
	}

	// ORG_API_KEY should come from hub scope (no override)
	orgKey, ok := byName["ORG_API_KEY"]
	if !ok {
		t.Fatal("expected ORG_API_KEY in resolved secrets")
	}
	if orgKey.Value != "hub-org-api-key" {
		t.Errorf("expected ORG_API_KEY value %q, got %q", "hub-org-api-key", orgKey.Value)
	}
	if orgKey.Source != store.ScopeHub {
		t.Errorf("expected ORG_API_KEY source %q, got %q", store.ScopeHub, orgKey.Source)
	}

	// API_KEY should be overridden by user scope
	apiKey, ok := byName["API_KEY"]
	if !ok {
		t.Fatal("expected API_KEY in resolved secrets")
	}
	if apiKey.Value != "user-personal-api-key" {
		t.Errorf("expected API_KEY value %q, got %q", "user-personal-api-key", apiKey.Value)
	}
	if apiKey.Source != store.ScopeUser {
		t.Errorf("expected API_KEY source %q, got %q", store.ScopeUser, apiKey.Source)
	}

	// DB_PASS should be overridden by project scope
	dbPass, ok := byName["DB_PASS"]
	if !ok {
		t.Fatal("expected DB_PASS in resolved secrets")
	}
	if dbPass.Value != "project-db-pass" {
		t.Errorf("expected DB_PASS value %q, got %q", "project-db-pass", dbPass.Value)
	}
	if dbPass.Source != store.ScopeProject {
		t.Errorf("expected DB_PASS source %q, got %q", store.ScopeProject, dbPass.Source)
	}

	// Total: ORG_API_KEY, API_KEY, DB_PASS = 3
	if len(resolved) != 3 {
		t.Errorf("expected 3 resolved secrets, got %d", len(resolved))
	}
}

func TestResolveSecrets_NoBackend(t *testing.T) {
	memStore := createTestStore(t)
	ctx := context.Background()

	// Dispatcher without a secret backend returns nil
	mockClient := &mockRuntimeBrokerClient{}
	dispatcher := NewHTTPAgentDispatcherWithClient(memStore, mockClient, false, slog.Default())

	agent := &store.Agent{
		ID:      "agent-1",
		Name:    "test-agent",
		OwnerID: "user-1",
		ProjectID: "project-1",
	}

	resolved, err := dispatcher.resolveSecrets(ctx, agent)
	if err != nil {
		t.Fatalf("resolveSecrets failed: %v", err)
	}
	if resolved != nil {
		t.Errorf("expected nil resolved secrets when no backend, got %d", len(resolved))
	}
}
