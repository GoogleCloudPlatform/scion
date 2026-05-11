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
	"bytes"
	"context"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
)

// HarnessConfigService handles harness config operations.
type HarnessConfigService interface {
	// List returns harness configs matching the filter criteria.
	List(ctx context.Context, opts *ListHarnessConfigsOptions) (*ListHarnessConfigsResponse, error)

	// Get returns a single harness config by ID.
	Get(ctx context.Context, id string) (*HarnessConfig, error)

	// Create creates a new harness config.
	Create(ctx context.Context, req *CreateHarnessConfigRequest) (*CreateHarnessConfigResponse, error)

	// Update updates a harness config.
	Update(ctx context.Context, id string, req *UpdateHarnessConfigRequest) (*HarnessConfig, error)

	// Delete removes a harness config.
	Delete(ctx context.Context, id string) error

	// RequestUploadURLs requests signed URLs for uploading harness config files.
	RequestUploadURLs(ctx context.Context, id string, files []FileUploadRequest) (*UploadResponse, error)

	// Finalize finalizes a harness config after file upload.
	Finalize(ctx context.Context, id string, manifest *HarnessConfigManifest) (*HarnessConfig, error)

	// RequestDownloadURLs requests signed URLs for downloading harness config files.
	RequestDownloadURLs(ctx context.Context, id string) (*DownloadResponse, error)

	// UploadFile uploads a file to the given signed URL.
	UploadFile(ctx context.Context, url string, method string, headers map[string]string, content io.Reader) error

	// DownloadFile downloads a file from the given signed URL.
	DownloadFile(ctx context.Context, url string) ([]byte, error)

	// UploadFilesMultipart uploads harness config files through the Hub API.
	UploadFilesMultipart(ctx context.Context, id string, files []FileInfo) error

	// ReadFile reads a harness config file through the Hub API.
	ReadFile(ctx context.Context, id, filePath string) ([]byte, error)
}

// harnessConfigService is the implementation of HarnessConfigService.
type harnessConfigService struct {
	c              *client
	transferClient *transfer.Client
}

// ListHarnessConfigsOptions configures harness config list filtering.
type ListHarnessConfigsOptions struct {
	Name    string
	Search  string
	Scope   string
	ScopeID string
	Harness string
	Status  string
	Page    apiclient.PageOptions
}

// ListHarnessConfigsResponse is the response from listing harness configs.
type ListHarnessConfigsResponse struct {
	HarnessConfigs []HarnessConfig
	Page           apiclient.PageResult
}

// CreateHarnessConfigRequest is the request for creating a harness config.
type CreateHarnessConfigRequest struct {
	Name        string              `json:"name"`
	Slug        string              `json:"slug,omitempty"`
	DisplayName string              `json:"displayName,omitempty"`
	Description string              `json:"description,omitempty"`
	Harness     string              `json:"harness"`
	Scope       string              `json:"scope"`
	ScopeID     string              `json:"scopeId,omitempty"`
	Config      *HarnessConfigData  `json:"config,omitempty"`
	Visibility  string              `json:"visibility,omitempty"`
	Files       []FileUploadRequest `json:"files,omitempty"`
}

// CreateHarnessConfigResponse is the response from creating a harness config.
type CreateHarnessConfigResponse struct {
	HarnessConfig *HarnessConfig  `json:"harnessConfig"`
	UploadURLs    []UploadURLInfo `json:"uploadUrls,omitempty"`
	ManifestURL   string          `json:"manifestUrl,omitempty"`
}

// UpdateHarnessConfigRequest is the request for updating a harness config.
type UpdateHarnessConfigRequest struct {
	Name        string `json:"name,omitempty"`
	DisplayName string `json:"displayName,omitempty"`
	Description string `json:"description,omitempty"`
	Visibility  string `json:"visibility,omitempty"`
}

// HarnessConfigManifest is the manifest of uploaded harness config files.
type HarnessConfigManifest struct {
	Version string         `json:"version"`
	Harness string         `json:"harness,omitempty"`
	Files   []TemplateFile `json:"files"`
}

// HarnessConfigFinalizeRequest is the request body for finalizing a harness config upload.
type HarnessConfigFinalizeRequest struct {
	Manifest *HarnessConfigManifest `json:"manifest"`
}

type harnessConfigFileContentResponse struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// List returns harness configs matching the filter criteria.
func (s *harnessConfigService) List(ctx context.Context, opts *ListHarnessConfigsOptions) (*ListHarnessConfigsResponse, error) {
	query := url.Values{}
	if opts != nil {
		if opts.Name != "" {
			query.Set("name", opts.Name)
		}
		if opts.Search != "" {
			query.Set("search", opts.Search)
		}
		if opts.Scope != "" {
			query.Set("scope", opts.Scope)
		}
		if opts.ScopeID != "" {
			query.Set("scopeId", opts.ScopeID)
		}
		if opts.Harness != "" {
			query.Set("harness", opts.Harness)
		}
		if opts.Status != "" {
			query.Set("status", opts.Status)
		}
		opts.Page.ToQuery(query)
	}

	resp, err := s.c.getWithQuery(ctx, "/api/v1/harness-configs", query, nil)
	if err != nil {
		return nil, err
	}

	type listResponse struct {
		HarnessConfigs []HarnessConfig `json:"harnessConfigs"`
		NextCursor     string          `json:"nextCursor,omitempty"`
		TotalCount     int             `json:"totalCount,omitempty"`
	}

	result, err := apiclient.DecodeResponse[listResponse](resp)
	if err != nil {
		return nil, err
	}

	return &ListHarnessConfigsResponse{
		HarnessConfigs: result.HarnessConfigs,
		Page: apiclient.PageResult{
			NextCursor: result.NextCursor,
			TotalCount: result.TotalCount,
		},
	}, nil
}

