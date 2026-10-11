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

package entadapter

import (
	"fmt"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
)

// fixtureStampColumns returns the fixture_root_id and fixture_issuer_id
// values a create writes for p: both nil for an empty stamp, both set for a
// complete one. A half-set stamp or a malformed ID is refused with
// store.ErrInvalidInput, so a row never carries a partial stamp.
//
// Only the CreateAgent, CreateProject, CreateSchedule and CreateGroup
// builders call it; the columns are Immutable, so no update builder can
// write them.
func fixtureStampColumns(p store.FixtureProvenance) (root, issuer *uuid.UUID, err error) {
	if !p.Valid() {
		return nil, nil, fmt.Errorf("%w: fixture provenance must set both the root and the issuer, or neither", store.ErrInvalidInput)
	}
	if p.IsZero() {
		return nil, nil, nil
	}
	r, err := parseUUID(p.RootID)
	if err != nil {
		return nil, nil, err
	}
	i, err := parseUUID(p.IssuerID)
	if err != nil {
		return nil, nil, err
	}
	return &r, &i, nil
}

// fixtureProvenanceFromColumns is the read side of fixtureStampColumns.
func fixtureProvenanceFromColumns(root, issuer *uuid.UUID) store.FixtureProvenance {
	var p store.FixtureProvenance
	if root != nil {
		p.RootID = root.String()
	}
	if issuer != nil {
		p.IssuerID = issuer.String()
	}
	return p
}
