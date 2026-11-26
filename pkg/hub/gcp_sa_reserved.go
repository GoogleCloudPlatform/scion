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
	"net/http"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/brokerownership"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

const (
	// gcpSAHubIdentityFlagError is persisted on existing service account rows that
	// match the Hub's own GCP service account identity when flagged by the startup
	// scan, and surfaced on status reads.
	gcpSAHubIdentityFlagError = "reserved: matches the Hub's own GCP service account identity and cannot be used by agents"

	// gcpSABrokerHostIdentityFlagError is persisted on existing service account
	// rows that match a Runtime Broker's host GCP service account identity when
	// flagged by the startup scan, and surfaced on status reads.
	gcpSABrokerHostIdentityFlagError = "reserved: matches a Runtime Broker's host GCP service account identity and cannot be used by agents"

	gcpSAHubIdentityRefusalMsg        = "the Hub's own GCP service account cannot be registered or used as an agent identity"
	gcpSABrokerHostIdentityRefusalMsg = "a Runtime Broker's host GCP service account cannot be registered or used as an agent identity"

	// reservedSAListPageSize is the page size used when listing Runtime Brokers
	// to check broker host service accounts.
	reservedSAListPageSize = 200
)

// isValidGCPServiceAccountEmail reports whether s has the shape of a GCP
// service account email address: no whitespace, no slashes, and exactly one
// "@" separating a non-empty local part and domain.
func isValidGCPServiceAccountEmail(s string) bool {
	if s == "" || strings.ContainsAny(s, " \t\r\n/") {
		return false
	}
	at := strings.IndexByte(s, '@')
	if at <= 0 || at == len(s)-1 {
		return false
	}
	return strings.IndexByte(s[at+1:], '@') < 0
}

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

// normalizeGCPSAEmail returns a lower-cased, whitespace-trimmed email for
// case-insensitive comparison and set membership checks.
func normalizeGCPSAEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

// isEmailInSet reports whether the normalized email is present in set.
func isEmailInSet(set map[string]struct{}, email string) bool {
	if len(set) == 0 {
		return false
	}
	norm := normalizeGCPSAEmail(email)
	if norm == "" {
		return false
	}
	_, ok := set[norm]
	return ok
}

