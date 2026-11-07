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
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/config/opsettings"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/version"
)

// image_tag on the hub (ptone/scion#6625). The hub alone resolves it, with
// the image_registry layer order:
//
//	hub env (SCION_IMAGE_TAG) > hub DB profile > file profile > file top > unset
//
// There is no hub DB top-level layer: image_tag is stored only in the hub
// profiles section. A set value applies only while hub.image_tag_setting is
// on; an unset value takes the version default while
// hub.image_tag_version_default is on (a release build of this hub uses its
// own version), and a set value suppresses that default even while
// hub.image_tag_setting is off. config.EffectiveImageTag expands both. The
// hub rewrites the images it sends or checks (config.RewriteImageRef) and
// sends the tag with each dispatch (api.HubAgentDefaults.ImageTag), so the
// broker applies it to the images it resolves from templates and harness
// configs. A Runtime Broker's own environment is never a layer.

// profilesImageTagKey is the profiles section key of image_tag.
const profilesImageTagKey = "image_tag"

// managedProfileImageTags returns the non-empty image_tag of each profile
// of a managed profiles row in the cache. A seeded row, or no row,
// contributes nothing (its values are copies of the settings file).
func (o *OperationalSettings) managedProfileImageTags() map[string]string {
	o.mu.RLock()
	prof, ok := o.cache["profiles"]
	o.mu.RUnlock()
	if !ok || prof.Origin != opsettings.OriginManaged || prof.Malformed {
		return nil
	}
	var doc map[string]struct {
		ImageTag string `json:"image_tag"`
	}
	if json.Unmarshal(prof.Value, &doc) != nil {
		return nil
	}
	var out map[string]string
	for name, p := range doc {
		if v := strings.TrimSpace(p.ImageTag); v != "" {
			if out == nil {
				out = map[string]string{}
			}
			out[name] = v
		}
	}
	return out
}

// ImageTagHubInputs returns the hub-side image_tag layers in the
// image_registry inputs shape: the managed profile values, this replica's
// settings-file tier and the profile catalog in effect. ManagedTop is
// always empty (no hub DB top-level layer).
func (o *OperationalSettings) ImageTagHubInputs() config.ImageRegistryHubInputs {
	catalog := make([]string, 0)
	for name := range o.Snapshot().Profiles {
		catalog = append(catalog, name)
	}
	sort.Strings(catalog)
	return config.ImageRegistryHubInputs{
		ManagedProfiles: o.managedProfileImageTags(),
		FileTier:        config.ImageTagFileTierFromKoanf(o.bootstrapKoanf),
		Catalog:         catalog,
	}
}

// fileImageTagWithoutOps is the settings-file top-level image_tag for a
// server without operational settings (tests and embedded servers).
func fileImageTagWithoutOps() string {
	globalDir, err := config.GetGlobalDir()
	if err != nil {
		return ""
	}
	vs, err := config.LoadVersionedSettings(globalDir)
	if err != nil || vs == nil {
		return ""
	}
	return vs.ImageTag
}

// resolveImageTag resolves the image_tag setting for profile; an empty
// profile resolves at hub scope (the hub env and the file top level only).
func (s *Server) resolveImageTag(profile string) config.ResolvedImageTag {
	s.mu.RLock()
	nodeEnv := s.config.NodeImageTag
	s.mu.RUnlock()
	ops := s.GetOperationalSettings()
	if ops == nil {
		return config.ResolveImageTagLayers(config.ImageTagLayers{NodeEnv: nodeEnv, File: fileImageTagWithoutOps()})
	}
	in := ops.ImageTagHubInputs()
	if profile == "" {
		return config.ResolveImageTagLayers(in.TopLayers(nodeEnv))
	}
	return config.ResolveImageTagLayers(in.Layers(profile, nodeEnv))
}