// Get returns a single harness config by ID.
func (s *harnessConfigService) Get(ctx context.Context, id string) (*HarnessConfig, error) {
	resp, err := s.c.get(ctx, "/api/v1/harness-configs/"+id, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeResponse[HarnessConfig](resp)
}

// Create creates a new harness config.
func (s *harnessConfigService) Create(ctx context.Context, req *CreateHarnessConfigRequest) (*CreateHarnessConfigResponse, error) {
	resp, err := s.c.post(ctx, "/api/v1/harness-configs", req, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeResponse[CreateHarnessConfigResponse](resp)
}

// Update updates a harness config.
func (s *harnessConfigService) Update(ctx context.Context, id string, req *UpdateHarnessConfigRequest) (*HarnessConfig, error) {
	resp, err := s.c.patch(ctx, "/api/v1/harness-configs/"+id, req, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeResponse[HarnessConfig](resp)
}

// Delete removes a harness config.
func (s *harnessConfigService) Delete(ctx context.Context, id string) error {
	resp, err := s.c.delete(ctx, "/api/v1/harness-configs/"+id, nil)
	if err != nil {
		return err
	}
	return apiclient.CheckResponse(resp)
}

// RequestUploadURLs requests signed URLs for uploading harness config files.
func (s *harnessConfigService) RequestUploadURLs(ctx context.Context, id string, files []FileUploadRequest) (*UploadResponse, error) {
	req := struct {
		Files []FileUploadRequest `json:"files"`
	}{
		Files: files,
	}
	resp, err := s.c.post(ctx, "/api/v1/harness-configs/"+id+"/upload", req, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeResponse[UploadResponse](resp)
}

// Finalize finalizes a harness config after file upload.
func (s *harnessConfigService) Finalize(ctx context.Context, id string, manifest *HarnessConfigManifest) (*HarnessConfig, error) {
	req := HarnessConfigFinalizeRequest{
		Manifest: manifest,
	}
	resp, err := s.c.post(ctx, "/api/v1/harness-configs/"+id+"/finalize", req, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeResponse[HarnessConfig](resp)
}

// RequestDownloadURLs requests signed URLs for downloading harness config files.
func (s *harnessConfigService) RequestDownloadURLs(ctx context.Context, id string) (*DownloadResponse, error) {
	resp, err := s.c.get(ctx, "/api/v1/harness-configs/"+id+"/download", nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeResponse[DownloadResponse](resp)
}

// UploadFile uploads a file to the given signed URL.
func (s *harnessConfigService) UploadFile(ctx context.Context, signedURL string, method string, headers map[string]string, content io.Reader) error {
	client := s.getTransferClient()
	return client.UploadFileWithMethod(ctx, signedURL, method, headers, content)
}

// DownloadFile downloads a file from the given signed URL.
func (s *harnessConfigService) DownloadFile(ctx context.Context, signedURL string) ([]byte, error) {
	client := s.getTransferClient()
	return client.DownloadFile(ctx, signedURL)
}

// UploadFilesMultipart uploads harness config files through the Hub API.
func (s *harnessConfigService) UploadFilesMultipart(ctx context.Context, id string, files []FileInfo) error {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)

	for _, file := range files {
		if err := appendMultipartFile(writer, file); err != nil {
			return err
		}
	}

	if err := writer.Close(); err != nil {
		return fmt.Errorf("finalize multipart body: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		s.c.transport.BaseURL+"/api/v1/harness-configs/"+id+"/files", &body)
	if err != nil {
		return fmt.Errorf("create multipart upload request: %w", err)
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())

	resp, err := s.c.transport.Do(ctx, req)
	if err != nil {
		return err
	}
	return apiclient.CheckResponse(resp)
}

// ReadFile reads a harness config file through the Hub API.
func (s *harnessConfigService) ReadFile(ctx context.Context, id, filePath string) ([]byte, error) {
	endpoint := "/api/v1/harness-configs/" + id + "/files/" + escapePathSegments(filePath)
	resp, err := s.c.get(ctx, endpoint, nil)
	if err != nil {
		return nil, err
	}

	result, err := apiclient.DecodeResponse[harnessConfigFileContentResponse](resp)
	if err != nil {
		return nil, err
	}
	return []byte(result.Content), nil
}

func (s *harnessConfigService) getTransferClient() *transfer.Client {
	if s.transferClient == nil {
		s.transferClient = transfer.NewClient(s.c.transport.AuthenticatedHTTPClient())
	}
	return s.transferClient
}

func appendMultipartFile(writer *multipart.Writer, file FileInfo) (err error) {
	src, err := os.Open(file.FullPath)
	if err != nil {
		return fmt.Errorf("open file for multipart upload: %w", err)
	}
	defer func() {
		if closeErr := src.Close(); err == nil && closeErr != nil {
			err = fmt.Errorf("close file for multipart upload: %w", closeErr)
		}
	}()

	part, err := writer.CreateFormFile(file.Path, path.Base(file.Path))
	if err != nil {
		return fmt.Errorf("create multipart form file: %w", err)
	}

	if _, err := io.Copy(part, src); err != nil {
		return fmt.Errorf("copy file into multipart body: %w", err)
	}

	return nil
}

func escapePathSegments(filePath string) string {
	parts := strings.Split(filePath, "/")
	for i, part := range parts {
		parts[i] = url.PathEscape(part)
	}
	return strings.Join(parts, "/")
}
