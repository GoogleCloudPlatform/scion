package hub

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
)

const (
	authorizedListBatchSize = 50
	// authorizedListMaxCandidates bounds the per-request scan cost of both
	// passes below (the count pass and the page-fill pass), not
	// correctness: crossing it never fails the request or leaks an
	// out-of-scope row. It only means the response degrades — an
	// approximate (lower-bound) total instead of an exact one, and/or a
	// short page plus a resume cursor instead of a full one — rather than
	// scanning an unbounded number of candidates in a single request.
	//
	// ptone/scion#1916 follow-up (C3): removing hasCatalogWideListAccess's
	// agent branch moved template/harness_config list traffic that used to
	// skip this scan onto it. The original design here treated crossing this
	// cap as a hard failure (a 503 for the whole request); that turned "the
	// hub-wide candidate pool is larger than the cap" into an outage for
	// every non-privileged caller, not just a slower response for the one
	// who caused it. Bumped modestly as part of the same fix — this is a
	// cost bound, not a correctness threshold, so the exact value is a
	// judgment call, not a contract. The real fix is pushing the scope
	// predicate into the store query (the pattern
	// skillAccessScopePredicate/ResolveListScopes already established for
	// skills, pkg/store/entadapter/skill_store.go) so COUNT/LIMIT run on the
	// already-scoped set instead of a bounded in-memory scan at any size.
	authorizedListMaxCandidates = 2000
	authorizedListMaxPageSize   = 100
)

type authorizedCandidatePage[T any] struct {
	Items      []T
	NextCursor string
}

// authorizedListResult is authorizedList's outcome. TotalCount is exact
// unless TotalCountApproximate is set, in which case it is a lower bound —
// the count pass stopped at authorizedListMaxCandidates before exhausting
// the candidate pool. Items and NextCursor are always correct for the
// caller's authorized scope regardless of TotalCountApproximate: an
// approximate total never implies an incomplete or unscoped page.
type authorizedListResult[T any] struct {
	Items                 []T
	NextCursor            string
	TotalCount            int
	TotalCountApproximate bool
}

// hasCatalogWideListAccess reports whether template and harness-config lists
// can use a direct store query instead of per-resource authorization checks.
//
// User principals only: an AgentIdentity always goes through the bounded
// per-resource scan (authorizeEach in the callers below), never this
// shortcut. Agents hold only a project-scoped grant (see
// buildAgentSyntheticBindings), so "wide" access for an agent would mean
// every project's and every user's private catalog entries, not just the
// hub-wide (global) ones the wide-access comment here originally intended —
// that was a ptone/scion#1916 follow-up finding. An agent still sees the
// hub-wide catalog and its own project's entries through the per-resource
// scan: CheckAccess (Decide step 5b) promotes the agent's synthetic binding
// to system scope specifically for a global-scope template/harness_config
// check, and scopeApplies' ordinary project-scope containment covers the
// agent's own project — the same two cases getTemplateV2/getHarnessConfig's
// per-ID authorizeRead already grant, so list and detail agree.
func (s *Server) hasCatalogWideListAccess(ctx context.Context, identity Identity, resourceType, permissionID string) bool {
	user, ok := identity.(UserIdentity)
	if !ok {
		return false
	}
	return s.authzService.Decide(ctx, AuthzRequest{
		Principal:  principalContextForIdentity(user),
		Credential: credentialContextForIdentity(user),
		Resource:   Resource{Type: resourceType, ID: "hub"},
		Action:     ActionList,
		Permission: permissionID,
	}).Allowed
}

// catalogListReadBatch returns the per-resource read-batch function a
// template/harness-config list handler should pass to listAuthorizedOrAll.
//
// The authorization kernel has no notion of a "broker" principal — brokers
// authenticate over HMAC, not through the role-binding pipeline — so
// s.authzService.AuthorizeReadBatch denies every resource for one. That is
// merely the safe default, not the correct answer: a broker must see the
// hub-wide catalog and its own served projects' entries in list results
// exactly as its per-ID reads already allow (authorizeTemplateReadRoute /
// authorizeHarnessConfigRoute, both via brokerMayReadCatalogResource), or
// list and detail disagree. When identity is a broker, this returns a
// closure that answers each candidate with brokerMayReadCatalogResource
// instead of going through the kernel.
//
// An agent identity is a narrower case: its synthetic binding (Decide step
// 5b) is project-scoped, so the kernel correctly grants its own project's
// entries but — by design, not omission — never a parentless (global)
// entry; a shared-kernel carve-out would leak into every other parentless
// resource type the kernel evaluates for agents (broker, group, user,
// github_app — see TestAuthz_AgentProjectReadBaseline_NoProjectDenied). So
// the hub-wide-catalog exception for agents is applied here instead, one
// resource type at a time, the same way authorizeTemplateReadRoute /
// authorizeHarnessConfigRoute apply it to the per-ID GET: a global-scope
// candidate is allowed outright, and everything else still goes through the
// kernel so an agent's own-project visibility is unaffected.
//
// Every other identity kind uses the ordinary AuthorizeReadBatch path
// unchanged.
func (s *Server) catalogListReadBatch(identity Identity) func(context.Context, Identity, []Resource) ([]bool, error) {
	if broker, ok := identity.(BrokerIdentity); ok {
		return func(ctx context.Context, _ Identity, resources []Resource) ([]bool, error) {
			allowed := make([]bool, len(resources))
			for i, res := range resources {
				allowed[i] = s.brokerMayReadCatalogResource(ctx, broker, res.ScopeKind, res.ParentID)
			}
			return allowed, nil
		}
	}
	if _, ok := identity.(AgentIdentity); ok {
		return func(ctx context.Context, agentIdentity Identity, resources []Resource) ([]bool, error) {
			kernelAllowed, err := s.authzService.AuthorizeReadBatch(ctx, agentIdentity, resources)
			if err != nil {
				return nil, err
			}
			allowed := make([]bool, len(resources))
			for i, res := range resources {
				allowed[i] = kernelAllowed[i] || res.ScopeKind == store.TemplateScopeGlobal
			}
			return allowed, nil
		}
	}
	return s.authzService.AuthorizeReadBatch
}

