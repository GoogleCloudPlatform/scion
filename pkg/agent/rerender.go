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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/agentplugin"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
)

// Start-time re-render of provision-rendered files (ptone/scion#3982).
//
// Provisioning renders four config keys into files in the agent home: the
// agent instructions and system prompt (through the harness), the skills
// (installed into the harness's skills directory, with a resolution
// record) and the services (.scion/scion-services.yaml). A start only
// merges the hub's inline config into scion-agent.json, so a change to one
// of them would otherwise reach the agent only at a reincarnation. The hub
// lists the keys it changed in ScionConfig.ChangedFields (and the ones it
// cleared in ClearedFields); the broker passes them to Start as
// StartOptions.RerenderKeys, and Start re-runs the provisioning step of
// each listed key (rerenderProvisionedFiles) with the same code
// ProvisionAgent uses.
//
// The hub's lists are sticky, so a key stays listed on every later start.
// The re-render runs once per change: the rerender record keeps a
// fingerprint of what was last rendered for each group of keys, and a
// group whose fingerprint is unchanged is not written again, so files the
// agent or its harness changed since are left alone until the hub's value
// changes again. The record lies in the agent directory, outside every
// container mount (config.RerenderRecordFileName), and provisioning
// resets it (dropping every fingerprint and recording where it rendered
// the system prompt), so a new generation of the agent never matches a
// fingerprint from the previous one.

// Rerender groups: the keys whose files one provisioning step renders
// together. The instructions and the system prompt are one group because
// a harness may prepend the system prompt to the instructions.
const (
	rerenderGroupPrompts  = "prompts"
	rerenderGroupSkills   = "skills"
	rerenderGroupServices = "services"
)

// rerenderGroupOf maps a rerenderable config key to its group.
var rerenderGroupOf = map[string]string{
	"system_prompt":      rerenderGroupPrompts,
	"agent_instructions": rerenderGroupPrompts,
	"skills":             rerenderGroupSkills,
	"services":           rerenderGroupServices,
}

// promptInputNames are the control-plane inputs the prompt group stages for
// a container-script harness (its InjectAgentInstructions and
// InjectSystemPrompt).
var promptInputNames = []string{"instructions.md", "system-prompt.md"}

// resolvedSkillsInputName is the control-plane input the skills group
// stages for a container-script harness.
const resolvedSkillsInputName = "resolved-skills.json"

// rerenderInputs is what rerenderProvisionedFiles needs from the start.
type rerenderInputs struct {
	// Keys are the config keys to re-render (StartOptions.RerenderKeys).
	Keys       []string
	AgentName  string
	AgentDir   string
	AgentHome  string
	ProjectDir string
	Harness    api.Harness
	// Chain is the agent's live template chain; nil when it does not
	// resolve.
	Chain []*config.Template
	// Config is the agent's resolved config for this start.
	Config *api.ScionConfig
	// Inline is the hub's inline config for this start: a prompt value it
	// sets is content, as at provision.
	Inline *api.ScionConfig
	// WriteHome runs a write into the agent home (writeAgentHome at
	// start); nil runs it directly.
	WriteHome func(func() error) error
}

// rerenderRecord is the rerender record.
type rerenderRecord struct {
	// Fingerprints holds the fingerprint of what was last rendered for
	// each group.
	Fingerprints map[string]string `json:"fingerprints,omitempty"`
	// SystemPromptPaths are the agent-home paths (slash-separated,
	// relative) Scion last rendered the system prompt to, for a harness
	// that reads prompts from the agent home: written by provisioning and
	// by each prompt re-render. Only these are removed when a later render
	// no longer produces them, so a file a template's home/ supplies is
	// kept, as a reincarnation keeps it. (One narrow exception: when
	// provisioning rendered the prompt over a file the template's home/
	// supplies at the same path, that path is recorded, and a later clear
	// removes it where a reincarnation would restore the template's file.)
	SystemPromptPaths []string `json:"systemPromptPaths,omitempty"`
	// SystemPromptPathsKnown is set once SystemPromptPaths is maintained
	// for this agent (by provisioning or a prompt re-render). Without it
	// (an agent provisioned before the record existed), every system-prompt
	// target of the harness that the render does not produce is treated as
	// Scion-rendered, so a clear still removes the old prompt.
	SystemPromptPathsKnown bool `json:"systemPromptPathsKnown,omitempty"`
}

