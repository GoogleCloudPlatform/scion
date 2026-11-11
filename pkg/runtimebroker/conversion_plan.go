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

package runtimebroker

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"os"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/brokerconvert"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// A legacy Runtime Broker reports its plan for moving to flat Runtime
// Broker instances on every heartbeat (brokerconvert.Presentation), so the
// Hub can show it on the broker's page. The plan is computed from the same
// settings the heartbeat's service account report loads (the global
// settings plus the installed overlay) and the raw global settings file,
// and is recomputed only when they change: the first heartbeat computes it,
// and a later settings change, whether an edit of the file or a Hub reload
// of the overlay, is seen by the next heartbeat. A flat instance never
// reports one.

// conversionPlanCache is a legacy server's last computed plan.
type conversionPlanCache struct {
	mu sync.Mutex
	// fingerprint identifies the settings the snapshot was computed from:
	// a SHA-256 over the raw settings file and the canonical JSON of the
	// effective settings. It exists only in memory to detect a change; it
	// is never logged, sent or stored.
	fingerprint [sha256.Size]byte
	computed    bool
	snapshot    *api.ConversionPlanPresentation

	// loadRaw, configuredRuntimes and now replace reading the raw global
	// settings file, config.ConfiguredRuntimeNames and time.Now. Tests
	// set them before the first heartbeat.
	loadRaw            func() ([]byte, error)
	configuredRuntimes func() (map[string]bool, bool)
	now                func() time.Time
}

// heartbeatConversionPlan returns the plan snapshot for the next heartbeat,
// recomputing it when the settings changed since the last one. It returns
// nil, which omits the field and keeps the Hub's stored value, on a flat
// instance, when the settings cannot be read, or when the host already
// declares flat instances.
func (s *Server) heartbeatConversionPlan() *api.ConversionPlanPresentation {
	if s.isFlat() {
		return nil
	}
	vs, err := s.loadHeartbeatMappingSettings()
	if err != nil || vs == nil {
		return nil
	}
	c := &s.conversionPlan
	raw, err := c.readRaw()
	if err != nil {
		return nil
	}
	fp, ok := conversionPlanFingerprint(raw, vs)
	if !ok {
		return nil
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.computed && fp == c.fingerprint {
		return c.snapshot
	}
	in := brokerconvert.Input{Settings: vs, RawSettings: raw}
	if names, ok := c.configured(); ok {
		in.ConfiguredRuntimes = func(name string) bool { return names[name] }
	}
	c.fingerprint, c.computed, c.snapshot = fp, true, nil
	plan, err := brokerconvert.BuildLocal(in)
	if err != nil {
		return nil
	}
	c.snapshot = brokerconvert.Presentation(plan, c.clock())
	return c.snapshot
}

func (c *conversionPlanCache) readRaw() ([]byte, error) {
	if c.loadRaw != nil {
		return c.loadRaw()
	}
	globalDir, err := config.GetGlobalDir()
	if err != nil {
		return nil, err
	}
	path := config.GetSettingsPath(globalDir)
	if path == "" {
		return nil, nil
	}
	return os.ReadFile(path)
}

func (c *conversionPlanCache) configured() (map[string]bool, bool) {
	if c.configuredRuntimes != nil {
		return c.configuredRuntimes()
	}
	return config.ConfiguredRuntimeNames()
}

func (c *conversionPlanCache) clock() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

// conversionPlanFingerprint hashes the converter's inputs. ok is false when
// the effective settings do not encode.
func conversionPlanFingerprint(raw []byte, vs *config.VersionedSettings) (fp [sha256.Size]byte, ok bool) {
	eff, err := json.Marshal(vs)
	if err != nil {
		return fp, false
	}
	h := sha256.New()
	var n [8]byte
	binary.BigEndian.PutUint64(n[:], uint64(len(raw)))
	h.Write(n[:])
	h.Write(raw)
	h.Write(eff)
	copy(fp[:], h.Sum(nil))
	return fp, true
}