// imageTagState is a resolved image_tag setting and the tag it applies.
type imageTagState struct {
	// Setting is the setting as written ("version" unexpanded).
	Setting string
	// Source is the layer Setting came from.
	Source config.ImageRegistrySource
	// Enabled is the hub.image_tag_setting experiment state.
	Enabled bool
	// VersionDefault is the hub.image_tag_version_default experiment state.
	VersionDefault bool
	// Tag is the tag applied to images: "" when nothing applies (a set
	// value with hub.image_tag_setting off, an unset value with the version
	// default off or a non-release build, or an invalid value).
	Tag string
	// Invalid is the validation error of an invalid setting.
	Invalid string
	// Profiles summarises, sorted by name, every profile that sets its own
	// image_tag (hub profiles section or settings file) as
	// "<name>: value=<v> source=<s> tag=<t>" joined by "; ", so a change to
	// a profile value is logged too. It is set only for the log state.
	Profiles string
}

// imageTagStateOf applies the experiments and resolves the tag with this
// hub's version (config.EffectiveImageTag, the single resolution). A set
// value needs hub.image_tag_setting; an unset value needs
// hub.image_tag_version_default. A set value with hub.image_tag_setting off
// applies nothing and also suppresses the version default.
func (s *Server) imageTagStateOf(r config.ResolvedImageTag) imageTagState {
	st := imageTagState{
		Setting:        r.Value,
		Source:         r.Source,
		Enabled:        s.experimentEnabled(experiments.ImageTagSetting),
		VersionDefault: s.experimentEnabled(experiments.ImageTagVersionDefault),
	}
	if (r.Value != "" && !st.Enabled) || (r.Value == "" && !st.VersionDefault) {
		return st
	}
	tag, err := config.EffectiveImageTag(r.Value, version.Version)
	if err != nil {
		st.Invalid = err.Error()
		return st
	}
	st.Tag = tag
	return st
}

// hubScopeImageTag is the tag applied to images the hub checks or pulls
// itself with no target broker (harness-config image status, check and
// pull, and the system image pull): hub scope, no profile.
func (s *Server) hubScopeImageTag() string {
	return s.imageTagStateOf(s.resolveImageTag("")).Tag
}

// hubScopeImageRefs returns the forms of image the hub checks or pulls at
// hub scope: short is image with the hub-scope tag applied (a short-form
// scion-* image whose tag is latest or absent), and resolved is short
// rewritten to the hub-scope registry (config.RewriteImageRef). With the
// experiment off or no tag, short is image and resolved is exactly the
// image_registry rewrite.
func (s *Server) hubScopeImageRefs(image, registry string) (short, resolved string) {
	tag := s.hubScopeImageTag()
	short = config.RewriteImageTag(image, tag)
	return short, config.RewriteImageRef(short, registry, tag)
}

// dispatchImageTagFor returns the tag the hub applies to an agent
// dispatched to brokerID with profile, or "" for none. Unlike
// image_registry, the hub resolves the tag for every broker, because a
// broker has no image_tag layers of its own:
//
//   - a flat broker (a runtime target, no profiles) resolves at hub scope;
//   - an agent with no profile resolves to the broker's default profile
//     against the hub's profile catalog (config.EffectiveDefaultProfile),
//     the co-located broker included;
//   - a broker row that cannot be read resolves at hub scope when the
//     agent has no profile.
func (s *Server) dispatchImageTagFor(brokerID, profile string) string {
	if !s.experimentEnabled(experiments.ImageTagSetting) && !s.experimentEnabled(experiments.ImageTagVersionDefault) {
		return ""
	}
	if brokerID != "" && s.store != nil {
		ctx, cancel := context.WithTimeout(context.Background(), imageRegistryLookupTimeout)
		b, err := s.store.GetRuntimeBroker(ctx, brokerID)
		cancel()
		switch {
		case err == nil && b != nil && b.RuntimeTarget != nil:
			profile = ""
		case err == nil && b != nil:
			if profile == "" {
				profile = config.EffectiveDefaultProfile(b.DefaultProfile, s.imageTagCatalog())
			}
		case errors.Is(err, store.ErrNotFound):
		default:
			slog.Warn("image_tag: cannot read the target broker; an agent without a profile resolves at hub scope", "broker_id", brokerID, "error", err)
		}
	}
	return s.imageTagStateOf(s.resolveImageTag(profile)).Tag
}

