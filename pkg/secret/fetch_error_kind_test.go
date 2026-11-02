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

package secret

import (
	"context"
	"errors"
	"testing"

	smpb "cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// fetchErrorKindStore is a migrated in-memory store for the fetch error
// kind tests.
func fetchErrorKindStore(t *testing.T) store.Store {
	t.Helper()
	s, err := newTestStore(":memory:")
	if err != nil {
		t.Fatalf("failed to create test store: %v", err)
	}
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatalf("failed to migrate test store: %v", err)
	}
	return s
}

// TestLocalBackend_FetchValues_DecryptFailureIsResolveDecrypt: a stored
// value the backend cannot decrypt is reported by FetchValues as the typed
// error Resolve returns for it (errors.Is ErrResolveDecrypt), naming the
// key and scope, with an empty value.
func TestLocalBackend_FetchValues_DecryptFailureIsResolveDecrypt(t *testing.T) {
	ctx := context.Background()
	s := fetchErrorKindStore(t)
	foreign, err := NewLegacyValueCipher("FAKE-foreign-secret").Encrypt("fake-value")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CreateSecret(ctx, &store.Secret{
		ID: tid("fetch-kind-undecryptable"), Key: "UNDECRYPTABLE", EncryptedValue: foreign,
		SecretType: store.SecretTypeEnvironment, Target: "UNDECRYPTABLE",
		Scope: store.ScopeProject, ScopeID: "project-1", InjectionMode: store.InjectionModeAlways,
	}); err != nil {
		t.Fatal(err)
	}
	backend := NewLocalBackend(s, "test-hub-id", "FAKE-hub-secret")
	meta, err := backend.GetMeta(ctx, "UNDECRYPTABLE", store.ScopeProject, "project-1")
	if err != nil {
		t.Fatalf("GetMeta failed: %v", err)
	}

	results, err := backend.FetchValues(ctx, []SecretMeta{*meta})
	if err != nil {
		t.Fatalf("FetchValues failed: %v", err)
	}
	res := results[meta.ID]
	if !errors.Is(res.Err, ErrResolveDecrypt) {
		t.Fatalf("FetchValues item err = %v, want ErrResolveDecrypt", res.Err)
	}
	var rde *ResolveDecryptError
	if !errors.As(res.Err, &rde) || len(rde.Failures) != 1 ||
		rde.Failures[0].Key != "UNDECRYPTABLE" || rde.Failures[0].Scope != store.ScopeProject {
		t.Fatalf("FetchValues item err = %#v, want one failure for project/UNDECRYPTABLE", res.Err)
	}
	if res.Value != "" || res.Ref != "" {
		t.Fatalf("a failed item carries value %q ref %q, want both empty", res.Value, res.Ref)
	}
	if errors.Is(res.Err, ErrValueAccess) {
		t.Fatal("a local decrypt failure must not match ErrValueAccess")
	}

	// Resolve fails on the same row with the same error kind.
	if _, err := backend.Resolve(ctx, "", "project-1", "", nil); !errors.Is(err, ErrResolveDecrypt) {
		t.Fatalf("Resolve err = %v, want ErrResolveDecrypt", err)
	}
}

// unavailableSMClient fails every value access with Unavailable.
type unavailableSMClient struct {
	mockSMClient
}

func (m *unavailableSMClient) AccessSecretVersion(_ context.Context, _ *smpb.AccessSecretVersionRequest) (*smpb.AccessSecretVersionResponse, error) {
	return nil, status.Error(codes.Unavailable, "transient backend error")
}

// TestGCPBackend_FetchValues_AccessErrorIsValueAccess: a Secret Manager
// value access that fails other than not found is reported by FetchValues
// as an error matching ErrValueAccess (a permission failure still matches
// *PermissionError too), the failure GCPBackend.Resolve skips: Resolve
// returns no error and leaves the secret out.
func TestGCPBackend_FetchValues_AccessErrorIsValueAccess(t *testing.T) {
	for _, tc := range []struct {
		name       string
		client     SMClient
		permission bool
	}{
		{"permission_denied", newPermDeniedSMClient(), true},
		{"unavailable", &unavailableSMClient{mockSMClient: mockSMClient{secrets: map[string]*smpb.Secret{}, versions: map[string][]byte{}}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			s := fetchErrorKindStore(t)
			if err := s.CreateSecret(ctx, &store.Secret{
				ID: tid("fetch-kind-" + tc.name), Key: "SM_KEY", SecretRef: "gcpsm:projects/test-project/secrets/sm-key",
				SecretType: store.SecretTypeEnvironment, Target: "SM_KEY",
				Scope: store.ScopeProject, ScopeID: "project-1", InjectionMode: store.InjectionModeAlways,
			}); err != nil {
				t.Fatal(err)
			}
			backend := NewGCPBackendWithClient(s, tc.client, "test-project", "test-hub-id")
			meta, err := backend.GetMeta(ctx, "SM_KEY", store.ScopeProject, "project-1")
			if err != nil {
				t.Fatalf("GetMeta failed: %v", err)
			}

			results, err := backend.FetchValues(ctx, []SecretMeta{*meta})
			if err != nil {
				t.Fatalf("FetchValues failed: %v", err)
			}
			res := results[meta.ID]
			if !errors.Is(res.Err, ErrValueAccess) {
				t.Fatalf("FetchValues item err = %v, want ErrValueAccess", res.Err)
			}
			if errors.Is(res.Err, store.ErrNotFound) || errors.Is(res.Err, ErrResolveDecrypt) {
				t.Fatalf("FetchValues item err = %v matches the wrong kind", res.Err)
			}
			var permErr *PermissionError
			if got := errors.As(res.Err, &permErr); got != tc.permission {
				t.Fatalf("errors.As(*PermissionError) = %v, want %v (err %v)", got, tc.permission, res.Err)
			}
			if res.Value != "" || res.Ref != "" {
				t.Fatalf("a failed item carries value %q ref %q, want both empty", res.Value, res.Ref)
			}

			resolved, err := backend.Resolve(ctx, "", "project-1", "", nil)
			if err != nil {
				t.Fatalf("Resolve err = %v, want nil (an access error is skipped)", err)
			}
			for _, r := range resolved {
				if r.Name == "SM_KEY" {
					t.Fatalf("Resolve returned SM_KEY despite the access error")
				}
			}
		})
	}
}
