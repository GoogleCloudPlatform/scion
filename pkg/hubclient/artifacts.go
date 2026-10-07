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
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
)

// ArtifactService handles artifact operations (/api/v1/artifacts).
type ArtifactService interface {
	// Publish uploads one file as a new artifact and returns it with its
	// first version.
	Publish(ctx context.Context, req *PublishArtifactRequest) (*ArtifactResponse, error)

	// Get returns an artifact and its current version.
	Get(ctx context.Context, id string) (*ArtifactResponse, error)

	// List returns one page of the artifacts the caller owns, is granted,
	// or that are shared to a project it is a member of, newest first.
	// opts may be nil.
	List(ctx context.Context, opts *ListArtifactsOptions) (*ArtifactListResponse, error)

	// OpenFile opens a file of version seq of an artifact (0 = the current
	// version). The caller must close the returned reader.
	OpenFile(ctx context.Context, id string, seq int, filePath string) (io.ReadCloser, error)

	// CreateVersion starts a two-step publish. With id "" it posts to the
	// collection: a new artifact, or a new version of the caller's
	// artifact with req.Key in the scope. With an id it appends a version
	// to that artifact. The response lists the files to upload.
	CreateVersion(ctx context.Context, id string, req *CreateVersionRequest) (*PendingVersionResponse, error)

	// UploadFile uploads one file of pending version seq.
	UploadFile(ctx context.Context, id string, seq int, filePath string, content io.Reader, size int64, sha256 string) error

	// FinalizeVersion makes pending version seq ready and current.
	FinalizeVersion(ctx context.Context, id string, seq int) (*ArtifactResponse, error)

	// ListVersions returns the ready versions of an artifact, newest
	// first, without their files.
	ListVersions(ctx context.Context, id string) ([]ArtifactVersion, error)

	// GetVersion returns an artifact with version seq and its files.
	GetVersion(ctx context.Context, id string, seq int) (*ArtifactResponse, error)
}

// CreateVersionRequest mirrors the hub's version manifest.
type CreateVersionRequest struct {
	Title string         `json:"title,omitempty"`
	Key   string         `json:"key,omitempty"`
	Scope string         `json:"scope,omitempty"`
	Kind  string         `json:"kind,omitempty"`
	Entry string         `json:"entry"`
	Note  string         `json:"note,omitempty"`
	Files []ManifestFile `json:"files"`
}

// ManifestFile is one file of a version manifest.
type ManifestFile struct {
	Path      string `json:"path"`
	Size      int64  `json:"size"`
	SHA256    string `json:"sha256"`
	MediaType string `json:"mediaType,omitempty"`
}

// PendingVersionResponse mirrors the hub's answer to CreateVersion.
type PendingVersionResponse struct {
	Artifact Artifact         `json:"artifact"`
	Version  *ArtifactVersion `json:"version"`
	Upload   struct {
		Required []string `json:"required"`
	} `json:"upload"`
}

// PublishArtifactRequest describes a single-file publish.
type PublishArtifactRequest struct {
	// Name is the file name (no directories); it becomes the entry path.
	Name string
	// Title is the artifact title; the hub defaults it to Name.
	Title string
	// Scope is the home project id; the hub defaults it to the caller's
	// project (agents). Users must set it.
	Scope string
	// Content is the file's bytes.
	Content io.Reader
	// Size is the content length in bytes, or -1 when unknown.
	Size int64
	// SHA256 is the hex digest of Content. When set, the hub rejects a
	// body that does not match.
	SHA256 string
	// ContentType is the declared media type; the hub prefers the type
	// implied by Name's extension.
	ContentType string
}

// ListArtifactsOptions filters and pages List.
type ListArtifactsOptions struct {
	// Query keeps artifacts whose title or key contains it.
	Query string
	// ReviewPending keeps artifacts whose current version is a review.
	ReviewPending bool
	// OwnedOnly keeps artifacts the caller owns.
	OwnedOnly bool
	// Limit is the page size (0 = the hub's default).
	Limit int
	// Cursor is the NextCursor of the previous page.
	Cursor string
}

// ArtifactListResponse mirrors the hub's list response body.
type ArtifactListResponse struct {
	Artifacts []ArtifactListItem `json:"artifacts"`
	// NextCursor is empty on the last page.
	NextCursor string `json:"nextCursor,omitempty"`
}

// ArtifactListItem is one listed artifact.
type ArtifactListItem struct {
	Artifact
	ReviewPending bool `json:"reviewPending"`
}

// ArtifactResponse mirrors the hub's artifact response body.
type ArtifactResponse struct {
	Artifact Artifact         `json:"artifact"`
	Version  *ArtifactVersion `json:"version,omitempty"`
	Warnings []string         `json:"warnings,omitempty"`
}

