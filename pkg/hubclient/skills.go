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

package hubclient

import (
	"context"
	"io"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
)

// SkillService handles skill operations.
type SkillService interface {
	// List returns skills matching the filter criteria.
	List(ctx context.Context, opts *ListSkillsOptions) (*ListSkillsResponse, error)

	// Get returns a single skill by ID.
	Get(ctx context.Context, skillID string) (*Skill, error)

	// Create creates a new skill.
	Create(ctx context.Context, req *CreateSkillRequest) (*CreateSkillResponse, error)

	// Update updates specific skill fields.
	Update(ctx context.Context, skillID string, req *UpdateSkillRequest) (*Skill, error)

	// Delete removes a skill (soft delete).
	Delete(ctx context.Context, skillID string) error

	// PublishVersion creates a draft version and returns upload URLs.
	PublishVersion(ctx context.Context, skillID string, req *PublishVersionRequest) (*PublishVersionResponse, error)

	// ListVersions returns versions for a skill.
	ListVersions(ctx context.Context, skillID string) (*ListSkillVersionsResponse, error)

	// FinalizeVersion verifies files and transitions a version from draft to published.
	FinalizeVersion(ctx context.Context, skillID string, req *FinalizeSkillVersionRequest) (*SkillVersion, error)

	// RequestUploadURLs requests signed upload URLs for a skill version's files.
	RequestUploadURLs(ctx context.Context, skillID string, version string, files []FileUploadRequest) (*UploadResponse, error)

	// UploadFile uploads a file to the given signed URL.
	UploadFile(ctx context.Context, url string, method string, headers map[string]string, content io.Reader) error

	// DownloadFile downloads a file from the given signed URL.
	DownloadFile(ctx context.Context, url string) ([]byte, error)

	// DeprecateVersion marks a published version as deprecated.
	DeprecateVersion(ctx context.Context, skillID, versionID string, req *DeprecateVersionRequest) (*SkillVersion, error)

	// Resolve performs batch skill resolution.
	Resolve(ctx context.Context, req *ResolveSkillsRequest) (*ResolveSkillsResponse, error)
}

// skillService is the implementation of SkillService.
type skillService struct {
	c              *client
	transferClient *transfer.Client
	transferOnce   sync.Once
}

// Skill represents a skill from the Hub API.
type Skill struct {
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	Slug          string    `json:"slug"`
	Description   string    `json:"description,omitempty"`
	Tags          []string  `json:"tags,omitempty"`
	Scope         string    `json:"scope"`
	ScopeID       string    `json:"scopeId,omitempty"`
	StorageURI    string    `json:"storageUri,omitempty"`
	StorageBucket string    `json:"storageBucket,omitempty"`
	StoragePath   string    `json:"storagePath,omitempty"`
	Status        string    `json:"status"`
	OwnerID       string    `json:"ownerId,omitempty"`
	CreatedBy     string    `json:"createdBy,omitempty"`
	UpdatedBy     string    `json:"updatedBy,omitempty"`
	Visibility    string    `json:"visibility"`
	Created       time.Time `json:"created"`
	Updated       time.Time `json:"updated"`
}

// SkillVersion represents a version of a skill from the Hub API.
type SkillVersion struct {
	ID                 string         `json:"id"`
	SkillID            string         `json:"skillId"`
	Version            string         `json:"version"`
	Status             string         `json:"status"`
	ContentHash        string         `json:"contentHash,omitempty"`
	Files              []TemplateFile `json:"files,omitempty"`
	PublisherID        string         `json:"publisherId,omitempty"`
	DeprecationMessage string         `json:"deprecationMessage,omitempty"`
	ReplacementURI     string         `json:"replacementUri,omitempty"`
	DownloadCount      int64          `json:"downloadCount"`
	Created            time.Time      `json:"created"`
}

// ListSkillsOptions configures skill list filtering.
type ListSkillsOptions struct {
	Name    string
	Scope   string
	ScopeID string
	OwnerID string
	Search  string
	Status  string
	Tags    []string
	Page    apiclient.PageOptions
}

// ListSkillsResponse is the response from listing skills.
type ListSkillsResponse struct {
	Skills []Skill
	Page   apiclient.PageResult
}

// ListSkillVersionsResponse is the response from listing skill versions.
type ListSkillVersionsResponse struct {
	Items      []SkillVersion `json:"items"`
	TotalCount int            `json:"totalCount"`
}