func rerenderRecordPath(agentDir string) string {
	return filepath.Join(agentDir, config.RerenderRecordFileName)
}

func readRerenderRecord(agentDir string) rerenderRecord {
	rec := rerenderRecord{}
	if data, err := os.ReadFile(rerenderRecordPath(agentDir)); err == nil {
		if json.Unmarshal(data, &rec) != nil {
			rec = rerenderRecord{}
		}
	}
	if rec.Fingerprints == nil {
		rec.Fingerprints = map[string]string{}
	}
	return rec
}

// writeRerenderRecord writes rec to a new temporary file in agentDir and
// renames it over the record, so a reader never sees a torn record.
func writeRerenderRecord(agentDir string, rec rerenderRecord) error {
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(agentDir, "."+config.RerenderRecordFileName+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, rerenderRecordPath(agentDir))
}

// resetRerenderRecord replaces the rerender record with one holding no
// fingerprints and the system-prompt paths provisioning just rendered
// (ProvisionAgent: the files it renders are the new baseline).
func resetRerenderRecord(agentDir string, systemPromptPaths []string) error {
	return writeRerenderRecord(agentDir, rerenderRecord{SystemPromptPaths: systemPromptPaths, SystemPromptPathsKnown: true})
}

// rerenderProvisionedFiles re-renders the files provisioning wrote for
// in.Keys whose fingerprint changed since the last render, and records the
// new fingerprints. It writes only the files the provisioning step of each
// key writes: the harness's instruction and system-prompt files (for a
// container-script harness, the control-plane inputs record they are
// restaged from), the installed skills and their resolution record, and
// the services file. It returns warnings for keys it could not re-render;
// an error fails the start, as the same failure fails a provision.
func rerenderProvisionedFiles(ctx context.Context, in rerenderInputs) ([]string, error) {
	groups := map[string]bool{}
	for _, k := range in.Keys {
		if g, ok := rerenderGroupOf[k]; ok {
			groups[g] = true
		}
	}
	if len(groups) == 0 || in.Config == nil || in.Harness == nil {
		return nil, nil
	}
	write := in.WriteHome
	if write == nil {
		write = func(f func() error) error { return f() }
	}
	rec := readRerenderRecord(in.AgentDir)
	var warnings []string
	changed := false

	if groups[rerenderGroupPrompts] {
		fp, warn, err := rerenderPrompts(in, &rec, write)
		if err != nil {
			return warnings, err
		}
		if warn != "" {
			warnings = append(warnings, warn)
		} else if fp != rec.Fingerprints[rerenderGroupPrompts] {
			rec.Fingerprints[rerenderGroupPrompts] = fp
			changed = true
		}
	}
	if groups[rerenderGroupSkills] {
		fp, err := fingerprintJSON(in.Config.Skills)
		if err != nil {
			return warnings, err
		}
		if fp != rec.Fingerprints[rerenderGroupSkills] {
			if err := write(func() error { return rerenderSkills(ctx, in) }); err != nil {
				return warnings, err
			}
			rec.Fingerprints[rerenderGroupSkills] = fp
			changed = true
		}
	}
	if groups[rerenderGroupServices] {
		fp, err := fingerprintJSON(in.Config.Services)
		if err != nil {
			return warnings, err
		}
		if fp != rec.Fingerprints[rerenderGroupServices] {
			if err := write(func() error { return rerenderServices(in.AgentHome, in.Config.Services) }); err != nil {
				return warnings, err
			}
			rec.Fingerprints[rerenderGroupServices] = fp
			changed = true
		}
	}
	if changed {
		if err := writeRerenderRecord(in.AgentDir, rec); err != nil {
			return warnings, fmt.Errorf("record re-rendered files: %w", err)
		}
	}
	return warnings, nil
}

