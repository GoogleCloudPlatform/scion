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

package config

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMoveBundleConfig_Defaults(t *testing.T) {
	var zero MoveBundleConfig
	exp := zero.ExportLimits()
	assert.Equal(t, int64(4<<30), exp.WorkspaceMaxBytes)
	assert.Equal(t, int64(250_000), exp.WorkspaceMaxEntries)
	assert.Equal(t, int64(1<<30), exp.HomeMaxBytes)
	assert.Equal(t, int64(100_000), exp.HomeMaxEntries)
	assert.Equal(t, 64, exp.MaxDepth)
	assert.Equal(t, 4096, exp.MaxPathLen)
	assert.Equal(t, int64(64<<20), exp.PartSize)
	assert.Equal(t, 1200, exp.TimeoutSeconds)
	assert.Equal(t, 120, exp.PartStallSeconds)

	imp := zero.ImportLimits()
	assert.Equal(t, exp, imp)
}

func TestMoveBundleConfig_OverridesAndTimeouts(t *testing.T) {
	cfg := MoveBundleConfig{
		WorkspaceMaxBytes:   2 << 30,
		WorkspaceMaxEntries: 50_000,
		HomeMaxBytes:        512 << 20,
		HomeMaxEntries:      25_000,
		MaxDepth:            32,
		MaxPathLen:          2048,
		PartSize:            32 << 20,
		ExportTimeout:       15 * time.Minute,
		ImportTimeout:       30 * time.Minute,
		PartStallTimeout:    90 * time.Second,
	}

	exp := cfg.ExportLimits()
	assert.Equal(t, int64(2<<30), exp.WorkspaceMaxBytes)
	assert.Equal(t, int64(50_000), exp.WorkspaceMaxEntries)
	assert.Equal(t, int64(512<<20), exp.HomeMaxBytes)
	assert.Equal(t, int64(25_000), exp.HomeMaxEntries)
	assert.Equal(t, 32, exp.MaxDepth)
	assert.Equal(t, 2048, exp.MaxPathLen)
	assert.Equal(t, int64(32<<20), exp.PartSize)
	assert.Equal(t, 900, exp.TimeoutSeconds)
	assert.Equal(t, 90, exp.PartStallSeconds)

	imp := cfg.ImportLimits()
	assert.Equal(t, 1800, imp.TimeoutSeconds)
	assert.Equal(t, 90, imp.PartStallSeconds)
}

func TestMoveBundleConfig_SubSecondDurationsRoundUp(t *testing.T) {
	cfg := MoveBundleConfig{
		ExportTimeout:    500 * time.Millisecond,
		ImportTimeout:    1 * time.Millisecond,
		PartStallTimeout: 1500 * time.Millisecond,
	}
	exp := cfg.ExportLimits()
	assert.Equal(t, 1, exp.TimeoutSeconds)
	assert.Equal(t, 2, exp.PartStallSeconds)
	assert.NoError(t, exp.Validate())

	imp := cfg.ImportLimits()
	assert.Equal(t, 1, imp.TimeoutSeconds)
	assert.Equal(t, 2, imp.PartStallSeconds)
	assert.NoError(t, imp.Validate())
}

func TestV1ServerConfig_ReincarnateMoveBundleRoundTrip(t *testing.T) {
	v1 := &V1ServerConfig{
		Reincarnate: &V1ReincarnateConfig{
			MoveBundle: &V1MoveBundleConfig{
				WorkspaceMaxBytes:   8 << 30,
				WorkspaceMaxEntries: 300_000,
				HomeMaxBytes:        2 << 30,
				HomeMaxEntries:      150_000,
				MaxDepth:            48,
				MaxPathLen:          3072,
				PartSize:            128 << 20,
				ExportTimeout:       "25m",
				ImportTimeout:       "35m",
				PartStallTimeout:    "3m",
			},
		},
	}

	gc := ConvertV1ServerToGlobalConfig(v1)
	assert.Equal(t, int64(8<<30), gc.Reincarnate.MoveBundle.WorkspaceMaxBytes)
	assert.Equal(t, int64(300_000), gc.Reincarnate.MoveBundle.WorkspaceMaxEntries)
	assert.Equal(t, int64(2<<30), gc.Reincarnate.MoveBundle.HomeMaxBytes)
	assert.Equal(t, int64(150_000), gc.Reincarnate.MoveBundle.HomeMaxEntries)
	assert.Equal(t, 48, gc.Reincarnate.MoveBundle.MaxDepth)
	assert.Equal(t, 3072, gc.Reincarnate.MoveBundle.MaxPathLen)
	assert.Equal(t, int64(128<<20), gc.Reincarnate.MoveBundle.PartSize)
	assert.Equal(t, 25*time.Minute, gc.Reincarnate.MoveBundle.ExportTimeout)
	assert.Equal(t, 35*time.Minute, gc.Reincarnate.MoveBundle.ImportTimeout)
	assert.Equal(t, 3*time.Minute, gc.Reincarnate.MoveBundle.PartStallTimeout)

	back := ConvertGlobalToV1ServerConfig(gc)
	require.NotNil(t, back.Reincarnate)
	require.NotNil(t, back.Reincarnate.MoveBundle)
	assert.Equal(t, int64(8<<30), back.Reincarnate.MoveBundle.WorkspaceMaxBytes)
	assert.Equal(t, "25m0s", back.Reincarnate.MoveBundle.ExportTimeout)
	assert.Equal(t, "35m0s", back.Reincarnate.MoveBundle.ImportTimeout)
	assert.Equal(t, "3m0s", back.Reincarnate.MoveBundle.PartStallTimeout)

	// Zero config omits server.reincarnate on round-trip.
	empty := ConvertGlobalToV1ServerConfig(&GlobalConfig{})
	assert.Nil(t, empty.Reincarnate)
}
