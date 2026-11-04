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
	"sort"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Skipped secrets: a secret the secret store could not return for a
// dispatch is skipped, and the dispatch goes on without it (upstream
// #4318's skip). The skip is reported as one warning per dispatch, in the
// response through the dispatch-warnings collector and always in the
// server log. Neither carries a value, a ref or resource path, a scope ID
// or backend error text: only key names, scope kinds and the skip class.

// Skip classes of a skippedSecret.
const (
	// skipClassAccess: the Secret Manager read failed (secret.ErrValueAccess).
	skipClassAccess = "access"
	// skipClassNotFound: the record or its external value is gone
	// (store.ErrNotFound; on Secret Manager, a dangling ref).
	skipClassNotFound = "not_found"
	// skipClassMissing: the backend returned no result for the item.
	skipClassMissing = "missing"
)

// skippedSecret is one secret a dispatch skipped because the secret store
// could not return it. Scope is the store scope constant (store.Scope*);
// it is rendered as a scope kind, never with its scope ID.
type skippedSecret struct {
	Name  string
	Scope string
	Class string
}

// skippedSecretScopeKind maps a store scope constant to the scope kind the
// warning and the log show. ok is false for any other value.
func skippedSecretScopeKind(scope string) (kind string, ok bool) {
	switch scope {
	case store.ScopeHub:
		return "hub", true
	case store.ScopeRuntimeBroker:
		return "broker", true
	case store.ScopeProject:
		return "project", true
	case store.ScopeUser:
		return "user", true
	}
	return "", false
}

// skippedSecretCountOrder is the order count entries follow the named
// entries in.
var skippedSecretCountOrder = []string{"hub", "broker", "user"}

// formatSkippedSecretWarning is the only producer of the skipped-secret
// warning text. It returns "" when nothing was skipped. An item whose scope
// is none of the four kinds is left out (the WARN log still lists it). verb is "started",
// "restarted" or "created".
//
// Audience: a project-scope item is listed by name for every requester; a
// user-scope item is listed by name only when audienceIsOwner (the
// requester is the agent's owner), else counted; hub- and broker-scope
// items are always counted. Named entries are de-duplicated and sorted by
// name then scope kind; count entries follow in the order hub, broker,
// user.
func formatSkippedSecretWarning(skipped []skippedSecret, verb string, audienceIsOwner bool) string {
	type entry struct{ name, kind string }
	seen := make(map[entry]bool)
	var named []entry
	counts := make(map[string]int)
	for _, s := range skipped {
		kind, ok := skippedSecretScopeKind(s.Scope)
		if !ok {
			continue
		}
		e := entry{s.Name, kind}
		if seen[e] {
			continue
		}
		seen[e] = true
		if kind == "project" || (kind == "user" && audienceIsOwner) {
			named = append(named, e)
		} else {
			counts[kind]++
		}
	}
	if len(named) == 0 && len(counts) == 0 {
		return ""
	}
	sort.Slice(named, func(i, j int) bool {
		if named[i].name != named[j].name {
			return named[i].name < named[j].name
		}
		return named[i].kind < named[j].kind
	})
	items := make([]string, 0, len(named)+len(counts))
	for _, e := range named {
		items = append(items, e.name+" ("+e.kind+")")
	}
	for _, kind := range skippedSecretCountOrder {
		switch n := counts[kind]; {
		case n == 1:
			items = append(items, "1 "+kind+"-scope secret")
		case n > 1:
			items = append(items, fmt.Sprintf("%d %s-scope secrets", n, kind))
		}
	}
	return "secrets not delivered because the secret store could not return them: " +
		strings.Join(items, ", ") + "; the agent was " + verb + " without them"
}

// skippedSecretLogItem is one entry of the WARN log line's items list.
type skippedSecretLogItem struct {
	Name  string `json:"name"`
	Scope string `json:"scope"`
	Class string `json:"class"`
}

// skippedSecretLogScopeUnknown is the log's scope kind for an item whose
// store scope is none of the four kinds. Such an item is never named in
// the response; the log keeps it so an operator can see it.
const skippedSecretLogScopeUnknown = "unknown"

// skippedSecretLogItems returns the log entries for skipped: every name,
// whatever the audience, de-duplicated and sorted by name then scope kind.
func skippedSecretLogItems(skipped []skippedSecret) []skippedSecretLogItem {
	seen := make(map[skippedSecretLogItem]bool)
	items := make([]skippedSecretLogItem, 0, len(skipped))
	for _, s := range skipped {
		kind, ok := skippedSecretScopeKind(s.Scope)
		if !ok {
			kind = skippedSecretLogScopeUnknown
		}
		it := skippedSecretLogItem{Name: s.Name, Scope: kind, Class: s.Class}
		if seen[it] {
			continue
		}
		seen[it] = true
		items = append(items, it)
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].Name != items[j].Name {
			return items[i].Name < items[j].Name
		}
		if items[i].Scope != items[j].Scope {
			return items[i].Scope < items[j].Scope
		}
		return items[i].Class < items[j].Class
	})
	return items
}

// logSkippedSecrets writes the one WARN line a dispatch logs for its
// skipped secrets. It does nothing when nothing was skipped.
func logSkippedSecrets(ctx context.Context, log *slog.Logger, agentID string, purpose MaterialPurpose, skipped []skippedSecret) {
	items := skippedSecretLogItems(skipped)
	if len(items) == 0 || log == nil {
		return
	}
	log.WarnContext(ctx, "launch material skipped: secret store read failed",
		"agent_id", agentID, "purpose", string(purpose), "count", len(items), "items", items)
}

// skippedSecretInitiatorKey carries, on an owner node executing a
// cross-node dispatch, the initiating principal recorded on the dispatch
// row.
type skippedSecretInitiatorKey struct{}

type skippedSecretInitiator struct{ kind, id string }

// withDispatchRowInitiator returns ctx carrying the dispatch row's initiator
// attribution, for skippedSecretAudienceIsOwner on the owner node.
func withDispatchRowInitiator(ctx context.Context, d *store.BrokerDispatch) context.Context {
	if d == nil {
		return ctx
	}
	return context.WithValue(ctx, skippedSecretInitiatorKey{}, skippedSecretInitiator{kind: d.InitiatorPrincipalKind, id: d.InitiatorPrincipalID})
}

// skippedSecretAudienceIsOwner reports whether the requester of this
// dispatch is agent's owner. On an owner node executing a dispatch row, the
// requester is the row's initiator attribution; otherwise it is the request
// principal. Any other or missing principal (an agent, a broker, a system
// dispatch, a row without attribution) is not the owner.
func skippedSecretAudienceIsOwner(ctx context.Context, agent *store.Agent) bool {
	if agent == nil || agent.OwnerID == "" || ctx == nil {
		return false
	}
	if init, ok := ctx.Value(skippedSecretInitiatorKey{}).(skippedSecretInitiator); ok {
		return isAgentOwnerPrincipal(init.kind, init.id, agent)
	}
	identity := GetIdentityFromContext(ctx)
	if isNilIdentity(identity) {
		return false
	}
	return isAgentOwnerPrincipal(identity.Type(), identity.ID(), agent)
}

// isAgentOwnerPrincipal reports whether the principal of kind and id is
// agent's owner: a user, or a workstation dev-auth user (dev auth is a
// single local user, whose agents it owns), whose ID is agent.OwnerID.
func isAgentOwnerPrincipal(kind, id string, agent *store.Agent) bool {
	if kind != string(PrincipalKindUser) && kind != string(PrincipalKindDev) {
		return false
	}
	return id != "" && id == agent.OwnerID
}
