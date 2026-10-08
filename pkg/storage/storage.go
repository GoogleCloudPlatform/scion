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

// Package storage provides an abstraction layer for cloud storage operations.
// It supports multiple backends (GCS, S3, Azure Blob, local filesystem) with
// a unified interface for template storage and other Hub storage needs.
package storage

import (
	"context"
	"errors"
	"io"
	"strings"
	"time"
)

var (
	// ErrNotFound indicates the requested object was not found.
	ErrNotFound = errors.New("object not found")
	// ErrAccessDenied indicates access to the object was denied.
	ErrAccessDenied = errors.New("access denied")
	// ErrInvalidPath indicates the provided path is invalid.
	ErrInvalidPath = errors.New("invalid path")
)

// Provider identifies the storage backend type.
type Provider string

const (
	ProviderGCS   Provider = "gcs"
	ProviderS3    Provider = "s3"
	ProviderAzure Provider = "azure"
	ProviderLocal Provider = "local"
)

// Config holds storage configuration.
type Config struct {
	// Provider is the storage backend type.
	Provider Provider `json:"provider" yaml:"provider"`
	// Bucket is the bucket or container name.
	Bucket string `json:"bucket" yaml:"bucket"`
	// Region is the cloud region (for S3).
	Region string `json:"region,omitempty" yaml:"region,omitempty"`
	// Endpoint is a custom endpoint URL (for S3-compatible storage).
	Endpoint string `json:"endpoint,omitempty" yaml:"endpoint,omitempty"`
	// Credentials contains provider-specific credentials.
	Credentials *Credentials `json:"credentials,omitempty" yaml:"credentials,omitempty"`
	// LocalPath is the base path for local storage provider.
	LocalPath string `json:"localPath,omitempty" yaml:"localPath,omitempty"`
}

// Credentials holds authentication credentials.
type Credentials struct {
	// ServiceAccountJSON is the GCS service account key JSON.
	ServiceAccountJSON string `json:"serviceAccountJson,omitempty" yaml:"serviceAccountJson,omitempty"`
	// AccessKeyID is the S3 access key.
	AccessKeyID string `json:"accessKeyId,omitempty" yaml:"accessKeyId,omitempty"`
	// SecretAccessKey is the S3 secret key.
	SecretAccessKey string `json:"secretAccessKey,omitempty" yaml:"secretAccessKey,omitempty"`
	// ConnectionString is the Azure connection string.
	ConnectionString string `json:"connectionString,omitempty" yaml:"connectionString,omitempty"`
}

// Object represents a storage object.
type Object struct {
	// Name is the object name/path.
	Name string `json:"name"`
	// Size is the object size in bytes.
	Size int64 `json:"size"`
	// ContentType is the MIME content type.
	ContentType string `json:"contentType,omitempty"`
	// ETag is the entity tag for cache validation.
	ETag string `json:"etag,omitempty"`
	// Created is when the object was created.
	Created time.Time `json:"created,omitempty"`
	// Updated is when the object was last modified.
	Updated time.Time `json:"updated,omitempty"`
	// Metadata is custom metadata attached to the object.
	Metadata map[string]string `json:"metadata,omitempty"`
	// Generation identifies this version of the object's content, for
	// providers that version objects (GCS); 0 when the provider does not.
	Generation int64 `json:"generation,omitempty"`
}

// ErrPreconditionFailed is returned by DeleteIfGeneration when the object
// no longer has the given generation.
var ErrPreconditionFailed = errors.New("object generation does not match")

// GenerationDeleter is implemented by providers that can delete an object
// only while it still has a given generation (GCS), so a delete that
// reaches the provider late does not remove content written since.
// Callers that must not lose content written after a delete was issued use
// it for such providers; an unconditional Delete is only safe on providers
// whose delete is synchronous (it never applies after the call returns),
// such as local storage, which does not implement this interface.
type GenerationDeleter interface {
	// DeleteIfGeneration deletes the object at objectPath if its
	// generation is generation. It returns ErrNotFound when there is no
	// object and ErrPreconditionFailed when its generation differs.
	DeleteIfGeneration(ctx context.Context, objectPath string, generation int64) error
}

// SignedURLOptions configures signed URL generation.
type SignedURLOptions struct {
	// Method is the HTTP method (GET, PUT).
	Method string
	// Expires is the URL expiration duration.
	Expires time.Duration
	// ContentType is the expected content type (for PUT).
	ContentType string
	// ContentMD5 is the expected MD5 hash (for PUT).
	ContentMD5 string
	// ResponseContentType, when set on a GET URL, overrides the
	// Content-Type the object store answers with. Providers that cannot
	// override response headers ignore it.
	ResponseContentType string
	// ResponseContentDisposition, when set on a GET URL, overrides the
	// Content-Disposition the object store answers with. Providers that
	// cannot override response headers ignore it.
	ResponseContentDisposition string
}

