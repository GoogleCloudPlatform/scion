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

package brokerconvert

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// AgentsNotMovableNote closes the bound-agents section.
const AgentsNotMovableNote = "Agents cannot be moved onto a flat Runtime Broker today: finish or recreate them before the host switches."

// WriteJSON writes the plan as one indented JSON object.
func WriteJSON(w io.Writer, p *Plan) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(p)
}

// HumanOptions are the human output's options.
type HumanOptions struct {
	// ShowEnvValues prints harness env values in the settings block
	// instead of RedactedValue. The output then contains secrets.
	ShowEnvValues bool
}

// ShowEnvValuesWarning heads a human plan printed with ShowEnvValues.
const ShowEnvValuesWarning = "WARNING: this output contains harness env values from your settings, which can be secrets. Do not share it."

// InstancesYAML is the proposed server.broker.instances block, ready to
// paste into the global settings file. Harness env values are replaced by
// RedactedValue unless showEnvValues is set; keys are kept.
func InstancesYAML(instances []config.V1RuntimeBrokerInstanceConfig, showEnvValues bool) (string, error) {
	if !showEnvValues {
		instances = redactEnvValues(instances)
	}
	doc := map[string]any{
		"server": map[string]any{
			"broker": map[string]any{
				"instances": instances,
			},
		},
	}
	var buf bytes.Buffer
	enc := yamlv3.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return "", err
	}
	if err := enc.Close(); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// redactEnvValues returns a copy of instances whose harness env values are
// RedactedValue. instances itself is not changed.
func redactEnvValues(instances []config.V1RuntimeBrokerInstanceConfig) []config.V1RuntimeBrokerInstanceConfig {
	out := make([]config.V1RuntimeBrokerInstanceConfig, len(instances))
	for i, inst := range instances {
		out[i] = inst
		if inst.AgentDefaults == nil || len(inst.AgentDefaults.HarnessOverrides) == 0 {
			continue
		}
		ad := *inst.AgentDefaults
		overrides := make(map[string]config.V1HarnessOverride, len(ad.HarnessOverrides))
		for name, o := range ad.HarnessOverrides {
			if len(o.Env) > 0 {
				env := make(map[string]string, len(o.Env))
				for k := range o.Env {
					env[k] = RedactedValue
				}
				o.Env = env
			}
			overrides[name] = o
		}
		ad.HarnessOverrides = overrides
		out[i].AgentDefaults = &ad
	}
	return out
}

// WriteHuman writes the plan's five sections and the summary, in a fixed
// order.
func WriteHuman(w io.Writer, p *Plan, opts HumanOptions) error {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n", TipNote)
	if opts.ShowEnvValues {
		fmt.Fprintf(&b, "%s\n", ShowEnvValuesWarning)
	}
	if p.LegacyBroker != nil {
		fmt.Fprintf(&b, "Legacy Runtime Broker: %s (%s)\n", p.LegacyBroker.Name, p.LegacyBroker.ID)
	}
	if p.Summary.ProfilesInScope == 0 {
		b.WriteString("\nNothing to convert: no profiles in scope.\n")
		_, err := io.WriteString(w, b.String())
		return err
	}

	b.WriteString("\n1. Proposed instances\n")
	if len(p.settingsInstances) == 0 {
		b.WriteString("   (none: every profile in scope has a blocking finding)\n")
	} else {
		y, err := InstancesYAML(p.settingsInstances, opts.ShowEnvValues)
		if err != nil {
			return err
		}
		b.WriteString("# Proposed, not applied. Keys and names are editable.\n")
		b.WriteString(y)
	}

	b.WriteString("\n2. Profile map\n")
	for _, m := range p.ProfileMap {
		if m.Instance != "" {
			fmt.Fprintf(&b, "   %s -> %s\n", m.Profile, m.Instance)
		} else {
			fmt.Fprintf(&b, "   %s -> (no mapping: %s)\n", m.Profile, m.BlockedBy)
		}
	}
	if lb := p.LegacyBroker; lb != nil && (len(lb.LocalOnly) > 0 || len(lb.HubOnly) > 0) {
		b.WriteString("   Notice: the Hub's profile list for this Runtime Broker differs from the local settings.\n")
		if len(lb.LocalOnly) > 0 {
			fmt.Fprintf(&b, "     local only: %s\n", strings.Join(lb.LocalOnly, ", "))
		}
		if len(lb.HubOnly) > 0 {
			fmt.Fprintf(&b, "     Hub only: %s\n", strings.Join(lb.HubOnly, ", "))
		}
	}

	b.WriteString("\n3. Findings\n")
	if len(p.Findings) == 0 {
		b.WriteString("   (none)\n")
	}
	for _, f := range p.Findings {
		fmt.Fprintf(&b, "   [%s] %s %s", f.Severity, f.Code, f.Path)
		if len(f.Profiles) > 0 {
			fmt.Fprintf(&b, " (profiles: %s)", strings.Join(f.Profiles, ", "))
		}
		fmt.Fprintf(&b, "\n     %s\n", f.Text)
	}

	b.WriteString("\n4. Capacity\n")
	writeCapacity(&b, p.Capacity)

	b.WriteString("\n5. Still bound on the Hub\n")
	if p.Agents == nil && p.Projects == nil {
		b.WriteString("   (skipped: needs the Hub)\n")
	} else {
		writeAgents(&b, p.Agents)
		writeProjects(&b, p.Projects)
	}

	b.WriteString("\nSummary\n")
	s := p.Summary
	fmt.Fprintf(&b, "   %s in scope, %d mapped, %s, %s, %s",
		count(s.ProfilesInScope, "profile"), s.MappedProfiles, count(s.Instances, "proposed instance"),
		count(s.BlockingFindings, "blocking finding"), count(s.NoticeFindings, "notice finding"))
	if p.Agents != nil || p.Projects != nil {
		fmt.Fprintf(&b, ", %s, %s", count(s.Agents, "bound agent"), count(s.Projects, "providing project"))
	}
	b.WriteString(".\n")
	var verdict []string
	if s.BlockingFindings == 0 {
		verdict = append(verdict, "no blocking findings")
	} else {
		verdict = append(verdict, count(s.BlockingFindings, "blocking finding"))
	}
	if s.DecisionRequired {
		verdict = append(verdict, "operator decisions required")
	}
	switch p.Capacity.Status {
	case CapacityNeedsHub:
		verdict = append(verdict, "capacity not planned (needs the Hub)")
	case CapacityUnavailable:
		verdict = append(verdict, "capacity not planned (the Runtime Broker's settings were not read)")
	}
	fmt.Fprintf(&b, "   Result: %s.\n", strings.Join(verdict, "; "))

	_, err := io.WriteString(w, b.String())
	return err
}