// imageTagCatalog is the profile catalog image_tag resolves against, or
// nil without operational settings.
func (s *Server) imageTagCatalog() []string {
	if ops := s.GetOperationalSettings(); ops != nil {
		return ops.ImageTagHubInputs().Catalog
	}
	return nil
}

// imageTagLogState records the image_tag state last logged, so a settings
// apply logs only a change.
type imageTagLogState struct {
	mu      sync.Mutex
	started bool
	last    imageTagState
}

// LogImageTag logs, once at hub startup, the hub-scope image_tag: the
// setting, its source, the experiment state and the tag applied, plus the
// profiles that set their own value. With the experiment off and a value
// set anywhere, it warns that the value is ignored, or, when every set
// value is latest (the opt-out), logs that at Info. Later changes are
// logged by logImageTagIfChanged.
func (s *Server) LogImageTag() {
	s.logImageTag(true)
	s.warnImageTagWithoutExperiment()
}

// logImageTagIfChanged logs the hub-scope image_tag when it differs from
// the state last logged (a settings save, an experiment toggle, or a
// settings change another replica made). It logs nothing before
// LogImageTag has run.
func (s *Server) logImageTagIfChanged() {
	s.logImageTag(false)
}

func (s *Server) logImageTag(startup bool) {
	st := s.imageTagStateOf(s.resolveImageTag(""))
	st.Profiles = s.imageTagProfileStates()
	s.imageTagLog.mu.Lock()
	defer s.imageTagLog.mu.Unlock()
	if !startup && (!s.imageTagLog.started || st == s.imageTagLog.last) {
		return
	}
	s.imageTagLog.started = true
	s.imageTagLog.last = st
	msg := "image_tag resolved"
	if !startup {
		msg = "image_tag changed"
	}
	attrs := []any{
		"value", st.Setting,
		"source", config.ImageTagSourceLabel(st.Source),
		"experiment", experiments.ImageTagSetting,
		"enabled", st.Enabled,
		"version_default", st.VersionDefault,
		"tag", st.Tag,
		"profiles", st.Profiles,
	}
	if st.Invalid != "" {
		slog.Warn(msg+"; the value is invalid and is ignored", append(attrs, "error", st.Invalid)...)
		return
	}
	slog.Info(msg, attrs...)
}

// imageTagProfileStates resolves every profile that sets its own
// image_tag (imageTagProfiles) and summarises each as
// "<name>: value=<v> source=<s> tag=<t>", sorted and joined by "; ".
func (s *Server) imageTagProfileStates() string {
	names := s.imageTagProfiles()
	parts := make([]string, 0, len(names))
	for _, name := range names {
		ps := s.imageTagStateOf(s.resolveImageTag(name))
		parts = append(parts, name+": value="+ps.Setting+" source="+config.ImageTagSourceLabel(ps.Source)+" tag="+ps.Tag)
	}
	return strings.Join(parts, "; ")
}

