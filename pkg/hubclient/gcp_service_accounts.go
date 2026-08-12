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

package hubclient

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// GCP service account operations.
//
// THIS SERVICE IS NOT PINNED TO A PROJECT, and that is the substance of P5
// item A. It used to be: Client.GCPServiceAccounts(projectID) handed back a
// service that could only ever address one project's accounts. Hub-scoped
// accounts belong to no project, so under that shape they were unreachable
// from this client at any address -- not merely inconvenient to reach. The
// accessor is now no-arg, matching Client.Templates() and
// Client.HarnessConfigs(), and scope travels with the individual call.
//
// The scope vocabulary is store's, not a private copy: store.ScopeHub is the
// string "hub" and is deliberately NOT normalised to the templates' "global".
// Two vocabularies that mean different things stay spelled differently rather
// than being papered over in a client. The constants are imported rather than
// redeclared for the reason #33 exists: a private copy of a shared contract
// cannot be repaired by fixing the shared one.

// GCPServiceAccountService handles GCP service account operations at any scope.
type GCPServiceAccountService interface {
	// List returns the service accounts selected by opts. opts is required;
	// there is no default scope.
	List(ctx context.Context, opts *ListGCPServiceAccountsOptions) ([]GCPServiceAccount, error)

	// Get returns a single service account.
	Get(ctx context.Context, ref GCPServiceAccountRef) (*GCPServiceAccount, error)

	// Create registers an existing GCP service account at the scope named in
	// the request.
	Create(ctx context.Context, req *CreateGCPServiceAccountRequest) (*GCPServiceAccount, error)

	// Delete removes a service account registration.
	Delete(ctx context.Context, ref GCPServiceAccountRef) error

	// Verify re-runs the Hub's impersonation check against the account.
	Verify(ctx context.Context, ref GCPServiceAccountRef) (*GCPServiceAccount, error)

	// Mint creates a new GCP service account in the Hub's GCP project, against
	// the named project's mint quota. Project-scoped by nature: projectID is a
	// required positional argument rather than an option, because there is no
	// hub-scoped form of this operation to select between.
	Mint(ctx context.Context, projectID string, req *MintGCPServiceAccountRequest) (*GCPServiceAccount, error)
}

// GCPServiceAccountCapabilities reports the actions the calling identity may
// perform on a service account, as computed by the Hub.
//
// CALLERS THAT RENDER AFFORDANCES MUST USE THIS AND NOT EXISTENCE. A hub-scoped
// account is listable by every authenticated user but manageable by very few,
// so a UI or CLI that offers Delete for every account it can see offers an
// action that fails for the common caller rather than the rare one.
type GCPServiceAccountCapabilities struct {
	Actions []string `json:"actions"`
}

// Can reports whether action is permitted. Nil-safe: an account decoded from a
// response that carried no capabilities answers false for everything, which is
// the fail-closed direction -- absent information must not read as permission.
func (c *GCPServiceAccountCapabilities) Can(action string) bool {
	if c == nil {
		return false
	}
	for _, a := range c.Actions {
		if a == action {
			return true
		}
	}
	return false
}

// GCPServiceAccount represents a registered GCP service account.
type GCPServiceAccount struct {
	ID    string `json:"id"`
	Scope string `json:"scope"`

	// ScopeID is the SCION project that owns this registration (project scope),
	// or the hub instance that registered it (hub scope). It is the ID that
	// appears in the Hub's routes.
	ScopeID string `json:"scopeId"`

	Email string `json:"email"`

	// ProjectID is the GCP project the service account itself lives in. IT IS
	// NOT A ROUTE COMPONENT and never belongs in ProjectScopedRef -- see the
	// GCPServiceAccountRef doc, and use Ref() instead of choosing.
	ProjectID string `json:"projectId"`

	DisplayName        string    `json:"displayName"`
	DefaultScopes      []string  `json:"defaultScopes,omitempty"`
	Verified           bool      `json:"verified"`
	VerifiedAt         time.Time `json:"verifiedAt,omitempty"`
	VerificationStatus string    `json:"verificationStatus,omitempty"`
	VerificationError  string    `json:"verificationError,omitempty"`
	CreatedBy          string    `json:"createdBy"`
	CreatedAt          time.Time `json:"createdAt"`
	Managed            bool      `json:"managed"`
	ManagedBy          string    `json:"managedBy,omitempty"`

	// Capabilities is what the calling identity may do with this account. The
	// Hub sends it on list and flat by-id reads; it is absent on responses
	// from surfaces that do not compute it, so treat nil as "unknown", which
	// Can() already renders as "no".
	Capabilities *GCPServiceAccountCapabilities `json:"_capabilities,omitempty"`
}