func writeCapacity(b *strings.Builder, c Capacity) {
	switch c.Status {
	case CapacityPlanned:
		fmt.Fprintf(b, "   Outcome: %s\n", c.Outcome)
		if c.MaxAgents != nil {
			fmt.Fprintf(b, "   Legacy maxAgents: %d\n", *c.MaxAgents)
		} else {
			b.WriteString("   Legacy maxAgents: not set (inherited)\n")
		}
		for _, a := range c.Allocations {
			fmt.Fprintf(b, "   %s: maxAgents %d\n", a.Instance, a.MaxAgents)
		}
		for _, r := range c.Reasons {
			fmt.Fprintf(b, "   Decision: %s\n", r)
		}
		for _, f := range c.Flags {
			fmt.Fprintf(b, "   Note: %s\n", f)
		}
	default:
		fmt.Fprintf(b, "   (%s) %s\n", c.Status, c.Note)
	}
}

func refText(r ProfileRef) string {
	switch r.Mapping {
	case MappingMapped:
		return fmt.Sprintf("%s -> %s", r.Profile, r.Instance)
	case MappingProfileUnknown:
		return "(profile not recorded)"
	}
	return fmt.Sprintf("%s -> (no mapping)", r.Profile)
}

func writeAgents(b *strings.Builder, s *AgentsSection) {
	b.WriteString("   Agents:\n")
	if s == nil {
		return
	}
	if s.Note != "" {
		fmt.Fprintf(b, "     (%s)\n", s.Note)
	}
	if len(s.Items) == 0 && s.Note == "" {
		b.WriteString("     (none)\n")
	}
	for _, a := range s.Items {
		fmt.Fprintf(b, "     %s  %s  project %s  phase %s  %s\n", a.ID, a.Name, a.Project, a.Phase, refText(a.ProfileRef))
	}
	if s.TerminalCount > 0 {
		fmt.Fprintf(b, "     (%s in a terminal phase not listed)\n", count(s.TerminalCount, "agent"))
	}
	fmt.Fprintf(b, "     %s\n", AgentsNotMovableNote)
}

func writeProjects(b *strings.Builder, s *ProjectsSection) {
	b.WriteString("   Projects:\n")
	if s == nil {
		return
	}
	if s.Note != "" {
		fmt.Fprintf(b, "     (%s)\n", s.Note)
	}
	if len(s.Items) == 0 && s.Note == "" {
		b.WriteString("     (none)\n")
	}
	for _, e := range s.Items {
		fmt.Fprintf(b, "     %s  %s\n", e.ID, e.Name)
		if e.Note != "" {
			fmt.Fprintf(b, "       (%s)\n", e.Note)
		}
		if e.ActiveProfile != nil {
			fmt.Fprintf(b, "       activeProfile: %s\n", refText(*e.ActiveProfile))
		}
		for _, r := range e.ServiceAccountProfiles {
			fmt.Fprintf(b, "       service account for profile: %s\n", refText(r))
		}
	}
}

// count formats n with noun, adding "s" unless n is 1.
func count(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
