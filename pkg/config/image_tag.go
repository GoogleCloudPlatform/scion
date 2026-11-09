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
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/knadh/koanf/v2"
)

// image_tag selects the tag of the bundled scion-* agent images
// (ptone/scion#6625). A set value is gated by the hub.image_tag_setting
// experiment; an unset value takes the version default, gated by the
// hub.image_tag_version_default experiment (on by default). The hub checks
// both. The hub resolves the setting, expands it with its own version
// (EffectiveImageTag), rewrites the images it sends or checks itself
// (RewriteImageRef), and sends the resulting tag with each dispatch
// (api.HubAgentDefaults.ImageTag), so the Runtime Broker applies it to the
// template and harness-config images it resolves. Maintenance builds and
// the system image build keep latest.
//
// Values:
//
//	""         the version default: a release build (ReleaseImageTag) uses
//	           its own version; any other build makes no change (images keep
//	           :latest)
//	"version"  the binary's release version, or "latest" for any other build
//	any other  a literal tag, which must match the docker tag grammar
//	           ("latest" opts out of the version default)
//
// A release build is classified from pkg/version.Version alone. A build of
// a tagged commit with local changes is stamped with the tag
// (hack/version.sh does not mark dirty trees) and counts as that release.
//
// Precedence follows image_registry (ImageRegistryLayers), with one
// deliberate difference: the env layer is the hub process's
// SCION_IMAGE_TAG only. A Runtime Broker's own environment is never a
// layer, because the hub resolves the tag and sends the result with each
// dispatch, so no new broker capability field is needed and the hub alone
// decides which tag its agents run.

// EnvImageTag is the hub process environment variable that overrides every
// other image_tag source. It is not a settings override: it is excluded from
// the settings env mappers (settingsExcludedEnvVars) and read only by
// ImageTagFromEnv.
const EnvImageTag = "SCION_IMAGE_TAG"

// ImageTagVersion is the image_tag value that selects the binary's own
// version.
const ImageTagVersion = "version"

// ImageTagLatest is the tag the bundled images carry by default, the
// fallback of ImageTagVersion for any non-release build, and, as a set
// image_tag, the opt-out from the version default.
const ImageTagLatest = "latest"

// ImageTagSettingPattern is the JSON schema pattern of an image_tag
// setting value: empty, "version", or a docker tag. The settings schema
// (schemas/settings-v1.schema.json) and the hub profiles section use it.
const ImageTagSettingPattern = `^(version|[A-Za-z0-9_][A-Za-z0-9_.-]{0,127})?$`

// imageTagPattern is the docker tag grammar.
var imageTagPattern = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)

