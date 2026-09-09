package hub

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	authorizedListBatchSize     = 50
	authorizedListMaxCandidates = 1000
	authorizedListMaxPageSize   = 100
)

var ErrAuthorizedListIncomplete = errors.New("authorized list exceeds candidate limit")

func writeAuthorizedListError(w http.ResponseWriter, err error) {
	if errors.Is(err, ErrAuthorizedListIncomplete) {
		writeError(w, http.StatusServiceUnavailable, ErrCodeRuntimeError, "authorized list is temporarily unavailable", nil)
		return
	}
	writeErrorFromErr(w, err, "")
}

type authorizedCandidatePage[T any] struct {
	Items      []T
	NextCursor string
}

type authorizedListResult[T any] struct {
	Items      []T
	NextCursor string
	TotalCount int
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

// authorizedList returns an exact authorized total and a page of authorized
// candidates. It deliberately rescans from the start for totals, retaining
// only response items, so denied candidates cannot affect either result.
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
	total := 0
	candidateCount := 0
	for cursor := ""; ; {
		if err := ctx.Err(); err != nil {
			return authorizedListResult[T]{}, err
		}
		page, err := fetch(ctx, cursor, authorizedListBatchSize)
		if err != nil {
			return authorizedListResult[T]{}, err
		}
		candidateCount += len(page.Items)
		if candidateCount > authorizedListMaxCandidates ||
			(candidateCount == authorizedListMaxCandidates && page.NextCursor != "") {
			return authorizedListResult[T]{}, ErrAuthorizedListIncomplete
		}
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
		cursor = page.NextCursor
	}

	result := authorizedListResult[T]{TotalCount: total}
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
				return result, nil
			}
			item := page.Items[i]
			result.Items = append(result.Items, item)
			result.NextCursor = cursorFor(&item)
		}
		if page.NextCursor == "" {
			result.NextCursor = ""
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
