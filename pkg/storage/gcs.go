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

package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"cloud.google.com/go/storage"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
)

// GCSStorage implements Storage using Google Cloud Storage.
type GCSStorage struct {
	client *storage.Client
	bucket *storage.BucketHandle
	config Config
}

// NewGCS creates a new GCS storage client.
func NewGCS(ctx context.Context, cfg Config) (*GCSStorage, error) {
	if cfg.Bucket == "" {
		return nil, errors.New("bucket name is required for GCS storage")
	}

	var opts []option.ClientOption

	// Use service account credentials if provided
	if cfg.Credentials != nil && cfg.Credentials.ServiceAccountJSON != "" {
		opts = append(opts, option.WithCredentialsJSON([]byte(cfg.Credentials.ServiceAccountJSON)))
	}

	// Create the client
	client, err := storage.NewClient(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("failed to create GCS client: %w", err)
	}

	return &GCSStorage{
		client: client,
		bucket: client.Bucket(cfg.Bucket),
		config: cfg,
	}, nil
}

// Bucket returns the bucket name.
func (s *GCSStorage) Bucket() string {
	return s.config.Bucket
}

// Provider returns the storage provider type.
func (s *GCSStorage) Provider() Provider {
	return ProviderGCS
}

// GenerateSignedURL creates a signed URL for object access.
func (s *GCSStorage) GenerateSignedURL(ctx context.Context, objectPath string, opts SignedURLOptions) (*SignedURL, error) {
	if objectPath == "" {
		return nil, ErrInvalidPath
	}

	// Clean the path
	objectPath = strings.TrimPrefix(objectPath, "/")

	// Set default expiration
	expires := opts.Expires
	if expires == 0 {
		expires = 15 * time.Minute
	}

	// Prepare signing options
	signOpts := &storage.SignedURLOptions{
		Scheme:  storage.SigningSchemeV4,
		Method:  opts.Method,
		Expires: time.Now().Add(expires),
	}

	if opts.ContentType != "" {
		signOpts.ContentType = opts.ContentType
	}

	// Generate the signed URL
	url, err := s.bucket.SignedURL(objectPath, signOpts)
	if err != nil {
		return nil, fmt.Errorf("failed to generate signed URL: %w", err)
	}

	result := &SignedURL{
		URL:     url,
		Method:  opts.Method,
		Expires: signOpts.Expires,
	}

	// Add required headers for uploads
	if opts.Method == "PUT" && opts.ContentType != "" {
		result.Headers = map[string]string{
			"Content-Type": opts.ContentType,
		}
	}

	return result, nil
}

// Upload uploads data to the specified path.
func (s *GCSStorage) Upload(ctx context.Context, objectPath string, reader io.Reader, opts UploadOptions) (*Object, error) {
	if objectPath == "" {
		return nil, ErrInvalidPath
	}

	objectPath = strings.TrimPrefix(objectPath, "/")
	obj := s.bucket.Object(objectPath)
	writer := obj.NewWriter(ctx)

	// Set content type
	if opts.ContentType != "" {
		writer.ContentType = opts.ContentType
	}

	// Set cache control
	if opts.CacheControl != "" {
		writer.CacheControl = opts.CacheControl
	}

	// Set content disposition
	if opts.ContentDisposition != "" {
		writer.ContentDisposition = opts.ContentDisposition
	}

	// Set metadata
	if len(opts.Metadata) > 0 {
		writer.Metadata = opts.Metadata
	}

	// Copy data
	size, err := io.Copy(writer, reader)
	if err != nil {
		writer.Close()
		return nil, fmt.Errorf("failed to upload data: %w", err)
	}

	// Close and finalize
	if err := writer.Close(); err != nil {
		return nil, fmt.Errorf("failed to finalize upload: %w", err)
	}

	// Get object attributes
	attrs, err := obj.Attrs(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get object attributes: %w", err)
	}

	return &Object{
		Name:        objectPath,
		Size:        size,
		ContentType: attrs.ContentType,
		ETag:        attrs.Etag,
		Created:     attrs.Created,
		Updated:     attrs.Updated,
		Metadata:    attrs.Metadata,
	}, nil
}

// Download downloads data from the specified path.
func (s *GCSStorage) Download(ctx context.Context, objectPath string) (io.ReadCloser, *Object, error) {
	if objectPath == "" {
		return nil, nil, ErrInvalidPath
	}

	objectPath = strings.TrimPrefix(objectPath, "/")
	obj := s.bucket.Object(objectPath)

	// Get object attributes first
	attrs, err := obj.Attrs(ctx)
	if err != nil {
		if errors.Is(err, storage.ErrObjectNotExist) {
			return nil, nil, ErrNotFound
		}
		return nil, nil, fmt.Errorf("failed to get object attributes: %w", err)
	}

	// Create reader
	reader, err := obj.NewReader(ctx)
	if err != nil {
		if errors.Is(err, storage.ErrObjectNotExist) {
			return nil, nil, ErrNotFound
		}
		return nil, nil, fmt.Errorf("failed to create reader: %w", err)
	}

	return reader, &Object{
		Name:        objectPath,
		Size:        attrs.Size,
		ContentType: attrs.ContentType,
		ETag:        attrs.Etag,
		Created:     attrs.Created,
		Updated:     attrs.Updated,
		Metadata:    attrs.Metadata,
	}, nil
}

