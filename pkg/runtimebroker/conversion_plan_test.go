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
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/brokerconvert"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// conversionPlanSettings is a legacy host with two Docker profiles and a
// Kubernetes profile; registry differs by the argument.
func conversionPlanSettings(registry string) *config.VersionedSettings {
	return &config.VersionedSettings{
		Runtimes: map[string]config.V1RuntimeConfig{
			"docker":  {Type: "docker"},
			"cluster": {Type: "kubernetes", Namespace: "agents", Context: "PLANTED-CONTEXT-rb1"},
		},
		Profiles: map[string]config.V1ProfileConfig{
			"build": {Runtime: "docker", ImageRegistry: registry},
			"local": {Runtime: "docker", ImageRegistry: registry},
			"k8s":   {Runtime: "cluster"},
		},
	}
}

// conversionPlanFixture is a legacy test server whose conversion plan
// inputs are fixed: settings from *vs, raw bytes from *raw, and a clock
// whose calls are counted (one per recompute).
type conversionPlanFixture struct {
	srv     *Server
	mu      sync.Mutex
	vs      *config.VersionedSettings
	raw     []byte
	loadErr error
	clockN  int
	now     time.Time
}

func newConversionPlanFixture(t *testing.T) *conversionPlanFixture {
	t.Helper()
	f := &conversionPlanFixture{
		srv: newTestServerWithManager(t, &mockManager{}),
		vs:  conversionPlanSettings("reg.example/a"),
		raw: []byte("schema_version: \"1\"\n"),
		now: time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC),
	}
	f.srv.loadMappingSettings = func() (*config.VersionedSettings, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.vs, f.loadErr
	}
	f.srv.conversionPlan.loadRaw = func() ([]byte, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.raw, nil
	}
	f.srv.conversionPlan.configuredRuntimes = func() (map[string]bool, bool) { return nil, false }
	f.srv.conversionPlan.now = func() time.Time {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.clockN++
		f.now = f.now.Add(time.Minute)
		return f.now
	}
	return f
}

func (f *conversionPlanFixture) beat(t *testing.T) *api.ConversionPlanPresentation {
	t.Helper()
	client := &mockRuntimeBrokerService{}
	svc := f.srv.newHeartbeatService(client, "legacy", "", time.Hour)
	return lastHeartbeat(t, svc, client).ConversionPlanPresentation
}

// TestLegacyHeartbeat_CarriesConversionPlan: a legacy heartbeat carries
// exactly brokerconvert.Presentation of the plan built from the broker's
// settings.
func TestLegacyHeartbeat_CarriesConversionPlan(t *testing.T) {
	f := newConversionPlanFixture(t)
	got := f.beat(t)
	if got == nil {
		t.Fatal("a legacy heartbeat carries no conversion plan")
	}
	plan, err := brokerconvert.BuildLocal(brokerconvert.Input{Settings: f.vs, RawSettings: f.raw})
	if err != nil {
		t.Fatal(err)
	}
	want := brokerconvert.Presentation(plan, got.ComputedAt)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("snapshot = %+v, want %+v", got, want)
	}
	if len(got.Instances) == 0 || got.SchemaVersion != api.ConversionPlanPresentationSchemaVersion {
		t.Fatalf("the fixture yields no instances: %+v", got)
	}
	enc, _ := json.Marshal(got)
	if strings.Contains(string(enc), "PLANTED-") {
		t.Fatalf("the snapshot carries a settings value: %s", enc)
	}
}

// TestLegacyHeartbeat_ConversionPlanRecomputedOnlyOnChange: an unchanged
// setting reuses the cached snapshot; a change to the effective settings
// or to the raw file recomputes it on the next heartbeat.
func TestLegacyHeartbeat_ConversionPlanRecomputedOnlyOnChange(t *testing.T) {
	f := newConversionPlanFixture(t)
	first := f.beat(t)
	second := f.beat(t)
	if f.clockN != 1 || first != second {
		t.Fatalf("unchanged settings recomputed the plan: %d computations", f.clockN)
	}

	f.mu.Lock()
	f.vs = conversionPlanSettings("reg.example/b")
	f.mu.Unlock()
	third := f.beat(t)
	if f.clockN != 2 || !third.ComputedAt.After(first.ComputedAt) {
		t.Fatalf("a settings change was not recomputed: %d computations", f.clockN)
	}

	f.mu.Lock()
	f.raw = append(f.raw, []byte("# edited\n")...)
	f.mu.Unlock()
	f.beat(t)
	if f.clockN != 3 {
		t.Fatalf("a raw file change was not recomputed: %d computations", f.clockN)
	}
}

// TestLegacyHeartbeat_ConversionPlanOmittedWhenSettingsUnreadable: the
// field is omitted, so the Hub keeps its stored value.
func TestLegacyHeartbeat_ConversionPlanOmittedWhenSettingsUnreadable(t *testing.T) {
	f := newConversionPlanFixture(t)
	f.loadErr = errors.New("unreadable")
	f.vs = nil
	if got := f.beat(t); got != nil {
		t.Fatalf("unreadable settings sent a plan: %+v", got)
	}
}

// TestLegacyHeartbeat_ConversionPlanOmittedWhenInstancesConfigured: a host
// that already declares flat instances has nothing to convert.
func TestLegacyHeartbeat_ConversionPlanOmittedWhenInstancesConfigured(t *testing.T) {
	f := newConversionPlanFixture(t)
	f.vs.Server = &config.V1ServerConfig{Broker: &config.V1BrokerConfig{
		Instances: []config.V1RuntimeBrokerInstanceConfig{{Key: "docker-1"}},
	}}
	if got := f.beat(t); got != nil {
		t.Fatalf("a host with instances sent a plan: %+v", got)
	}
}

// TestConversionPlanFingerprint_NeverSent: the fingerprint lives in memory
// only; the heartbeat carries no trace of it.
func TestConversionPlanFingerprint_NeverSent(t *testing.T) {
	f := newConversionPlanFixture(t)
	client := &mockRuntimeBrokerService{}
	svc := f.srv.newHeartbeatService(client, "legacy", "", time.Hour)
	hb := lastHeartbeat(t, svc, client)
	fp, ok := conversionPlanFingerprint(f.raw, f.vs)
	if !ok {
		t.Fatal("no fingerprint")
	}
	enc, _ := json.Marshal(hb)
	if strings.Contains(string(enc), hex.EncodeToString(fp[:])) || strings.Contains(strings.ToLower(string(enc)), "fingerprint") {
		t.Fatalf("the heartbeat carries the fingerprint: %s", enc)
	}
}

// TestFlatHeartbeat_NoConversionPlan: a flat instance never reports a
// conversion plan, and never loads settings for one.
func TestFlatHeartbeat_NoConversionPlan(t *testing.T) {
	f := newFlatInstanceTestServer(t, flatInstanceOpts{hubInProcess: true})
	called := false
	f.srv.conversionPlan.loadRaw = func() ([]byte, error) { called = true; return nil, nil }
	client := &mockRuntimeBrokerService{}
	svc := f.srv.newHeartbeatService(client, f.identity.RuntimeBrokerID, f.srv.config.HubEndpoint, time.Hour)
	hb := lastHeartbeat(t, svc, client)
	if hb.ConversionPlanPresentation != nil || called {
		t.Fatalf("a flat heartbeat carries a conversion plan (loaded=%v)", called)
	}
	if got := f.srv.heartbeatConversionPlan(); got != nil {
		t.Fatal("a flat server computes a conversion plan")
	}
}