// IsHubScoped reports whether the account belongs to the hub rather than to a
// project or a user.
//
// Scope ALONE decides this, and no caller should compare ScopeID against a hub
// ID to answer it. On a hub-scoped account ScopeID records which hub instance
// registered the account -- it is provenance, not a predicate -- and the hub ID
// derives from config or a hostname hash, so it is not stable across a
// redeploy. A check keyed on it would silently stop recognising every
// hub-scoped account the first time a hostname changed.
func (sa *GCPServiceAccount) IsHubScoped() bool {
	return sa != nil && sa.Scope == store.ScopeHub
}

// GCPServiceAccountRef names one account for a by-id operation.
//
// THE PROJECT ID SELECTS AN ADDRESS, NOT A PERMISSION. The Hub exposes two
// by-id surfaces and they serve disjoint sets:
//
//   - /api/v1/projects/{pid}/gcp-service-accounts/{id} -- a project's own
//     accounts, reached by naming the project.
//   - /api/v1/gcp-service-accounts/{id} -- PARENTLESS accounts, hub and user
//     scope. A project-scoped account 404s there, deliberately.
//
// ITS FIELDS ARE UNEXPORTED ON PURPOSE. Build one with Ref(), HubScopedRef or
// ProjectScopedRef; outside this package the struct literal does not compile.
// That is not tidiness, it is closing two traps that both surface as a 404
// reading like a missing record:
//
//   - THE ZERO VALUE. A forgotten project and a deliberate hub-scoped ref are
//     byte-identical structs, so the careless call and the correct one are
//     indistinguishable to a reader and to the compiler.
//
//   - THE POPULATED WRONG VALUE, which is worse because it punishes care rather
//     than carelessness (sa-arch). GCPServiceAccount carries TWO project-ish
//     fields: ScopeID, the Scion project that owns the registration, and
//     ProjectID, the GCP project the service account itself lives in. So
//
//     ProjectScopedRef(sa.ProjectID, sa.ID)
//
//     compiles, reads correctly, matches the parameter by name -- and puts a
//     GCP project ID into a Scion route. The field whose name matches is the
//     wrong one.
//
// Both traps disappear on the dominant path, which is "I got this account from
// a list": call sa.Ref() and pick nothing at all.
type GCPServiceAccountRef struct {
	id string

	// projectID selects the project-nested address. Empty means the flat,
	// parentless address.
	projectID string
}

// Ref returns the address of this account, chosen from its own scope.
//
// THE ONE TO REACH FOR. Every by-id operation on an account that came back from
// the Hub should route through here, because the account already knows which of
// the two surfaces serves it and the caller then cannot confuse ScopeID with
// ProjectID.
func (sa *GCPServiceAccount) Ref() GCPServiceAccountRef {
	if sa == nil {
		return GCPServiceAccountRef{}
	}

	// ScopeID, never ProjectID: the route names the SCION project that owns the
	// registration, not the GCP project the service account lives in.
	if sa.Scope == store.ScopeProject && sa.ScopeID != "" {
		return ProjectScopedRef(sa.ScopeID, sa.ID)
	}

	// Hub and user scope are parentless and belong on the flat route.
	//
	// So does a project-scoped account with no ScopeID, which is a malformed
	// record rather than a caller error. It 404s here, and that is the better
	// of two failures: the nested branch would build /api/v1/projects//gcp-
	// service-accounts/{id} and ask the Hub to make sense of an empty path
	// segment.
	return HubScopedRef(sa.ID)
}

// HubScopedRef names a hub-scoped account, which has no project.
func HubScopedRef(id string) GCPServiceAccountRef {
	return GCPServiceAccountRef{id: id}
}

// ProjectScopedRef names an account belonging to a specific Scion project.
//
// projectID is a SCION project ID, the one in the Hub's route. It is not the
// GCP project ID carried by GCPServiceAccount.ProjectID; see the type doc.
func ProjectScopedRef(projectID, id string) GCPServiceAccountRef {
	return GCPServiceAccountRef{id: id, projectID: projectID}
}

