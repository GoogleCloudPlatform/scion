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

package entadapter

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/gcpserviceaccount"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/githubinstallation"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/predicate"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/useraccesstoken"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// ExternalStore implements the external-identity store sub-interfaces backed by
// Ent: GCP service accounts, GitHub App installations, and user access tokens.
//
// The legacy api_keys table (superseded by user_access_tokens in V34) is
// schematized in Ent for migration fidelity but has no store interface and is
// intentionally not surfaced here.
type ExternalStore struct {
	client *ent.Client
}

// NewExternalStore creates a new Ent-backed ExternalStore.
func NewExternalStore(client *ent.Client) *ExternalStore {
	return &ExternalStore{client: client}
}

// ============================================================================
// GCP Service Accounts
// ============================================================================

// entGCPToStore converts an Ent GCPServiceAccount to the store model.
func entGCPToStore(e *ent.GCPServiceAccount) *store.GCPServiceAccount {
	sa := &store.GCPServiceAccount{
		ID:                 e.ID.String(),
		Scope:              e.Scope,
		ScopeID:            e.ScopeID,
		Email:              e.Email,
		ProjectID:          e.ProjectID,
		DisplayName:        e.DisplayName,
		Verified:           e.Verified,
		VerificationStatus: e.VerificationStatus,
		VerificationError:  e.VerificationError,
		CreatedBy:          e.CreatedBy,
		CreatedAt:          e.Created,
		Managed:            e.Managed,
		ManagedBy:          e.ManagedBy,
	}
	// default_scopes is stored as a CSV string for parity with the SQLite store.
	if e.DefaultScopes != "" {
		sa.DefaultScopes = strings.Split(e.DefaultScopes, ",")
	}
	if e.VerifiedAt != nil {
		sa.VerifiedAt = *e.VerifiedAt
	}
	return sa
}

// normalizeGCPVerification makes the (Verified, VerificationStatus,
// VerificationError) triple coherent before it is persisted. The empty status
// string is not one of the three valid states, so callers that populate only
// the Verified bool still have to end up with a coherent row. Resolving it
// here — once, on write — is what lets reads return the stored value verbatim
// instead of re-deriving it every time.
//
// Verified == true forces "verified" and clears any error. On a well-formed row
// this is a no-op; on a malformed one it is a repair. The case that needs it is
// a read-modify-write of a row already sitting at "failed": flipping Verified to
// true without this would leave status "failed" beside a stale error message,
// and nothing would ever heal it, because both backfill rules require the row to
// still be at "unverified".
//
// Verified == false only fills in a blank status and otherwise leaves the row
// alone, which preserves the verified=false + "failed" + error combination the
// live verification path writes.
//
// The backfill (BackfillGCPVerificationStatus) deliberately does not apply this
// same coherence pass, and the asymmetry is not that the write path is better
// informed in general — for Verified == true both have the same information.
// It is that verified=false with verified_at NULL is irreducibly ambiguous from
// stored state alone: never-checked and failed-its-first-check look identical.
// Only a caller performing the write can tell those apart, so the backfill has
// to leave every verified=false row where it found it.
func normalizeGCPVerification(sa *store.GCPServiceAccount) {
	switch {
	case sa.Verified:
		sa.VerificationStatus = store.GCPVerificationVerified
		sa.VerificationError = ""
	case sa.VerificationStatus == "":
		sa.VerificationStatus = store.GCPVerificationUnverified
	}
}

