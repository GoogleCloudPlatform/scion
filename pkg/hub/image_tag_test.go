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
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/version"
)

// setImageTagExperiment turns hub.image_tag_setting on or off.
func setImageTagExperiment(t *testing.T, ops *OperationalSettings, on bool) {
	t.Helper()
	doc := `{"overrides":{"` + experiments.ImageTagSetting + `":false}}`
	if on {
		doc = `{"overrides":{"` + experiments.ImageTagSetting + `":true}}`
	}
	_, err := ops.Update(context.Background(), "experiments", json.RawMessage(doc), "test", ops.SectionRevision("experiments"), "managed")
	require.NoError(t, err)
}

// setImageTagOverrides stores exactly these experiment overrides; every
// other experiment follows its registry default.
func setImageTagOverrides(t *testing.T, ops *OperationalSettings, overrides map[string]bool) {
	t.Helper()
	doc, err := json.Marshal(map[string]map[string]bool{"overrides": overrides})
	require.NoError(t, err)
	_, err = ops.Update(context.Background(), "experiments", json.RawMessage(doc), "test", ops.SectionRevision("experiments"), "managed")
	require.NoError(t, err)
}

// setTestImageTagFileTier sets this replica's settings-file tier of
// image_tag (its bootstrap merge) on ops.
func setTestImageTagFileTier(t *testing.T, ops *OperationalSettings, top string, profiles map[string]string) {
	t.Helper()
	if top != "" {
		require.NoError(t, ops.bootstrapKoanf.Set("image_tag", top))
	}
	for name, v := range profiles {
		require.NoError(t, ops.bootstrapKoanf.Set("profiles."+name+".image_tag", v))
	}
}

// setTestVersion sets the hub binary version for the test.
func setTestVersion(t *testing.T, v string) {
	t.Helper()
	old := version.Version
	version.Version = v
	t.Cleanup(func() { version.Version = old })
}

// TestResolveImageTag_Layers: hub env > hub DB profile > file profile >
// file top; hub scope (no profile) skips the profile layers. A seeded
// profiles row is not a hub DB layer.
func TestResolveImageTag_Layers(t *testing.T) {
	ctx := context.Background()
	srv, fake, ops := newTestDBServer(t)

	assert.Equal(t, config.ResolvedImageTag{Source: config.ImageRegistrySourceDefault}, srv.resolveImageTag(""))

	setTestImageTagFileTier(t, ops, "file-top", map[string]string{"k8s": "file-k8s"})
	assert.Equal(t, config.ResolvedImageTag{Value: "file-top", Source: config.ImageRegistrySourceFile}, srv.resolveImageTag(""),
		"hub scope uses the file top, never a profile value")

	fake.seedWithOrigin("profiles", json.RawMessage(`{"k8s":{"runtime":"kubernetes","image_tag":"seeded-k8s"},"local":{"runtime":"docker"}}`), "seeded")
	_, err := ops.Refresh(ctx)
	require.NoError(t, err)
	assert.Equal(t, config.ResolvedImageTag{Value: "file-k8s", Source: config.ImageRegistrySourceFileProfile}, srv.resolveImageTag("k8s"),
		"a seeded row is not hub DB; the file profile applies")
	assert.Equal(t, config.ResolvedImageTag{Value: "file-top", Source: config.ImageRegistrySourceFile}, srv.resolveImageTag("local"))

	_, err = ops.Update(ctx, "profiles", json.RawMessage(`{"k8s":{"runtime":"kubernetes","image_tag":"db-k8s"},"local":{"runtime":"docker"}}`), "admin", -1, "managed")
	require.NoError(t, err)
	assert.Equal(t, config.ResolvedImageTag{Value: "db-k8s", Source: config.ImageRegistrySourceHubDBProfile}, srv.resolveImageTag("k8s"))
	assert.Equal(t, config.ResolvedImageTag{Value: "file-top", Source: config.ImageRegistrySourceFile}, srv.resolveImageTag(""),
		"a saved profile value never reaches hub scope")

	srv.config.NodeImageTag = "env-tag"
	assert.Equal(t, config.ResolvedImageTag{Value: "env-tag", Source: config.ImageRegistrySourceNodeEnv}, srv.resolveImageTag("k8s"))
	assert.Equal(t, config.ResolvedImageTag{Value: "env-tag", Source: config.ImageRegistrySourceNodeEnv}, srv.resolveImageTag(""))
}

