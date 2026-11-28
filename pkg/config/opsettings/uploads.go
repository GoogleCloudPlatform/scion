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

package opsettings

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// Compiled defaults and bounds for the resumable chunked upload fields of the
// uploads operational settings section (ptone/scion#8654 §3.11).
// UploadsSectionName and UploadsSettings are declared in sections.go alongside
// the per-file limit fields (ptone/scion#9619).
const (
	UploadsDefaultChunkBytes int64 = 8 << 20  // 8 MiB
	UploadsMinChunkBytes     int64 = 1 << 20  // 1 MiB
	UploadsMaxChunkBytes     int64 = 16 << 20 // 16 MiB

	UploadsDefaultSessionIdleTTL = 1 * time.Hour
	UploadsMinSessionIdleTTL     = 5 * time.Minute
	UploadsMaxSessionIdleTTL     = 24 * time.Hour

	UploadsDefaultSessionMaxLifetime = 24 * time.Hour
	UploadsMaxSessionMaxLifetime     = 7 * 24 * time.Hour // 7d

	UploadsDefaultMaxSessionsPerUser = 4
	UploadsMinMaxSessionsPerUser     = 1
	UploadsMaxMaxSessionsPerUser     = 64

	UploadsDefaultMaxPendingBytesPerUser int64 = 2 << 30  // 2 GiB
	UploadsDefaultMaxPendingBytesHub     int64 = 16 << 30 // 16 GiB
)

// UploadsConfig is the resolved uploads configuration: every field holds its
// effective value.
type UploadsConfig struct {
	ChunkBytes             int64
	SessionIdleTTL         time.Duration
	SessionMaxLifetime     time.Duration
	MaxSessionsPerUser     int
	MaxPendingBytesPerUser int64
	MaxPendingBytesHub     int64

	// Malformed is true when the stored document could not be parsed or failed
	// validation. Callers receive compiled defaults alongside Malformed=true.
	Malformed bool
}

// DefaultUploadsConfig returns the resolved configuration used when the
// uploads section is absent.
func DefaultUploadsConfig() UploadsConfig {
	return UploadsConfig{
		ChunkBytes:             UploadsDefaultChunkBytes,
		SessionIdleTTL:         UploadsDefaultSessionIdleTTL,
		SessionMaxLifetime:     UploadsDefaultSessionMaxLifetime,
		MaxSessionsPerUser:     UploadsDefaultMaxSessionsPerUser,
		MaxPendingBytesPerUser: UploadsDefaultMaxPendingBytesPerUser,
		MaxPendingBytesHub:     UploadsDefaultMaxPendingBytesHub,
	}
}

// MalformedUploadsConfig returns the compiled defaults marked as Malformed.
func MalformedUploadsConfig() UploadsConfig {
	c := DefaultUploadsConfig()
	c.Malformed = true
	return c
}

// PendingBytesWarning returns a human-readable warning if
// MaxPendingBytesPerUser is smaller than largestPerFileMax, or empty string
// otherwise (§3.11: warn, not error).
func (c UploadsConfig) PendingBytesWarning(largestPerFileMax int64) string {
	if largestPerFileMax > 0 && c.MaxPendingBytesPerUser < largestPerFileMax {
		return fmt.Sprintf("uploads.max_pending_bytes_per_user (%d) is below the largest effective per-file limit (%d)",
			c.MaxPendingBytesPerUser, largestPerFileMax)
	}
	return ""
}

// parseUploadDuration parses a positive duration string, accepting both Go
// time.ParseDuration syntax (e.g. "5m", "1h", "24h", "168h") and whole-day
// shorthands like "7d".
func parseUploadDuration(raw string) (time.Duration, bool) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return 0, false
	}
	if strings.HasSuffix(s, "d") {
		n, err := strconv.Atoi(strings.TrimSuffix(s, "d"))
		if err != nil || n <= 0 {
			return 0, false
		}
		return time.Duration(n) * 24 * time.Hour, true
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return 0, false
	}
	return d, true
}

