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
	"sort"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
)

// Skill content hash v2 (ptone/scion#5935).
//
// A skill version has two content hashes, both "sha256:<hex64>":
//
//   - v1 (computeSkillContentHash): file hashes only. It is what
//     skill_versions.content_hash records and what every contentHash field
//     carries, because brokers before ptone/scion#5935 verify an installed
//     bundle by recomputing it and existing registry pins hold it.
//   - v2 (computeSkillContentHashV2): paths, normalised modes and file
//     hashes. It is derived from the immutable file list, never stored.
//
// While the hub.skill_content_hash_v2 experiment is off the API is the one
// before ptone/scion#5935. While it is on, responses add contentHashV2, and
// an "@sha256:" version and a pinned registry hash match either form, v1
// first.

// skillContentHashV2Enabled reports whether the hub.skill_content_hash_v2
// experiment is on. The experiment is read at every call, so toggling it
// needs no restart.
func (s *Server) skillContentHashV2Enabled() bool {
	return s != nil && s.experimentEnabled(experiments.SkillContentHashV2)
}

// skillVersionHasContent reports whether sv is a published or deprecated
// version with files, the versions that have a content hash.
func skillVersionHasContent(sv *store.SkillVersion) bool {
	if sv == nil || len(sv.Files) == 0 {
		return false
	}
	return sv.Status == store.SkillVersionStatusPublished || sv.Status == store.SkillVersionStatusDeprecated
}

// withSkillContentHashV2 sets sv.ContentHashV2 for a response while the
// experiment is on, and clears it while it is off, so the field is never
// sent with the experiment off. It returns sv.
func (s *Server) withSkillContentHashV2(sv *store.SkillVersion) *store.SkillVersion {
	if sv == nil {
		return nil
	}
	sv.ContentHashV2 = ""
	if s.skillContentHashV2Enabled() && skillVersionHasContent(sv) {
		sv.ContentHashV2 = computeSkillContentHashV2(sv.Files)
	}
	return sv
}

// resolveSkillVersion resolves a version constraint of skillID like
// store.ResolveSkillVersion. While the experiment is on, a "sha256:"
// constraint that matches no version's v1 hash is matched against the v2
// hash of each published or deprecated version (v1 first, then v2). When
// several versions share that v2 (the same files republished), the oldest
// wins (created ascending, then ID). The store's v1 lookup uses the same
// status set and order (ptone/scion#6248).
func (s *Server) resolveSkillVersion(ctx context.Context, skillID, constraint string) (*store.SkillVersion, error) {
	sv, err := s.store.ResolveSkillVersion(ctx, skillID, constraint)
	if err == nil || !errors.Is(err, store.ErrNotFound) ||
		!transfer.IsContentHash(constraint) || !s.skillContentHashV2Enabled() {
		return sv, err
	}
	notFound := err

	versions, listErr := s.listAllSkillVersions(ctx, skillID)
	if listErr != nil {
		return nil, listErr
	}
	var matches []store.SkillVersion
	for i := range versions {
		if skillVersionHasContent(&versions[i]) && computeSkillContentHashV2(versions[i].Files) == constraint {
			matches = append(matches, versions[i])
		}
	}
	if len(matches) == 0 {
		return nil, notFound
	}
	sort.SliceStable(matches, func(i, j int) bool {
		if !matches[i].Created.Equal(matches[j].Created) {
			return matches[i].Created.Before(matches[j].Created)
		}
		return matches[i].ID < matches[j].ID
	})
	return &matches[0], nil
}

// listAllSkillVersions returns every version of skillID, in any status.
func (s *Server) listAllSkillVersions(ctx context.Context, skillID string) ([]store.SkillVersion, error) {
	result, err := s.store.ListSkillVersions(ctx, skillID, store.ListOptions{})
	if err != nil {
		return nil, err
	}
	if result.TotalCount > len(result.Items) {
		result, err = s.store.ListSkillVersions(ctx, skillID, store.ListOptions{Limit: result.TotalCount})
		if err != nil {
			return nil, err
		}
	}
	return result.Items, nil
}