// TestImageTag_ExperimentGatesTheTag: with the experiment off no tag is
// applied anywhere; on, the setting applies, and "version" is the hub's
// version (latest for a development build).
func TestImageTag_ExperimentGatesTheTag(t *testing.T) {
	srv, _, ops := newTestDBServer(t)
	setTestImageTagFileTier(t, ops, "v0.3.0", nil)

	assert.Equal(t, "", srv.hubScopeImageTag(), "off by default: no tag")
	assert.Equal(t, "", srv.dispatchImageTagFor("", ""), "off: no dispatch tag")
	short, resolved := srv.hubScopeImageRefs("scion-claude:latest", "ghcr.io/org")
	assert.Equal(t, "scion-claude:latest", short)
	assert.Equal(t, "ghcr.io/org/scion-claude:latest", resolved, "off: exactly the registry rewrite")

	setImageTagExperiment(t, ops, true)
	assert.Equal(t, "v0.3.0", srv.hubScopeImageTag())
	assert.Equal(t, "v0.3.0", srv.dispatchImageTagFor("", ""))
	short, resolved = srv.hubScopeImageRefs("scion-claude:latest", "ghcr.io/org")
	assert.Equal(t, "scion-claude:v0.3.0", short)
	assert.Equal(t, "ghcr.io/org/scion-claude:v0.3.0", resolved)
	short, resolved = srv.hubScopeImageRefs("scion-claude:v0.2.0", "ghcr.io/org")
	assert.Equal(t, "scion-claude:v0.2.0", short, "an explicit tag is kept")
	assert.Equal(t, "ghcr.io/org/scion-claude:v0.2.0", resolved)

	srv.config.NodeImageTag = "version"
	setTestVersion(t, "v0.3.0-preview.5")
	assert.Equal(t, "v0.3.0-preview.5", srv.hubScopeImageTag(), "version is the hub binary version")
	setTestVersion(t, "dev")
	assert.Equal(t, "latest", srv.hubScopeImageTag(), "a development build falls back to latest")

	setImageTagExperiment(t, ops, false)
	assert.Equal(t, "", srv.hubScopeImageTag(), "an admin toggle applies at the next read")
}

// brokerRowStore answers GetRuntimeBroker with broker or err.
type brokerRowStore struct {
	store.Store
	broker *store.RuntimeBroker
	err    error
}

func (b brokerRowStore) GetRuntimeBroker(context.Context, string) (*store.RuntimeBroker, error) {
	return b.broker, b.err
}

// TestDispatchImageTagFor_Profile: an agent's profile selects the profile
// layers; with no profile the broker's default profile does, and a broker
// row that cannot be read falls back to hub scope.
func TestDispatchImageTagFor_Profile(t *testing.T) {
	ctx := context.Background()
	newSrv := func(t *testing.T, st store.Store) *Server {
		srv, _, ops := newTestDBServerWithStore(t, st)
		setTestImageTagFileTier(t, ops, "file-top", nil)
		_, err := ops.Update(ctx, "profiles", json.RawMessage(`{"k8s":{"runtime":"kubernetes","image_tag":"db-k8s"},"local":{"runtime":"docker"}}`), "admin", -1, "managed")
		require.NoError(t, err)
		setImageTagExperiment(t, ops, true)
		return srv
	}

	srv := newSrv(t, brokerRowStore{broker: &store.RuntimeBroker{ID: "b1", DefaultProfile: "k8s"}})
	assert.Equal(t, "db-k8s", srv.dispatchImageTagFor("b1", "k8s"))
	assert.Equal(t, "file-top", srv.dispatchImageTagFor("b1", "local"))
	assert.Equal(t, "db-k8s", srv.dispatchImageTagFor("b1", ""), "no profile: the broker's default profile")

	srv = newSrv(t, brokerRowStore{err: errors.New("database unavailable")})
	assert.Equal(t, "file-top", srv.dispatchImageTagFor("b1", ""), "unreadable broker: hub scope")
	assert.Equal(t, "db-k8s", srv.dispatchImageTagFor("b1", "k8s"))

	// A flat broker (a runtime target, no profiles) resolves at hub scope,
	// even for an agent that recorded a profile; the tag reaches it through
	// HubAgentDefaults.
	srv = newSrv(t, brokerRowStore{broker: &store.RuntimeBroker{ID: "flat", DefaultProfile: "k8s", RuntimeTarget: &api.RuntimeTargetDescriptor{}}})
	assert.Equal(t, "file-top", srv.dispatchImageTagFor("flat", ""), "flat broker: hub scope")
	assert.Equal(t, "file-top", srv.dispatchImageTagFor("flat", "k8s"), "flat broker: hub scope, profile ignored")

	// The co-located broker with no saved profile resolves to its default
	// profile against the hub catalog (the hub does not leave the tag to it,
	// as it does the registry).
	srv = newSrv(t, brokerRowStore{broker: &store.RuntimeBroker{ID: "colocated", DefaultProfile: "k8s"}})
	srv.SetEmbeddedBrokerID("colocated")
	assert.Equal(t, "db-k8s", srv.dispatchImageTagFor("colocated", ""))
	srv = newSrv(t, brokerRowStore{broker: &store.RuntimeBroker{ID: "colocated", DefaultProfile: "gone"}})
	srv.SetEmbeddedBrokerID("colocated")
	assert.Equal(t, "file-top", srv.dispatchImageTagFor("colocated", ""), "a default profile outside the catalog has no profile layers")
}