// releaseVersionPattern is the release version grammar: vX.Y.Z,
// vX.Y.Z-preview.N or vX.Y.Z-rc.N, the tag forms the release process cuts
// and publishes images under (hack/version.sh, without the nightly form).
var releaseVersionPattern = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-(preview|rc)\.(0|[1-9][0-9]*))?$`)

// ReleaseImageTag returns v and true when v is a release version (vX.Y.Z,
// vX.Y.Z-preview.N or vX.Y.Z-rc.N), else "" and false: an empty version,
// "dev", a nightly, a Go pseudo-version, or any other form.
func ReleaseImageTag(v string) (string, bool) {
	if releaseVersionPattern.MatchString(v) {
		return v, true
	}
	return "", false
}

// ImageTagFromEnv returns this process's SCION_IMAGE_TAG. It is the only
// reader of that variable.
func ImageTagFromEnv() string {
	return strings.TrimSpace(os.Getenv(EnvImageTag))
}

// ImageTagLayers holds one value per image_tag layer, in the same order as
// image_registry: env > hub DB profile > hub DB top > file profile > file
// top > default ("").
type ImageTagLayers = ImageRegistryLayers

// ResolvedImageTag is the winning image_tag setting value and its layer.
// Value is the setting as written ("version" is not expanded); see
// EffectiveImageTag.
type ResolvedImageTag struct {
	Value  string
	Source ImageRegistrySource
}

// ResolveImageTagLayers returns the first non-blank layer in precedence
// order, or an empty value from the default layer when all are blank.
func ResolveImageTagLayers(l ImageTagLayers) ResolvedImageTag {
	return ResolvedImageTag(ResolveImageRegistryLayers(l))
}

// ValidateImageTag reports whether v is an accepted image_tag setting
// value: empty, "version", or a docker tag. It does not trim: a value with
// leading or trailing whitespace is refused, so what is stored is exactly
// what the settings schema (ImageTagSettingPattern) accepts.
func ValidateImageTag(v string) error {
	if v == "" || v == ImageTagVersion || imageTagPattern.MatchString(v) {
		return nil
	}
	if t := strings.TrimSpace(v); t != v {
		return fmt.Errorf("invalid image_tag %q: remove the leading or trailing whitespace", v)
	}
	return fmt.Errorf("invalid image_tag %q: use %q or a tag of letters, digits, '_', '.' and '-' (at most 128 characters, not starting with '.' or '-')", v, ImageTagVersion)
}

// EffectiveImageTag returns the tag an image_tag setting selects, given the
// binary's version (pkg/version.Version). It is the single resolution of
// the tag:
//
//	unset (blank)  the release version (ReleaseImageTag), or "" (no change)
//	               for any other build
//	"version"      the release version, or "latest" for any other build
//	any other      the literal, which always wins
//
// The setting is trimmed first, as the resolver trims every layer. An
// invalid setting returns an error and no tag. The caller decides whether
// the unset default applies (hub.image_tag_version_default).
func EffectiveImageTag(setting, binaryVersion string) (string, error) {
	setting = strings.TrimSpace(setting)
	if err := ValidateImageTag(setting); err != nil {
		return "", err
	}
	if setting != "" && setting != ImageTagVersion {
		return setting, nil
	}
	if v, ok := ReleaseImageTag(strings.TrimSpace(binaryVersion)); ok {
		return v, nil
	}
	if setting == "" {
		return "", nil
	}
	return ImageTagLatest, nil
}

// RewriteImageTag sets the tag of a short-form scion-* image reference to
// tag when its tag is "latest" or absent. It leaves unchanged: an empty tag,
// a digest reference, a fully qualified reference (first path component
// with "." or ":", as RewriteImageRegistry), an image whose name does not
// start with "scion-", and an explicit tag other than "latest".
func RewriteImageTag(image, tag string) string {
	if tag == "" || image == "" || strings.Contains(image, "@") {
		return image
	}
	if firstSlash := strings.Index(image, "/"); firstSlash >= 0 && strings.ContainsAny(image[:firstSlash], ".:") {
		return image
	}
	return retagBasename(image, tag)
}

// RewriteImageRef applies image_tag and image_registry to image in that
// order: the tag rewrite needs the short form, which the registry rewrite
// turns into a fully qualified reference. A reference that is exactly the
// registry rewrite's own output (<registry>/<name>) is retagged too, so
// applying RewriteImageRef to its own result is stable. Other fully
// qualified references are left alone.
func RewriteImageRef(image, registry, tag string) string {
	if reg := strings.TrimRight(registry, "/"); tag != "" && reg != "" && strings.HasPrefix(image, reg+"/") {
		if rest := image[len(reg)+1:]; rest != "" && !strings.Contains(rest, "/") && !strings.Contains(rest, "@") {
			return reg + "/" + retagBasename(rest, tag)
		}
	}
	return RewriteImageRegistry(RewriteImageTag(image, tag), registry)
}

// retagBasename sets the tag of image (whose last path component is the
// name[:tag]) to tag when the name starts with "scion-" and the tag is
// "latest" or absent.
func retagBasename(image, tag string) string {
	prefix, base := "", image
	if i := strings.LastIndex(image, "/"); i >= 0 {
		prefix, base = image[:i+1], image[i+1:]
	}
	name, current, hasTag := strings.Cut(base, ":")
	if !strings.HasPrefix(name, "scion-") || (hasTag && current != ImageTagLatest) {
		return image
	}
	return prefix + name + ":" + tag
}

// ImageTagFileTierFromKoanf reads the settings-file tier of image_tag from
// a bootstrap koanf (top-level image_tag and profiles.<name>.image_tag), as
// ImageRegistryFileTierFromKoanf does for image_registry. A nil koanf gives
// an empty tier.
func ImageTagFileTierFromKoanf(k *koanf.Koanf) ImageRegistryFileTier {
	return fileTierFromKoanf(k, "image_tag")
}

// ImageTagSourceLabel names the layer an image_tag value came from, for
// logs: env (the hub process's SCION_IMAGE_TAG), hub_settings_profile (a
// saved hub profile), hub_settings (reserved: a saved hub top-level value,
// a layer the hub does not have yet; image_tag is stored only per profile),
// settings_file_profile, settings_file, or unset.
func ImageTagSourceLabel(s ImageRegistrySource) string {
	switch s {
	case ImageRegistrySourceNodeEnv:
		return "env"
	case ImageRegistrySourceHubDBProfile:
		return "hub_settings_profile"
	case ImageRegistrySourceHubDB:
		// Reserved for a future hub top-level layer; the hub never
		// resolves image_tag from it today.
		return "hub_settings"
	case ImageRegistrySourceFileProfile:
		return "settings_file_profile"
	case ImageRegistrySourceFile:
		return "settings_file"
	default:
		return "unset"
	}
}

// ImageTagLayers returns the settings-file image_tag layers for profileName
// ("" = the active profile) with nodeEnv as the env layer. There is no hub
// database layer here: the hub adds its managed values itself. A flat
// instance view has no profile layer (instance agent_defaults carry no
// image_tag).
func (vs *VersionedSettings) ImageTagLayers(profileName, nodeEnv string) ImageTagLayers {
	l := ImageTagLayers{NodeEnv: nodeEnv}
	if vs == nil {
		return l
	}
	l.File = vs.ImageTag
	if vs.flat != nil {
		return l
	}
	if profileName == "" {
		profileName = vs.ActiveProfile
	}
	if p, ok := vs.Profiles[profileName]; ok {
		l.FileProfile = p.ImageTag
	}
	return l
}