// Artifact describes an artifact.
type Artifact struct {
	ID         string    `json:"id"`
	Ref        string    `json:"ref"`
	ScopeKind  string    `json:"scopeKind"`
	ScopeRef   string    `json:"scopeRef"`
	OwnerKind  string    `json:"ownerKind"`
	OwnerRef   string    `json:"ownerRef"`
	Key        string    `json:"key,omitempty"`
	Title      string    `json:"title"`
	CurrentSeq int       `json:"currentSeq"`
	CreatedAt  time.Time `json:"createdAt"`
	UpdatedAt  time.Time `json:"updatedAt"`
}

// ArtifactVersion describes one version of an artifact.
type ArtifactVersion struct {
	Seq           int            `json:"seq"`
	Ref           string         `json:"ref"`
	Kind          string         `json:"kind"`
	EntryPath     string         `json:"entryPath"`
	Note          string         `json:"note,omitempty"`
	TotalBytes    int64          `json:"totalBytes"`
	FileCount     int            `json:"fileCount"`
	CreatedByKind string         `json:"createdByKind,omitempty"`
	CreatedByRef  string         `json:"createdByRef,omitempty"`
	CreatedAt     time.Time      `json:"createdAt"`
	State         string         `json:"state"`
	Files         []ArtifactFile `json:"files"`
}

// ArtifactFile describes one file of a version.
type ArtifactFile struct {
	Path      string `json:"path"`
	Size      int64  `json:"size"`
	SHA256    string `json:"sha256"`
	MediaType string `json:"mediaType"`
	// Origin is "remote" for an image the hub fetched at publish time;
	// SourceURL and FetchStatus describe that fetch.
	Origin      string `json:"origin,omitempty"`
	SourceURL   string `json:"sourceUrl,omitempty"`
	FetchStatus string `json:"fetchStatus,omitempty"`
}

type artifactService struct {
	c *client
	// objectClient fetches the signed object-store URL a file read
	// redirects to. It carries no hub credentials. Nil means a default
	// client.
	objectClient *http.Client
}

// Publish implements ArtifactService. It is sent once, never retried,
// because a replay would create a second artifact.
func (s *artifactService) Publish(ctx context.Context, req *PublishArtifactRequest) (*ArtifactResponse, error) {
	q := url.Values{}
	q.Set("name", req.Name)
	if req.Title != "" {
		q.Set("title", req.Title)
	}
	if req.Scope != "" {
		q.Set("scope", req.Scope)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		s.c.transport.BaseURL+"/api/v1/artifacts?"+q.Encode(), req.Content)
	if err != nil {
		return nil, fmt.Errorf("create publish request: %w", err)
	}
	if req.Size >= 0 {
		httpReq.ContentLength = req.Size
	}
	ct := req.ContentType
	if ct == "" {
		ct = "application/octet-stream"
	}
	httpReq.Header.Set("Content-Type", ct)
	if req.SHA256 != "" {
		httpReq.Header.Set("X-Content-SHA256", req.SHA256)
	}
	resp, err := s.longTransport().DoNoRetry(ctx, httpReq)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeRequired[ArtifactResponse](resp)
}

// Get implements ArtifactService.
func (s *artifactService) Get(ctx context.Context, id string) (*ArtifactResponse, error) {
	resp, err := s.c.get(ctx, "/api/v1/artifacts/"+url.PathEscape(id), nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeRequired[ArtifactResponse](resp)
}

// List implements ArtifactService.
func (s *artifactService) List(ctx context.Context, opts *ListArtifactsOptions) (*ArtifactListResponse, error) {
	q := url.Values{}
	q.Set("mine", "1")
	if opts != nil {
		if opts.Query != "" {
			q.Set("q", opts.Query)
		}
		if opts.ReviewPending {
			q.Set("review_pending", "1")
		}
		if opts.OwnedOnly {
			q.Set("owner", "me")
		}
		if opts.Limit > 0 {
			q.Set("limit", strconv.Itoa(opts.Limit))
		}
		if opts.Cursor != "" {
			q.Set("cursor", opts.Cursor)
		}
	}
	resp, err := s.c.getWithQuery(ctx, "/api/v1/artifacts", q, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeRequired[ArtifactListResponse](resp)
}

// OpenFile implements ArtifactService. The hub either streams the bytes or
// redirects to a short-lived signed object-store URL. The redirect is
// followed here, by a client without hub credentials, so neither the hub
// token nor transport credentials are sent to the object store.
func (s *artifactService) OpenFile(ctx context.Context, id string, seq int, filePath string) (io.ReadCloser, error) {
	p := "/api/v1/artifacts/" + url.PathEscape(id)
	if seq > 0 {
		p += "/versions/" + strconv.Itoa(seq)
	}
	p += "/files/" + escapePathSegments(filePath)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.c.transport.BaseURL+p, nil)
	if err != nil {
		return nil, fmt.Errorf("create file request: %w", err)
	}
	resp, err := s.longTransport().DoNoRetry(ctx, req)
	if err != nil {
		return nil, err
	}
	switch {
	case resp.StatusCode == http.StatusOK:
		return resp.Body, nil
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		loc, lerr := resp.Location()
		_ = resp.Body.Close()
		if lerr != nil {
			return nil, fmt.Errorf("artifact file redirect without a usable Location: %w", lerr)
		}
		return s.fetchObject(ctx, loc.String())
	default:
		return nil, apiclient.CheckResponse(resp)
	}
}

func (s *artifactService) fetchObject(ctx context.Context, signedURL string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, signedURL, nil)
	if err != nil {
		return nil, fmt.Errorf("create object request: %w", err)
	}
	hc := s.objectClient
	if hc == nil {
		// No whole-exchange timeout: the caller's context bounds it.
		hc = &http.Client{}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch artifact object: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("fetch artifact object: unexpected status %s", resp.Status)
	}
	return resp.Body, nil
}