// TestImageTag_VersionDefault: with image_tag unset, a release build of the
// hub applies its own version (hub.image_tag_version_default, on by
// default), even while hub.image_tag_setting is off; any other build
// leaves images unchanged.
func TestImageTag_VersionDefault(t *testing.T) {
	srv, _, ops := newTestDBServer(t)

	for _, v := range []string{"v0.3.0", "v0.3.0-preview.5", "v0.3.0-rc.1"} {
		setTestVersion(t, v)
		assert.Equal(t, v, srv.hubScopeImageTag(), "a release build uses its version: %s", v)
		assert.Equal(t, v, srv.dispatchImageTagFor("", ""), "dispatch carries the version: %s", v)
	}
	setTestVersion(t, "v0.3.0-preview.5")
	short, resolved := srv.hubScopeImageRefs("scion-claude:latest", "ghcr.io/homebrew-scion")
	assert.Equal(t, "scion-claude:v0.3.0-preview.5", short)
	assert.Equal(t, "ghcr.io/homebrew-scion/scion-claude:v0.3.0-preview.5", resolved)

	for _, v := range []string{"", "dev", "nightly-20261105", "v0.3.0-dirty", "v0.0.0-20261108002803-39f9e3d31e76"} {
		setTestVersion(t, v)
		assert.Equal(t, "", srv.hubScopeImageTag(), "not a release build, images keep latest: %q", v)
		assert.Equal(t, "", srv.dispatchImageTagFor("", ""), "no dispatch tag: %q", v)
	}

	setTestVersion(t, "v0.3.0")
	setImageTagOverrides(t, ops, map[string]bool{experiments.ImageTagVersionDefault: false})
	assert.Equal(t, "", srv.hubScopeImageTag(), "the version default can be turned off")
	assert.Equal(t, "", srv.dispatchImageTagFor("", ""), "off: no dispatch tag")
}

// TestImageTag_ExplicitValueWinsOverVersionDefault: a set image_tag always
// wins over the version default; with hub.image_tag_setting off it applies
// nothing and suppresses the default, so image_tag latest opts out.
func TestImageTag_ExplicitValueWinsOverVersionDefault(t *testing.T) {
	srv, _, ops := newTestDBServer(t)
	setTestVersion(t, "v0.3.0")
	setTestImageTagFileTier(t, ops, "latest", nil)
	assert.Equal(t, "", srv.hubScopeImageTag(), "a set value suppresses the default while the setting is off")
	assert.Equal(t, "", srv.dispatchImageTagFor("", ""))

	setImageTagExperiment(t, ops, true)
	assert.Equal(t, "latest", srv.hubScopeImageTag(), "the explicit value applies")

	srv.config.NodeImageTag = "v0.2.9"
	assert.Equal(t, "v0.2.9", srv.hubScopeImageTag(), "an explicit older tag wins over the version")
}

// TestLogImageTag_VersionDefault: the startup line records the version
// default and the tag it applies, with no warning.
func TestLogImageTag_VersionDefault(t *testing.T) {
	buf := captureImageRegistrySlog(t)
	srv, _, _ := newTestDBServer(t)
	setTestVersion(t, "v0.3.0-preview.5")
	srv.LogImageTag()
	out := buf.String()
	assert.Contains(t, out, "source=unset")
	assert.Contains(t, out, "version_default=true")
	assert.Contains(t, out, "tag=v0.3.0-preview.5")
	assert.NotContains(t, out, "experiment is off")
}