// rerenderPrompts renders the instructions and system prompt into a
// scratch home with injectPromptFiles (the provisioning step), fingerprints
// the result, and, when it differs from recorded, copies it into place: the
// control-plane inputs record for a container-script harness (Start
// restages inputs from it), else the agent home. It returns the
// fingerprint, or a warning when the prompts cannot be rendered because a
// value names a template file and the template does not resolve.
func rerenderPrompts(in rerenderInputs, rec *rerenderRecord, write func(func() error) error) (string, string, error) {
	recorded := rec.Fingerprints[rerenderGroupPrompts]
	if len(in.Chain) == 0 {
		// Without a template, provisioning renders a value as content; a
		// value the hub did not send is a template file name here.
		for _, f := range []struct{ cfg, inline string }{
			{in.Config.AgentInstructions, inlineString(in.Inline, func(c *api.ScionConfig) string { return c.AgentInstructions })},
			{in.Config.SystemPrompt, inlineString(in.Inline, func(c *api.ScionConfig) string { return c.SystemPrompt })},
		} {
			if f.cfg != "" && f.inline == "" {
				return "", "system_prompt, agent_instructions: not re-rendered at this start because the agent's template could not be resolved; they take effect at the next reincarnation", nil
			}
		}
	}
	scratch, err := os.MkdirTemp("", "scion-rerender-")
	if err != nil {
		return "", "", err
	}
	defer func() { _ = os.RemoveAll(scratch) }()
	// A prompt value the hub sends is content and wins, as the merge into
	// the persisted config made it win at provision.
	cfg := *in.Config
	if in.Inline != nil {
		if in.Inline.AgentInstructions != "" {
			cfg.AgentInstructions = in.Inline.AgentInstructions
		}
		if in.Inline.SystemPrompt != "" {
			cfg.SystemPrompt = in.Inline.SystemPrompt
		}
	}
	if _, err := injectPromptFiles(in.Harness, scratch, in.Chain, &cfg, in.Inline); err != nil {
		return "", "", err
	}
	files, err := readTree(scratch)
	if err != nil {
		return "", "", err
	}
	fp := fingerprintFiles(files)
	if fp == recorded {
		return fp, "", nil
	}
	if isContainerScriptHarness(in.Harness) {
		record := filepath.Join(in.AgentDir, controlPlaneInputsDirName)
		if err := os.MkdirAll(record, 0o755); err != nil {
			return "", "", err
		}
		staged := filepath.ToSlash(filepath.Join(".scion", "harness", "inputs"))
		for _, name := range promptInputNames {
			dst := filepath.Join(record, name)
			data, ok := files[staged+"/"+name]
			if !ok {
				if err := os.Remove(dst); err != nil && !os.IsNotExist(err) {
					return "", "", err
				}
				continue
			}
			if err := os.WriteFile(dst, data, 0o644); err != nil {
				return "", "", err
			}
		}
		util.Debugf("rerender: staged prompts for agent %s in the inputs record", in.AgentName)
		return fp, "", nil
	}
	// A system-prompt file Scion rendered last time (provisioning or an
	// earlier re-render, rec.SystemPromptPaths) that this render no longer
	// produces is removed: a cleared prompt with no template default
	// reverts to nothing. Any other file the render does not produce, such
	// as one a template's home/ supplies, is left alone, as a reincarnation
	// leaves it.
	targets, err := systemPromptTargets(in.Harness)
	if err != nil {
		return "", "", err
	}
	var stale, rendered []string
	scionRendered := rec.SystemPromptPaths
	if !rec.SystemPromptPathsKnown && in.Inline != nil && slices.Contains(in.Inline.ClearedFields, "system_prompt") {
		// An agent provisioned before the record listed these paths: when
		// this start clears the system prompt, every target counts as
		// Scion-rendered, so the old prompt goes. Otherwise nothing is
		// removed.
		scionRendered = targets
	}
	for _, rel := range scionRendered {
		if _, ok := files[rel]; !ok {
			stale = append(stale, rel)
		}
	}
	for _, rel := range targets {
		if _, ok := files[rel]; ok {
			rendered = append(rendered, rel)
		}
	}
	// Writes are confined to the agent home (home_confined_write_*.go).
	err = write(func() error {
		names := make([]string, 0, len(files))
		for rel := range files {
			names = append(names, rel)
		}
		sort.Strings(names)
		for _, rel := range names {
			if err := writeHomeFile(in.AgentHome, rel, files[rel], 0o644); err != nil {
				return fmt.Errorf("write %s: %w", rel, err)
			}
		}
		for _, rel := range stale {
			if err := removeHomeFile(in.AgentHome, rel); err != nil {
				return fmt.Errorf("remove %s: %w", rel, err)
			}
		}
		return nil
	})
	if err != nil {
		return "", "", err
	}
	rec.SystemPromptPaths = rendered
	rec.SystemPromptPathsKnown = true
	util.Debugf("rerender: wrote prompts for agent %s into the agent home", in.AgentName)
	return fp, "", nil
}

