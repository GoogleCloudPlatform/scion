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

package hub

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// gcpSAHubIdentityFlagError is persisted on existing service account rows that
// match the Hub's own GCP service account identity when flagged by the startup
// scan, and surfaced on status reads.
const gcpSAHubIdentityFlagError = "reserved: matches the Hub's own GCP service account identity and cannot be used by agents"

// gcpServiceAccountEmailsEqual reports whether two GCP service account emails
// refer to the same identity (case-insensitive, surrounding whitespace trimmed).
// Returns false when either email is empty after trimming.
func gcpServiceAccountEmailsEqual(a, b string) bool {
	na := strings.TrimSpace(a)
	nb := strings.TrimSpace(b)
	if na == "" || nb == "" {
		return false
	}
	return strings.EqualFold(na, nb)
}

// isHubOwnServiceAccountEmail reports whether email matches the Hub's own GCP
// service account identity (case-insensitive, trimmed).
//
// When the Hub's own identity is unknown (for example when no GCP token
// generator is configured or the Hub is not running on GCP and its metadata
// email is empty), this returns false so non-GCP deployments are unaffected.
func (s *Server) isHubOwnServiceAccountEmail(email string) bool {
	return gcpServiceAccountEmailsEqual(email, s.hubSAEmail())
}

// gcpServiceAccountEmailRefusal reports whether email is a reserved identity
// that may not be registered, verified, assigned, or used for agent token
// minting. Currently this covers the Hub's own GCP service account; additional
// reserved identities (such as broker-host accounts) can be checked here.
//
// Returns ("", "", false) when email is allowed, including when the Hub's own
// identity is unknown.
func (s *Server) gcpServiceAccountEmailRefusal(email string) (code, msg string, refused bool) {
	if s.isHubOwnServiceAccountEmail(email) {
		return ErrCodeIdentityReserved,
			"the Hub's own GCP service account cannot be registered or used as an agent identity",
			true
	}
	return "", "", false
}

// ScanReservedGCPServiceAccounts scans all stored GCP service account rows at
// startup and flags any row whose email matches the Hub's own GCP service
// account identity. Flagged rows have Verified set to false,
// VerificationStatus set to "failed", and VerificationError set to
// gcpSAHubIdentityFlagError so they are no longer served and their status is
// visible to admins through existing list, detail, and status surfaces.
//
// When the Hub's own identity is unknown (not running on GCP or no token
// generator configured), this returns (0, nil) without querying or modifying
// the store.
func (s *Server) ScanReservedGCPServiceAccounts(ctx context.Context) (int, error) {
	if strings.TrimSpace(s.hubSAEmail()) == "" {
		return 0, nil
	}

	all, err := s.store.ListGCPServiceAccounts(ctx, store.GCPServiceAccountFilter{})
	if err != nil {
		slog.Warn("startup scan for reserved GCP service accounts failed to list accounts", "error", err)
		return 0, fmt.Errorf("list GCP service accounts: %w", err)
	}

	flagged := 0
	var firstErr error
	for i := range all {
		sa := all[i]
		if !s.isHubOwnServiceAccountEmail(sa.Email) {
			continue
		}
		flagged++
		slog.Warn("startup scan found registered GCP service account matching Hub's own identity",
			"sa_id", sa.ID,
			"scope", sa.Scope,
			"scope_id", sa.ScopeID,
			"email", sa.Email,
		)
		if !sa.Verified && sa.VerificationStatus == store.GCPVerificationFailed && sa.VerificationError == gcpSAHubIdentityFlagError {
			continue
		}
		sa.Verified = false
		sa.VerificationStatus = store.GCPVerificationFailed
		sa.VerificationError = gcpSAHubIdentityFlagError
		if err := s.store.UpdateGCPServiceAccount(ctx, &sa); err != nil {
			slog.Warn("startup scan failed to flag reserved GCP service account",
				"sa_id", sa.ID,
				"scope", sa.Scope,
				"scope_id", sa.ScopeID,
				"error", err,
			)
			if firstErr == nil {
				firstErr = fmt.Errorf("flag reserved GCP service account %s: %w", sa.ID, err)
			}
		}
	}

	return flagged, firstErr
}
