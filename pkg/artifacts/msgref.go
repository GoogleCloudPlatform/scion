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

package artifacts

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
)

// Artifact references in messages (design D10).
//
// A message names artifacts in its metadata under MessageMetadataKey: a JSON
// array of reference strings, scion://artifact/<id>[@<seq>]. A reference
// carries the artifact id and optionally a version, nothing else. Whoever
// renders a reference (the web chat for a viewer, the hub for an agent
// recipient) resolves it with ResolveRefs under that reader's own identity,
// so a reference never grants access and never tells a reader anything about
// an artifact it cannot read.

// MessageMetadataKey is the message metadata key that carries artifact
// references. The hub owns it: it rewrites the value at every send site
// that accepts it and removes it everywhere else.
const MessageMetadataKey = "artifacts"

// MaxMessageRefs is the most artifact references one message may carry.
const MaxMessageRefs = 10

// MessageRef is one artifact reference in a message. Seq is 0 when the
// reference names the artifact's current version.
type MessageRef struct {
	ArtifactID string
	Seq        int
}

// String returns the canonical reference string.
func (r MessageRef) String() string { return FormatRef(r.ArtifactID, r.Seq) }

// ParseMessageRef parses one reference from message metadata. Unlike
// ParseRef it requires the scion://artifact/ scheme and no surrounding
// space, so only the canonical form reaches a message.
func ParseMessageRef(s string) (MessageRef, error) {
	if !strings.HasPrefix(s, RefScheme) || strings.TrimSpace(s) != s {
		return MessageRef{}, ErrBadRef
	}
	id, seq, err := ParseRef(s)
	if err != nil {
		return MessageRef{}, err
	}
	return MessageRef{ArtifactID: id, Seq: seq}, nil
}

// ParseMessageRefs decodes a MessageMetadataKey value. It returns the
// well-formed references in order, without duplicates (a second reference
// to the same artifact is dropped, whatever its version) and at most
// MaxMessageRefs of them. dropped counts every entry that was not returned:
// malformed, duplicate or over the limit. A value that is not a JSON array
// of strings yields no references and dropped = 1.
func ParseMessageRefs(value string) (refs []MessageRef, dropped int) {
	if value == "" {
		return nil, 0
	}
	var raw []string
	if err := json.Unmarshal([]byte(value), &raw); err != nil {
		return nil, 1
	}
	seen := make(map[string]bool, len(raw))
	for _, s := range raw {
		r, err := ParseMessageRef(s)
		if err != nil || seen[r.ArtifactID] || len(refs) >= MaxMessageRefs {
			dropped++
			continue
		}
		seen[r.ArtifactID] = true
		refs = append(refs, r)
	}
	return refs, dropped
}

// EncodeMessageRefs encodes references as a MessageMetadataKey value. It
// returns "" for no references.
func EncodeMessageRefs(refs []MessageRef) string {
	if len(refs) == 0 {
		return ""
	}
	out := make([]string, len(refs))
	for i, r := range refs {
		out[i] = r.String()
	}
	b, _ := json.Marshal(out)
	return string(b)
}

// RefView is a reference as one reader sees it. When Available is false
// the reader may not read the artifact, or it (or the named version) does
// not exist or is not ready; the two cases look the same and every field
// past Seq is empty.
type RefView struct {
	// Ref is the canonical reference string.
	Ref string `json:"ref"`
	// ID is the artifact id.
	ID string `json:"id"`
	// Seq is the version the reference pins, or 0 for the current one.
	Seq int `json:"seq,omitempty"`
	// Available reports whether the reader may read the referenced version.
	Available bool `json:"available"`
	// Title is the artifact's title.
	Title string `json:"title,omitempty"`
	// Version is the version the reference resolves to: Seq when pinned,
	// otherwise the artifact's current version.
	Version int `json:"version,omitempty"`
	// OwnerKind and OwnerRef identify the artifact's owner.
	OwnerKind string `json:"ownerKind,omitempty"`
	OwnerRef  string `json:"ownerRef,omitempty"`
}

// ResolveRefs resolves references for the caller of ctx, with the same
// read check as GET /api/v1/artifacts/{id}. It returns one view per
// reference, in order. When the service has no store yet every view is
// unavailable.
func (s *Service) ResolveRefs(ctx context.Context, refs []MessageRef) []RefView {
	out := make([]RefView, len(refs))
	b, ok := s.backend()
	for i, r := range refs {
		out[i] = RefView{Ref: r.String(), ID: r.ArtifactID, Seq: r.Seq}
		if !ok || b.store == nil {
			continue
		}
		s.resolveRef(ctx, b, r, &out[i])
	}
	return out
}

func (s *Service) resolveRef(ctx context.Context, b backend, r MessageRef, v *RefView) {
	if !canonicalID(r.ArtifactID) || r.Seq < 0 {
		return
	}
	a, err := b.store.GetArtifact(ctx, r.ArtifactID)
	if err != nil {
		if !errors.Is(err, ErrNotFound) {
			slog.ErrorContext(ctx, "artifacts: resolve ref: get artifact failed", "error", err)
		}
		return
	}
	if !s.canRead(ctx, b, a) {
		return
	}
	seq := r.Seq
	if seq == 0 {
		seq = a.CurrentSeq
	}
	if seq == 0 {
		return
	}
	if r.Seq != 0 {
		ver, err := b.store.GetVersion(ctx, a.ID, seq)
		if err != nil || ver.State != VersionStateReady {
			if err != nil && !errors.Is(err, ErrNotFound) {
				slog.ErrorContext(ctx, "artifacts: resolve ref: get version failed", "error", err)
			}
			return
		}
	}
	v.Available = true
	v.Title = a.Title
	v.Version = seq
	v.OwnerKind = a.OwnerKind
	v.OwnerRef = a.OwnerRef
}
