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

package harness

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// ErrEmptyGenericCommand is returned when starting a generic harness would
// produce an empty command because neither a task, harness args, nor
// command_args were provided.
var ErrEmptyGenericCommand = errors.New("generic harness needs a task, harness args or command_args")

// ErrGenericShellResumeUnsupported is returned when resuming a generic harness
// configured with a shell-form command (e.g. "sh -c") without an explicit task,
// which would otherwise invoke the shell with no command string and exit 2.
var ErrGenericShellResumeUnsupported = errors.New("generic harness with shell-form command (sh -c) does not support resume without a task; start the agent fresh or pass a task explicitly")

// ErrGenericShellCommandMissingScript is returned when starting a generic
// harness configured with a shell-form command (e.g. "sh -c") and no command
// string, which would otherwise invoke the shell with no script argument and
// exit 2.
var ErrGenericShellCommandMissingScript = errors.New("generic harness with shell-form command (sh -c) requires a task or command string")

type commandValidator interface {
	validateCommand(task string, resume bool, baseArgs []string) error
}

type modelFlagSupport interface {
	supportsModelFlag() bool
}

// SupportsModelFlag reports whether h accepts a CLI "--model <name>" flag
// prepended to CommandArgs. Container-script harnesses and non-generic
// declarative harnesses with a non-shell command.base support it; Generic,
// Shell, and generic/shell-based declarative harnesses do not (they still
// receive SCION_MODEL in their environment).
func SupportsModelFlag(h api.Harness) bool {
	if s, ok := h.(modelFlagSupport); ok {
		return s.supportsModelFlag()
	}
	return false
}

// isShellCommandBase reports whether base invokes a POSIX-style shell binary
// (e.g. "sh", "/bin/sh", "bash"), which does not accept a "--model" flag.
func isShellCommandBase(base []string) bool {
	if len(base) == 0 {
		return false
	}
	switch filepath.Base(base[0]) {
	case "sh", "bash", "zsh", "dash", "ash":
		return true
	default:
		return false
	}
}

// HonoursSettingsArgs reports whether settings.yaml's
// harness_configs.<name>.args field is applied for the given harness type.
// Only the generic harness honours settings-level harness args; other
// harnesses ignore the field (ptone/scion#5082, ptone/scion#8490).
func HonoursSettingsArgs(harnessName string) bool {
	return config.HarnessHonoursSettingsArgs(harnessName)
}

// shellCommandMissingScript reports whether argv invokes a POSIX-style shell
// with a -c option (including combined short flags such as -lc or -ec) and no
// command string operand, which always exits with status 2 at runtime.
func shellCommandMissingScript(argv []string) bool {
	if !isShellCommandBase(argv) {
		return false
	}
	hasC := false
	i := 1
	for i < len(argv) {
		arg := argv[i]
		if arg == "--" {
			i++
			break
		}
		if arg == "-o" || arg == "+o" {
			i += 2
			continue
		}
		if strings.HasPrefix(arg, "--") {
			i++
			continue
		}
		if len(arg) > 1 && arg[0] == '-' {
			if strings.ContainsRune(arg[1:], 'c') {
				hasC = true
			}
			i++
			continue
		}
		if len(arg) > 1 && arg[0] == '+' {
			i++
			continue
		}
		break
	}
	return hasC && i >= len(argv)
}

func validateGenericCommand(builtin bool, cmd []string, resume bool) error {
	if builtin && len(cmd) == 0 {
		return ErrEmptyGenericCommand
	}
	if shellCommandMissingScript(cmd) {
		if resume {
			return ErrGenericShellResumeUnsupported
		}
		return ErrGenericShellCommandMissingScript
	}
	return nil
}

// ValidateCommand checks that h produces a valid command for the given start
// inputs. The built-in generic harness has no executable of its own, so an
// empty command is rejected with ErrEmptyGenericCommand; generic and
// declarative-generic harnesses whose command is a bare POSIX shell -c
// invocation with no command string are rejected with
// ErrGenericShellResumeUnsupported (on resume) or
// ErrGenericShellCommandMissingScript (on fresh start). Other harnesses are
// unaffected here (the runtime still guards against any empty command line as
// defense in depth).
func ValidateCommand(h api.Harness, task string, resume bool, baseArgs []string) error {
	if v, ok := h.(commandValidator); ok {
		return v.validateCommand(task, resume, baseArgs)
	}
	return nil
}

// ValidateEntryCommand checks the effective harness-config entry Resolve would
// build from, without constructing a container-script harness (for preflight
// admission checks that hold only an EffectiveConfig entry and its source).
func ValidateEntryCommand(source config.HarnessConfigSource, entry config.HarnessConfigEntry, task string, resume bool, baseArgs []string) error {
	if entry.Provisioner != nil || entry.Harness == config.ShellHarnessConfigName {
		return nil
	}
	d := NewDeclarativeGenericHarness(entry)
	d.builtin = source == config.HarnessConfigSourceBuiltin
	return d.validateCommand(task, resume, baseArgs)
}

// Generic is the harness for a harness-config without a provisioner or
// declarative metadata, and for names harness.New does not know.
type Generic struct {
	// ready, when set, is the resolved ready spec of the harness-config
	// entry Resolve built this harness from; nil uses the delay default.
	ready *api.ReadySpec
	// builtin is true when Resolve built this harness from the synthetic
	// built-in "generic" harness-config entry (HarnessConfigSourceBuiltin).
	builtin bool
}