// CreateSkillRequest is the request for creating a skill.
type CreateSkillRequest struct {
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	Scope       string   `json:"scope"`
	ScopeID     string   `json:"scopeId,omitempty"`
	Visibility  string   `json:"visibility,omitempty"`
	Tags        []string `json:"tags,omitempty"`
}

// CreateSkillResponse is the response from creating a skill.
type CreateSkillResponse struct {
	Skill *Skill `json:"skill"`
}

// UpdateSkillRequest is the request for updating a skill.
type UpdateSkillRequest struct {
	Name        string   `json:"name,omitempty"`
	Description string   `json:"description,omitempty"`
	Visibility  string   `json:"visibility,omitempty"`
	Tags        []string `json:"tags,omitempty"`
}

// PublishVersionRequest is the request for creating a skill version.
type PublishVersionRequest struct {
	Version string            `json:"version"`
	Files   []FileUploadRequest `json:"files,omitempty"`
}

// PublishVersionResponse is the response from creating a skill version.
type PublishVersionResponse struct {
	Version    *SkillVersion   `json:"version"`
	UploadURLs []UploadURLInfo `json:"uploadUrls,omitempty"`
}

// FinalizeSkillVersionRequest is the request for finalizing a skill version.
type FinalizeSkillVersionRequest struct {
	Version  string        `json:"version"`
	Manifest *SkillManifest `json:"manifest"`
}

// SkillManifest is the manifest of uploaded skill files.
type SkillManifest struct {
	Files []TemplateFile `json:"files"`
}

// DeprecateVersionRequest is the request for deprecating a skill version.
type DeprecateVersionRequest struct {
	Message        string `json:"message"`
	ReplacementURI string `json:"replacementUri,omitempty"`
}

// ResolveSkillsRequest is the request for batch skill resolution.
type ResolveSkillsRequest struct {
	Skills    []ResolveSkillRef `json:"skills"`
	ProjectID string            `json:"projectId,omitempty"`
	UserID    string            `json:"userId,omitempty"`
}

// ResolveSkillRef is a reference to a skill to resolve.
type ResolveSkillRef struct {
	URI string `json:"uri"`
}

// ResolveSkillsResponse is the response for batch skill resolution.
type ResolveSkillsResponse struct {
	Resolved []ResolvedSkill    `json:"resolved"`
	Errors   []ResolveSkillError `json:"errors,omitempty"`
}

// ResolvedSkill is a single resolved skill in the batch response.
type ResolvedSkill struct {
	URI                string            `json:"uri"`
	Name               string            `json:"name"`
	ResolvedVersion    string            `json:"resolvedVersion"`
	ContentHash        string            `json:"contentHash"`
	Files              []DownloadURLInfo `json:"files"`
	Deprecated         bool              `json:"deprecated,omitempty"`
	DeprecationMessage string            `json:"deprecationMessage,omitempty"`
	ReplacementURI     string            `json:"replacementUri,omitempty"`
}