func artifactPath(id string) string { return "/api/v1/artifacts/" + url.PathEscape(id) }

// longTransport is the client's transport without its whole-exchange
// timeout. Publishing, uploading, finalizing and reading file bytes can
// legitimately take longer than that timeout (large files, or the hub
// fetching a markdown entry's remote images before it answers), so these
// calls are bounded by the caller's context instead. A publish that timed
// out on the client while the hub completed it would invite a retry that
// creates a second artifact or version.
func (s *artifactService) longTransport() *apiclient.Transport {
	t := *s.c.transport
	hc := http.Client{}
	if t.HTTPClient != nil {
		hc = *t.HTTPClient
	}
	hc.Timeout = 0
	t.HTTPClient = &hc
	return &t
}

// CreateVersion implements ArtifactService. It is sent once, never retried,
// because a replay would start a second version.
func (s *artifactService) CreateVersion(ctx context.Context, id string, req *CreateVersionRequest) (*PendingVersionResponse, error) {
	p := "/api/v1/artifacts"
	if id != "" {
		p = artifactPath(id) + "/versions"
	}
	resp, err := s.longTransport().PostNoRetry(ctx, p, req, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeRequired[PendingVersionResponse](resp)
}

// UploadFile implements ArtifactService.
func (s *artifactService) UploadFile(ctx context.Context, id string, seq int, filePath string, content io.Reader, size int64, sha256 string) error {
	p := artifactPath(id) + "/versions/" + strconv.Itoa(seq) + "/files/" + escapePathSegments(filePath)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, s.c.transport.BaseURL+p, content)
	if err != nil {
		return fmt.Errorf("create upload request: %w", err)
	}
	req.ContentLength = size
	req.Header.Set("Content-Type", "application/octet-stream")
	if sha256 != "" {
		req.Header.Set("X-Content-SHA256", sha256)
	}
	resp, err := s.longTransport().DoNoRetry(ctx, req)
	if err != nil {
		return err
	}
	return apiclient.CheckResponse(resp)
}

// FinalizeVersion implements ArtifactService.
func (s *artifactService) FinalizeVersion(ctx context.Context, id string, seq int) (*ArtifactResponse, error) {
	resp, err := s.longTransport().PostNoRetry(ctx, artifactPath(id)+"/versions/"+strconv.Itoa(seq)+"/finalize", nil, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeRequired[ArtifactResponse](resp)
}

// ListVersions implements ArtifactService.
func (s *artifactService) ListVersions(ctx context.Context, id string) ([]ArtifactVersion, error) {
	var all []ArtifactVersion
	before := 0
	for {
		p := artifactPath(id) + "/versions"
		if before > 0 {
			p += "?before=" + strconv.Itoa(before)
		}
		resp, err := s.c.get(ctx, p, nil)
		if err != nil {
			return nil, err
		}
		page, err := apiclient.DecodeRequired[struct {
			Versions   []ArtifactVersion `json:"versions"`
			NextBefore int               `json:"nextBefore"`
		}](resp)
		if err != nil {
			return nil, err
		}
		all = append(all, page.Versions...)
		if page.NextBefore <= 0 || (before > 0 && page.NextBefore >= before) {
			return all, nil
		}
		before = page.NextBefore
	}
}

// GetVersion implements ArtifactService.
func (s *artifactService) GetVersion(ctx context.Context, id string, seq int) (*ArtifactResponse, error) {
	resp, err := s.c.get(ctx, artifactPath(id)+"/versions/"+strconv.Itoa(seq), nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeRequired[ArtifactResponse](resp)
}