// Delete deletes the object at the specified path.
func (s *GCSStorage) Delete(ctx context.Context, objectPath string) error {
	if objectPath == "" {
		return ErrInvalidPath
	}

	objectPath = strings.TrimPrefix(objectPath, "/")
	obj := s.bucket.Object(objectPath)

	if err := obj.Delete(ctx); err != nil {
		if errors.Is(err, storage.ErrObjectNotExist) {
			return ErrNotFound
		}
		return fmt.Errorf("failed to delete object: %w", err)
	}

	return nil
}

// DeletePrefix deletes all objects with the given prefix.
func (s *GCSStorage) DeletePrefix(ctx context.Context, prefix string) error {
	if prefix == "" {
		return ErrInvalidPath
	}

	prefix = strings.TrimPrefix(prefix, "/")

	// List all objects with the prefix
	it := s.bucket.Objects(ctx, &storage.Query{Prefix: prefix})

	var errors []error
	for {
		attrs, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			errors = append(errors, fmt.Errorf("failed to list object: %w", err))
			continue
		}

		if err := s.bucket.Object(attrs.Name).Delete(ctx); err != nil && !isNotExist(err) {
			errors = append(errors, fmt.Errorf("failed to delete %s: %w", attrs.Name, err))
		}
	}

	if len(errors) > 0 {
		return fmt.Errorf("failed to delete some objects: %v", errors)
	}

	return nil
}

// List lists objects matching the given options.
func (s *GCSStorage) List(ctx context.Context, opts ListOptions) (*ListResult, error) {
	query := &storage.Query{}

	if opts.Prefix != "" {
		query.Prefix = strings.TrimPrefix(opts.Prefix, "/")
	}

	if opts.Delimiter != "" {
		query.Delimiter = opts.Delimiter
	}

	if opts.StartOffset != "" {
		query.StartOffset = opts.StartOffset
	}

	it := s.bucket.Objects(ctx, query)

	result := &ListResult{}
	count := 0

	for {
		if opts.MaxResults > 0 && count >= opts.MaxResults {
			// Get next item to check if there are more
			attrs, err := it.Next()
			if err == nil {
				result.NextOffset = attrs.Name
			}
			break
		}

		attrs, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("failed to list objects: %w", err)
		}

		// Handle prefixes (directories)
		if attrs.Prefix != "" {
			result.Prefixes = append(result.Prefixes, attrs.Prefix)
			continue
		}

		result.Objects = append(result.Objects, Object{
			Name:        attrs.Name,
			Size:        attrs.Size,
			ContentType: attrs.ContentType,
			ETag:        attrs.Etag,
			Created:     attrs.Created,
			Updated:     attrs.Updated,
			Metadata:    attrs.Metadata,
		})
		count++
	}

	return result, nil
}

// Exists checks if an object exists.
func (s *GCSStorage) Exists(ctx context.Context, objectPath string) (bool, error) {
	if objectPath == "" {
		return false, ErrInvalidPath
	}

	objectPath = strings.TrimPrefix(objectPath, "/")
	_, err := s.bucket.Object(objectPath).Attrs(ctx)

	if err != nil {
		if errors.Is(err, storage.ErrObjectNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("failed to check object existence: %w", err)
	}

	return true, nil
}

// GetObject returns object metadata without downloading content.
func (s *GCSStorage) GetObject(ctx context.Context, objectPath string) (*Object, error) {
	if objectPath == "" {
		return nil, ErrInvalidPath
	}

	objectPath = strings.TrimPrefix(objectPath, "/")
	attrs, err := s.bucket.Object(objectPath).Attrs(ctx)

	if err != nil {
		if errors.Is(err, storage.ErrObjectNotExist) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("failed to get object attributes: %w", err)
	}

	return &Object{
		Name:        objectPath,
		Size:        attrs.Size,
		ContentType: attrs.ContentType,
		ETag:        attrs.Etag,
		Created:     attrs.Created,
		Updated:     attrs.Updated,
		Metadata:    attrs.Metadata,
	}, nil
}

// Copy copies an object from src to dst within the same bucket.
func (s *GCSStorage) Copy(ctx context.Context, srcPath, dstPath string) (*Object, error) {
	if srcPath == "" || dstPath == "" {
		return nil, ErrInvalidPath
	}

	srcPath = strings.TrimPrefix(srcPath, "/")
	dstPath = strings.TrimPrefix(dstPath, "/")

	src := s.bucket.Object(srcPath)
	dst := s.bucket.Object(dstPath)

	attrs, err := dst.CopierFrom(src).Run(ctx)
	if err != nil {
		if errors.Is(err, storage.ErrObjectNotExist) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("failed to copy object: %w", err)
	}

	return &Object{
		Name:        dstPath,
		Size:        attrs.Size,
		ContentType: attrs.ContentType,
		ETag:        attrs.Etag,
		Created:     attrs.Created,
		Updated:     attrs.Updated,
		Metadata:    attrs.Metadata,
	}, nil
}

// Close releases any resources held by the storage client.
func (s *GCSStorage) Close() error {
	return s.client.Close()
}

// isNotExist checks if an error is a "not found" error.
func isNotExist(err error) bool {
	return errors.Is(err, storage.ErrObjectNotExist)
}

// Ensure GCSStorage implements Storage interface.
var _ Storage = (*GCSStorage)(nil)
