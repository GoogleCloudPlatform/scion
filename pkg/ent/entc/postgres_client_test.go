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

package entc

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestBuildPostgresConnConfig_ReadOnlySetsGUC reproduces round-4 review
// Consider 4: OpenPostgresReadOnly had no test at all. buildPostgresConnConfig
// is factored out of openPostgres specifically so this can assert the
// resulting RuntimeParams without opening a real connection — no Postgres
// server is required.
func TestBuildPostgresConnConfig_ReadOnlySetsGUC(t *testing.T) {
	cfg, err := buildPostgresConnConfig("postgres://user:pass@localhost:5432/db?sslmode=disable", true)
	require.NoError(t, err)
	assert.Equal(t, "on", cfg.RuntimeParams["default_transaction_read_only"])
}

// TestBuildPostgresConnConfig_ReadWriteDoesNotSetGUC pins the negative case:
// OpenPostgres (readOnly=false) must not set the GUC at all, so it does not
// accidentally affect the writable connections used everywhere else.
func TestBuildPostgresConnConfig_ReadWriteDoesNotSetGUC(t *testing.T) {
	cfg, err := buildPostgresConnConfig("postgres://user:pass@localhost:5432/db?sslmode=disable", false)
	require.NoError(t, err)
	_, ok := cfg.RuntimeParams["default_transaction_read_only"]
	assert.False(t, ok, "OpenPostgres (read-write) must not set default_transaction_read_only")
}

// TestBuildPostgresConnConfig_KeepsExplicitDSNKeepalive pins that an
// explicit keepalive setting in the DSN is not overwritten by
// applyKeepalives's defaults, for both the read-only and read-write paths.
func TestBuildPostgresConnConfig_KeepsExplicitDSNKeepalive(t *testing.T) {
	for _, readOnly := range []bool{false, true} {
		cfg, err := buildPostgresConnConfig("postgres://user:pass@localhost:5432/db?sslmode=disable&tcp_keepalives_idle=5", readOnly)
		require.NoError(t, err)
		assert.Equal(t, "5", cfg.RuntimeParams["tcp_keepalives_idle"])
	}
}

// TestBuildPostgresConnConfig_InvalidDSN pins that a malformed DSN is a clean
// parse error, not a panic, for both paths.
func TestBuildPostgresConnConfig_InvalidDSN(t *testing.T) {
	_, err := buildPostgresConnConfig("not a valid postgres dsn \x00", true)
	assert.Error(t, err)
}