// CreateGCPServiceAccount registers a new GCP service account.
func (s *ExternalStore) CreateGCPServiceAccount(ctx context.Context, sa *store.GCPServiceAccount) error {
	id, err := parseUUID(sa.ID)
	if err != nil {
		return err
	}
	if sa.CreatedAt.IsZero() {
		sa.CreatedAt = time.Now()
	}
	// Normalise in place so the caller's struct — which handlers return directly
	// in the HTTP response — matches the persisted row.
	normalizeGCPVerification(sa)

	create := s.client.GCPServiceAccount.Create().
		SetID(id).
		SetScope(sa.Scope).
		SetScopeID(sa.ScopeID).
		SetEmail(sa.Email).
		SetProjectID(sa.ProjectID).
		SetDisplayName(sa.DisplayName).
		SetDefaultScopes(strings.Join(sa.DefaultScopes, ",")).
		SetVerified(sa.Verified).
		SetVerificationStatus(sa.VerificationStatus).
		SetVerificationError(sa.VerificationError).
		SetCreatedBy(sa.CreatedBy).
		SetManaged(sa.Managed).
		SetManagedBy(sa.ManagedBy).
		SetCreated(sa.CreatedAt)

	if !sa.VerifiedAt.IsZero() {
		create.SetVerifiedAt(sa.VerifiedAt)
	}

	if _, err := create.Save(ctx); err != nil {
		return mapError(err)
	}
	return nil
}

// GetGCPServiceAccount retrieves a GCP service account by ID.
func (s *ExternalStore) GetGCPServiceAccount(ctx context.Context, id string) (*store.GCPServiceAccount, error) {
	uid, err := parseGetID(id)
	if err != nil {
		return nil, err
	}
	e, err := s.client.GCPServiceAccount.Get(ctx, uid)
	if err != nil {
		return nil, mapError(err)
	}
	return entGCPToStore(e), nil
}

// UpdateGCPServiceAccount updates a GCP service account record.
func (s *ExternalStore) UpdateGCPServiceAccount(ctx context.Context, sa *store.GCPServiceAccount) error {
	id, err := parseUUID(sa.ID)
	if err != nil {
		return err
	}
	normalizeGCPVerification(sa)

	update := s.client.GCPServiceAccount.UpdateOneID(id).
		SetEmail(sa.Email).
		SetProjectID(sa.ProjectID).
		SetDisplayName(sa.DisplayName).
		SetDefaultScopes(strings.Join(sa.DefaultScopes, ",")).
		SetVerified(sa.Verified).
		SetVerificationStatus(sa.VerificationStatus).
		SetVerificationError(sa.VerificationError).
		SetManaged(sa.Managed).
		SetManagedBy(sa.ManagedBy)

	if sa.VerifiedAt.IsZero() {
		update.ClearVerifiedAt()
	} else {
		update.SetVerifiedAt(sa.VerifiedAt)
	}

	if _, err := update.Save(ctx); err != nil {
		return mapError(err)
	}
	return nil
}

// DeleteGCPServiceAccount removes a GCP service account by ID.
func (s *ExternalStore) DeleteGCPServiceAccount(ctx context.Context, id string) error {
	uid, err := parseUUID(id)
	if err != nil {
		return err
	}
	if err := s.client.GCPServiceAccount.DeleteOneID(uid).Exec(ctx); err != nil {
		return mapError(err)
	}
	return nil
}

