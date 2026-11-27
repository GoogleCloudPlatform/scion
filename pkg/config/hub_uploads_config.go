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

package config

import "fmt"

// Upload limit defaults and bounds (settings server.hub.uploads).
const (
	// DefaultUploadMaxAttachmentSize is the per-file limit of a chat
	// attachment (stored outside the artifact service) when
	// server.hub.uploads.max_attachment_size is unset.
	DefaultUploadMaxAttachmentSize int64 = 10 << 20
	// DefaultUploadMaxWorkspaceFileSize is the per-file limit of a project
	// workspace or shared directory upload when
	// server.hub.uploads.max_workspace_file_size is unset.
	DefaultUploadMaxWorkspaceFileSize int64 = 50 << 20
	// MaxUploadFileSize is the largest per-file limit accepted for single-request
	// multipart uploads (matching the 100 MiB workspace request body cap).
	MaxUploadFileSize int64 = 100 << 20
)

// HubUploadsConfig holds web upload limits (settings server.hub.uploads).
// Read at startup. Zero means the default for every field.
type HubUploadsConfig struct {
	// MaxAttachmentSize is the per-file limit, in bytes, of a chat
	// attachment stored by the hub's attachment store. Attachments stored in
	// the artifact service keep that service's own file size limit.
	MaxAttachmentSize int64 `json:"maxAttachmentSize,omitempty" yaml:"maxAttachmentSize,omitempty" koanf:"maxAttachmentSize"`
	// MaxWorkspaceFileSize is the per-file limit, in bytes, of an upload to
	// a project workspace or shared directory.
	MaxWorkspaceFileSize int64 `json:"maxWorkspaceFileSize,omitempty" yaml:"maxWorkspaceFileSize,omitempty" koanf:"maxWorkspaceFileSize"`
}

// IsZero reports whether no upload limit is configured.
func (c HubUploadsConfig) IsZero() bool { return c == HubUploadsConfig{} }

// EffectiveMaxAttachmentSize returns MaxAttachmentSize, or its default.
func (c HubUploadsConfig) EffectiveMaxAttachmentSize() int64 {
	if c.MaxAttachmentSize > 0 {
		return c.MaxAttachmentSize
	}
	return DefaultUploadMaxAttachmentSize
}

// EffectiveMaxWorkspaceFileSize returns MaxWorkspaceFileSize, or its default.
func (c HubUploadsConfig) EffectiveMaxWorkspaceFileSize() int64 {
	if c.MaxWorkspaceFileSize > 0 {
		return c.MaxWorkspaceFileSize
	}
	return DefaultUploadMaxWorkspaceFileSize
}

// Validate reports a value the hub refuses at startup: a negative value or a
// per-file limit above MaxUploadFileSize.
func (c HubUploadsConfig) Validate() error {
	for _, f := range []struct {
		key string
		v   int64
	}{
		{"max_attachment_size", c.MaxAttachmentSize},
		{"max_workspace_file_size", c.MaxWorkspaceFileSize},
	} {
		if f.v < 0 {
			return fmt.Errorf("invalid server.hub.uploads.%s %d: must not be negative", f.key, f.v)
		}
		if f.v > MaxUploadFileSize {
			return fmt.Errorf("invalid server.hub.uploads.%s %d: must be at most %d", f.key, f.v, MaxUploadFileSize)
		}
	}
	return nil
}