// ListGCPServiceAccountsOptions selects which accounts a list call returns.
//
// Scope is required and has no default. An unfiltered list would be a
// cross-project enumeration of every service account on the hub, which no Hub
// route offers; defaulting the field here would mean quietly asking for one and
// receiving a 400 that reads as a client bug.
type ListGCPServiceAccountsOptions struct {
	// Scope is store.ScopeHub or store.ScopeProject.
	Scope string

	// ScopeID is the project ID. Required for project scope; MUST be empty for
	// hub scope -- the Hub resolves its own ID and rejects a client-supplied
	// one, so that a request cannot name a hub that is not the one answering.
	ScopeID string

	// IncludeHubScoped widens a project list to also return hub-scoped
	// accounts. Only valid with project scope.
	IncludeHubScoped bool
}

// ListHubScoped selects every hub-scoped account.
func ListHubScoped() *ListGCPServiceAccountsOptions {
	return &ListGCPServiceAccountsOptions{Scope: store.ScopeHub}
}

// ListForProject selects the accounts REGISTERED TO one project. Hub-scoped
// accounts are not members of that set: they are registered to the hub.
func ListForProject(projectID string) *ListGCPServiceAccountsOptions {
	return &ListGCPServiceAccountsOptions{Scope: store.ScopeProject, ScopeID: projectID}
}

// ListForProjectIncludingHubScoped selects the accounts ASSIGNABLE TO an agent
// in one project: the project's own registrations plus every hub-scoped
// account, which is assignable from everywhere.
//
// TWO CONSTRUCTORS BECAUSE THOSE ARE TWO SETS, not because two kinds of caller
// want different affordances. The asymmetry that makes the distinction load-
// bearing runs one way only (sa-arch):
//
//   - A PICKER given the narrow set silently under-offers. Hub-scoped accounts
//     the user is permitted to assign simply do not appear, and their absence
//     reads as "no such account". Nothing defends this.
//
//   - A MANAGEMENT view given the wide set is already defended, by Capabilities
//     and Can(): rows the caller cannot act on render read-only. It is not the
//     list's job to re-litigate that.
//
// So pick by which set you mean, not by which screen you are drawing.
func ListForProjectIncludingHubScoped(projectID string) *ListGCPServiceAccountsOptions {
	return &ListGCPServiceAccountsOptions{
		Scope:            store.ScopeProject,
		ScopeID:          projectID,
		IncludeHubScoped: true,
	}
}

// CreateGCPServiceAccountRequest is the request for registering an existing GCP
// service account with the Hub.
type CreateGCPServiceAccountRequest struct {
	// Scope is store.ScopeHub or store.ScopeProject. Required.
	//
	// HUB SCOPE IS REFUSED BY THE HUB TODAY. It is representable here on
	// purpose rather than being rejected client-side: the refusal is a
	// deliberate server-side hold with a message explaining itself, and a
	// client that pre-empted it would report a different, less true reason and
	// would keep reporting it after the hold is lifted.
	Scope string `json:"-"`

	// ScopeID is the project ID for project scope. Must be empty for hub
	// scope.
	ScopeID string `json:"-"`

	Email       string   `json:"email"`
	ProjectID   string   `json:"projectId"`
	DisplayName string   `json:"displayName,omitempty"`
	Scopes      []string `json:"defaultScopes,omitempty"`
}

// MintGCPServiceAccountRequest is the request for minting a new GCP SA.
type MintGCPServiceAccountRequest struct {
	AccountID   string `json:"account_id,omitempty"`
	DisplayName string `json:"display_name,omitempty"`
	Description string `json:"description,omitempty"`
}

// gcpServiceAccountService is the implementation of GCPServiceAccountService.
type gcpServiceAccountService struct {
	c *client
}

const (
	gcpSAFlatPath = "/api/v1/gcp-service-accounts"
)

func gcpSANestedPath(projectID string) string {
	return fmt.Sprintf("/api/v1/projects/%s/gcp-service-accounts", projectID)
}

// byIDPath resolves a ref to the address that serves it.
//
// The two surfaces serve disjoint sets, so this is the single place the choice
// is made; see GCPServiceAccountRef.
func (r GCPServiceAccountRef) byIDPath() string {
	if r.projectID != "" {
		return fmt.Sprintf("%s/%s", gcpSANestedPath(r.projectID), r.id)
	}
	return fmt.Sprintf("%s/%s", gcpSAFlatPath, r.id)
}

