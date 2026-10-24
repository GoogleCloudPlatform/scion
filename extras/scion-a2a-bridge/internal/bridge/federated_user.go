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

package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/uuid"

	"github.com/GoogleCloudPlatform/scion/extras/scion-a2a-bridge/internal/identity"
	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/transportauth"
)

// Federated user callers (people from another issuer, typically another
// Scion hub, whom a project owner on this hub has let message agents).
//
// The bridge never decides what a federated user may do. The hub answers,
// asked with the caller's own credential (X-Scion-Federation-Token): which
// account it belongs to (GET /api/v1/auth/me), which projects and agents it
// may reach, and, before each agent reply is forwarded, whether it may still
// read its conversation with that agent. The hub admits a user access
// credential from another issuer only for a project visitor account; it
// refuses any other user (for example, an unlinked identity issuer user)
// with 403 (GoogleCloudPlatform/scion#3577), and with its project visitor
// experiment off it refuses a user access credential from another hub with
// 401. The bridge passes every hub refusal on.

// federatedUserRefusalText is the one answer a federated user gets for every
// refusal: an unknown project or agent, an agent it may not message, and an
// access the hub no longer grants. It is the hub's own refusal text, so the
// bridge's answers cannot be told apart from the hub's.
const federatedUserRefusalText = "You don't have access to this resource"

// errFederatedUserRefused reports that the hub refused a federated user's
// request (HTTP 401, 403 or 404).
var errFederatedUserRefused = errors.New("federated user refused by hub")

// federatedUserHubRefusal is the error for a hub refusal of a federated
// user's request. It matches errFederatedUserRefused and keeps the hub's
// status, so a 401 (credential not accepted) can be passed on as a 401.
type federatedUserHubRefusal struct {
	status int
	path   string
}

func (e *federatedUserHubRefusal) Error() string {
	return fmt.Sprintf("%v (HTTP %d on %s)", errFederatedUserRefused, e.status, e.path)
}

func (e *federatedUserHubRefusal) Is(target error) bool { return target == errFederatedUserRefused }

// isHubUnauthenticated reports whether err is a hub 401 for a federated
// user's request: the hub did not accept the credential itself.
func isHubUnauthenticated(err error) bool {
	var refusal *federatedUserHubRefusal
	return errors.As(err, &refusal) && refusal.status == http.StatusUnauthorized
}

// federatedUserHubTimeout bounds each hub call made for a federated user.
const federatedUserHubTimeout = 10 * time.Second

// isHubRefusal reports whether err is a hub answer that refuses the caller:
// errFederatedUserRefused, or an API error with status 401, 403 or 404.
func isHubRefusal(err error) bool {
	if errors.Is(err, errFederatedUserRefused) {
		return true
	}
	var apiErr *apiclient.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound:
			return true
		}
	}
	return false
}

// federatedUserGet sends GET path (with query) to the hub as the federated
// user caller and decodes a 200 JSON answer into out. The request carries
// the caller's credential in X-Scion-Federation-Token and the bridge's
// minted credential as the bearer, the same pair callerHubClient sends for
// federation callers. A 401, 403 or 404 answer returns
// errFederatedUserRefused.
func (b *Bridge) federatedUserGet(ctx context.Context, caller *CallerIdentity, path string, query url.Values, out interface{}) error {
	if b.minter == nil {
		return fmt.Errorf("federated user request: bridge has no signing key")
	}
	if !hubEndpointOK(b.config.Hub.Endpoint) {
		return errFederatedUserEndpoint
	}
	endpoint := strings.TrimRight(b.config.Hub.Endpoint, "/") + path
	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}
	ctx, cancel := context.WithTimeout(ctx, federatedUserHubTimeout)
	defer cancel()
	// Errors name the path, never the endpoint: the endpoint may carry user
	// information, and url.Error text quotes the full URL.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return errors.New("federated user request: hub endpoint is not a valid URL")
	}

	if err := b.federatedUserBridgeAuth().ApplyAuth(req); err != nil {
		return fmt.Errorf("federated user request: %w", err)
	}

	base := http.DefaultTransport
	if b.transportSrc != nil {
		base = transportauth.Wrap(base, b.transportSrc, b.transportMode)
	}
	transport, err := newFederationHeaderTransport(base, caller.RawToken, b.config.Hub.Endpoint)
	if err != nil {
		return fmt.Errorf("federated user request: %w", err)
	}
	client := &http.Client{Transport: transport}
	resp, err := client.Do(req)
	if err != nil {
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			err = urlErr.Err
		}
		return fmt.Errorf("federated user request: GET %s: %w", path, err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound:
		return &federatedUserHubRefusal{status: resp.StatusCode, path: path}
	default:
		return fmt.Errorf("federated user request: unexpected hub answer HTTP %d on %s", resp.StatusCode, path)
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(out); err != nil {
		return fmt.Errorf("federated user request: decode %s: %w", path, err)
	}
	return nil
}

