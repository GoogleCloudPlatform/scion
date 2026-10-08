// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//go:build !no_sqlite

package hub

import (
	"context"
	"database/sql"
	"net/http"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// An admin commit adopts a legacy edge whose stored updated text (SQLite) is
// not in canonical form but names the same instant.
func TestDelegationAdoptionCommitAdoptsNonCanonicalUpdatedText(t *testing.T) {
	inst := time.Date(2026, 9, 18, 1, 24, 24, 503338758, time.UTC)
	cest := time.FixedZone("CEST", 2*3600)
	for name, text := range map[string]string{
		"rfc3339-z":           inst.Format(time.RFC3339Nano),
		"rfc3339-offset":      inst.In(cest).Format(time.RFC3339Nano),
		"go-string-monotonic": inst.String() + " m=+12.345678901",
	} {
		t.Run(name, func(t *testing.T) {
			f := newLegacyFixture(t, "adopt-ts-"+name)
			admin := adoptionAdmin(t, f.store, "adopt-ts-admin-"+name)
			original := activeEdgesFor(t, f.store, f.legacy.ID)[0]
			dbs, ok := f.store.(interface{ DB() *sql.DB })
			require.True(t, ok, "the test store exposes its database")
			_, err := dbs.DB().ExecContext(context.Background(),
				"UPDATE delegation_edges SET updated = ? WHERE id = ?", text, original.ID)
			require.NoError(t, err)
			var raw string
			require.NoError(t, dbs.DB().QueryRowContext(context.Background(),
				"SELECT CAST(updated AS TEXT) FROM delegation_edges WHERE id = ?", original.ID).Scan(&raw))
			require.Equal(t, text, raw)

			body := adoptBody(f.legacy.ID)
			p := f.adoptionPreview(t, admin, body)
			require.Len(t, p.Hops, 1)
			require.Equal(t, "adopt", p.Hops[0].Outcome)
			rec := f.adoptionCommit(t, admin, withFingerprint(body, p))
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			var commit delegationAdoptionCommitResponse
			decodeJSONBody(t, rec, &commit)
			assert.Equal(t, 1, commit.Committed)
			require.Len(t, commit.Records, 1)
			assert.Equal(t, store.DelegationAdoptionAdopted, commit.Records[0].Status)

			e := activeEdgesFor(t, f.store, f.legacy.ID)[0]
			assert.NotEqual(t, original.ID, e.ID)
			assert.Equal(t, store.EffectCeilingBounded, e.Kind)
			assert.Equal(t, admin.ID, e.InitiatorPrincipalID)
		})
	}
}