// listAuthorizedOrAll runs either the bounded per-resource authorization scan
// or the direct store query selected by the caller's visibility decision.
func listAuthorizedOrAll[T any](
	ctx context.Context,
	identity Identity,
	requestCursor string,
	pageLimit int,
	cursorBinding string,
	authorizeEach bool,
	list func(context.Context, store.ListOptions) (*store.ListResult[T], error),
	resource func(*T) Resource,
	cursorFor func(*T) string,
	read func(context.Context, Identity, []Resource) ([]bool, error),
) (authorizedListResult[T], error) {
	if !authorizeEach {
		result, err := list(ctx, store.ListOptions{
			Limit:         pageLimit,
			Cursor:        requestCursor,
			CursorBinding: cursorBinding,
		})
		if err != nil {
			return authorizedListResult[T]{}, err
		}
		return authorizedListResult[T]{
			Items:      result.Items,
			NextCursor: result.NextCursor,
			TotalCount: result.TotalCount,
		}, nil
	}

	return authorizedList(ctx, identity, requestCursor, pageLimit,
		func(ctx context.Context, cursor string, limit int) (authorizedCandidatePage[T], error) {
			page, err := list(ctx, store.ListOptions{
				Limit:          limit,
				Cursor:         cursor,
				SkipTotalCount: true,
				CursorBinding:  cursorBinding,
			})
			if err != nil {
				return authorizedCandidatePage[T]{}, err
			}
			return authorizedCandidatePage[T]{Items: page.Items, NextCursor: page.NextCursor}, nil
		}, resource, cursorFor, read)
}

func parseAuthorizedListLimit(raw string) (int, error) {
	if raw == "" {
		return authorizedListBatchSize, nil
	}
	limit, err := strconv.Atoi(raw)
	if err != nil || limit <= 0 || limit > authorizedListMaxPageSize {
		return 0, fmt.Errorf("limit must be between 1 and %d", authorizedListMaxPageSize)
	}
	return limit, nil
}

// authorizedList returns an authorized total (exact, unless the candidate
// pool is large enough to mark it approximate — see authorizedListResult)
// and a page of authorized candidates. It rescans from the start for the
// total, retaining only response items, so denied candidates cannot affect
// either result.
//
// Neither pass ever fails, or returns an out-of-scope item, purely because
// the underlying (unscoped) candidate pool is large: crossing
// authorizedListMaxCandidates degrades the response (an approximate total;
// a short page plus a resume cursor) rather than erroring the request. See
// the cap's own doc comment for why a hard failure here was worse than a
// bounded scan cost.
func authorizedList[T any](
	ctx context.Context,
	identity Identity,
	requestCursor string,
	pageLimit int,
	fetch func(context.Context, string, int) (authorizedCandidatePage[T], error),
	resource func(*T) Resource,
	cursorFor func(*T) string,
	read func(context.Context, Identity, []Resource) ([]bool, error),
) (authorizedListResult[T], error) {
	if err := ctx.Err(); err != nil {
		return authorizedListResult[T]{}, err
	}

	// Pass 1: count. Scans candidates in batches up to
	// authorizedListMaxCandidates. Reaching the cap before the pool is
	// exhausted marks the total approximate (a lower bound) rather than
	// scanning further or failing — see authorizedListResult.
	total := 0
	candidateCount := 0
	totalApproximate := false
	for cursor := ""; ; {
		if err := ctx.Err(); err != nil {
			return authorizedListResult[T]{}, err
		}
		page, err := fetch(ctx, cursor, authorizedListBatchSize)
		if err != nil {
			return authorizedListResult[T]{}, err
		}
		candidateCount += len(page.Items)
		allowed, err := authorizeCandidatePage(ctx, identity, page.Items, resource, read)
		if err != nil {
			return authorizedListResult[T]{}, err
		}
		for _, ok := range allowed {
			if ok {
				total++
			}
		}
		if page.NextCursor == "" {
			break
		}
		if candidateCount >= authorizedListMaxCandidates {
			totalApproximate = true
			break
		}
		cursor = page.NextCursor
	}

	// Pass 2: fill the requested page, starting from the caller's cursor.
	// Bounded by the same candidate cap so a caller cannot force an
	// unbounded per-request scan by holding a large denied (or merely
	// unauthorized) candidate pool ahead of their own visible items: if the
	// budget runs out before the page fills, return the short page found so
	// far plus a resume cursor rather than continuing the scan.
	result := authorizedListResult[T]{TotalCount: total, TotalCountApproximate: totalApproximate}
	scanned := 0
	for cursor := requestCursor; ; {
		if err := ctx.Err(); err != nil {
			return authorizedListResult[T]{}, err
		}
		page, err := fetch(ctx, cursor, authorizedListBatchSize)
		if err != nil {
			return authorizedListResult[T]{}, err
		}
		allowed, err := authorizeCandidatePage(ctx, identity, page.Items, resource, read)
		if err != nil {
			return authorizedListResult[T]{}, err
		}
		for i := range page.Items {
			if !allowed[i] {
				continue
			}
			if len(result.Items) == pageLimit {
				// Page filled. Resume from this (not-yet-included) item.
				result.NextCursor = cursorFor(&page.Items[i])
				return result, nil
			}
			item := page.Items[i]
			result.Items = append(result.Items, item)
			result.NextCursor = cursorFor(&item)
		}
		scanned += len(page.Items)
		if page.NextCursor == "" {
			result.NextCursor = ""
			return result, nil
		}
		if scanned >= authorizedListMaxCandidates {
			// Budget exhausted before the page filled (or before the scan
			// could confirm no candidates remain). Resume from the last
			// candidate this request examined; the page returned so far,
			// though possibly short of pageLimit, contains only items this
			// caller is authorized to see.
			result.NextCursor = cursorFor(&page.Items[len(page.Items)-1])
			return result, nil
		}
		cursor = page.NextCursor
	}
}

