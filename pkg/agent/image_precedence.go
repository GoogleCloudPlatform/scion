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

package agent

import (
	"context"
	"log/slog"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
)

// Image tier names, as they appear in the image-resolution log lines.
const (
	imageTierHarnessConfigFile = "harness-config file"
	imageTierSettings          = "settings harness_configs"
	imageTierTemplate          = "template"
	imageTierTemplateSnapshot  = "recorded template snapshot"
	imageTierInline            = "inline config"
	imageTierProfileOverride   = "profile harness_overrides"
	imageTierRequest           = "explicit --image / request image"
	imageTierRecordedRequest   = "recorded explicit request image"
	imageTierProvisioned       = "provisioned config"
)

// imageCandidate is one tier's contribution to the container image (or to
// its Kubernetes pull policy). An empty value means the tier does not set one.
type imageCandidate struct {
	source string
	image  string
}

// pickImage resolves the container image from candidates ordered from the
// LOWEST tier to the HIGHEST: each non-empty candidate replaces whatever a
// lower tier supplied. Every time that replaces a different, non-empty image
// from a lower tier, it logs at Info what replaced what and why, so a pinned
// image losing to a higher tier is never silent (ptone/scion#1799).
//
// It returns the winning image and the tier that supplied it ("" when no
// tier set an image).
func pickImage(logger *slog.Logger, agentName string, candidates []imageCandidate) (image, source string) {
	return pickImageField(logger, agentName, "image", candidates)
}

// pickPullPolicy is pickImage for kubernetes.imagePullPolicy.
func pickPullPolicy(logger *slog.Logger, agentName string, candidates []imageCandidate) (policy, source string) {
	return pickImageField(logger, agentName, "image pull policy", candidates)
}

func pickImageField(logger *slog.Logger, agentName, field string, candidates []imageCandidate) (value, source string) {
	if logger == nil {
		logger = slog.Default()
	}
	for _, c := range candidates {
		if c.image == "" {
			continue
		}
		if value != "" && value != c.image {
			logger.Info("image resolution: lower-tier "+field+" replaced",
				"agent", agentName,
				"field", field,
				"replaced_value", value,
				"replaced_source", source,
				"value", c.image,
				"source", c.source,
				"reason", c.source+" outranks "+source)
		}
		value, source = c.image, c.source
		util.Debugf("image resolution: %s from %s: %s", field, source, value)
	}
	return value, source
}

// templateChainImage returns the template chain's own image and
// kubernetes.imagePullPolicy (a later template in the chain wins), with no
// other source folded in. Templates whose config cannot be loaded are skipped.
func templateChainImage(chain []*config.Template) (image, pullPolicy string) {
	for _, tpl := range chain {
		tplCfg, err := tpl.LoadConfig()
		if err != nil || tplCfg == nil {
			continue
		}
		if tplCfg.Image != "" {
			image = tplCfg.Image
		}
		if tplCfg.Kubernetes != nil && tplCfg.Kubernetes.ImagePullPolicy != "" {
			pullPolicy = tplCfg.Kubernetes.ImagePullPolicy
		}
	}
	return image, pullPolicy
}

// withProvisionedImage returns cfg with Image set to the image Start will
// run for this freshly provisioned agent: the user's request image, then an
// explicit profile harness_overrides image, then the provisioned config's
// own (inline over template over settings over file) image. Only the
// returned copy changes: the persisted scion-agent.json keeps the
// provisioned value, so a profile pin is never baked into the record a later
// Start falls back to (ptone/scion#1799). The broker reports this image in
// its provision-only response, which the hub records as AppliedConfig.Image.
//
// The profile override is looked up with the profile recorded in the
// broker-side image provenance ProvisionAgent just wrote (falling back to
// opts.Profile only if it is missing), the same profile Start will use.
// Settings are read in the request's profile-resolution mode (ctx).
func withProvisionedImage(ctx context.Context, opts api.StartOptions, agentDir string, cfg *api.ScionConfig) (*api.ScionConfig, error) {
	if cfg == nil {
		return nil, nil
	}
	profile := opts.Profile
	p, err := readImageProvenance(agentDir)
	if err != nil {
		return cfg, err
	}
	if p != nil {
		profile = p.Profile
	}
	profileImage := ""
	if projectDir, err := config.GetResolvedProjectDir(opts.ProjectPath); err == nil {
		// The request's profile-resolution mode applies: a flat Runtime Broker
		// instance skips the profile tier, including the active_profile
		// fallback (ProfileHarnessOverrideImage with an empty profile).
		if settings, _, _ := config.LoadEffectiveSettingsFor(ctx, projectDir); settings != nil {
			profileImage = settings.ProfileHarnessOverrideImage(profile, cfg.HarnessConfig)
		}
	}
	image, _ := pickImage(slog.Default(), opts.Name, []imageCandidate{
		{source: imageTierProvisioned, image: cfg.Image},
		{source: imageTierProfileOverride, image: profileImage},
		{source: imageTierRequest, image: opts.Image},
	})
	if image == cfg.Image {
		return cfg, nil
	}
	out := *cfg
	out.Image = image
	return &out, nil
}