// Resolve applies compiled defaults to omitted fields and validates all ranges
// and cross-field rules.
func (s UploadsSettings) Resolve() (UploadsConfig, error) {
	c := DefaultUploadsConfig()

	if s.ChunkBytes != nil {
		c.ChunkBytes = *s.ChunkBytes
	}
	if raw := strings.TrimSpace(s.SessionIdleTTL); raw != "" {
		d, ok := parseUploadDuration(raw)
		if !ok {
			return MalformedUploadsConfig(), fmt.Errorf("uploads settings: session_idle_ttl must be a valid positive duration, got %q", s.SessionIdleTTL)
		}
		c.SessionIdleTTL = d
	}
	if raw := strings.TrimSpace(s.SessionMaxLifetime); raw != "" {
		d, ok := parseUploadDuration(raw)
		if !ok {
			return MalformedUploadsConfig(), fmt.Errorf("uploads settings: session_max_lifetime must be a valid positive duration, got %q", s.SessionMaxLifetime)
		}
		c.SessionMaxLifetime = d
	}
	if s.MaxSessionsPerUser != nil {
		c.MaxSessionsPerUser = *s.MaxSessionsPerUser
	}
	if s.MaxPendingBytesPerUser != nil {
		c.MaxPendingBytesPerUser = *s.MaxPendingBytesPerUser
	}
	if s.MaxPendingBytesHub != nil {
		c.MaxPendingBytesHub = *s.MaxPendingBytesHub
	}

	var fileLimits config.HubUploadsConfig
	if s.MaxAttachmentSize != nil {
		fileLimits.MaxAttachmentSize = *s.MaxAttachmentSize
	}
	if s.MaxWorkspaceFileSize != nil {
		fileLimits.MaxWorkspaceFileSize = *s.MaxWorkspaceFileSize
	}
	if err := fileLimits.Validate(); err != nil {
		return MalformedUploadsConfig(), err
	}

	var err error
	switch {
	case c.ChunkBytes < UploadsMinChunkBytes || c.ChunkBytes > UploadsMaxChunkBytes:
		err = fmt.Errorf("chunk_bytes must be between %d and %d, got %d", UploadsMinChunkBytes, UploadsMaxChunkBytes, c.ChunkBytes)
	case c.SessionIdleTTL < UploadsMinSessionIdleTTL || c.SessionIdleTTL > UploadsMaxSessionIdleTTL:
		err = fmt.Errorf("session_idle_ttl must be between %s and %s, got %s", UploadsMinSessionIdleTTL, UploadsMaxSessionIdleTTL, c.SessionIdleTTL)
	case c.SessionMaxLifetime < c.SessionIdleTTL:
		err = fmt.Errorf("session_max_lifetime (%s) must be at least session_idle_ttl (%s)", c.SessionMaxLifetime, c.SessionIdleTTL)
	case c.SessionMaxLifetime > UploadsMaxSessionMaxLifetime:
		err = fmt.Errorf("session_max_lifetime must be at most %s, got %s", UploadsMaxSessionMaxLifetime, c.SessionMaxLifetime)
	case c.MaxSessionsPerUser < UploadsMinMaxSessionsPerUser || c.MaxSessionsPerUser > UploadsMaxMaxSessionsPerUser:
		err = fmt.Errorf("max_sessions_per_user must be between %d and %d, got %d", UploadsMinMaxSessionsPerUser, UploadsMaxMaxSessionsPerUser, c.MaxSessionsPerUser)
	case c.MaxPendingBytesPerUser < 1:
		err = fmt.Errorf("max_pending_bytes_per_user must be at least 1, got %d", c.MaxPendingBytesPerUser)
	case c.MaxPendingBytesHub < 1:
		err = fmt.Errorf("max_pending_bytes_hub must be at least 1, got %d", c.MaxPendingBytesHub)
	case c.MaxPendingBytesHub < c.MaxPendingBytesPerUser:
		err = fmt.Errorf("max_pending_bytes_hub (%d) must be at least max_pending_bytes_per_user (%d)", c.MaxPendingBytesHub, c.MaxPendingBytesPerUser)
	}
	if err != nil {
		return MalformedUploadsConfig(), fmt.Errorf("uploads settings: %w", err)
	}
	return c, nil
}

// ParseUploadsDoc parses and resolves a stored uploads document. Unknown
// fields, invalid JSON, and out-of-range or cross-field violations return
// MalformedUploadsConfig and an error.
func ParseUploadsDoc(raw json.RawMessage) (UploadsConfig, error) {
	if !json.Valid(raw) {
		return MalformedUploadsConfig(), fmt.Errorf("uploads settings: invalid JSON")
	}
	var doc UploadsSettings
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&doc); err != nil {
		return MalformedUploadsConfig(), fmt.Errorf("uploads settings: %w", err)
	}
	if dec.More() {
		return MalformedUploadsConfig(), fmt.Errorf("uploads settings: trailing data")
	}
	return doc.Resolve()
}