// TestLogImageTag_StartupAndChange (R6-F1): the hub logs the resolved
// value and its source once at startup, then only when a settings apply
// changes it; nothing is logged for a change before the startup line.
func TestLogImageTag_StartupAndChange(t *testing.T) {
	buf := captureImageRegistrySlog(t)
	srv, _, ops := newTestDBServer(t)
	setTestImageTagFileTier(t, ops, "v0.3.0", nil)

	srv.logImageTagIfChanged()
	assert.Empty(t, buf.String(), "nothing before the startup line")

	srv.LogImageTag()
	out := buf.String()
	assert.Contains(t, out, `msg="image_tag resolved"`)
	assert.Contains(t, out, "value=v0.3.0")
	assert.Contains(t, out, "source=settings_file")
	assert.Contains(t, out, "enabled=false")
	assert.Contains(t, out, "experiment is off", "a value with the experiment off is warned about")

	buf.Reset()
	srv.logImageTagIfChanged()
	assert.Empty(t, buf.String(), "no change, no line")

	setImageTagExperiment(t, ops, true)
	srv.logImageTagIfChanged()
	out = buf.String()
	assert.Contains(t, out, `msg="image_tag changed"`)
	assert.Contains(t, out, "enabled=true")
	assert.Contains(t, out, "tag=v0.3.0")

	buf.Reset()
	srv.config.NodeImageTag = "env-tag"
	srv.logImageTagIfChanged()
	out = buf.String()
	assert.Contains(t, out, `msg="image_tag changed"`)
	assert.Contains(t, out, "value=env-tag")
	assert.Contains(t, out, "source=env")
}

// TestLogImageTag_UnsetAndNoWarning: unset logs source=unset, and no
// warning is given when nothing is set.
func TestLogImageTag_UnsetAndNoWarning(t *testing.T) {
	buf := captureImageRegistrySlog(t)
	srv, _, _ := newTestDBServer(t)
	srv.LogImageTag()
	out := buf.String()
	assert.Contains(t, out, `msg="image_tag resolved"`)
	assert.Contains(t, out, "source=unset")
	assert.NotContains(t, out, "experiment is off")
}

// TestLogImageTag_InvalidValueWarns: a value that is not a valid tag (it
// can only come from the environment) is logged as a warning and applies
// no tag.
func TestLogImageTag_InvalidValueWarns(t *testing.T) {
	buf := captureImageRegistrySlog(t)
	srv, _, ops := newTestDBServer(t)
	setImageTagExperiment(t, ops, true)
	srv.config.NodeImageTag = "not a tag"
	srv.LogImageTag()
	assert.Contains(t, buf.String(), "level=WARN")
	assert.Contains(t, buf.String(), "the value is invalid and is ignored")
	assert.Equal(t, "", srv.hubScopeImageTag())
}

// TestLogImageTag_ExperimentOffLevel (ptone/scion#7541): with
// hub.image_tag_setting off, image_tag latest everywhere it is set, the
// documented opt-out from the version default, is logged at Info; any
// other set value, alone or next to latest, keeps the warning.
func TestLogImageTag_ExperimentOffLevel(t *testing.T) {
	const optOut = `level=INFO msg="image_tag is latest, the opt-out from the version default: agent images keep their tags (latest); hub.image_tag_setting is off"`
	const warn = `level=WARN msg="image_tag is set but the hub.image_tag_setting experiment is off; the value is ignored`
	tests := []struct {
		name     string
		env      string
		top      string
		profiles map[string]string
		managed  string // a saved hub profiles section, stored as a managed row
		optOut   bool
	}{
		{name: "file top latest", top: "latest", optOut: true},
		{name: "env latest", env: "latest", optOut: true},
		{name: "file profile latest", profiles: map[string]string{"k8s": "latest"}, optOut: true},
		{name: "file top non-latest", top: "v0.3.0"},
		{name: "latest next to a non-latest profile", top: "latest", profiles: map[string]string{"k8s": "v1"}},
		{name: "file top latest next to a saved hub profile v1", top: "latest", managed: `{"k8s":{"runtime":"kubernetes","image_tag":"v1"}}`},
		{name: "saved hub profile latest only", managed: `{"k8s":{"runtime":"kubernetes","image_tag":"latest"}}`, optOut: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			buf := captureImageRegistrySlog(t)
			srv, _, ops := newTestDBServer(t)
			setTestImageTagFileTier(t, ops, tt.top, tt.profiles)
			if tt.managed != "" {
				_, err := ops.Update(context.Background(), "profiles", json.RawMessage(tt.managed), "admin", -1, "managed")
				require.NoError(t, err)
			}
			srv.config.NodeImageTag = tt.env
			srv.LogImageTag()
			out := buf.String()
			if tt.optOut {
				assert.Contains(t, out, optOut)
				assert.NotContains(t, out, "level=WARN")
				assert.NotContains(t, out, "experiment is off")
				return
			}
			assert.Contains(t, out, warn)
			assert.NotContains(t, out, "the opt-out")
		})
	}
}