// errFederatedUserEndpoint is returned by federatedUserGet for a hub endpoint
// that hubEndpointOK refuses. Its text never includes the endpoint.
var errFederatedUserEndpoint = errors.New("federated user request: hub endpoint must be a URL with a host, no user information, query or fragment")

// hubEndpointOK reports whether endpoint is a URL with a host and no user
// information, query or fragment. ValidateConfig and federatedUserGet both
// use it. An endpoint holding an '@' anywhere is refused: a raw (not
// percent-encoded) '@', '/', '?' or '#' in user information can make
// url.Parse read part of it as the host or the path. An endpoint holding a
// '?' or '#' anywhere is refused too, even with nothing after it, since
// paths are appended to the endpoint.
func hubEndpointOK(endpoint string) bool {
	if strings.Contains(endpoint, "@") {
		return false
	}
	if strings.ContainsAny(endpoint, "?#") {
		return false
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return false
	}
	return u.User == nil && u.Opaque == "" && u.Host != ""
}

// federatedUserBridgeAuth returns the authenticator for the bridge's own
// credential that federatedUserGet sends next to the caller's: one for the
// Bridge, so its minted credential is reused until it nears expiry instead
// of being minted for every hub call. b.minter must be set.
func (b *Bridge) federatedUserBridgeAuth() *identity.MintingAuth {
	b.federatedUserAuthOnce.Do(func() {
		hubUserID := b.config.Hub.UserID
		if hubUserID == "" {
			hubUserID = b.config.Hub.User
		}
		b.federatedUserAuth = identity.NewMintingAuth(b.minter, hubUserID, b.config.Hub.User, "admin", 5*time.Minute)
	})
	return b.federatedUserAuth
}

// identifyFederatedUser completes a federated user caller from
// decodeFederationToken: it asks the hub (GET /api/v1/auth/me under the
// caller's credential) which account the credential belongs to.
//
//   - The hub names a project visitor account (a user, not an agent, whose
//     ID is a canonical UUID): it returns a copy of caller with UserID set
//     to that account ID. The email stays the credential's.
//   - The hub refuses the credential (401, 403 or 404): the refusal
//     (errFederatedUserRefused; isHubUnauthenticated tells a 401 apart). A
//     user with no project visitor account (for example, an identity issuer
//     user not linked to an account) gets 403; a user access credential from
//     another hub gets 401 while the hub's project visitor experiment is off.
//   - The hub answers but names no project visitor account: also a refusal
//     (errFederatedUserRefused), so the caller is never served under another
//     identity.
//   - The hub could not be asked: another error, so the caller can retry.
func (b *Bridge) identifyFederatedUser(ctx context.Context, caller *CallerIdentity) (*CallerIdentity, error) {
	if caller == nil || !caller.IsFederatedUser() {
		return nil, fmt.Errorf("identify federated user: not a federated user caller")
	}
	var me userResponse
	if err := b.federatedUserGet(ctx, caller, "/api/v1/auth/me", nil, &me); err != nil {
		return nil, err
	}
	if me.Role == "agent" || !isCanonicalUUID(me.ID) {
		return nil, fmt.Errorf("%w: hub named no project visitor account", errFederatedUserRefused)
	}
	identified := *caller
	identified.UserID = me.ID
	identified.Role = me.Role
	return &identified, nil
}

// isCanonicalUUID reports whether id is a UUID in canonical form (lowercase
// hex with hyphens), the form of hub account IDs.
func isCanonicalUUID(id string) bool {
	u, err := uuid.Parse(id)
	return err == nil && u.String() == id
}

// visitorOwnGrant mirrors an entry of GET /api/v1/users/me/visitor-grants.
type visitorOwnGrant struct {
	ProjectID   string `json:"projectId"`
	ProjectSlug string `json:"projectSlug"`
	Status      string `json:"status"`
}