// BackfillGCPVerificationStatus derives verification_status for rows written
// before that column existed. Ent's ADD COLUMN fills them with the column
// default, "unverified", which is wrong for any account the Hub had actually
// checked.
//
// Two rules, both upgrades from the default:
//
//   - verified = true → "verified".
//   - verified = false and verified_at IS NOT NULL → "failed". verified_at is
//     only ever set together with verified = true, and UpdateGCPServiceAccount
//     clears it when the timestamp is zero, so a surviving timestamp means the
//     account was verified once and no longer is. That is a failure, not an
//     account nobody ever checked.
//
// The second rule is a weak signal — it cannot recover the error text, which is
// gone, and it misses accounts that failed their very first check (those never
// had a verified_at to keep). It is still the right label: "unverified" reads as
// "never set up" and would hide a broken integration, whereas "failed" invites a
// re-verify, which repopulates the real error. Under-detailed beats wrong.
//
// Idempotent, and safe to run on every boot: both rules require the row to still
// be at the default, so a "failed" status written by the live code path is never
// overwritten — including the first-check-failure case, where verified_at is
// NULL and the else-branch of a naive three-way backfill would clobber it back
// to "unverified".
//
// Deliberately not on store.GCPServiceAccountStore. This is a one-off migration
// concern with a single caller (CompositeStore.Migrate, which reaches it through
// the embedded *ExternalStore); putting it on the domain interface would oblige
// every future store implementation to carry it forever.
func (s *ExternalStore) BackfillGCPVerificationStatus(ctx context.Context) error {
	if _, err := s.client.GCPServiceAccount.Update().
		Where(
			gcpserviceaccount.VerificationStatusEQ(store.GCPVerificationUnverified),
			gcpserviceaccount.VerifiedEQ(true),
		).
		SetVerificationStatus(store.GCPVerificationVerified).
		Save(ctx); err != nil {
		return fmt.Errorf("backfill gcp verification status (verified): %w", err)
	}

	if _, err := s.client.GCPServiceAccount.Update().
		Where(
			gcpserviceaccount.VerificationStatusEQ(store.GCPVerificationUnverified),
			gcpserviceaccount.VerifiedEQ(false),
			gcpserviceaccount.VerifiedAtNotNil(),
		).
		SetVerificationStatus(store.GCPVerificationFailed).
		Save(ctx); err != nil {
		return fmt.Errorf("backfill gcp verification status (failed): %w", err)
	}

	return nil
}

// gcpFilterPredicates builds the Ent predicates for a GCPServiceAccountFilter.
func gcpFilterPredicates(filter store.GCPServiceAccountFilter) []predicate.GCPServiceAccount {
	var preds []predicate.GCPServiceAccount

	// Scope and ScopeID are collected separately from the other terms because
	// IncludeHubScoped rewrites their combination rather than adding to it.
	var scopePreds []predicate.GCPServiceAccount
	if filter.Scope != "" {
		scopePreds = append(scopePreds, gcpserviceaccount.ScopeEQ(filter.Scope))
	}
	if filter.ScopeID != "" {
		scopePreds = append(scopePreds, gcpserviceaccount.ScopeIDEQ(filter.ScopeID))
	}

	switch {
	case !filter.IncludeHubScoped, len(scopePreds) == 0:
		// No widening requested, or nothing to widen: with no scope terms the
		// query already spans every scope, and OR-ing hub scope onto it would
		// be a no-op that reads like a restriction.
		preds = append(preds, scopePreds...)
	default:
		// (scope terms) OR (scope = hub), as one predicate, so the union is a
		// single SQL statement and Email/Managed below still AND across the
		// whole of it.
		preds = append(preds, gcpserviceaccount.Or(
			gcpserviceaccount.And(scopePreds...),
			gcpserviceaccount.ScopeEQ(store.ScopeHub),
		))
	}

	if filter.Email != "" {
		preds = append(preds, gcpserviceaccount.EmailEQ(filter.Email))
	}
	if filter.Managed != nil {
		preds = append(preds, gcpserviceaccount.ManagedEQ(*filter.Managed))
	}
	return preds
}

// ListGCPServiceAccounts returns GCP service accounts matching the filter.
func (s *ExternalStore) ListGCPServiceAccounts(ctx context.Context, filter store.GCPServiceAccountFilter) ([]store.GCPServiceAccount, error) {
	rows, err := s.client.GCPServiceAccount.Query().
		Where(gcpFilterPredicates(filter)...).
		Order(gcpserviceaccount.ByCreated(entDesc())).
		All(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]store.GCPServiceAccount, 0, len(rows))
	for _, e := range rows {
		out = append(out, *entGCPToStore(e))
	}
	return out, nil
}

// CountGCPServiceAccounts returns the number of GCP service accounts matching the filter.
func (s *ExternalStore) CountGCPServiceAccounts(ctx context.Context, filter store.GCPServiceAccountFilter) (int, error) {
	return s.client.GCPServiceAccount.Query().
		Where(gcpFilterPredicates(filter)...).
		Count(ctx)
}

