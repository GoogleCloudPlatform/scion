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
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// The global project's slug identifies it to the hub and, through dispatch,
// to the broker (see isGlobalHubProject). It is reserved: client project
// routes cannot create or rename a project to it, in any letter case. Only
// the global project's own creation takes it: the combined hub and broker
// server's store write, and project register without a git remote (the CLI
// global-project flow).

// reservedProjectSlugMessage explains a refused reserved slug.
var reservedProjectSlugMessage = fmt.Sprintf("project slug %q is reserved for the global project", globalProjectSlug)

// isReservedProjectSlug reports whether slug is the reserved global slug, in
// any letter case.
func isReservedProjectSlug(slug string) bool {
	return strings.EqualFold(slug, globalProjectSlug)
}

// nextAvailableUnreservedSlug is store.NextAvailableSlug for a slug derived
// from a name: it never returns the reserved slug. A reserved base starts at
// "<base>-1".
func (s *Server) nextAvailableUnreservedSlug(ctx context.Context, baseSlug string) (string, error) {
	if !isReservedProjectSlug(baseSlug) {
		return s.store.NextAvailableSlug(ctx, baseSlug)
	}
	for i := 1; ; i++ {
		candidate := fmt.Sprintf("%s-%d", globalProjectSlug, i)
		_, err := s.store.GetProjectBySlug(ctx, candidate)
		if errors.Is(err, store.ErrNotFound) {
			return candidate, nil
		}
		if err != nil {
			return "", err
		}
	}
}