// imageTagProfiles returns the sorted names of the profiles that set their
// own image_tag, in the hub profiles section or the settings file.
func (s *Server) imageTagProfiles() []string {
	ops := s.GetOperationalSettings()
	if ops == nil {
		return nil
	}
	in := ops.ImageTagHubInputs()
	seen := map[string]bool{}
	for name := range in.ManagedProfiles {
		seen[name] = true
	}
	for name := range in.FileTier.Profiles {
		seen[name] = true
	}
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// warnImageTagWithoutExperiment warns once at startup when image_tag is set
// (hub env, settings file, or a saved hub profile) while
// hub.image_tag_setting is off: the value is ignored, image references are
// unchanged, and the version default does not apply. A hub save is refused
// in that state (412
// experiment_disabled), but a settings file or the environment cannot see
// the experiment. When every set value is latest, the opt-out from the
// version default, which has exactly its documented effect in that state,
// it logs the opt-out at Info instead.
func (s *Server) warnImageTagWithoutExperiment() {
	if s.experimentEnabled(experiments.ImageTagSetting) {
		return
	}
	s.mu.RLock()
	nodeEnv := s.config.NodeImageTag
	s.mu.RUnlock()
	var sources []string
	if nodeEnv != "" {
		sources = append(sources, "env")
	}
	ops := s.GetOperationalSettings()
	var in config.ImageRegistryHubInputs
	if ops != nil {
		in = ops.ImageTagHubInputs()
	} else {
		in.FileTier.Top = strings.TrimSpace(fileImageTagWithoutOps())
	}
	if in.FileTier.Top != "" || len(in.FileTier.Profiles) > 0 {
		sources = append(sources, "settings_file")
	}
	if len(in.ManagedProfiles) > 0 {
		sources = append(sources, "hub_settings_profile")
	}
	if len(sources) == 0 {
		return
	}
	if imageTagValuesAllLatest(nodeEnv, in) {
		slog.Info("image_tag is latest, the opt-out from the version default: agent images keep latest; "+experiments.ImageTagSetting+" is off",
			"sources", sources, "profiles", s.imageTagProfiles())
		return
	}
	slog.Warn("image_tag is set but the "+experiments.ImageTagSetting+" experiment is off; the value is ignored, agent images keep their tags and the version default does not apply",
		"sources", sources, "profiles", s.imageTagProfiles())
}

// imageTagValuesAllLatest reports whether every set image_tag value (the
// hub env, the settings-file top level and profiles, and the saved hub
// profiles) is latest, the documented opt-out from the version default,
// and at least one is set.
func imageTagValuesAllLatest(nodeEnv string, in config.ImageRegistryHubInputs) bool {
	values := []string{nodeEnv, in.FileTier.Top}
	for _, v := range in.FileTier.Profiles {
		values = append(values, v)
	}
	for _, v := range in.ManagedProfiles {
		values = append(values, v)
	}
	set := false
	for _, v := range values {
		switch strings.TrimSpace(v) {
		case "":
		case config.ImageTagLatest:
			set = true
		default:
			return false
		}
	}
	return set
}

// profilesSaveSetsImageTag reports whether a profiles section save (doc)
// sets or changes a non-empty image_tag compared with base (the document
// a save that echoes GET would write, profilesFilteredBase). An echo of a
// stored value, or a clear, is not a change.
func profilesSaveSetsImageTag(doc, base json.RawMessage) bool {
	// Only image_tag is decoded, so no other field of a profile can make
	// the decode fail and skip the check.
	type profile struct {
		ImageTag string `json:"image_tag"`
	}
	tags := func(raw json.RawMessage) (map[string]config.V1ProfileConfig, bool) {
		var m map[string]profile
		if json.Unmarshal(raw, &m) != nil {
			return nil, false
		}
		out := make(map[string]config.V1ProfileConfig, len(m))
		for name, p := range m {
			out[name] = config.V1ProfileConfig{ImageTag: p.ImageTag}
		}
		return out, true
	}
	sent, ok := tags(doc)
	if !ok {
		return false
	}
	var had map[string]config.V1ProfileConfig
	if len(base) > 0 {
		had, _ = tags(base)
	}
	return profileMapsSetImageTag(sent, had)
}

// profileMapsSetImageTag reports whether sent sets or changes a non-empty
// profile image_tag compared with had (the stored profiles).
func profileMapsSetImageTag(sent, had map[string]config.V1ProfileConfig) bool {
	for name, p := range sent {
		v := strings.TrimSpace(p.ImageTag)
		if v != "" && v != strings.TrimSpace(had[name].ImageTag) {
			return true
		}
	}
	return false
}

// writeImageTagExperimentDisabled writes the refusal of a settings save
// that sets image_tag while hub.image_tag_setting is off: 412
// experiment_disabled naming the experiment, as the hub refuses other
// experiment-gated saves (experimentDisabledRefusal).
func writeImageTagExperimentDisabled(w http.ResponseWriter) {
	writeError(w, http.StatusPreconditionFailed, ErrCodeExperimentDisabled,
		"image_tag is not enabled on this Hub (experiment "+experiments.ImageTagSetting+" is off): enable it, or remove image_tag from the profiles",
		map[string]interface{}{"experiment": experiments.ImageTagSetting})
}