// SignedURL contains a signed URL for object access.
type SignedURL struct {
	// URL is the signed URL.
	URL string `json:"url"`
	// Method is the HTTP method this URL is valid for.
	Method string `json:"method"`
	// Expires is when the URL expires.
	Expires time.Time `json:"expires"`
	// Headers are required headers for the request.
	Headers map[string]string `json:"headers,omitempty"`
}

// ListOptions configures object listing.
type ListOptions struct {
	// Prefix filters objects by prefix.
	Prefix string
	// Delimiter is used for hierarchical listing (e.g., "/").
	Delimiter string
	// MaxResults limits the number of results.
	MaxResults int
	// StartOffset skips objects before this key.
	StartOffset string
}

// ListResult contains the result of a list operation.
type ListResult struct {
	// Objects are the matching objects.
	Objects []Object `json:"objects"`
	// Prefixes are common prefixes when using a delimiter.
	Prefixes []string `json:"prefixes,omitempty"`
	// NextOffset is the key to use for the next page.
	NextOffset string `json:"nextOffset,omitempty"`
}

// Storage defines the interface for storage backends.
type Storage interface {
	// Bucket returns the bucket name.
	Bucket() string

	// Provider returns the storage provider type.
	Provider() Provider

	// GenerateSignedURL creates a signed URL for object access.
	// For uploads (PUT), the URL allows direct upload without going through the server.
	// For downloads (GET), the URL allows direct download.
	GenerateSignedURL(ctx context.Context, objectPath string, opts SignedURLOptions) (*SignedURL, error)

	// Upload uploads data to the specified path.
	Upload(ctx context.Context, objectPath string, reader io.Reader, opts UploadOptions) (*Object, error)

	// Download downloads data from the specified path.
	Download(ctx context.Context, objectPath string) (io.ReadCloser, *Object, error)

	// Delete deletes the object at the specified path.
	Delete(ctx context.Context, objectPath string) error

	// DeletePrefix deletes all objects with the given prefix.
	DeletePrefix(ctx context.Context, prefix string) error

	// List lists objects matching the given options.
	List(ctx context.Context, opts ListOptions) (*ListResult, error)

	// Exists checks if an object exists.
	Exists(ctx context.Context, objectPath string) (bool, error)

	// GetObject returns object metadata without downloading content.
	GetObject(ctx context.Context, objectPath string) (*Object, error)

	// Copy copies an object from src to dst within the same bucket.
	Copy(ctx context.Context, srcPath, dstPath string) (*Object, error)

	// Close releases any resources held by the storage client.
	Close() error
}

// UploadOptions configures upload operations.
type UploadOptions struct {
	// ContentType is the MIME type of the content.
	ContentType string
	// ContentDisposition sets the Content-Disposition header.
	ContentDisposition string
	// CacheControl sets the Cache-Control header.
	CacheControl string
	// Metadata is custom metadata to attach to the object.
	Metadata map[string]string
	// Idempotent says that writing these bytes again is harmless (a
	// content-addressed object always gets the same bytes) and that the
	// caller retries a failed write itself, with its own bound on attempts
	// and time: the provider makes one attempt per call (GCS turns off its
	// client's retrying) and returns its error.
	Idempotent bool
}

// New creates a new storage client based on the configuration.
func New(ctx context.Context, cfg Config) (Storage, error) {
	switch cfg.Provider {
	case ProviderGCS:
		return NewGCS(ctx, cfg)
	case ProviderLocal:
		return NewLocal(cfg)
	default:
		return nil, errors.New("unsupported storage provider: " + string(cfg.Provider))
	}
}

// ResourceKind identifies a storable, file-based resource type. The
// resource-storage layout collapses the parallel template and
// harness-config storage code onto a single kind-keyed implementation; this is
// the first shared seam — the storage-path layout.
type ResourceKind string

const (
	// ResourceKindTemplate is a provisioning template.
	ResourceKindTemplate ResourceKind = "template"
	// ResourceKindHarnessConfig is a harness configuration bundle.
	ResourceKindHarnessConfig ResourceKind = "harness-config"
	// ResourceKindSkill is a skill bank skill.
	ResourceKindSkill ResourceKind = "skill"
)

// resourcePrefix returns the top-level storage prefix for a resource kind.
func resourcePrefix(kind ResourceKind) string {
	switch kind {
	case ResourceKindHarnessConfig:
		return "harness-configs"
	case ResourceKindSkill:
		return "skills"
	default:
		return "templates"
	}
}

