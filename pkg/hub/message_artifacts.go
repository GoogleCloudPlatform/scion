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
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/artifacts"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/messaging"
)

// Artifact references in messages (ptone/scion#3222, design D10).
//
// A message names artifacts in the hub-owned metadata key
// artifacts.MessageMetadataKey. The value reaches a message only through
// admitMessageArtifacts, at the send sites listed in
// artifactAdmittingSites (message_artifacts_coverage_test.go). Every other
// dispatch site removes it with messaging.StripReservedMetadata. A
// reference carries an artifact id and optionally a version; what a reader
// sees of it (title, version, owner) always comes from
// resolveArtifactRefs under that reader's own credential.

// artifactRefsDroppedWarning is the warning a sender gets when references
// were not attached on a hub where references are enabled. It says the same
// thing whatever the reason, so a reference to an artifact the sender cannot
// read gets the same warning as one to an artifact that does not exist.
const artifactRefsDroppedWarning = "%d artifact reference(s) not attached: each must be a scion://artifact/<id>[@<seq>] reference you can read, at most %d per message"

// artifactRefsDisabledWarning is the warning when references are not
// enabled on the hub at all. It depends on no reference.
const artifactRefsDisabledWarning = "artifact references are not enabled on this hub: %d reference(s) not attached"

// artifactRefsUncheckedWarning is the warning when references could not be
// checked because a store read failed (artifacts.ResolveError). Like
// artifactRefsDroppedWarning it carries a count only.
const artifactRefsUncheckedWarning = "%d artifact reference(s) not attached: they could not be checked because of a server error; try again"

// artifactRefsWarning returns the warning for references dropped as
// unreadable or malformed and for references that could not be checked,
// or "" when there are none.
func artifactRefsWarning(dropped, unchecked int) string {
	var parts []string
	if dropped > 0 {
		parts = append(parts, fmt.Sprintf(artifactRefsDroppedWarning, dropped, artifacts.MaxMessageRefs))
	}
	if unchecked > 0 {
		parts = append(parts, fmt.Sprintf(artifactRefsUncheckedWarning, unchecked))
	}
	return strings.Join(parts, "; ")
}

// artifactRefsDisabled returns the warning for references sent while the
// feature is off, or "".
func artifactRefsDisabled(dropped int) string {
	if dropped <= 0 {
		return ""
	}
	return fmt.Sprintf(artifactRefsDisabledWarning, dropped)
}

// artifactRefsActive reports whether messages may carry artifact references
// on this hub: the hub.artifacts experiment is on, the artifacts settings
// section is enabled and an artifact store is installed.
func (s *Server) artifactRefsActive() bool {
	return s.experimentEnabled(experiments.Artifacts) && s.artifactsConfig().Enabled && s.ArtifactStore() != nil
}

// resolveArtifactRefs resolves refs for the caller of ctx with the artifact
// service's own read check (the one GET /api/v1/artifacts/{id} applies).
// It is the only place a reference's title, version or owner is looked up,
// for send-time admission and for every web view.
//
// It resolves only for a request-authenticated caller
// (requestCredentialBindsIdentity): ctx's identity must be the one the
// authentication middleware derived from the request's credentials and
// recorded its credential context for. A context the hub built in process,
// holding only an identity, or a request context whose identity was
// replaced, resolves nothing, so artifacts.Host keeps seeing only
// request-derived identities. With references inactive every view is
// unavailable too.
//
// unchecked counts the references that could not be checked because a
// store read failed (artifacts.ResolveError); their views are unavailable.
func (s *Server) resolveArtifactRefs(ctx context.Context, refs []artifacts.MessageRef) (views []artifacts.RefView, unchecked int) {
	if len(refs) == 0 {
		return nil, 0
	}
	if !s.artifactRefsActive() || !requestCredentialBindsIdentity(ctx) {
		out := make([]artifacts.RefView, len(refs))
		for i, r := range refs {
			out[i] = artifacts.RefView{Ref: r.String(), ID: r.ArtifactID, Seq: r.Seq}
		}
		return out, 0
	}
	views, err := s.artifactRefResolver().ResolveRefs(ctx, refs)
	if err != nil {
		var resErr *artifacts.ResolveError
		if !errors.As(err, &resErr) {
			// ResolveRefs returns only *ResolveError; count every
			// reference if that ever changes.
			resErr = &artifacts.ResolveError{Unchecked: len(refs), Err: err}
		}
		slog.ErrorContext(ctx, "artifact references could not be checked", "unchecked", resErr.Unchecked, "error", err)
		unchecked = resErr.Unchecked
	}
	return views, unchecked
}

