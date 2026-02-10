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

// Package transfer provides shared file transfer types and utilities.
// This package consolidates common file transfer code used by both
// template and workspace synchronization.
package transfer

import "time"

// FileInfo describes a file for transfer operations.
// Used for both local file collection and remote manifest representation.
type FileInfo struct {
	// Path is the relative path within the source directory.
	Path string `json:"path"`

	// FullPath is the absolute path on the local filesystem.
	// This field is not serialized and is only used during collection.
	FullPath string `json:"-"`

	// Size is the file size in bytes.
	Size int64 `json:"size"`

	// Hash is the SHA-256 content hash in format "sha256:<hex>".
	Hash string `json:"hash"`

	// Mode is the file permissions in octal format (e.g., "0644").
	Mode string `json:"mode,omitempty"`
}

// Manifest describes a collection of files for transfer.
type Manifest struct {
	// Version is the manifest format version.
	Version string `json:"version"`

	// ContentHash is the overall hash computed from all file hashes.
	ContentHash string `json:"contentHash,omitempty"`

	// Files is the list of files in the manifest.
	Files []FileInfo `json:"files"`
}

// UploadURLInfo contains a signed URL for uploading a file.
type UploadURLInfo struct {
	// Path is the relative file path.
	Path string `json:"path"`

	// URL is the signed URL for uploading.
	URL string `json:"url"`

	// Method is the HTTP method to use (typically "PUT").
	Method string `json:"method"`

	// Headers contains additional headers to include in the request.
	Headers map[string]string `json:"headers,omitempty"`

	// Expires is when the signed URL expires.
	Expires time.Time `json:"expires,omitempty"`
}

// DownloadURLInfo contains a signed URL for downloading a file.
type DownloadURLInfo struct {
	// Path is the relative file path.
	Path string `json:"path"`

	// URL is the signed URL for downloading.
	URL string `json:"url"`

	// Size is the expected file size in bytes.
	Size int64 `json:"size"`

	// Hash is the expected file hash for verification.
	Hash string `json:"hash,omitempty"`
}

// ProgressCallback is called during file transfer operations to report progress.
// The bytesTransferred indicates progress for the current file.
// Return an error to abort the transfer.
type ProgressCallback func(file FileInfo, bytesTransferred int64) error