// scopeQuery renders scope selection as the Hub's query parameters.
//
// ONE FUNCTION BUILDS THIS, EVERYWHERE. Every caller that needs to say "hub
// scope" over the wire goes through here, so the spelling of the parameters and
// the rule that scopeId is omitted at hub scope are written down once. Inlining
// url.Values at each call site is how the CLI's --global and the picker's
// hub-wide read would come to disagree about what they are asking for.
func gcpSAScopeQuery(scope, scopeID string, includeHubScoped bool) (url.Values, error) {
	q := url.Values{}

	switch scope {
	case store.ScopeHub:
		if scopeID != "" {
			// Refused rather than dropped. Dropping it would send a request
			// that means something different from what the caller wrote, and
			// the Hub -- which rejects a client-supplied hub ID for the same
			// reason -- would never see that the caller had asked for it.
			return nil, errors.New("scopeId must be empty for hub scope; the hub resolves its own ID")
		}
		q.Set("scope", store.ScopeHub)

	case store.ScopeProject:
		if scopeID == "" {
			return nil, errors.New("scopeId is required for project scope")
		}
		q.Set("scope", store.ScopeProject)
		q.Set("scopeId", scopeID)

	case "":
		return nil, errors.New("scope is required (expected \"project\" or \"hub\")")

	default:
		return nil, fmt.Errorf("invalid scope %q (expected \"project\" or \"hub\")", scope)
	}

	if includeHubScoped {
		if scope != store.ScopeProject {
			return nil, errors.New("includeHubScoped is only valid with project scope")
		}
		q.Set("includeHubScoped", "true")
	}

	return q, nil
}

// listGCPServiceAccountsResponse is the wire shape both Hub list handlers
// write: pkg/hub/handlers_gcp_identity.go:301.
//
// Unexported. Its only extra field beyond Items is a scope-level capability
// block, which no caller needs yet; exporting a type to carry a field nobody
// reads invites it into signatures it would then be awkward to remove from.
type listGCPServiceAccountsResponse struct {
	Items []GCPServiceAccount `json:"items"`
}

func (s *gcpServiceAccountService) List(ctx context.Context, opts *ListGCPServiceAccountsOptions) ([]GCPServiceAccount, error) {
	if opts == nil {
		return nil, errors.New("list options are required: scope has no default")
	}

	query, err := gcpSAScopeQuery(opts.Scope, opts.ScopeID, opts.IncludeHubScoped)
	if err != nil {
		return nil, err
	}

	// The flat route serves every scope, including project scope, so listing
	// does not branch on address the way by-id operations do.
	resp, err := s.c.getWithQuery(ctx, gcpSAFlatPath, query, nil)
	if err != nil {
		return nil, err
	}

	result, err := apiclient.DecodeResponse[listGCPServiceAccountsResponse](resp)
	if err != nil {
		return nil, err
	}
	if result == nil {
		// 204: DecodeResponse yields (nil, nil). No accounts is an empty list,
		// not a nil-pointer dereference.
		return []GCPServiceAccount{}, nil
	}
	return result.Items, nil
}

func (s *gcpServiceAccountService) Get(ctx context.Context, ref GCPServiceAccountRef) (*GCPServiceAccount, error) {
	resp, err := s.c.get(ctx, ref.byIDPath(), nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeResponse[GCPServiceAccount](resp)
}

func (s *gcpServiceAccountService) Create(ctx context.Context, req *CreateGCPServiceAccountRequest) (*GCPServiceAccount, error) {
	if req == nil {
		return nil, errors.New("create request is required")
	}

	query, err := gcpSAScopeQuery(req.Scope, req.ScopeID, false)
	if err != nil {
		return nil, err
	}

	resp, err := s.c.post(ctx, gcpSAFlatPath+"?"+query.Encode(), req, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeResponse[GCPServiceAccount](resp)
}

func (s *gcpServiceAccountService) Delete(ctx context.Context, ref GCPServiceAccountRef) error {
	resp, err := s.c.delete(ctx, ref.byIDPath(), nil)
	if err != nil {
		return err
	}
	// The transport error is nil for a 403. Without this, a refused deletion of
	// a credential binding returns success and the caller confirms a removal
	// that did not happen. See #33.
	return apiclient.CheckResponse(resp)
}

func (s *gcpServiceAccountService) Verify(ctx context.Context, ref GCPServiceAccountRef) (*GCPServiceAccount, error) {
	resp, err := s.c.post(ctx, ref.byIDPath()+"/verify", nil, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeResponse[GCPServiceAccount](resp)
}

func (s *gcpServiceAccountService) Mint(ctx context.Context, projectID string, req *MintGCPServiceAccountRequest) (*GCPServiceAccount, error) {
	if projectID == "" {
		return nil, errors.New("projectId is required to mint: minting draws on a project's quota")
	}

	resp, err := s.c.post(ctx, gcpSANestedPath(projectID)+"/mint", req, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeResponse[GCPServiceAccount](resp)
}