// ============================================================================
// GitHub App Installations
// ============================================================================

// marshalRepos serializes the repositories slice to the JSON string stored in
// the dialect-neutral repositories column.
func marshalRepos(repos []string) string {
	if repos == nil {
		repos = []string{}
	}
	b, _ := json.Marshal(repos)
	return string(b)
}

// entGitHubToStore converts an Ent GithubInstallation to the store model.
func entGitHubToStore(e *ent.GithubInstallation) *store.GitHubInstallation {
	inst := &store.GitHubInstallation{
		InstallationID: e.ID,
		AccountLogin:   e.AccountLogin,
		AccountType:    e.AccountType,
		AppID:          e.AppID,
		Status:         e.Status,
		CreatedAt:      e.Created,
		UpdatedAt:      e.Updated,
	}
	if e.Repositories != "" {
		_ = json.Unmarshal([]byte(e.Repositories), &inst.Repositories)
	}
	return inst
}

// CreateGitHubInstallation creates a new GitHub App installation record.
//
// installation_id is the GitHub-provided natural key; mirroring the legacy
// "INSERT OR IGNORE" behavior, creating an installation that already exists is a
// no-op (idempotent) rather than an error.
func (s *ExternalStore) CreateGitHubInstallation(ctx context.Context, installation *store.GitHubInstallation) error {
	// Idempotency guard: if the natural key already exists, do nothing.
	exists, err := s.client.GithubInstallation.Query().
		Where(githubinstallation.IDEQ(installation.InstallationID)).
		Exist(ctx)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}

	if installation.CreatedAt.IsZero() {
		installation.CreatedAt = time.Now()
	}
	if installation.UpdatedAt.IsZero() {
		installation.UpdatedAt = installation.CreatedAt
	}
	if installation.Status == "" {
		installation.Status = store.GitHubInstallationStatusActive
	}
	accountType := installation.AccountType
	if accountType == "" {
		accountType = "Organization"
	}

	err = s.client.GithubInstallation.Create().
		SetID(installation.InstallationID).
		SetAccountLogin(installation.AccountLogin).
		SetAccountType(accountType).
		SetAppID(installation.AppID).
		SetRepositories(marshalRepos(installation.Repositories)).
		SetStatus(installation.Status).
		SetCreated(installation.CreatedAt).
		SetUpdated(installation.UpdatedAt).
		Exec(ctx)
	if err != nil {
		// Another writer may have created it concurrently — stay idempotent.
		if ent.IsConstraintError(err) {
			return nil
		}
		return mapError(err)
	}
	return nil
}

// GetGitHubInstallation retrieves a GitHub App installation by installation ID.
func (s *ExternalStore) GetGitHubInstallation(ctx context.Context, installationID int64) (*store.GitHubInstallation, error) {
	e, err := s.client.GithubInstallation.Get(ctx, installationID)
	if err != nil {
		return nil, mapError(err)
	}
	return entGitHubToStore(e), nil
}

// UpdateGitHubInstallation updates an existing GitHub App installation.
func (s *ExternalStore) UpdateGitHubInstallation(ctx context.Context, installation *store.GitHubInstallation) error {
	installation.UpdatedAt = time.Now()

	_, err := s.client.GithubInstallation.UpdateOneID(installation.InstallationID).
		SetAccountLogin(installation.AccountLogin).
		SetAccountType(installation.AccountType).
		SetAppID(installation.AppID).
		SetRepositories(marshalRepos(installation.Repositories)).
		SetStatus(installation.Status).
		SetUpdated(installation.UpdatedAt).
		Save(ctx)
	if err != nil {
		return mapError(err)
	}
	return nil
}

