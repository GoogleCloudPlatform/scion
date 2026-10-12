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
	"errors"
	"fmt"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/predicate"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/user"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
)

// notFederated selects every user row except federated accounts. Every
// email-keyed user lookup and the general user list apply it, so a
// federated account is reachable only through the federated account path.
func notFederated() predicate.User {
	return user.KindNEQ(user.KindFederated)
}

// isFederatedUser reports whether the user id names a federated account.
// A missing user reports false with no error.
func isFederatedUser(ctx context.Context, client *ent.Client, id uuid.UUID) (bool, error) {
	ok, err := client.User.Query().
		Where(user.IDEQ(id), user.KindEQ(user.KindFederated)).
		Exist(ctx)
	if err != nil {
		return false, mapError(err)
	}
	return ok, nil
}

// validateFederatedUser checks and normalizes the fields CreateFederatedUser
// requires (see store.Store.CreateFederatedUser).
func validateFederatedUser(u *store.User) error {
	if u.Kind != store.UserKindFederated {
		return fmt.Errorf("%w: kind must be %s", store.ErrInvalidInput, store.UserKindFederated)
	}
	if u.Status != store.UserStatusPending && u.Status != store.UserStatusActive {
		return fmt.Errorf("%w: a federated account must be %s or %s", store.ErrInvalidInput, store.UserStatusPending, store.UserStatusActive)
	}
	if u.Role != "" && u.Role != store.UserRoleViewer {
		return fmt.Errorf("%w: a federated account has the %s role", store.ErrInvalidInput, store.UserRoleViewer)
	}
	if u.Email != "" {
		return fmt.Errorf("%w: a federated account's email is assigned by the store", store.ErrInvalidInput)
	}
	if u.ExpiresAt != nil || u.IssuedBy != nil || u.Purpose != nil || u.IssuanceID != nil ||
		u.IssuerCredentialID != nil || u.InvitedBy != nil || u.InviteNote != nil {
		return fmt.Errorf("%w: test identity and invite fields are not set on a federated account", store.ErrInvalidInput)
	}
	if u.HomeIssuer == nil || store.NormalizeFederatedIssuer(*u.HomeIssuer) == "" {
		return fmt.Errorf("%w: a federated account requires a home issuer", store.ErrInvalidInput)
	}
	if u.FederatedEmail == nil || !strings.Contains(store.NormalizeFederatedEmail(*u.FederatedEmail), "@") {
		return fmt.Errorf("%w: a federated account requires an email", store.ErrInvalidInput)
	}
	issuer := store.NormalizeFederatedIssuer(*u.HomeIssuer)
	email := store.NormalizeFederatedEmail(*u.FederatedEmail)
	u.HomeIssuer = &issuer
	u.FederatedEmail = &email
	u.Role = store.UserRoleViewer
	return nil
}

// CreateFederatedUser creates a federated account (see
// store.Store.CreateFederatedUser).
func (c *CompositeStore) CreateFederatedUser(ctx context.Context, u *store.User) error {
	if u == nil {
		return fmt.Errorf("%w: user is required", store.ErrInvalidInput)
	}
	if err := validateFederatedUser(u); err != nil {
		return err
	}
	if u.ID == "" {
		u.ID = uuid.NewString()
	}
	uid, err := uuid.Parse(u.ID)
	if err != nil {
		return fmt.Errorf("%w: invalid user ID %q", store.ErrInvalidInput, u.ID)
	}
	u.ID = uid.String()
	email, err := store.FederatedSyntheticEmail(u.ID, *u.HomeIssuer)
	if err != nil {
		return err
	}
	u.Email = email
	return c.UserStore.createUser(ctx, u, func(create *ent.UserCreate) {
		create.SetKind(user.KindFederated).
			SetHomeIssuer(*u.HomeIssuer).
			SetFederatedEmail(*u.FederatedEmail)
	})
}