// brokerConfiguredHostSAAuthorityBacked reports whether a Runtime Broker's
// configured GCPHostServiceAccountEmail carries platform or IAM-verified
// authority and should be reserved hub-wide.
//
// HostObservation.Email is always reserved (it is written only by the Hub from
// GCE metadata). A broker's configured GCPHostServiceAccountEmail is reserved
// only when:
//   - the broker is admin-managed (AdminManaged, gated by broker.manage),
//   - the broker is an embedded broker (hosted by this server or carrying the
//     Hub-reserved embedded role label),
//   - the configured email was server-detected (GCPHostSource == "detected",
//     e.g. embedded flat Cloud Run runtime_target.service_account), or
//   - the Hub's gcpIamCheckMode is enforce (so evaluateHostSAClaim requires
//     GCP iam.serviceAccounts.actAs on the claimed email).
//
// Under gcpIamCheckMode=off, an unprivileged hub-member's self-registered
// non-AdminManaged broker does not reserve its configured email hub-wide,
// preventing cross-tenant DoS against other projects' service accounts.
func brokerConfiguredHostSAAuthorityBacked(b *store.RuntimeBroker, embedded embeddedBrokerState, enforceMode bool) bool {
	if b == nil {
		return false
	}
	if b.AdminManaged || b.GCPHostSource == store.GCPHostSourceDetected || enforceMode {
		return true
	}
	if embedded.has(b.ID) {
		return true
	}
	return b.Labels[store.BrokerRoleLabel] == brokerownership.BrokerRoleEmbedded
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

// forEachReservedBrokerHostEmail paginates all Runtime Brokers without holding
// s.mu across store calls and invokes fn for each normalized (lower-cased,
// trimmed) reserved broker host GCP service account email. Stops early when
// fn returns false.
func (s *Server) forEachReservedBrokerHostEmail(ctx context.Context, fn func(email string) bool) error {
	if s.store == nil {
		return nil
	}
	embedded := s.embeddedBrokerSnapshot()
	enforceMode := s.saAssignCheckModeValue() == SAAssignCheckEnforce
	cursor := ""
	for {
		page, err := s.store.ListRuntimeBrokers(ctx, store.RuntimeBrokerFilter{}, store.ListOptions{
			Limit:          reservedSAListPageSize,
			Cursor:         cursor,
			SkipTotalCount: true,
		})
		if err != nil {
			return fmt.Errorf("list runtime brokers: %w", err)
		}
		for i := range page.Items {
			b := &page.Items[i]
			if b.HostObservation != nil {
				if observed := normalizeGCPSAEmail(b.HostObservation.Email); observed != "" {
					if !fn(observed) {
						return nil
					}
				}
			}
			if brokerConfiguredHostSAAuthorityBacked(b, embedded, enforceMode) {
				if configured := normalizeGCPSAEmail(b.GCPHostServiceAccountEmail); configured != "" {
					if !fn(configured) {
						return nil
					}
				}
			}
		}
		if page.NextCursor == "" || page.NextCursor == cursor {
			return nil
		}
		cursor = page.NextCursor
	}
}

// isBrokerHostServiceAccountEmail reports whether email matches any reserved
// Runtime Broker host GCP service account (case-insensitive, trimmed): any
// observed HostObservation.Email, or any authority-backed broker's configured
// GCPHostServiceAccountEmail.
func (s *Server) isBrokerHostServiceAccountEmail(ctx context.Context, email string) (bool, error) {
	norm := normalizeGCPSAEmail(email)
	if norm == "" || s.store == nil {
		return false, nil
	}
	matched := false
	err := s.forEachReservedBrokerHostEmail(ctx, func(reserved string) bool {
		if reserved == norm {
			matched = true
			return false
		}
		return true
	})
	if err != nil {
		return false, err
	}
	return matched, nil
}

// brokerHostServiceAccountEmailSet returns the set of normalized (lower-case,
// trimmed) reserved host GCP service account emails across all Runtime Brokers.
// Callers checking many service accounts (such as ScanReservedGCPServiceAccounts
// and project clone) call this once to avoid N+1 store queries. Does not hold
// s.mu across store calls.
func (s *Server) brokerHostServiceAccountEmailSet(ctx context.Context) (map[string]struct{}, error) {
	if s.store == nil {
		return nil, nil
	}
	emails := make(map[string]struct{})
	err := s.forEachReservedBrokerHostEmail(ctx, func(reserved string) bool {
		emails[reserved] = struct{}{}
		return true
	})
	if err != nil {
		return nil, err
	}
	return emails, nil
}

// gcpServiceAccountEmailRefusal reports whether email is a reserved identity
// that may not be registered, verified, assigned, or used for agent or
// lifecycle-hook token minting. This covers the Hub's own GCP service account,
// any observed Runtime Broker host GCP service account, and any
// authority-backed Runtime Broker's configured host GCP service account.
//
// Returns ("", "", false, nil) when email is allowed, including when no Hub
// identity or broker host identities are known. Returns a non-nil error if
// Runtime Brokers cannot be queried so callers fail closed.
func (s *Server) gcpServiceAccountEmailRefusal(ctx context.Context, email string) (code, msg string, refused bool, err error) {
	if s.isHubOwnServiceAccountEmail(email) {
		return ErrCodeIdentityReserved, gcpSAHubIdentityRefusalMsg, true, nil
	}
	isBrokerHost, err := s.isBrokerHostServiceAccountEmail(ctx, email)
	if err != nil {
		return "", "", false, err
	}
	if isBrokerHost {
		return ErrCodeIdentityReserved, gcpSABrokerHostIdentityRefusalMsg, true, nil
	}
	return "", "", false, nil
}

// gcpServiceAccountRefusal is the row-aware counterpart of
// gcpServiceAccountEmailRefusal for callers that already hold a stored
// GCPServiceAccount row. It always refuses the Hub's own GCP service account,
// and refuses any reserved Runtime Broker host service account except on
// Hub-created passthrough translation records
// (store.IsHostSATranslationRecord), whose email equals their bound broker's
// host SA and which carry their own dedicated refusals on direct-reference
// surfaces.
func (s *Server) gcpServiceAccountRefusal(ctx context.Context, sa *store.GCPServiceAccount) (code, msg string, refused bool, err error) {
	if sa == nil {
		return "", "", false, nil
	}
	if s.isHubOwnServiceAccountEmail(sa.Email) {
		return ErrCodeIdentityReserved, gcpSAHubIdentityRefusalMsg, true, nil
	}
	if store.IsHostSATranslationRecord(sa) {
		return "", "", false, nil
	}
	isBrokerHost, err := s.isBrokerHostServiceAccountEmail(ctx, sa.Email)
	if err != nil {
		return "", "", false, err
	}
	if isBrokerHost {
		return ErrCodeIdentityReserved, gcpSABrokerHostIdentityRefusalMsg, true, nil
	}
	return "", "", false, nil
}

// refuseReservedHookExecutionIdentity refuses a lifecycle-hook write whose
// executionIdentity references a reserved GCP service account (the Hub's own
// identity or a Runtime Broker's host identity). Returns true after writing
// the 400 identity_reserved response (or error response on store failure);
// lookup errors on the SA itself are left to lifecyclehooks.ValidateHook.
func (s *Server) refuseReservedHookExecutionIdentity(w http.ResponseWriter, ctx context.Context, executionIdentity string) bool {
	if executionIdentity == "" {
		return false
	}
	sa, err := s.store.GetGCPServiceAccount(ctx, executionIdentity)
	if err != nil || sa == nil {
		return false
	}
	code, msg, refused, err := s.gcpServiceAccountRefusal(ctx, sa)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return true
	}
	if refused {
		writeError(w, http.StatusBadRequest, code, msg, nil)
		return true
	}
	return false
}

