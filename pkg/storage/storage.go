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

// TemplateStoragePath returns the storage path for a template.
// Templates are stored under the /templates prefix with scope-based organization.
func TemplateStoragePath(scope, scopeID, templateSlug string) string {
	switch scope {
	case "global":
		return "templates/global/" + templateSlug
	case "grove":
		return "templates/groves/" + scopeID + "/" + templateSlug
	case "user":
		return "templates/users/" + scopeID + "/" + templateSlug
	default:
		return "templates/" + templateSlug
	}
}

// TemplateStorageURI returns the full storage URI for a template.
func TemplateStorageURI(bucket, scope, scopeID, templateSlug string) string {
	path := TemplateStoragePath(scope, scopeID, templateSlug)
	return "gs://" + bucket + "/" + path + "/"
}

// HarnessConfigStoragePath returns the storage path for a harness config.
// Harness configs are stored under the /harness-configs prefix with scope-based organization.
func HarnessConfigStoragePath(scope, scopeID, slug string) string {
	switch scope {
	case "global":
		return "harness-configs/global/" + slug
	case "grove":
		return "harness-configs/groves/" + scopeID + "/" + slug
	case "user":
		return "harness-configs/users/" + scopeID + "/" + slug
	default:
		return "harness-configs/" + slug
	}
}

// HarnessConfigStorageURI returns the full storage URI for a harness config.
func HarnessConfigStorageURI(bucket, scope, scopeID, slug string) string {
	path := HarnessConfigStoragePath(scope, scopeID, slug)
	return "gs://" + bucket + "/" + path + "/"
}

// WorkspaceStoragePath returns the storage path for an agent's workspace.
// Workspaces are stored under /workspaces/{groveId}/{agentId}/.
func WorkspaceStoragePath(groveID, agentID string) string {
	return "workspaces/" + groveID + "/" + agentID
}

// ProjectWorkspaceStoragePath returns the storage path for a hub-native grove's shared workspace.
// Hub-native groves share a single workspace across agents (no per-agent worktrees),
// so the path is grove-level rather than agent-level.
func ProjectWorkspaceStoragePath(groveID string) string {
	return "workspaces/" + groveID + "/grove-workspace"
}

// WorkspaceStorageURI returns the full storage URI for an agent's workspace.
func WorkspaceStorageURI(bucket, groveID, agentID string) string {
	path := WorkspaceStoragePath(groveID, agentID)
	return "gs://" + bucket + "/" + path + "/"
}