// systemPromptTargets returns the agent-home paths (slash-separated,
// relative) harness h writes only the system prompt to, found by rendering
// placeholder content into scratch homes: the paths InjectSystemPrompt
// writes that InjectAgentInstructions does not (a system prompt prepended
// to the instructions shares their file, which is always rendered).
func systemPromptTargets(h api.Harness) ([]string, error) {
	instructions, err := scratchRender(func(dir string) error { return h.InjectAgentInstructions(dir, []byte("instructions")) })
	if err != nil {
		return nil, err
	}
	prompt, err := scratchRender(func(dir string) error { return h.InjectSystemPrompt(dir, []byte("system prompt")) })
	if err != nil {
		return nil, err
	}
	var out []string
	for rel := range prompt {
		if _, shared := instructions[rel]; !shared {
			out = append(out, rel)
		}
	}
	sort.Strings(out)
	return out, nil
}

// scratchRender runs render on a new scratch directory and returns the
// files it wrote.
func scratchRender(render func(dir string) error) (map[string][]byte, error) {
	scratch, err := os.MkdirTemp("", "scion-rerender-targets-")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(scratch) }()
	if err := render(scratch); err != nil {
		return nil, err
	}
	return readTree(scratch)
}

// rerenderSkippedWarning is the start warning for re-render keys a start
// could not act on because the harness configuration did not resolve.
func rerenderSkippedWarning(keys []string) string {
	return strings.Join(keys, ", ") + ": not re-rendered at this start because the agent's harness configuration could not be resolved; they take effect at the next start that resolves it, or at the next reincarnation"
}