// GetFederatedUserByEmail returns the federated account of (homeIssuer,
// federatedEmail) (see store.Store.GetFederatedUserByEmail).
func (c *CompositeStore) GetFederatedUserByEmail(ctx context.Context, homeIssuer, federatedEmail string) (*store.User, error) {
	issuer := store.NormalizeFederatedIssuer(homeIssuer)
	email := store.NormalizeFederatedEmail(federatedEmail)
	if issuer == "" || email == "" {
		return nil, store.ErrNotFound
	}
	u, err := c.client.User.Query().
		Where(
			user.KindEQ(user.KindFederated),
			user.HomeIssuerEQ(issuer),
			user.FederatedEmailEQ(email),
		).
		Only(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	return entUserToStore(u), nil
}

// LinkFederatedIdentity links (issuer, subject) to the federated account
// userID (see store.Store.LinkFederatedIdentity).
//
// The reads below give clear errors for the common cases. The partial
// unique index externalidentity_user_id_federation is what holds "one
// federation link per account" when two links of one account run
// concurrently: the later insert fails and returns ErrAlreadyExists.
func (c *CompositeStore) LinkFederatedIdentity(ctx context.Context, userID, issuer, subject, email string) (*store.ExternalIdentityBinding, error) {
	issuer = store.NormalizeFederatedIssuer(issuer)
	if issuer == "" || strings.TrimSpace(subject) == "" {
		return nil, fmt.Errorf("%w: an issuer and a subject are required", store.ErrInvalidInput)
	}
	var created *store.ExternalIdentityBinding
	err := c.WithTx(ctx, func(tx store.Store) error {
		txc, ok := tx.(*CompositeStore)
		if !ok {
			return fmt.Errorf("link federated identity: unexpected transactional store %T", tx)
		}
		u, err := txc.GetUser(ctx, userID)
		if err != nil {
			return err
		}
		if !u.IsFederated() {
			return fmt.Errorf("%w: user %s is not a federated account", store.ErrInvalidInput, userID)
		}
		if u.HomeIssuer == nil || *u.HomeIssuer != issuer {
			return fmt.Errorf("%w: issuer does not match the account's home issuer", store.ErrInvalidInput)
		}
		existing, err := txc.GetExternalIdentitiesByUserID(ctx, u.ID)
		if err != nil {
			return err
		}
		for _, b := range existing {
			if b.Provider == store.FederationIdentityProvider {
				return fmt.Errorf("%w: the federated account is already linked", store.ErrInvalidInput)
			}
		}
		if _, err := txc.GetExternalIdentity(ctx, store.FederationIdentityProvider, issuer, subject); err == nil {
			return store.ErrAlreadyExists
		} else if !errors.Is(err, store.ErrNotFound) {
			return err
		}
		binding := &store.ExternalIdentityBinding{
			ID:       uuid.NewString(),
			Provider: store.FederationIdentityProvider,
			Issuer:   issuer,
			Subject:  subject,
			UserID:   u.ID,
			Email:    store.NormalizeFederatedEmail(email),
		}
		if err := txc.insertFederationBinding(ctx, binding); err != nil {
			return err
		}
		created, err = txc.GetExternalIdentity(ctx, store.FederationIdentityProvider, issuer, subject)
		return err
	})
	if err != nil {
		return nil, err
	}
	return created, nil
}

// insertFederationBinding writes a federation binding. A unique violation,
// either on (provider, issuer, subject) or on the account's single
// federation binding, returns ErrAlreadyExists.
func (c *CompositeStore) insertFederationBinding(ctx context.Context, binding *store.ExternalIdentityBinding) error {
	if err := c.CreateExternalIdentity(ctx, binding); err != nil {
		if isUniqueViolation(err) {
			return store.ErrAlreadyExists
		}
		return err
	}
	return nil
}

// GetFederatedUserBySubject resolves (issuer, subject) to its federated
// account (see store.Store.GetFederatedUserBySubject).
func (c *CompositeStore) GetFederatedUserBySubject(ctx context.Context, issuer, subject string) (*store.User, error) {
	issuer = store.NormalizeFederatedIssuer(issuer)
	if issuer == "" || strings.TrimSpace(subject) == "" {
		return nil, store.ErrNotFound
	}
	binding, err := c.GetExternalIdentity(ctx, store.FederationIdentityProvider, issuer, subject)
	if err != nil {
		return nil, err
	}
	u, err := c.GetUser(ctx, binding.UserID)
	if err != nil {
		return nil, err
	}
	if !u.IsFederated() {
		return nil, store.ErrNotFound
	}
	return u, nil
}
