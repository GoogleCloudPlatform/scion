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
	"context"
	"fmt"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/testinframarker"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// testInfraMarkerRowID is the fixed id of the single marker row
// (MarkerID of the offline marker package, which pkg/store cannot import).
const testInfraMarkerRowID = "test-infra-marker"

// GetTestInfraBinding reads the marker's hub id, class and creation time
// for the offline delegation redeem gate (ptone/scion#4240, phase DL).
//
// This file is the ONLY production file in pkg/store or pkg/hub that
// references the marker entity (TestTestInfraMarker_NoRouteOrHubCodeTouchesIt
// pins that). It is read-only: it selects exactly the three columns below,
// so the web session key and the attested facts are never loaded, and it
// has no create, update or delete. The marker is written only by the
// offline host commands.
func (s *TestIdentityDelegationStore) GetTestInfraBinding(ctx context.Context) (*store.TestInfraBinding, error) {
	m, err := s.client.TestInfraMarker.Query().
		Where(testinframarker.IDEQ(testInfraMarkerRowID)).
		Select(testinframarker.FieldHubID, testinframarker.FieldClass, testinframarker.FieldCreatedAt).
		Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, store.ErrNotFound
		}
		return nil, fmt.Errorf("read test infrastructure binding: %w", err)
	}
	return &store.TestInfraBinding{HubID: m.HubID, Class: m.Class, CreatedAt: m.CreatedAt}, nil
}
