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
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/require"
)

const (
	mappedGSA   = "mapped@p.iam.gserviceaccount.com"
	unmappedGSA = "unmapped@p.iam.gserviceaccount.com"
)

// addProviderBroker creates a broker with profiles and links it to projectID.
func addProviderBroker(t *testing.T, s store.Store, projectID, name string, profiles ...store.BrokerProfile) *store.RuntimeBroker {
	t.Helper()
	ctx := context.Background()
	b := &store.RuntimeBroker{
		ID:       tid("map-broker-" + name + t.Name()),
		Name:     name,
		Slug:     "map-broker-" + name + "-" + tidSlugSafe(t.Name()),
		Endpoint: "http://localhost:9800",
		Status:   store.BrokerStatusOnline,
		Profiles: profiles,
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, b))
	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID: projectID, BrokerID: b.ID, BrokerName: b.Name, Status: b.Status,
	}))
	return b
}

func k8sProfile(name string, reported bool, gsas ...string) store.BrokerProfile {
	p := store.BrokerProfile{Name: name, Type: "kubernetes", Available: true, MappingsReported: reported}
	for _, g := range gsas {
		p.ServiceAccountMappings = append(p.ServiceAccountMappings, store.BrokerProfileSAMapping{GSA: g})
	}
	return p
}

func mappingTestSA(t *testing.T, s store.Store, projectID, email string) *store.GCPServiceAccount {
	t.Helper()
	sa := &store.GCPServiceAccount{
		ID: tid("map-sa-" + email + t.Name()), Scope: store.ScopeProject, ScopeID: projectID,
		Email: email, ProjectID: "p", Verified: true, CreatedAt: time.Now(),
	}
	require.NoError(t, s.CreateGCPServiceAccount(context.Background(), sa))
	return sa
}

func newMappingProject(t *testing.T) (*Server, store.Store, string) {
	t.Helper()
	srv, s := testServer(t)
	projectID := createTestProjectForSA(t, srv, s)
	return srv, s, projectID
}