// ResolveSkillError describes a resolution failure for a single skill.
type ResolveSkillError struct {
	URI     string `json:"uri"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// List returns skills matching the filter criteria.
func (s *skillService) List(ctx context.Context, opts *ListSkillsOptions) (*ListSkillsResponse, error) {
	query := url.Values{}
	if opts != nil {
		if opts.Name != "" {
			query.Set("name", opts.Name)
		}
		if opts.Scope != "" {
			query.Set("scope", opts.Scope)
		}
		if opts.ScopeID != "" {
			query.Set("scopeId", opts.ScopeID)
		}
		if opts.OwnerID != "" {
			query.Set("ownerId", opts.OwnerID)
		}
		if opts.Search != "" {
			query.Set("search", opts.Search)
		}
		if opts.Status != "" {
			query.Set("status", opts.Status)
		}
		if len(opts.Tags) > 0 {
			query.Set("tags", strings.Join(opts.Tags, ","))
		}
		opts.Page.ToQuery(query)
	}

	resp, err := s.c.getWithQuery(ctx, "/api/v1/skills", query, nil)
	if err != nil {
		return nil, err
	}

	type listResponse struct {
		Skills     []Skill `json:"skills"`
		NextCursor string  `json:"nextCursor,omitempty"`
		TotalCount int     `json:"totalCount,omitempty"`
	}

	result, err := apiclient.DecodeResponse[listResponse](resp)
	if err != nil {
		return nil, err
	}

	return &ListSkillsResponse{
		Skills: result.Skills,
		Page: apiclient.PageResult{
			NextCursor: result.NextCursor,
			TotalCount: result.TotalCount,
		},
	}, nil
}

// Get returns a single skill by ID.
func (s *skillService) Get(ctx context.Context, skillID string) (*Skill, error) {
	resp, err := s.c.get(ctx, "/api/v1/skills/"+skillID, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeResponse[Skill](resp)
}

// Create creates a new skill.
func (s *skillService) Create(ctx context.Context, req *CreateSkillRequest) (*CreateSkillResponse, error) {
	resp, err := s.c.post(ctx, "/api/v1/skills", req, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeResponse[CreateSkillResponse](resp)
}

// Update updates specific skill fields.
func (s *skillService) Update(ctx context.Context, skillID string, req *UpdateSkillRequest) (*Skill, error) {
	resp, err := s.c.patch(ctx, "/api/v1/skills/"+skillID, req, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeResponse[Skill](resp)
}

// Delete removes a skill (soft delete).
func (s *skillService) Delete(ctx context.Context, skillID string) error {
	resp, err := s.c.delete(ctx, "/api/v1/skills/"+skillID, nil)
	if err != nil {
		return err
	}
	return apiclient.CheckResponse(resp)
}

// PublishVersion creates a draft version and returns upload URLs.
func (s *skillService) PublishVersion(ctx context.Context, skillID string, req *PublishVersionRequest) (*PublishVersionResponse, error) {
	resp, err := s.c.post(ctx, "/api/v1/skills/"+skillID+"/versions", req, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeResponse[PublishVersionResponse](resp)
}

// ListVersions returns versions for a skill.
func (s *skillService) ListVersions(ctx context.Context, skillID string) (*ListSkillVersionsResponse, error) {
	resp, err := s.c.get(ctx, "/api/v1/skills/"+skillID+"/versions", nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeResponse[ListSkillVersionsResponse](resp)
}

// FinalizeVersion verifies files and transitions a version from draft to published.
func (s *skillService) FinalizeVersion(ctx context.Context, skillID string, req *FinalizeSkillVersionRequest) (*SkillVersion, error) {
	resp, err := s.c.post(ctx, "/api/v1/skills/"+skillID+"/finalize", req, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeResponse[SkillVersion](resp)
}

// RequestUploadURLs requests signed upload URLs for a skill version's files.
func (s *skillService) RequestUploadURLs(ctx context.Context, skillID string, version string, files []FileUploadRequest) (*UploadResponse, error) {
	req := struct {
		Version string            `json:"version"`
		Files   []FileUploadRequest `json:"files"`
	}{
		Version: version,
		Files:   files,
	}
	resp, err := s.c.post(ctx, "/api/v1/skills/"+skillID+"/upload", req, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeResponse[UploadResponse](resp)
}

// UploadFile uploads a file to the given signed URL.
func (s *skillService) UploadFile(ctx context.Context, signedURL string, method string, headers map[string]string, content io.Reader) error {
	tc := s.getTransferClient()
	return tc.UploadFileWithMethod(ctx, signedURL, method, headers, content)
}

// DownloadFile downloads a file from the given signed URL.
func (s *skillService) DownloadFile(ctx context.Context, signedURL string) ([]byte, error) {
	tc := s.getTransferClient()
	return tc.DownloadFile(ctx, signedURL)
}

// DeprecateVersion marks a published version as deprecated.
func (s *skillService) DeprecateVersion(ctx context.Context, skillID, versionID string, req *DeprecateVersionRequest) (*SkillVersion, error) {
	resp, err := s.c.post(ctx, "/api/v1/skills/"+skillID+"/versions/"+versionID+"/deprecate", req, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeResponse[SkillVersion](resp)
}

// Resolve performs batch skill resolution.
func (s *skillService) Resolve(ctx context.Context, req *ResolveSkillsRequest) (*ResolveSkillsResponse, error) {
	resp, err := s.c.post(ctx, "/api/v1/skills/resolve", req, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeResponse[ResolveSkillsResponse](resp)
}

func (s *skillService) getTransferClient() *transfer.Client {
	s.transferOnce.Do(func() {
		s.transferClient = transfer.NewClient(s.c.transport.AuthenticatedHTTPClient())
	})
	return s.transferClient
}