// ScanReservedGCPServiceAccounts scans all stored GCP service account rows at
// startup and flags any row whose email matches the Hub's own GCP service
// account identity or (for non-passthrough-translation rows) any Runtime
// Broker's configured or observed host GCP service account identity. Flagged
// rows have Verified set to false, VerificationStatus set to "failed", and
// VerificationError set to the matching reserved flag error so they are no
// longer served and their status is visible to admins through existing list,
// detail, and status surfaces.
//
// Broker host service accounts are loaded once upfront to avoid N+1 store
// calls, and no server lock is held across store calls. When neither the Hub's
// own identity nor any broker host identity is known, this returns (0, nil)
// without querying or modifying GCP service account rows.
func (s *Server) ScanReservedGCPServiceAccounts(ctx context.Context) (int, error) {
	hubKnown := strings.TrimSpace(s.hubSAEmail()) != ""
	brokerHostEmails, err := s.brokerHostServiceAccountEmailSet(ctx)
	if err != nil {
		slog.Warn("startup scan for reserved GCP service accounts failed to list runtime brokers", "error", err)
		return 0, fmt.Errorf("list runtime brokers: %w", err)
	}
	if !hubKnown && len(brokerHostEmails) == 0 {
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
		var flagErr, reason string
		switch {
		case s.isHubOwnServiceAccountEmail(sa.Email):
			flagErr = gcpSAHubIdentityFlagError
			reason = "hub_own_identity"
		case !store.IsHostSATranslationRecord(&sa) && isEmailInSet(brokerHostEmails, sa.Email):
			flagErr = gcpSABrokerHostIdentityFlagError
			reason = "broker_host_identity"
		default:
			continue
		}
		flagged++
		slog.Warn("startup scan found registered GCP service account matching reserved identity",
			"sa_id", sa.ID,
			"scope", sa.Scope,
			"scope_id", sa.ScopeID,
			"email", sa.Email,
			"reason", reason,
		)
		if !sa.Verified && sa.VerificationStatus == store.GCPVerificationFailed && sa.VerificationError == flagErr {
			continue
		}
		sa.Verified = false
		sa.VerificationStatus = store.GCPVerificationFailed
		sa.VerificationError = flagErr
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