func authorizeCandidatePage[T any](ctx context.Context, identity Identity, items []T, resource func(*T) Resource, read func(context.Context, Identity, []Resource) ([]bool, error)) ([]bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	resources := make([]Resource, len(items))
	for i := range items {
		resources[i] = resource(&items[i])
	}
	allowed, err := read(ctx, identity, resources)
	if err != nil {
		return nil, err
	}
	if len(allowed) != len(items) {
		return nil, errors.New("authorization result length mismatch")
	}
	return allowed, nil
}

func authorizedListCursor(created time.Time, id, binding string) string {
	return base64.URLEncoding.EncodeToString([]byte(created.Format(time.RFC3339Nano) + "," + id + "," + binding))
}

func authorizedListCursorBinding(endpoint string, filter any) string {
	encoded, _ := json.Marshal(filter)
	digest := sha256.Sum256(append([]byte(endpoint+":"), encoded...))
	return base64.RawURLEncoding.EncodeToString(digest[:])
}

// scopedCursorBinding creates a cursor binding that includes the endpoint,
// filter (which already contains the authorized scope), and identity context.
// This ensures cursors cannot be replayed across principals, credential kinds,
// or authorization scope changes.
//
// RS2: A cursor minted before an authority, group, lifecycle, constraint,
// suspension, or credential-scope change must not disclose data when replayed.
// Including the identity in the binding hash ensures cross-principal and
// cross-credential replay is rejected.
func scopedCursorBinding(endpoint string, filter any, identity Identity) string {
	encoded, _ := json.Marshal(filter)
	// Build the binding input: endpoint + filter + principal context.
	// The principal context includes the identity type and unique identifier
	// so that cursors are not transferable between principals or credential types.
	var identityKey string
	if identity != nil {
		// Include the concrete credential type to distinguish session JWT
		// from scoped UAT (same user ID, different authority ceiling).
		switch id := identity.(type) {
		case *ScopedUserIdentity:
			identityKey = fmt.Sprintf("scoped_uat:%s:%s:%s", id.ID(), id.ScopedProjectID(), id.CredentialID())
		case AgentIdentity:
			identityKey = fmt.Sprintf("agent_jwt:%s:%s:%s", id.ID(), id.ProjectID(), id.TokenID())
		default:
			identityKey = fmt.Sprintf("%s:%s", identity.Type(), identity.ID())
		}
	}
	raw := append([]byte(endpoint+":"), encoded...)
	raw = append(raw, []byte(":"+identityKey)...)
	digest := sha256.Sum256(raw)
	return base64.RawURLEncoding.EncodeToString(digest[:])
}

func validateAuthorizedListCursor(cursor, binding string) error {
	raw, err := base64.URLEncoding.DecodeString(cursor)
	if err != nil {
		return fmt.Errorf("invalid cursor: %w", err)
	}
	parts := strings.SplitN(string(raw), ",", 3)
	if len(parts) != 3 || parts[2] != binding {
		return errors.New("invalid cursor")
	}
	if _, err := time.Parse(time.RFC3339Nano, parts[0]); err != nil {
		return fmt.Errorf("invalid cursor: %w", err)
	}
	if _, err := uuid.Parse(parts[1]); err != nil {
		return fmt.Errorf("invalid cursor: %w", err)
	}
	return nil
}