// checkFederatedSkillHashes checks the content hashes of a skill resolved
// from the federated registry and enforces a pinned registry's pin. It
// never passes the registry's own contentHashV2 claim through.
//
// While the experiment is off it behaves as before ptone/scion#5935: a
// pinned registry's pin must equal the claimed contentHash (v1). A claimed
// v1 that differs from the v1 recomputed from the file list is only logged;
// the broker's own v1 check rejects such a bundle at install.
//
// While it is on, the hub recomputes v1 and v2 from the file list (paths,
// per-file hashes and modes; a missing mode is "0644", as the broker
// installs it). A claimed v1, or a claimed v2, that disagrees fails
// closed, at every trust level. A skill with no claimed contentHash whose
// hashes cannot be recomputed fails too, since nothing would verify it. A
// pinned registry additionally needs a file list it can recompute from,
// and its pin must equal the recomputed v1 or v2. The recomputed v2 is
// then sent as contentHashV2. Because the broker checks
// every file against its per-file hash and installs it with the listed
// mode, the installed files match what the pin was matched against, also
// on a broker that checks v1 only.
func (s *Server) checkFederatedSkillHashes(ctx context.Context, registry *store.SkillRegistry, registryName string, skillRef ResolveSkillRef, resolved *ResolvedSkillResponse) *ResolveSkillError {
	claimedV2 := resolved.ContentHashV2
	resolved.ContentHashV2 = ""
	pinned := registry.TrustLevel == store.SkillRegistryTrustPinned
	v1, v2, recomputed := resolvedSkillContentHashes(resolved.Files)

	trustViolation := func(detail string) *ResolveSkillError {
		return newSkillResolveError(ctx, skillResolveSourceFederation, skillRef.URI,
			agent.SkillErrCodeTrustViolation, 0, detail)
	}
	pinnedHash := func() (string, *ResolveSkillError) {
		pin, err := s.store.GetPinnedHash(ctx, registry.ID, skillRef.URI)
		if err != nil {
			return "", trustViolation(fmt.Sprintf("no pinned hash for %q in registry %q; use 'scion skills registries pin' first", skillRef.URI, registryName))
		}
		return pin, nil
	}

	if !s.skillContentHashV2Enabled() {
		if recomputed && resolved.ContentHash != v1 {
			slog.WarnContext(ctx, "federated skill content hash does not match its files",
				"registry", registryName, "uri", skillRef.URI,
				"claimed", resolved.ContentHash, "recomputed", v1)
		}
		if !pinned {
			return nil
		}
		pin, rerr := pinnedHash()
		if rerr != nil {
			return rerr
		}
		if resolved.ContentHash != pin {
			return trustViolation(fmt.Sprintf("content hash mismatch for %q from registry %q: expected %s, got %s",
				skillRef.URI, registryName, pin, resolved.ContentHash))
		}
		return nil
	}

	if !recomputed && resolved.ContentHash == "" {
		// Nothing would verify this skill: the hub cannot recompute a hash
		// and the broker has no bundle hash to check against.
		return trustViolation(fmt.Sprintf("cannot verify %q from registry %q: it sent no content hash and did not list a hash for every file",
			skillRef.URI, registryName))
	}
	if recomputed {
		if resolved.ContentHash != v1 {
			return trustViolation(fmt.Sprintf("content hash %s for %q from registry %q does not match its files (%s)",
				resolved.ContentHash, skillRef.URI, registryName, v1))
		}
		if claimedV2 != "" && claimedV2 != v2 {
			return trustViolation(fmt.Sprintf("content hash v2 %s for %q from registry %q does not match its files (%s)",
				claimedV2, skillRef.URI, registryName, v2))
		}
	}
	if pinned {
		pin, rerr := pinnedHash()
		if rerr != nil {
			return rerr
		}
		if !recomputed {
			return trustViolation(fmt.Sprintf("cannot verify %q from registry %q against its pin: the registry did not list a hash for every file",
				skillRef.URI, registryName))
		}
		switch pin {
		case v1:
			if v1 != v2 {
				slog.InfoContext(ctx, "federated skill matched a v1 pin; re-pin with its v2 hash to also cover paths and modes",
					"registry", registryName, "uri", skillRef.URI, "content_hash_v2", v2)
			}
		case v2:
		default:
			return trustViolation(fmt.Sprintf("content hash mismatch for %q from registry %q: expected %s, got %s (v2 %s)",
				skillRef.URI, registryName, pin, v1, v2))
		}
	}
	if recomputed {
		resolved.ContentHashV2 = v2
	}
	return nil
}