// rerenderSkills resolves and installs the agent's skills with the
// provisioning step (resolveAndInstallSkills) into a scratch home, then
// installs each resolved skill into the agent home confined to it
// (installHomeDir), writes the resolution record there with the skills
// already in the home listed as local, as provisioning does, and, for a
// container-script harness, records it in the control-plane inputs record
// Start restages from. Skills dropped from the list stay installed, as
// after a reprovision.
func rerenderSkills(ctx context.Context, in rerenderInputs) error {
	scratch, err := os.MkdirTemp("", "scion-rerender-skills-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(scratch) }()
	record, err := resolveAndInstallSkills(ctx, in.AgentName, in.ProjectDir, in.Harness, scratch, in.Config.Skills)
	if err != nil {
		return err
	}
	if record == nil {
		return nil
	}
	skillsDir := in.Harness.SkillsDir()
	resolved := map[string]bool{}
	for _, e := range record.Skills {
		name := path.Base(e.InstalledPath)
		if name == "." || name == "/" || name == ".." || strings.ContainsAny(name, "/\\") {
			return fmt.Errorf("invalid installed skill name %q", name)
		}
		resolved[name] = true
		rel := filepath.Join(skillsDir, name)
		if err := installHomeDir(filepath.Join(scratch, rel), in.AgentHome, rel); err != nil {
			return fmt.Errorf("install skill %s: %w", name, err)
		}
	}
	home, err := os.OpenRoot(in.AgentHome)
	if err != nil {
		return err
	}
	local, err := rootLocalSkills(home, skillsDir, resolved)
	_ = home.Close()
	if err != nil {
		return err
	}
	record.Skills = append(local, record.Skills...)
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	if err := writeHomeFile(in.AgentHome, filepath.Join(".scion", resolvedSkillsInputName), data, 0o644); err != nil {
		return fmt.Errorf("write skill resolution record: %w", err)
	}
	if !isContainerScriptHarness(in.Harness) {
		return nil
	}
	dir := filepath.Join(in.AgentDir, controlPlaneInputsDirName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, resolvedSkillsInputName), data, 0o644)
}

// rootLocalSkills lists, as enumerateLocalSkills does, the skill
// directories already in the agent home's skills directory, except the
// names in skip, without following links: an entry that is not a real
// directory is not a skill. A skills directory that is missing or not a
// real directory lists none.
func rootLocalSkills(home *os.Root, skillsDir string, skip map[string]bool) ([]SkillResolutionEntry, error) {
	if ok, err := realDirChain(home, filepath.Clean(skillsDir), true); err != nil || !ok {
		return nil, err
	}
	entries, err := readRootDir(home, skillsDir)
	if err != nil {
		return nil, err
	}
	var out []SkillResolutionEntry
	for _, e := range entries {
		if !e.IsDir() || e.Type()&os.ModeSymlink != 0 || strings.HasPrefix(e.Name(), ".") || skip[e.Name()] {
			continue
		}
		if fi, err := home.Lstat(filepath.Join(skillsDir, e.Name(), agentplugin.ManifestFile)); err == nil && fi.Mode().IsRegular() {
			continue
		}
		out = append(out, SkillResolutionEntry{
			Name:          e.Name(),
			InstalledPath: filepath.ToSlash(filepath.Join(skillsDir, e.Name())),
			Source:        "local",
		})
	}
	return out, nil
}

// rerenderServices writes the services file (writeServicesFile, the
// provisioning step), or removes it when the agent has no services,
// confined to the agent home.
func rerenderServices(agentHome string, services []api.ServiceSpec) error {
	if len(services) > 0 {
		return writeServicesFile(agentHome, services)
	}
	return removeHomeFile(agentHome, filepath.Join(".scion", servicesFileName))
}

func inlineString(c *api.ScionConfig, get func(*api.ScionConfig) string) string {
	if c == nil {
		return ""
	}
	return get(c)
}

// readTree reads every regular file under root, keyed by slash-separated
// relative path.
func readTree(root string) (map[string][]byte, error) {
	files := map[string][]byte{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		files[filepath.ToSlash(rel)] = data
		return nil
	})
	return files, err
}

// fingerprintFiles hashes files (path and content, in path order).
func fingerprintFiles(files map[string][]byte) string {
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	h := sha256.New()
	for _, n := range names {
		fmt.Fprintf(h, "%d:%s\n%d:", len(n), n, len(files[n]))
		h.Write(files[n])
	}
	return hex.EncodeToString(h.Sum(nil))
}

// fingerprintJSON hashes v's JSON encoding; an empty slice or map hashes
// like nil.
func fingerprintJSON(v any) (string, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	if s := strings.TrimSpace(string(data)); s == "[]" || s == "{}" {
		data = []byte("null")
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}