// TestProfilesSaveSetsImageTag: a save sets image_tag when a profile's
// non-empty value is new or different; an echo and a clear do not.
func TestProfilesSaveSetsImageTag(t *testing.T) {
	base := json.RawMessage(`{"k8s":{"runtime":"kubernetes","image_tag":"v1"},"local":{"runtime":"docker"}}`)
	for _, tt := range []struct {
		name string
		doc  string
		base json.RawMessage
		want bool
	}{
		{"echo", `{"k8s":{"runtime":"kubernetes","image_tag":"v1"},"local":{"runtime":"docker"}}`, base, false},
		{"clear", `{"k8s":{"runtime":"kubernetes"},"local":{"runtime":"docker"}}`, base, false},
		{"unrelated edit", `{"k8s":{"runtime":"kubernetes","image_tag":"v1"},"local":{"runtime":"podman"}}`, base, false},
		{"change", `{"k8s":{"runtime":"kubernetes","image_tag":"v2"}}`, base, true},
		{"new profile value", `{"local":{"runtime":"docker","image_tag":"v1"}}`, base, true},
		{"no base", `{"k8s":{"image_tag":"v1"}}`, nil, true},
		{"not an object", `[]`, base, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, profilesSaveSetsImageTag(json.RawMessage(tt.doc), tt.base))
		})
	}
}

// requireImageTagExperimentDisabled asserts the 412 experiment_disabled
// refusal naming hub.image_tag_setting.
func requireImageTagExperimentDisabled(t *testing.T, rr *httptest.ResponseRecorder) {
	t.Helper()
	require.Equal(t, http.StatusPreconditionFailed, rr.Code, rr.Body.String())
	var resp ErrorResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp), rr.Body.String())
	assert.Equal(t, ErrCodeExperimentDisabled, resp.Error.Code)
	assert.Equal(t, experiments.ImageTagSetting, resp.Error.Details["experiment"])
}