// DeleteGitHubInstallation removes a GitHub App installation by installation ID.
func (s *ExternalStore) DeleteGitHubInstallation(ctx context.Context, installationID int64) error {
	if err := s.client.GithubInstallation.DeleteOneID(installationID).Exec(ctx); err != nil {
		return mapError(err)
	}
	return nil
}

// ListGitHubInstallations returns all GitHub App installations matching the filter.
func (s *ExternalStore) ListGitHubInstallations(ctx context.Context, filter store.GitHubInstallationFilter) ([]store.GitHubInstallation, error) {
	query := s.client.GithubInstallation.Query()
	if filter.AccountLogin != "" {
		query = query.Where(githubinstallation.AccountLoginEQ(filter.AccountLogin))
	}
	if filter.Status != "" {
		query = query.Where(githubinstallation.StatusEQ(filter.Status))
	}
	if filter.AppID != 0 {
		query = query.Where(githubinstallation.AppIDEQ(filter.AppID))
	}

	rows, err := query.Order(githubinstallation.ByCreated()).All(ctx)
	if err != nil {
		return nil, err
	}

	// Never return a nil slice (parity with the SQLite store).
	results := make([]store.GitHubInstallation, 0, len(rows))
	for _, e := range rows {
		results = append(results, *entGitHubToStore(e))
	}
	return results, nil
}

// GetInstallationForRepository returns an active GitHub App installation that
// covers the given repository (owner/repo format).
func (s *ExternalStore) GetInstallationForRepository(ctx context.Context, repoFullName string) (*store.GitHubInstallation, error) {
	// Scan active installations whose repositories JSON array contains the repo.
	installations, err := s.ListGitHubInstallations(ctx, store.GitHubInstallationFilter{
		Status: store.GitHubInstallationStatusActive,
	})
	if err != nil {
		return nil, err
	}

	for i := range installations {
		for _, repo := range installations[i].Repositories {
			if repo == repoFullName {
				return &installations[i], nil
			}
		}
	}
	return nil, store.ErrNotFound
}

// ============================================================================
// User Access Tokens (UATs)
// ============================================================================

// marshalScopes serializes token scopes to the JSON string stored in the scopes
// column. The column is NotEmpty, so a nil/empty slice serializes to "[]".
func marshalScopes(scopes []string) string {
	if scopes == nil {
		scopes = []string{}
	}
	b, _ := json.Marshal(scopes)
	return string(b)
}

// entUATToStore converts an Ent UserAccessToken to the store model.
func entUATToStore(e *ent.UserAccessToken) *store.UserAccessToken {
	t := &store.UserAccessToken{
		ID:        e.ID.String(),
		UserID:    e.UserID.String(),
		Name:      e.Name,
		Prefix:    e.Prefix,
		KeyHash:   e.KeyHash,
		ProjectID: e.ProjectID.String(),
		Revoked:   e.Revoked,
		Created:   e.Created,
	}
	if e.Scopes != "" {
		_ = json.Unmarshal([]byte(e.Scopes), &t.Scopes)
	}
	if e.ExpiresAt != nil {
		t.ExpiresAt = e.ExpiresAt
	}
	if e.LastUsed != nil {
		t.LastUsed = e.LastUsed
	}
	return t
}

// CreateUserAccessToken creates a new user access token record.
func (s *ExternalStore) CreateUserAccessToken(ctx context.Context, token *store.UserAccessToken) error {
	id, err := parseUUID(token.ID)
	if err != nil {
		return err
	}
	userUID, err := parseUUID(token.UserID)
	if err != nil {
		return err
	}
	projectUID, err := parseUUID(token.ProjectID)
	if err != nil {
		return err
	}

	if token.Created.IsZero() {
		token.Created = time.Now()
	}

	create := s.client.UserAccessToken.Create().
		SetID(id).
		SetUserID(userUID).
		SetName(token.Name).
		SetPrefix(token.Prefix).
		SetKeyHash(token.KeyHash).
		SetProjectID(projectUID).
		SetScopes(marshalScopes(token.Scopes)).
		SetRevoked(token.Revoked).
		SetCreated(token.Created)

	if token.ExpiresAt != nil {
		create.SetExpiresAt(*token.ExpiresAt)
	}
	if token.LastUsed != nil {
		create.SetLastUsed(*token.LastUsed)
	}

	if _, err := create.Save(ctx); err != nil {
		return mapError(err)
	}
	return nil
}