func (g *Generic) Name() string {
	return "generic"
}

func (g *Generic) AdvancedCapabilities() api.HarnessAdvancedCapabilities {
	return api.HarnessAdvancedCapabilities{
		Harness: "generic",
		Limits: api.HarnessLimitCapabilities{
			MaxTurns:      api.CapabilityField{Support: api.SupportNo, Reason: "This harness has no hook dialect for turn events"},
			MaxModelCalls: api.CapabilityField{Support: api.SupportNo, Reason: "This harness has no hook dialect for model events"},
			MaxDuration:   api.CapabilityField{Support: api.SupportYes},
		},
		Telemetry: api.HarnessTelemetryCapabilities{
			EnabledConfig: api.CapabilityField{Support: api.SupportYes},
			NativeEmitter: api.CapabilityField{Support: api.SupportNo, Reason: "Native telemetry forwarding is not defined for generic harnesses"},
		},
		Prompts: api.HarnessPromptCapabilities{
			SystemPrompt:      api.CapabilityField{Support: api.SupportPartial, Reason: "Saved under .scion/system_prompt.md, without harness-native behavior"},
			AgentInstructions: api.CapabilityField{Support: api.SupportYes},
		},
		Auth: api.HarnessAuthCapabilities{
			APIKey:   api.CapabilityField{Support: api.SupportYes},
			AuthFile: api.CapabilityField{Support: api.SupportYes},
			VertexAI: api.CapabilityField{Support: api.SupportYes},
		},
		Resume: api.CapabilityField{Support: api.SupportNo, Reason: "Generic harness does not support session resume"},
	}
}

func (g *Generic) GetEnv(agentName string, agentHome string, unixUsername string) map[string]string {
	env := make(map[string]string)
	env["SCION_AGENT_NAME"] = agentName
	return env
}

func (g *Generic) supportsModelFlag() bool {
	return false
}

func (g *Generic) GetCommand(task string, resume bool, baseArgs []string) []string {
	args := append([]string{}, baseArgs...)
	if task != "" {
		args = append(args, task)
	}
	return args
}

func (g *Generic) validateCommand(task string, resume bool, baseArgs []string) error {
	return validateGenericCommand(g.builtin, g.GetCommand(task, resume, baseArgs), resume)
}

func (g *Generic) DefaultConfigDir() string {
	return ".scion"
}

func (g *Generic) SkillsDir() string {
	return GenericSkillsDir
}

func (g *Generic) HasSystemPrompt(agentHome string) bool {
	return false
}

func (g *Generic) Provision(ctx context.Context, agentName, agentDir, agentHome, agentWorkspace string) error {
	return nil
}

func (g *Generic) GetInterruptKey() string {
	return "C-c"
}

func (g *Generic) GetInterruptSequence() []string {
	return nil
}

// DeliverySpec returns the default delivery spec; Generic has no config.
func (g *Generic) DeliverySpec() api.DeliverySpec {
	return config.DefaultDeliverySpec()
}

// ReadySpec returns the ready spec of the entry Resolve built this harness
// from, or the default (delay) spec when there is none.
func (g *Generic) ReadySpec() api.ReadySpec {
	if g.ready != nil {
		return *g.ready
	}
	return config.DefaultReadySpec(config.ReadyStrategyDelay)
}

func (g *Generic) GetHarnessEmbedsFS() (embed.FS, string) {
	return embed.FS{}, ""
}

func (g *Generic) GetTelemetryEnv() map[string]string {
	return nil
}

func (g *Generic) InjectAgentInstructions(agentHome string, content []byte) error {
	target := filepath.Join(agentHome, "agents.md")
	return os.WriteFile(target, content, 0644)
}

func (g *Generic) ResolveAuth(auth api.AuthConfig) (*api.ResolvedAuth, error) {
	// Generic harness is a passthrough — map all available creds, never error.
	result := &api.ResolvedAuth{
		Method:  "passthrough",
		EnvVars: make(map[string]string),
	}

	// GCP shared fields
	if auth.GoogleCloudProject != "" {
		result.EnvVars["GOOGLE_CLOUD_PROJECT"] = auth.GoogleCloudProject
	}
	if auth.GoogleCloudRegion != "" {
		result.EnvVars["GOOGLE_CLOUD_REGION"] = auth.GoogleCloudRegion
	}

	// Forward all config-driven auth env vars
	for k, v := range auth.EnvVars {
		if _, exists := result.EnvVars[k]; !exists {
			result.EnvVars[k] = v
		}
	}

	// GCP ADC file
	if auth.GoogleAppCredentials != "" {
		result.Files = append(result.Files, api.FileMapping{
			SourcePath:    auth.GoogleAppCredentials,
			ContainerPath: "~/.config/gcloud/application_default_credentials.json",
		})
	}

	return result, nil
}

// GenericSystemPromptRel is the default system prompt path written by the
// generic harness, relative to the agent home.
const GenericSystemPromptRel = ".scion/system_prompt.md"

func (g *Generic) InjectSystemPrompt(agentHome string, content []byte) error {
	target := filepath.Join(agentHome, filepath.FromSlash(GenericSystemPromptRel))
	if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
		return fmt.Errorf("failed to create directory for system prompt: %w", err)
	}
	return os.WriteFile(target, content, 0644)
}
