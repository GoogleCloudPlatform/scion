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
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// Template environment keys (ptone/scion#4524).
//
// A template describes the agent's job, not its operating environment. Every
// template-chain config is passed through stripTemplateEnvironment before it
// is merged, on both the provisioning path (resolveTemplateAndHarnessConfig)
// and the restart path (GetAgent), so a template's telemetry, hub, image,
// resources, kubernetes, volumes or reserved env keys never reach an agent's
// config. Each template that sets such keys gives one WARN log and one start
// warning.

// templateEnvWarnings collects the start warnings for ignored template
// environment keys. Start installs one in ctx around GetAgent and appends
// what it collected to the start warnings.
type templateEnvWarnings struct {
	mu       sync.Mutex
	quiet    bool
	warnings []string
	seen     map[string]struct{}
}

type templateEnvWarningsKey struct{}

// withTemplateEnvWarnings returns ctx carrying a new collector.
func withTemplateEnvWarnings(ctx context.Context) (context.Context, *templateEnvWarnings) {
	c := &templateEnvWarnings{seen: map[string]struct{}{}}
	return context.WithValue(ctx, templateEnvWarningsKey{}, c), c
}

// withQuietTemplateEnvWarnings returns ctx carrying a collector that neither
// logs nor keeps warnings, for a resolution that is repeated later by the
// real provisioning (PreflightResolve), so the warning is reported once.
func withQuietTemplateEnvWarnings(ctx context.Context) context.Context {
	return context.WithValue(ctx, templateEnvWarningsKey{}, &templateEnvWarnings{quiet: true})
}

func templateEnvWarningsFrom(ctx context.Context) *templateEnvWarnings {
	c, _ := ctx.Value(templateEnvWarningsKey{}).(*templateEnvWarnings)
	return c
}

// Warnings returns the collected warnings in the order they were reported.
func (c *templateEnvWarnings) Warnings() []string {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.warnings...)
}

// templateEnvIgnoredWarning is the start warning for a template that sets
// environment keys.
func templateEnvIgnoredWarning(templateName string, ignored []string) string {
	return fmt.Sprintf("template %s sets environment key(s) %s; ignored (environment comes from settings)",
		templateName, strings.Join(ignored, ", "))
}

// stripTemplateEnvironment returns tplCfg without its environment keys (see
// config.StripTemplateEnvironmentKeys). When it drops any, it logs a WARN
// and records a start warning in ctx's collector, once per distinct
// warning. authMeta is the resolved harness-config's auth metadata, whose
// credential env keys are reserved too; nil reserves only the fixed keys.
func stripTemplateEnvironment(ctx context.Context, tpl *config.Template, tplCfg *api.ScionConfig, authMeta *config.HarnessAuthMetadata) *api.ScionConfig {
	stripped, ignored := config.StripTemplateEnvironmentKeys(tplCfg, authMeta)
	if len(ignored) == 0 {
		return stripped
	}
	c := templateEnvWarningsFrom(ctx)
	if c != nil && c.quiet {
		return stripped
	}
	name := templateRef(tpl)
	msg := templateEnvIgnoredWarning(name, ignored)
	if c != nil {
		c.mu.Lock()
		_, dup := c.seen[msg]
		if !dup {
			c.seen[msg] = struct{}{}
			c.warnings = append(c.warnings, msg)
		}
		c.mu.Unlock()
		if dup {
			return stripped
		}
	}
	slog.Warn("template sets environment keys; ignored (environment comes from settings)",
		"template", name, "path", tpl.Path, "keys", ignored)
	return stripped
}

// harnessConfigAuthMeta returns the auth metadata of harness-config name:
// the resolved on-disk harness-config hcDir's, else the settings entry's,
// matching Start's resolution. It returns nil when neither has any.
func harnessConfigAuthMeta(hcDir *config.HarnessConfigDir, settings *config.VersionedSettings, profileName, name string) *config.HarnessAuthMetadata {
	if hcDir != nil && hcDir.Config.Auth != nil {
		return hcDir.Config.Auth
	}
	if settings != nil && name != "" {
		if entry, err := settings.ResolveHarnessConfig(profileName, name); err == nil && entry.Auth != nil {
			return entry.Auth
		}
	}
	return nil
}

// restartTemplateAuthMeta returns the auth metadata of the harness-config a
// restarted agent uses, for stripping its template chain in GetAgent: the
// harness-config recorded in its stored config, else the name the chain
// itself resolves to. It returns nil when none can be resolved; the fixed
// environment keys are stripped regardless.
func restartTemplateAuthMeta(ctx context.Context, chain []*config.Template, agentCfg *api.ScionConfig, projectDir string, settings *config.VersionedSettings, profileName string) *config.HarnessAuthMetadata {
	if profileName == "" && settings != nil {
		profileName = settings.ActiveProfile
	}
	name := ""
	if agentCfg != nil {
		name = agentCfg.HarnessConfig
	}
	templatePaths := make([]string, 0, len(chain))
	chainCfg := &api.ScionConfig{}
	for _, tpl := range chain {
		templatePaths = append(templatePaths, tpl.Path)
		if name != "" {
			continue
		}
		if tplCfg, err := tpl.LoadConfig(); err == nil {
			stripped, _ := config.StripTemplateEnvironmentKeys(tplCfg, nil)
			chainCfg = config.MergeScionConfig(chainCfg, stripped)
		}
	}
	if name == "" {
		res, err := config.ResolveHarnessConfigName(config.HarnessConfigInputs{
			TemplateCfg: chainCfg,
			Settings:    settings,
			ProfileName: profileName,
		})
		if err != nil || res == nil {
			return nil
		}
		name = res.Name
	}
	if name == "" {
		return nil
	}
	hcDir, err := resolveHarnessConfigDir(ctx, name, projectDir, templatePaths...)
	if err != nil {
		hcDir = nil
	}
	return harnessConfigAuthMeta(hcDir, settings, profileName, name)
}