// GetUserAccessToken retrieves a user access token by ID.
func (s *ExternalStore) GetUserAccessToken(ctx context.Context, id string) (*store.UserAccessToken, error) {
	uid, err := parseGetID(id)
	if err != nil {
		return nil, err
	}
	e, err := s.client.UserAccessToken.Get(ctx, uid)
	if err != nil {
		return nil, mapError(err)
	}
	return entUATToStore(e), nil
}

// GetUserAccessTokenByHash retrieves a user access token by its key hash.
func (s *ExternalStore) GetUserAccessTokenByHash(ctx context.Context, hash string) (*store.UserAccessToken, error) {
	e, err := s.client.UserAccessToken.Query().
		Where(useraccesstoken.KeyHashEQ(hash)).
		Only(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	return entUATToStore(e), nil
}

// UpdateUserAccessTokenLastUsed updates the last used timestamp.
// Mirrors the SQLite store: a missing token is not treated as an error.
func (s *ExternalStore) UpdateUserAccessTokenLastUsed(ctx context.Context, id string) error {
	uid, err := parseUUID(id)
	if err != nil {
		return err
	}
	_, err = s.client.UserAccessToken.Update().
		Where(useraccesstoken.IDEQ(uid)).
		SetLastUsed(time.Now()).
		Save(ctx)
	return err
}

// RevokeUserAccessToken marks a token as revoked.
func (s *ExternalStore) RevokeUserAccessToken(ctx context.Context, id string) error {
	uid, err := parseUUID(id)
	if err != nil {
		return err
	}
	if _, err := s.client.UserAccessToken.UpdateOneID(uid).SetRevoked(true).Save(ctx); err != nil {
		return mapError(err)
	}
	return nil
}

// DeleteUserAccessToken permanently removes a token by ID.
func (s *ExternalStore) DeleteUserAccessToken(ctx context.Context, id string) error {
	uid, err := parseUUID(id)
	if err != nil {
		return err
	}
	if err := s.client.UserAccessToken.DeleteOneID(uid).Exec(ctx); err != nil {
		return mapError(err)
	}
	return nil
}

// ListUserAccessTokens returns all tokens for a user, newest first.
func (s *ExternalStore) ListUserAccessTokens(ctx context.Context, userID string) ([]store.UserAccessToken, error) {
	uid, err := parseUUID(userID)
	if err != nil {
		return nil, err
	}
	rows, err := s.client.UserAccessToken.Query().
		Where(useraccesstoken.UserIDEQ(uid)).
		Order(useraccesstoken.ByCreated(entDesc())).
		All(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]store.UserAccessToken, 0, len(rows))
	for _, e := range rows {
		out = append(out, *entUATToStore(e))
	}
	return out, nil
}

// CountUserAccessTokens returns the number of active (non-revoked) tokens for a user.
func (s *ExternalStore) CountUserAccessTokens(ctx context.Context, userID string) (int, error) {
	uid, err := parseUUID(userID)
	if err != nil {
		return 0, err
	}
	return s.client.UserAccessToken.Query().
		Where(
			useraccesstoken.UserIDEQ(uid),
			useraccesstoken.RevokedEQ(false),
		).
		Count(ctx)
}

// Ensure ExternalStore satisfies the external-identity store sub-interfaces.
var (
	_ store.GCPServiceAccountStore  = (*ExternalStore)(nil)
	_ store.GitHubInstallationStore = (*ExternalStore)(nil)
	_ store.UserAccessTokenStore    = (*ExternalStore)(nil)
)