// TestPutServerConfig_ImageTagNeedsExperiment: with the experiment off, a
// hub save that sets a profile image_tag is refused with 412
// experiment_disabled and stores nothing; an echo of a stored value still
// saves; with the experiment on the value is stored.
func TestPutServerConfig_ImageTagNeedsExperiment(t *testing.T) {
	ctx := context.Background()
	srv, fake, ops := newTestDBServer(t)

	rr := putServerConfigDB(t, srv, ops, `{"profiles":{"k8s":{"runtime":"kubernetes","image_tag":"v1"}}}`)
	requireImageTagExperimentDisabled(t, rr)
	assert.Nil(t, storedRow(fake, "profiles"), "a refused save stores nothing")

	setImageTagExperiment(t, ops, true)
	rr = putServerConfigDB(t, srv, ops, `{"profiles":{"k8s":{"runtime":"kubernetes","image_tag":"v1"}}}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	_, err := ops.Refresh(ctx)
	require.NoError(t, err)
	row := storedRow(fake, "profiles")
	require.NotNil(t, row)
	assert.True(t, strings.Contains(string(row.Value), `"image_tag":"v1"`), "stored: %s", row.Value)

	setImageTagExperiment(t, ops, false)
	rr = putServerConfigDB(t, srv, ops, `{"profiles":{"k8s":{"runtime":"kubernetes","image_tag":"v1"},"local":{"runtime":"docker"}}}`)
	require.Equal(t, http.StatusOK, rr.Code, "an echo of the stored value is not refused: %s", rr.Body.String())
	rr = putServerConfigDB(t, srv, ops, `{"profiles":{"k8s":{"runtime":"kubernetes","image_tag":"v2"}}}`)
	requireImageTagExperimentDisabled(t, rr)
}

// profileImageTagRow returns the stored profiles row and the image_tag of
// its k8s profile (present reports whether the key is stored at all).
func profileImageTagRow(t *testing.T, fake *fakeHubSettingStore) (origin string, rev int64, tag interface{}, present bool) {
	t.Helper()
	row := storedRow(fake, "profiles")
	require.NotNil(t, row, "no profiles row")
	var doc map[string]map[string]interface{}
	require.NoError(t, json.Unmarshal(row.Value, &doc))
	tag, present = doc["k8s"]["image_tag"]
	return row.Origin, row.Revision, tag, present
}

// TestImageTagPersistence_SeededCopiesStayFileValues: a seeded profiles
// row carries the settings file's image_tag; GET hides it, and neither an
// echo nor a setup runtime save stores it, so the file value keeps
// applying (mirroring image_registry, ptone/scion#3916). A managed value
// is shown and kept.
func TestImageTagPersistence_SeededCopiesStayFileValues(t *testing.T) {
	ctx := context.Background()
	newSrv := func(t *testing.T) (*Server, *fakeHubSettingStore, *OperationalSettings) {
		srv, fake, ops := newTestDBServer(t)
		setTestImageTagFileTier(t, ops, "", map[string]string{"k8s": "file-k8s"})
		fake.seedWithOrigin("profiles", json.RawMessage(`{"k8s":{"runtime":"kubernetes","image_tag":"file-k8s"},"local":{"runtime":"docker"}}`), "seeded")
		_, err := ops.Refresh(ctx)
		require.NoError(t, err)
		setImageTagExperiment(t, ops, true)
		return srv, fake, ops
	}

	t.Run("GET hides the seeded copy", func(t *testing.T) {
		srv, _, ops := newSrv(t)
		assert.Equal(t, "", getServerConfigDB(t, srv, ops).Profiles["k8s"].ImageTag)
	})
	t.Run("echo without an edit is a no-op", func(t *testing.T) {
		srv, fake, ops := newSrv(t)
		body, err := json.Marshal(map[string]interface{}{"profiles": getServerConfigDB(t, srv, ops).Profiles})
		require.NoError(t, err)
		rr := putHubNameServerConfigDB(t, srv, ops, string(body))
		require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
		origin, rev, _, _ := profileImageTagRow(t, fake)
		assert.Equal(t, "seeded", origin)
		assert.Equal(t, int64(1), rev)
	})
	t.Run("echo with an edit stores no copy", func(t *testing.T) {
		srv, fake, ops := newSrv(t)
		get := getServerConfigDB(t, srv, ops)
		local := get.Profiles["local"]
		local.Runtime = "podman"
		get.Profiles["local"] = local
		body, err := json.Marshal(map[string]interface{}{"profiles": get.Profiles})
		require.NoError(t, err)
		rr := putHubNameServerConfigDB(t, srv, ops, string(body))
		require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
		origin, _, _, present := profileImageTagRow(t, fake)
		assert.Equal(t, "managed", origin)
		assert.False(t, present, "the seeded copy is not adopted")
		assert.Equal(t, config.ResolvedImageTag{Value: "file-k8s", Source: config.ImageRegistrySourceFileProfile}, srv.resolveImageTag("k8s"))
	})
	t.Run("setup runtime save stores no copy", func(t *testing.T) {
		srv, fake, ops := newSrv(t)
		require.NoError(t, srv.putRuntimeProfileToDB(ctx, ops, "k8s", "kubernetes-new", "admin"))
		origin, _, _, present := profileImageTagRow(t, fake)
		assert.Equal(t, "managed", origin)
		assert.False(t, present)
		assert.Equal(t, config.ImageRegistrySourceFileProfile, srv.resolveImageTag("k8s").Source)
	})
	t.Run("a typed value is managed, shown and kept", func(t *testing.T) {
		srv, fake, ops := newSrv(t)
		rr := putHubNameServerConfigDB(t, srv, ops, `{"profiles":{"k8s":{"runtime":"kubernetes","image_tag":"db-k8s"},"local":{"runtime":"docker"}}}`)
		require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
		assert.Equal(t, "db-k8s", getServerConfigDB(t, srv, ops).Profiles["k8s"].ImageTag)
		require.NoError(t, srv.putRuntimeProfileToDB(ctx, ops, "local", "podman", "admin"))
		_, _, tag, _ := profileImageTagRow(t, fake)
		assert.Equal(t, "db-k8s", tag)
		assert.Equal(t, config.ResolvedImageTag{Value: "db-k8s", Source: config.ImageRegistrySourceHubDBProfile}, srv.resolveImageTag("k8s"))
	})
}

// TestComputeSupersededKeys_ImageTagInherited: a managed profile without
// image_tag inherits the file value; one with its own value supersedes it.
func TestComputeSupersededKeys_ImageTagInherited(t *testing.T) {
	ctx := context.Background()
	fake := newFakeHubSettingStore()
	bootstrap := newFileKoanf(t, map[string]interface{}{
		"profiles.k8s.runtime":   "kubernetes",
		"profiles.k8s.image_tag": "file-k8s",
	})
	ops := NewOperationalSettings(fake, bootstrap, emptyKoanf())
	srv := &Server{dbDriver: "postgres", maintenance: NewMaintenanceState(false, "")}
	srv.SetOperationalSettings(ops)
	_, err := ops.Update(ctx, "profiles", json.RawMessage(`{"k8s":{"runtime":"kubernetes"}}`), "admin", -1, "managed")
	require.NoError(t, err)
	assert.Empty(t, srv.computeSupersededKeys(ops)["profiles"], "a managed profile without image_tag inherits the file value")

	_, err = ops.Update(ctx, "profiles", json.RawMessage(`{"k8s":{"runtime":"kubernetes","image_tag":"db-k8s"}}`), "admin", -1, "managed")
	require.NoError(t, err)
	assert.NotEmpty(t, srv.computeSupersededKeys(ops)["profiles"], "a managed profile value supersedes the file one")
}

// TestLogImageTag_ProfileValues (R1): the log carries every profile that
// sets its own image_tag with its value and source, and a hub save of a
// profile value logs "image_tag changed" with source=hub_settings_profile.
func TestLogImageTag_ProfileValues(t *testing.T) {
	ctx := context.Background()
	buf := captureImageRegistrySlog(t)
	srv, _, ops := newTestDBServer(t)
	setTestImageTagFileTier(t, ops, "", map[string]string{"k8s": "file-k8s"})
	_, err := ops.Update(ctx, "profiles", json.RawMessage(`{"k8s":{"runtime":"kubernetes"},"local":{"runtime":"docker"}}`), "admin", -1, "managed")
	require.NoError(t, err)
	setImageTagExperiment(t, ops, true)

	srv.LogImageTag()
	out := buf.String()
	assert.Contains(t, out, `msg="image_tag resolved"`)
	assert.Contains(t, out, "k8s: value=file-k8s source=settings_file_profile tag=file-k8s")

	buf.Reset()
	_, err = ops.Update(ctx, "profiles", json.RawMessage(`{"k8s":{"runtime":"kubernetes","image_tag":"db-k8s"},"local":{"runtime":"docker"}}`), "admin", -1, "managed")
	require.NoError(t, err)
	srv.logImageTagIfChanged()
	out = buf.String()
	assert.Contains(t, out, `msg="image_tag changed"`, "a profile-only change is logged")
	assert.Contains(t, out, "k8s: value=db-k8s source=hub_settings_profile tag=db-k8s")
	assert.Contains(t, out, "source=unset", "the hub-scope value itself did not change")

	buf.Reset()
	srv.logImageTagIfChanged()
	assert.Empty(t, buf.String(), "no change, no line")
}

// TestApplySnapshot_LogsImageTagChange: the settings apply path calls
// logImageTagIfChanged, so a change applied by a save, a toggle or another
// replica is logged.
func TestApplySnapshot_LogsImageTagChange(t *testing.T) {
	tempSettingsHome(t)
	prev := config.GetGlobalSettingsOverlay()
	t.Cleanup(func() { config.SetGlobalSettingsOverlay(prev) })
	buf := captureImageRegistrySlog(t)
	srv, _, ops := newTestDBServer(t)
	srv.LogImageTag()
	buf.Reset()

	setImageTagExperiment(t, ops, true)
	srv.config.NodeImageTag = "env-tag"
	ApplySnapshot(srv, ops.Snapshot())
	out := buf.String()
	assert.Contains(t, out, `msg="image_tag changed"`)
	assert.Contains(t, out, "value=env-tag")
	assert.Contains(t, out, "source=env")
}