// DirPrefix returns the DeletePrefix/List prefix that matches exactly the
// objects under the directory-like storage path dir: dir with one trailing
// "/". Object stores such as GCS match prefixes as plain strings, so passing
// "a/foo" would also match the sibling "a/foo-bar/...". An empty (or
// all-slash) dir yields "", which DeletePrefix rejects with ErrInvalidPath,
// rather than "/", which would match every object in the bucket.
func DirPrefix(dir string) string {
	dir = strings.TrimRight(dir, "/")
	if dir == "" {
		return ""
	}
	return dir + "/"
}

// ResourceStoragePath returns the storage path for a file-based resource of the
// given kind, organized by scope. This is the single source of truth for the
// scope layout shared by all resource kinds; the per-kind helpers below delegate
// to it.
func ResourceStoragePath(hubID string, kind ResourceKind, scope, scopeID, slug string) string {
	prefix := resourcePrefix(kind)
	var scopePath string
	switch scope {
	case "global":
		scopePath = prefix + "/global/" + slug
	case "project":
		scopePath = prefix + "/groves/" + scopeID + "/" + slug
	case "user":
		scopePath = prefix + "/users/" + scopeID + "/" + slug
	default:
		scopePath = prefix + "/" + slug
	}
	if hubID != "" {
		return "hubs/" + hubID + "/" + scopePath
	}
	return scopePath
}

// ResourceStorageURI returns the full bucket URI for a file-based resource.
func ResourceStorageURI(hubID, bucket string, kind ResourceKind, scope, scopeID, slug string) string {
	return StorageURIForPath(bucket, ResourceStoragePath(hubID, kind, scope, scopeID, slug))
}

// StorageURIForPath returns the full bucket URI for an already-computed
// storage path, e.g. one made request-unique with a suffix the per-kind
// path helpers above don't know about. Callers that can express their path
// as (hubID, kind, scope, scopeID, slug) should use ResourceStorageURI (or
// one of its per-kind wrappers) instead, so the URI format stays defined in
// this one place either way.
func StorageURIForPath(bucket, path string) string {
	return "gs://" + bucket + "/" + strings.TrimRight(path, "/") + "/"
}

// TemplateStoragePath returns the storage path for a template.
// Templates are stored under the /templates prefix with scope-based organization.
func TemplateStoragePath(hubID, scope, scopeID, templateSlug string) string {
	return ResourceStoragePath(hubID, ResourceKindTemplate, scope, scopeID, templateSlug)
}

// TemplateStorageURI returns the full storage URI for a template.
func TemplateStorageURI(hubID, bucket, scope, scopeID, templateSlug string) string {
	return ResourceStorageURI(hubID, bucket, ResourceKindTemplate, scope, scopeID, templateSlug)
}

// SkillStoragePath returns the storage path for a skill.
// Skills are stored under the /skills prefix with scope-based organization.
func SkillStoragePath(hubID, scope, scopeID, slug string) string {
	return ResourceStoragePath(hubID, ResourceKindSkill, scope, scopeID, slug)
}

// SkillStorageURI returns the full storage URI for a skill.
func SkillStorageURI(hubID, bucket, scope, scopeID, slug string) string {
	return ResourceStorageURI(hubID, bucket, ResourceKindSkill, scope, scopeID, slug)
}

// HarnessConfigStoragePath returns the storage path for a harness config.
// Harness configs are stored under the /harness-configs prefix with scope-based organization.
func HarnessConfigStoragePath(hubID, scope, scopeID, slug string) string {
	return ResourceStoragePath(hubID, ResourceKindHarnessConfig, scope, scopeID, slug)
}

// HarnessConfigStorageURI returns the full storage URI for a harness config.
func HarnessConfigStorageURI(hubID, bucket, scope, scopeID, slug string) string {
	return ResourceStorageURI(hubID, bucket, ResourceKindHarnessConfig, scope, scopeID, slug)
}

// WorkspaceStoragePath returns the storage path for an agent's workspace.
// Workspaces are stored under /workspaces/{projectId}/{agentId}/.
func WorkspaceStoragePath(hubID, projectID, agentID string) string {
	path := "workspaces/" + projectID + "/" + agentID
	if hubID != "" {
		return "hubs/" + hubID + "/" + path
	}
	return path
}

// ProjectWorkspaceStoragePath returns the storage path for a hub-managed project's shared workspace.
// Hub-managed projects share a single workspace across agents (no per-agent worktrees),
// so the path is project-level rather than agent-level.
func ProjectWorkspaceStoragePath(hubID, projectID string) string {
	path := "workspaces/" + projectID + "/grove-workspace"
	if hubID != "" {
		return "hubs/" + hubID + "/" + path
	}
	return path
}

// WorkspaceStorageURI returns the full storage URI for an agent's workspace.
func WorkspaceStorageURI(hubID, bucket, projectID, agentID string) string {
	path := WorkspaceStoragePath(hubID, projectID, agentID)
	return "gs://" + bucket + "/" + path + "/"
}
