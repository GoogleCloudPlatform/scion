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

package agent

import (
	"context"
	"fmt"
	"io"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
)

// mockSkillService implements hubclient.SkillService for testing HubSkillResolver.
type mockSkillService struct {
	resolveResp *hubclient.ResolveSkillsResponse
	resolveErr  error
	resolveReq  *hubclient.ResolveSkillsRequest // captured for assertions
}

func (m *mockSkillService) Resolve(ctx context.Context, req *hubclient.ResolveSkillsRequest) (*hubclient.ResolveSkillsResponse, error) {
	m.resolveReq = req
	if m.resolveErr != nil {
		return nil, m.resolveErr
	}
	return m.resolveResp, nil
}

// Unused SkillService methods — satisfy the interface.
func (m *mockSkillService) List(context.Context, *hubclient.ListSkillsOptions) (*hubclient.ListSkillsResponse, error) {
	return nil, nil
}
func (m *mockSkillService) Get(context.Context, string) (*hubclient.Skill, error) {
	return nil, nil
}
func (m *mockSkillService) Create(context.Context, *hubclient.CreateSkillRequest) (*hubclient.CreateSkillResponse, error) {
	return nil, nil
}
func (m *mockSkillService) Update(context.Context, string, *hubclient.UpdateSkillRequest) (*hubclient.Skill, error) {
	return nil, nil
}
func (m *mockSkillService) Delete(context.Context, string) error { return nil }
func (m *mockSkillService) PublishVersion(context.Context, string, *hubclient.PublishVersionRequest) (*hubclient.PublishVersionResponse, error) {
	return nil, nil
}
func (m *mockSkillService) ListVersions(context.Context, string) (*hubclient.ListSkillVersionsResponse, error) {
	return nil, nil
}
func (m *mockSkillService) FinalizeVersion(context.Context, string, *hubclient.FinalizeSkillVersionRequest) (*hubclient.SkillVersion, error) {
	return nil, nil
}
func (m *mockSkillService) RequestUploadURLs(context.Context, string, string, []hubclient.FileUploadRequest) (*hubclient.UploadResponse, error) {
	return nil, nil
}
func (m *mockSkillService) UploadFile(context.Context, string, string, map[string]string, io.Reader) error {
	return nil
}
func (m *mockSkillService) DeprecateVersion(context.Context, string, string, *hubclient.DeprecateVersionRequest) (*hubclient.SkillVersion, error) {
	return nil, nil
}
func (m *mockSkillService) DownloadFile(context.Context, string) ([]byte, error) { return nil, nil }

func TestHubSkillResolver_Resolve(t *testing.T) {
	mock := &mockSkillService{
		resolveResp: &hubclient.ResolveSkillsResponse{
			Resolved: []hubclient.ResolvedSkill{
				{
					URI:             "skill://scion/core/scion@^1.0",
					Name:            "scion",
					ResolvedVersion: "1.2.3",
					ContentHash:     "sha256:abc123",
					Files: []hubclient.DownloadURLInfo{
						{Path: "CLAUDE.md", URL: "https://storage.example.com/scion/CLAUDE.md", Hash: "sha256:file1", Size: 1024},
						{Path: "hooks/pre-commit.sh", URL: "https://storage.example.com/scion/hooks/pre-commit.sh", Hash: "sha256:file2", Size: 512},
					},
				},
			},
		},
	}

	resolver := NewHubSkillResolver(mock)
	refs := []api.SkillReference{
		{URI: "skill://scion/core/scion@^1.0", As: "my-scion"},
	}
	opts := ResolveOpts{ProjectID: "proj-123", UserID: "user-456"}

	result, err := resolver.Resolve(context.Background(), refs, opts)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}

	// Verify request was built correctly
	if mock.resolveReq.ProjectID != "proj-123" {
		t.Errorf("expected ProjectID=proj-123, got %s", mock.resolveReq.ProjectID)
	}
	if mock.resolveReq.UserID != "user-456" {
		t.Errorf("expected UserID=user-456, got %s", mock.resolveReq.UserID)
	}
	if len(mock.resolveReq.Skills) != 1 || mock.resolveReq.Skills[0].URI != "skill://scion/core/scion@^1.0" {
		t.Errorf("unexpected skills in request: %+v", mock.resolveReq.Skills)
	}

	// Verify resolved skill mapping
	if len(result.Resolved) != 1 {
		t.Fatalf("expected 1 resolved skill, got %d", len(result.Resolved))
	}
	rs := result.Resolved[0]
	if rs.Name != "scion" {
		t.Errorf("Name = %q, want %q", rs.Name, "scion")
	}
	if rs.URI != "skill://scion/core/scion@^1.0" {
		t.Errorf("URI = %q, want %q", rs.URI, "skill://scion/core/scion@^1.0")
	}
	if rs.Version != "1.2.3" {
		t.Errorf("Version = %q, want %q", rs.Version, "1.2.3")
	}
	if rs.Hash != "sha256:abc123" {
		t.Errorf("Hash = %q, want %q", rs.Hash, "sha256:abc123")
	}
	if rs.As != "my-scion" {
		t.Errorf("As = %q, want %q — As must come from the original ref, not Hub response", rs.As, "my-scion")
	}

	// Verify file mapping
	if len(rs.Files) != 2 {
		t.Fatalf("expected 2 files, got %d", len(rs.Files))
	}
	if rs.Files[0].Path != "CLAUDE.md" || rs.Files[0].Hash != "sha256:file1" || rs.Files[0].Size != 1024 {
		t.Errorf("unexpected first file: %+v", rs.Files[0])
	}
	if rs.Files[1].Path != "hooks/pre-commit.sh" || rs.Files[1].URL == "" {
		t.Errorf("unexpected second file: %+v", rs.Files[1])
	}

	// No errors expected
	if len(result.Errors) != 0 {
		t.Errorf("expected 0 errors, got %d", len(result.Errors))
	}
}