// visitorAgentView mirrors an entry of the hub's visitor agent list: the
// only agent fields a federated user sees.
type visitorAgentView struct {
	Name   string `json:"name"`
	Slug   string `json:"slug"`
	Status string `json:"status"`
}

// federatedUserProjectID returns the hub ID of the project with slug
// projectSlug, from the caller's own active grants
// (GET /api/v1/users/me/visitor-grants). A project the caller holds no
// active grant on returns errFederatedUserRefused.
func (b *Bridge) federatedUserProjectID(ctx context.Context, caller *CallerIdentity, projectSlug string) (string, error) {
	var grants struct {
		Grants []visitorOwnGrant `json:"grants"`
	}
	if err := b.federatedUserGet(ctx, caller, "/api/v1/users/me/visitor-grants", nil, &grants); err != nil {
		return "", err
	}
	for _, g := range grants.Grants {
		if g.Status == "active" && g.ProjectSlug == projectSlug && g.ProjectID != "" {
			return g.ProjectID, nil
		}
	}
	return "", fmt.Errorf("%w: no active grant on project %q", errFederatedUserRefused, projectSlug)
}

// federatedUserMessageableAgents returns the agents of project projectSlug
// the caller may message now, as the hub lists them for the caller's
// credential (GET /api/v1/projects/{id}/agents?messageable=true).
func (b *Bridge) federatedUserMessageableAgents(ctx context.Context, caller *CallerIdentity, projectSlug string) ([]visitorAgentView, error) {
	projectID, err := b.federatedUserProjectID(ctx, caller, projectSlug)
	if err != nil {
		return nil, err
	}
	var list struct {
		Agents []visitorAgentView `json:"agents"`
	}
	query := url.Values{"messageable": []string{"true"}}
	if err := b.federatedUserGet(ctx, caller, "/api/v1/projects/"+url.PathEscape(projectID)+"/agents", query, &list); err != nil {
		return nil, err
	}
	return list.Agents, nil
}

// recheckFederatedUserAccess asks the hub, under the caller's credential,
// whether the caller may still read its conversation with agent agentID
// (GET /api/v1/chat/conversations/{dm:agent:<agentID>:user:<accountID>}/messages).
// The bridge calls it before it forwards an agent reply to a federated
// user: a revoked or expired grant, or an agent the caller may no longer
// message, makes the hub refuse the read, and the caller gets the refusal
// instead of the reply. Returns errFederatedUserRefused on a refusal and
// another error when the hub could not be asked; both stop the reply.
func (b *Bridge) recheckFederatedUserAccess(ctx context.Context, caller *CallerIdentity, agentID string) error {
	key, err := messages.DMConversationKey("agent", agentID, "user", caller.UserID)
	if err != nil {
		return fmt.Errorf("%w: %v", errFederatedUserRefused, err)
	}
	query := url.Values{"limit": []string{"1"}}
	return b.federatedUserGet(ctx, caller, "/api/v1/chat/conversations/"+url.PathEscape(key)+"/messages", query, nil)
}

// federatedUserRefusalMessage is the agent message of a task that ends with
// the uniform refusal.
func federatedUserRefusalMessage() *a2a.Message {
	return a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart(federatedUserRefusalText))
}

// federatedUserReplyRefusal re-checks a federated user's access to agent
// agentID (recheckFederatedUserAccess) before a reply is forwarded. It
// returns nil when the reply may be forwarded, otherwise the message the
// task ends with: the uniform refusal when the hub refused, or a failure
// when the hub could not be asked (the reply is not forwarded either way).
func (b *Bridge) federatedUserReplyRefusal(ctx context.Context, caller *CallerIdentity, agentID string) *a2a.Message {
	err := b.recheckFederatedUserAccess(ctx, caller, agentID)
	if err == nil {
		return nil
	}
	b.log.Debug("federated user access re-check failed", "error", err, "agent_id", agentID)
	if isHubRefusal(err) {
		return federatedUserRefusalMessage()
	}
	return a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart("Unable to verify access to agent"))
}

// writeFederatedUserRefusal answers a federated user's HTTP request with the
// uniform refusal.
func writeFederatedUserRefusal(w http.ResponseWriter) {
	http.Error(w, federatedUserRefusalText, http.StatusForbidden)
}