// admitMessageArtifacts is the only way artifact references enter a
// message. It returns md with every hub-reserved key removed
// (messaging.StripReservedMetadata), plus the artifact references from the
// client's value of artifacts.MessageMetadataKey that are well formed,
// within artifacts.MaxMessageRefs, and readable by the caller of ctx, which
// must be the sender's own request context. Admitted references are
// re-encoded canonically under the key; the key is absent when none are
// admitted. warning is "" when every reference was admitted, otherwise the
// sender-facing warning (artifactRefsWarning, which also counts references a
// failed store read left unchecked, or artifactRefsDisabled when
// references are off on this hub). md is never mutated.
func (s *Server) admitMessageArtifacts(ctx context.Context, md map[string]string) (out map[string]string, admitted []artifacts.MessageRef, warning string) {
	raw := md[artifacts.MessageMetadataKey]
	out = messaging.StripReservedMetadata(md)
	if raw == "" {
		return out, nil, ""
	}
	refs, dropped := artifacts.ParseMessageRefs(raw)
	if !s.artifactRefsActive() {
		return out, nil, artifactRefsDisabled(dropped + len(refs))
	}
	views, unchecked := s.resolveArtifactRefs(ctx, refs)
	for i, v := range views {
		if v.Available {
			admitted = append(admitted, refs[i])
		} else {
			dropped++
		}
	}
	// A reference that could not be checked is not attached (unavailable
	// above) and is warned about separately, as a server error.
	dropped -= unchecked
	if len(admitted) > 0 {
		next := make(map[string]string, len(out)+1)
		for k, v := range out {
			next[k] = v
		}
		next[artifacts.MessageMetadataKey] = artifacts.EncodeMessageRefs(admitted)
		out = next
	}
	if dropped > 0 || unchecked > 0 {
		slog.InfoContext(ctx, "message artifact references not attached", "dropped", dropped, "unchecked", unchecked, "admitted", len(admitted))
	}
	return out, admitted, artifactRefsWarning(dropped, unchecked)
}

// recordMessageArtifacts persists admitted references for a stored
// message, so the web chat can show them later, and returns the references
// it stored: refs, or nil when there is nothing to record, no artifact
// store, or the write failed. Callers publish the live chat event with the
// returned value, so the event never names a reference history would not
// return. A failure is logged, not returned: the message itself is already
// stored and its body still names the artifacts.
func (s *Server) recordMessageArtifacts(ctx context.Context, messageID string, refs []artifacts.MessageRef) []artifacts.MessageRef {
	if messageID == "" || len(refs) == 0 {
		return nil
	}
	st := s.ArtifactStore()
	if st == nil {
		return nil
	}
	if err := st.AddMessageRefs(ctx, messageID, refs); err != nil {
		slog.ErrorContext(ctx, "failed to record message artifact references", "message_id", messageID, "error", err)
		return nil
	}
	return refs
}

// chatArtifactRef is one artifact reference on a chat message, as the
// viewing user sees it. OwnerName is set only when Available.
type chatArtifactRef struct {
	artifacts.RefView
	OwnerName string `json:"ownerName,omitempty"`
}

// chatArtifactViews resolves refs for the caller of ctx and names the owner
// of every available artifact.
func (s *Server) chatArtifactViews(ctx context.Context, refs []artifacts.MessageRef) []chatArtifactRef {
	// A reference that could not be checked shows as unavailable;
	// resolveArtifactRefs logs the failure.
	views, _ := s.resolveArtifactRefs(ctx, refs)
	owners := map[string]string{}
	out := make([]chatArtifactRef, len(views))
	for i, v := range views {
		out[i] = chatArtifactRef{RefView: v}
		if v.Available {
			out[i].OwnerName = s.artifactOwnerName(ctx, v.OwnerKind, v.OwnerRef, owners)
		}
	}
	return out
}

// messageArtifactViews returns the recorded artifact references of
// messageIDs, keyed by message id, as the caller of ctx sees them. Messages
// without references are absent; nil when there are none or references are
// inactive.
//
// unavailable is true when the recorded references could not be read
// (ptone/scion#4295). The views are then nil: the caller reports the
// messages as "could not load artifact references", which a client must
// not take for "no references".
func (s *Server) messageArtifactViews(ctx context.Context, messageIDs []string) (views map[string][]chatArtifactRef, unavailable bool) {
	if len(messageIDs) == 0 || !s.artifactRefsActive() {
		return nil, false
	}
	st := s.ArtifactStore()
	if st == nil {
		return nil, false
	}
	recorded, err := st.ListMessageRefs(ctx, messageIDs)
	if err != nil {
		slog.ErrorContext(ctx, "failed to list message artifact references", "messages", len(messageIDs), "error", err)
		return nil, true
	}
	if len(recorded) == 0 {
		return nil, false
	}
	// Resolve each distinct reference once, however many messages carry it;
	// ResolveRefs then reads and checks each artifact once across versions.
	var unique []artifacts.MessageRef
	index := make(map[artifacts.MessageRef]int)
	for _, refs := range recorded {
		for _, r := range refs {
			if _, ok := index[r]; !ok {
				index[r] = len(unique)
				unique = append(unique, r)
			}
		}
	}
	resolved := s.chatArtifactViews(ctx, unique)
	out := make(map[string][]chatArtifactRef, len(recorded))
	for msgID, refs := range recorded {
		list := make([]chatArtifactRef, len(refs))
		for i, r := range refs {
			list[i] = resolved[index[r]]
		}
		out[msgID] = list
	}
	return out, false
}

// artifactOwnerName returns a display name for an artifact owner: the
// agent's name or the user's display name. cache memoizes lookups across
// one response.
func (s *Server) artifactOwnerName(ctx context.Context, kind, ref string, cache map[string]string) string {
	key := kind + ":" + ref
	if name, ok := cache[key]; ok {
		return name
	}
	name := ""
	switch kind {
	case artifacts.PrincipalKindAgent:
		if a, err := s.store.GetAgent(ctx, ref); err == nil && a != nil {
			name = a.Name
			if name == "" {
				name = a.Slug
			}
		}
	case artifacts.PrincipalKindUser:
		if u, err := s.store.GetUser(ctx, ref); err == nil && u != nil {
			name = u.DisplayName
		}
	}
	if cache != nil {
		cache[key] = name
	}
	return name
}