func TestHubSkillResolver_ResolveErrors(t *testing.T) {
	mock := &mockSkillService{
		resolveResp: &hubclient.ResolveSkillsResponse{
			Resolved: []hubclient.ResolvedSkill{
				{
					URI:             "skill://scion/core/scion@^1.0",
					Name:            "scion",
					ResolvedVersion: "1.0.0",
					ContentHash:     "sha256:ok",
				},
			},
			Errors: []hubclient.ResolveSkillError{
				{
					URI:     "skill://scion/core/missing@^2.0",
					Code:    "not_found",
					Message: "skill not found",
				},
			},
		},
	}

	resolver := NewHubSkillResolver(mock)
	refs := []api.SkillReference{
		{URI: "skill://scion/core/scion@^1.0"},
		{URI: "skill://scion/core/missing@^2.0", Optional: true},
	}

	result, err := resolver.Resolve(context.Background(), refs, ResolveOpts{})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}

	if len(result.Resolved) != 1 {
		t.Fatalf("expected 1 resolved, got %d", len(result.Resolved))
	}
	if len(result.Errors) != 1 {
		t.Fatalf("expected 1 error, got %d", len(result.Errors))
	}

	re := result.Errors[0]
	if re.URI != "skill://scion/core/missing@^2.0" {
		t.Errorf("error URI = %q, want %q", re.URI, "skill://scion/core/missing@^2.0")
	}
	if re.Code != "not_found" {
		t.Errorf("error Code = %q, want %q", re.Code, "not_found")
	}
	if re.Message != "skill not found" {
		t.Errorf("error Message = %q, want %q", re.Message, "skill not found")
	}
}

func TestHubSkillResolver_TransportError(t *testing.T) {
	mock := &mockSkillService{
		resolveErr: fmt.Errorf("connection refused"),
	}

	resolver := NewHubSkillResolver(mock)
	refs := []api.SkillReference{
		{URI: "skill://scion/core/scion@^1.0"},
	}

	_, err := resolver.Resolve(context.Background(), refs, ResolveOpts{})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if got := err.Error(); got != "hub skill resolution failed: connection refused" {
		t.Errorf("error = %q, want wrapping of transport error", got)
	}
}

func TestHubSkillResolver_MultipleSkills(t *testing.T) {
	mock := &mockSkillService{
		resolveResp: &hubclient.ResolveSkillsResponse{
			Resolved: []hubclient.ResolvedSkill{
				{URI: "skill://scion/core/scion@^1.0", Name: "scion", ResolvedVersion: "1.0.0", ContentHash: "sha256:a"},
				{URI: "skill://scion/core/team-creation@^1.0", Name: "team-creation", ResolvedVersion: "1.1.0", ContentHash: "sha256:b"},
			},
		},
	}

	resolver := NewHubSkillResolver(mock)
	refs := []api.SkillReference{
		{URI: "skill://scion/core/scion@^1.0"},
		{URI: "skill://scion/core/team-creation@^1.0", As: "teams"},
	}

	result, err := resolver.Resolve(context.Background(), refs, ResolveOpts{})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}

	if len(result.Resolved) != 2 {
		t.Fatalf("expected 2 resolved skills, got %d", len(result.Resolved))
	}

	// First skill: no As
	if result.Resolved[0].As != "" {
		t.Errorf("first skill As = %q, want empty", result.Resolved[0].As)
	}

	// Second skill: As set
	if result.Resolved[1].As != "teams" {
		t.Errorf("second skill As = %q, want %q", result.Resolved[1].As, "teams")
	}
}

func TestHubSkillResolver_EmptyRefs(t *testing.T) {
	mock := &mockSkillService{
		resolveResp: &hubclient.ResolveSkillsResponse{},
	}

	resolver := NewHubSkillResolver(mock)
	result, err := resolver.Resolve(context.Background(), nil, ResolveOpts{})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if len(result.Resolved) != 0 {
		t.Errorf("expected 0 resolved, got %d", len(result.Resolved))
	}
	if len(result.Errors) != 0 {
		t.Errorf("expected 0 errors, got %d", len(result.Errors))
	}
}
