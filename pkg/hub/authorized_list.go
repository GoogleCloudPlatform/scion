package hub

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/secret"
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
//
// requestCursor is the raw, opaque cursor a client sent (or "" for a first
// page); the returned NextCursor is opaque the same way. sealer opens
// requestCursor and seals the result on BOTH paths below, so a list
// endpoint's cursor format never depends on which path the caller's
// authority happened to select that request -- an identity that gains or
// loses wide access between page 1 and page 2 still gets a cursor the other
// path accepts. See listCursorSealer's doc comment; a resumed cursor is
// still only a position, never an access grant, on either path.
func listAuthorizedOrAll[T any](
	ctx context.Context,
	identity Identity,
	requestCursor string,
	pageLimit int,
	cursorBinding string,
	sealer *listCursorSealer,
	authorizeEach bool,
	list func(context.Context, store.ListOptions) (*store.ListResult[T], error),
	resource func(*T) Resource,
	cursorFor func(*T) string,
	read func(context.Context, Identity, []Resource) ([]bool, error),
) (authorizedListResult[T], error) {
	cursor, err := openAndValidateListCursor(sealer, requestCursor, cursorBinding)
	if err != nil {
		return authorizedListResult[T]{}, err
	}

	if !authorizeEach {
		result, err := list(ctx, store.ListOptions{
			Limit:         pageLimit,
			Cursor:        cursor,
			CursorBinding: cursorBinding,
		})
		if err != nil {
			return authorizedListResult[T]{}, err
		}
		nextCursor := result.NextCursor
		if nextCursor != "" {
			sealed, err := sealer.Seal(nextCursor, cursorBinding)
			if err != nil {
				return authorizedListResult[T]{}, err
			}
			nextCursor = sealed
		}
		return authorizedListResult[T]{
			Items:      result.Items,
			NextCursor: nextCursor,
			TotalCount: result.TotalCount,
		}, nil
	}

	result, err := authorizedList(ctx, identity, cursor, pageLimit,
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
	if err != nil {
		return authorizedListResult[T]{}, err
	}
	if result.NextCursor != "" {
		sealed, err := sealer.Seal(result.NextCursor, cursorBinding)
		if err != nil {
			return authorizedListResult[T]{}, err
		}
		result.NextCursor = sealed
	}
	return result, nil
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
				// Page filled. NextCursor already holds the cursor of the
				// last included item, set below when that item was
				// appended; store cursors are exclusive, so resuming from
				// it starts immediately after that item. Do not advance it
				// to this (not-yet-included) item, or that item is skipped
				// on the next page.
				//
				// Items examined earlier in this same batch but not
				// included (denied, or simply skipped) sit between the
				// last included item and this one. Advance the cursor past
				// them too, to the item just before this one, so the next
				// request resumes at this item directly instead of
				// re-examining ones already resolved here. i is always > 0
				// when this happens mid-batch (the append that reached
				// pageLimit occurred earlier in this same loop); when the
				// page instead fills exactly at a batch boundary, i is 0
				// here and NextCursor is left as already set above.
				if i > 0 {
					result.NextCursor = cursorFor(&page.Items[i-1])
				}
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
	return base64.URLEncoding.EncodeToString([]byte(created.UTC().Format(time.RFC3339Nano) + "," + id + "," + binding))
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
	// A nil identity, or a non-nil interface holding a nil pointer, carries
	// no principal; both bind with an empty identity component, matching
	// principalContextForIdentity, and no method is called on a nil receiver.
	var identityKey string
	if !isNilIdentity(identity) {
		// Include the concrete credential type to distinguish session JWT
		// from scoped UAT (same user ID, different authority ceiling).
		switch id := identity.(type) {
		case *ScopedUserIdentity:
			// Keyed on the boundary kind as well as its project, so a cursor
			// minted under a hub-boundary token never matches one minted
			// under a project-boundary token, or the reverse.
			boundary := id.Boundary()
			identityKey = fmt.Sprintf("scoped_uat:%s:%s:%s:%s", id.ID(), boundary.Kind, boundary.ProjectID, id.CredentialID())
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

// ---------------------------------------------------------------------------
// Opaque cursor sealing (ptone/scion#2124, ptone/scion#2151)
//
// authorizedList's cursors are opaque: a page reveals only items the caller
// may see, and the cursor that resumes a walk is a sealed, authenticated
// token rather than a readable encoding of the position it carries. Seal
// wraps the existing "created,id,binding" cursor payload with AES-256-GCM
// before it ever reaches a client; Open recovers that same payload on the
// way in, or fails closed. The payload format, the binding computation
// (scopedCursorBinding) and every pagination semantic (ordering,
// over-fetch, scan budget, live per-item authorization on resume) are
// unchanged -- sealing only makes the wire representation opaque and
// tamper-evident, and binds it to the exact endpoint, filter and caller it
// was issued for.
//
// Coverage: every authorizedList and listAuthorizedOrAll caller seals and
// opens through this helper -- templates, harness configs and groups, on
// both the per-item-scan path and the direct-query (wide-access/admin)
// path listAuthorizedOrAll and listGroups also provide. Hub list handlers
// that never run authorizedList's per-item scan because their store query
// is already fully scoped (listAgents, listProjects) are out of scope here,
// and are tracked as a follow-up instead of changed by this work
// (ptone/scion#2124). listSkills (skill_handlers.go) and listProjectAgents
// (handlers_projects_core.go) also drop items after the store query as
// defense in depth over an already-scoped query, then return the store's
// own (unsealed) NextCursor; they are the same kind of follow-up, tracked
// alongside listAgents/listProjects rather than changed here.
// ---------------------------------------------------------------------------

// SecretKeyListCursorKey is the secret key name for the dedicated
// authorizedList cursor-sealing key. It is separate from the agent, user,
// OIDC and download-signing keys (see download_signing.go): it never signs
// or verifies a credential, only seals a resume position, so its blast
// radius on rotation or compromise is limited to pagination cursors.
const SecretKeyListCursorKey = "list_cursor_key"

// listCursorSealDomain domain-separates the AEAD's associated data from any
// other AES-GCM use of the same key and binds it to this scheme; changing it
// invalidates every previously issued cursor the same way rotating the key
// does (see listCursorSealer.Open). The wire version marker is
// listCursorPrefix.
const listCursorSealDomain = "scion-list-cursor-v1:"

// listCursorPrefix is the literal, cheap-to-check version marker every
// sealed cursor starts with. It lets a server reject a cursor from an
// unknown or future version (or one that is not a sealed cursor at all)
// before spending an AEAD open on it, and gives a future key-rotation or
// scheme change (a "c2." prefix) a dispatch point that does not require
// trial-decrypting under every version's key and AAD.
const listCursorPrefix = "c1."

// errInvalidCursor is returned for every cursor failure a caller can hit --
// malformed input, truncation, a tampered byte, a cursor sealed under a key
// this sealer does not currently hold, or a binding (endpoint, filter or
// caller) that does not match the one the cursor was sealed under -- so all
// cursor failures get one uniform response.
var errInvalidCursor = errors.New("invalid cursor")

// errListCursorSealerUnavailable is returned by Seal when called on a nil
// sealer -- a state New() never produces in production (it always either
// provisions a sealer or fails startup outright), but one a test
// constructing a bare Server{} can reach. Failing closed here (no cursor is
// emitted; the caller turns this into a 500) is the same choice Open makes
// for a nil sealer via openAndValidateListCursor, just surfaced as a
// distinct error since an unsealable next page is a server-side problem,
// not a bad cursor the client sent.
var errListCursorSealerUnavailable = errors.New("list cursor sealer unavailable")

// listCursorSealer seals authorizedList's client-facing cursors with
// AES-256-GCM so a cursor carries no readable item ID or created time.
// Seal's output is opaque; Open only recovers the sealed plaintext when
// given the exact binding it was sealed under, so a cursor opened under a
// different query or caller fails the same way a tampered one does.
//
// A sealed cursor never confers access by itself: it only carries a
// position. Every page authorizedList returns -- including the first page
// of a resumed walk -- still runs live per-item authorization; see
// authorizedList's own doc comment.
type listCursorSealer struct {
	aead cipher.AEAD
}

// newListCursorSealer builds a sealer from a 32-byte AES-256 key.
func newListCursorSealer(key []byte) (*listCursorSealer, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("list cursor sealer: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("list cursor sealer: %w", err)
	}
	return &listCursorSealer{aead: aead}, nil
}

// Seal encodes inner -- the existing plaintext authorizedListCursor
// encoding -- into an opaque, authenticated cursor bound to binding
// (scopedCursorBinding's output: the endpoint, normalized filter and
// caller identity context), prefixed with listCursorPrefix. It never logs
// inner or the key. Called on a nil sealer, it fails closed with
// errListCursorSealerUnavailable instead of panicking; see that error's
// doc comment.
func (l *listCursorSealer) Seal(inner, binding string) (string, error) {
	if l == nil {
		return "", errListCursorSealerUnavailable
	}
	nonce := make([]byte, l.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		// crypto/rand failing is unrecoverable for this process: there is no
		// safe cursor left to hand back, sealed or otherwise.
		panic("list cursor sealer: generating nonce: " + err.Error())
	}
	ciphertext := l.aead.Seal(nil, nonce, []byte(inner), []byte(listCursorSealDomain+binding))
	return listCursorPrefix + base64.RawURLEncoding.EncodeToString(append(nonce, ciphertext...)), nil
}

// Open recovers the plaintext inner cursor Seal produced under binding, and
// additionally re-validates it with validateAuthorizedListCursor as a
// structural sanity check on the decrypted plaintext. It returns
// errInvalidCursor -- never a partially authenticated value, and never a
// logged one -- for anything else: a missing or unrecognized version
// prefix, malformed base64, truncation, a flipped byte, a binding that does
// not match the query or caller the cursor was sealed under, a legacy
// (pre-sealing) plaintext cursor, or a cursor sealed under a key this
// sealer does not currently hold (for example after key rotation). Called
// on a nil sealer, it also returns errInvalidCursor rather than panicking,
// matching Seal's fail-closed behavior; openAndValidateListCursor also
// rejects a non-empty cursor when the sealer is nil.
func (l *listCursorSealer) Open(sealed, binding string) (string, error) {
	if l == nil {
		return "", errInvalidCursor
	}
	body, ok := strings.CutPrefix(sealed, listCursorPrefix)
	if !ok {
		return "", errInvalidCursor
	}
	raw, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		return "", errInvalidCursor
	}
	nonceSize := l.aead.NonceSize()
	if len(raw) < nonceSize {
		return "", errInvalidCursor
	}
	nonce, ciphertext := raw[:nonceSize], raw[nonceSize:]
	plaintext, err := l.aead.Open(nil, nonce, ciphertext, []byte(listCursorSealDomain+binding))
	if err != nil {
		return "", errInvalidCursor
	}
	inner := string(plaintext)
	if err := validateAuthorizedListCursor(inner, binding); err != nil {
		return "", errInvalidCursor
	}
	return inner, nil
}

// openAndValidateListCursor turns a raw, client-supplied query-string cursor
// into the plaintext cursor the existing store/authorizedList plumbing
// expects. An empty cursor (a first-page request) is always valid and
// returns "" unconditionally, without touching sealer -- so a fresh first
// page always works even when sealer is nil, a key rotated, or a cursor
// from a previous key is being rejected elsewhere in the same request.
func openAndValidateListCursor(sealer *listCursorSealer, sealed, binding string) (string, error) {
	if sealed == "" {
		return "", nil
	}
	if sealer == nil {
		return "", errInvalidCursor
	}
	return sealer.Open(sealed, binding)
}

// initListCursorSealer loads or creates the list-cursor sealing key through
// the same persistence path as the Hub's other signing keys (see
// initDownloadSigningKey), and follows the same stable-key failure policy:
// a GCP secret backend or RequireStableSigningKey makes a missing,
// unloadable, or wrong-length key (AES-256 requires exactly 32 bytes) a
// startup failure. Replicas that disagree on this key would otherwise fail
// every cross-replica cursor resume -- confusing, since it surfaces as an
// intermittent ErrCodeInvalidCursor 400, but never unsafe: a cursor sealed
// under a key this hub does not hold only ever fails closed (see
// listCursorSealer.Open), and a fresh first-page request (no cursor) is
// unaffected either way.
//
// Otherwise (local development, single-node hubs) it falls back to an
// ephemeral in-memory key: a restart just invalidates outstanding cursors,
// the same fallback the download-signing key uses.
func (s *Server) initListCursorSealer(ctx context.Context) error {
	key, err := s.ensureSigningKey(ctx, SecretKeyListCursorKey, nil)
	if err == nil && len(key) != 32 {
		err = fmt.Errorf("list cursor key must be 32 bytes for AES-256, got %d", len(key))
	}
	if err != nil {
		_, isGCPBackend := s.secretBackend.(*secret.GCPBackend)
		if isGCPBackend || s.config.RequireStableSigningKey {
			return fmt.Errorf("list cursor key: %w", err)
		}
		slog.Warn("List cursor key could not be loaded or persisted; using an ephemeral in-memory key "+
			"(outstanding pagination cursors will not validate on other replicas or after restart)",
			"error", err)
		key = make([]byte, 32)
		if _, rerr := rand.Read(key); rerr != nil {
			return fmt.Errorf("generate ephemeral list cursor key: %w", rerr)
		}
	}
	sealer, err := newListCursorSealer(key)
	if err != nil {
		return fmt.Errorf("list cursor key: %w", err)
	}
	s.listCursorSealer = sealer
	return nil
}
