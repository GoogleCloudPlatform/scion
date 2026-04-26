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
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"gopkg.in/yaml.v3"
)

type HarnessConfigUpgradeOptions struct {
	DryRun         bool
	ActivateScript bool
	Force          bool
	Now            func() time.Time
}

type HarnessConfigUpgradePlan struct {
	Name    string                       `json:"name"`
	Path    string                       `json:"path"`
	Harness string                       `json:"harness"`
	DryRun  bool                         `json:"dry_run"`
	Changed bool                         `json:"changed"`
	Actions []HarnessConfigUpgradeAction `json:"actions,omitempty"`
	Backups []string                     `json:"backups,omitempty"`
}

type HarnessConfigUpgradeAction struct {
	Type   string `json:"type"`
	Path   string `json:"path,omitempty"`
	Detail string `json:"detail,omitempty"`
}

func UpgradeHarnessConfig(targetDir string, h api.Harness, opts HarnessConfigUpgradeOptions) (*HarnessConfigUpgradePlan, error) {
	if opts.Now == nil {
		opts.Now = time.Now
	}

	absTarget, err := filepath.Abs(targetDir)
	if err != nil {
		return nil, fmt.Errorf("resolve harness-config path: %w", err)
	}

	plan := &HarnessConfigUpgradePlan{
		Name:    filepath.Base(absTarget),
		Path:    absTarget,
		Harness: h.Name(),
		DryRun:  opts.DryRun,
	}

	if opts.Force {
		plan.Actions = append(plan.Actions, HarnessConfigUpgradeAction{
			Type:   "reset",
			Path:   absTarget,
			Detail: "reset harness-config to embedded defaults",
		})
		plan.Changed = true
		if !opts.DryRun {
			if err := SeedHarnessConfig(absTarget, h, true); err != nil {
				return plan, err
			}
		}
		return plan, nil
	}

	embedsFS, basePath := h.GetHarnessEmbedsFS()
	if basePath == "" {
		return plan, nil
	}

	configDir := h.DefaultConfigDir()
	configPath := filepath.Join(absTarget, "config.yaml")
	defaultConfigData, err := embedsFS.ReadFile(filepath.Join(basePath, "config.yaml"))
	if err != nil {
		return nil, fmt.Errorf("read embedded config.yaml: %w", err)
	}
	currentConfigData, err := os.ReadFile(configPath)
	if err != nil {
		return nil, fmt.Errorf("read harness-config config.yaml: %w", err)
	}

	mergedConfigData, changedConfig, fields, err := mergeHarnessConfigYAML(currentConfigData, defaultConfigData)
	if err != nil {
		return nil, err
	}
	for _, field := range fields {
		plan.Actions = append(plan.Actions, HarnessConfigUpgradeAction{
			Type:   "merge_config_field",
			Path:   "config.yaml",
			Detail: field,
		})
	}

	if opts.ActivateScript {
		hasScript := fileExists(filepath.Join(absTarget, "provision.py")) || embeddedFileExists(embedsFS, basePath, "provision.py")
		if !hasScript {
			return plan, fmt.Errorf("cannot activate script for %q: no provision.py found in existing or embedded harness-config", plan.Name)
		}
		var activated bool
		mergedConfigData, activated, err = activateContainerScriptProvisioner(mergedConfigData)
		if err != nil {
			return nil, err
		}
		if activated {
			changedConfig = true
			plan.Actions = append(plan.Actions, HarnessConfigUpgradeAction{
				Type:   "activate_script",
				Path:   "config.yaml",
				Detail: "set provisioner.type to container-script",
			})
		}
	}

	if changedConfig {
		plan.Changed = true
		if !opts.DryRun {
			backupPath, err := backupFile(configPath, opts.Now())
			if err != nil {
				return plan, err
			}
			plan.Backups = append(plan.Backups, backupPath)
			if err := os.WriteFile(configPath, mergedConfigData, 0644); err != nil {
				return plan, fmt.Errorf("write merged config.yaml: %w", err)
			}
		}
	}

	addedFiles, err := addMissingHarnessConfigFiles(absTarget, embedsFS, basePath, configDir, opts.DryRun)
	if err != nil {
		return plan, err
	}
	for _, relPath := range addedFiles {
		plan.Changed = true
		plan.Actions = append(plan.Actions, HarnessConfigUpgradeAction{
			Type: "add_file",
			Path: relPath,
		})
	}

	sort.SliceStable(plan.Actions, func(i, j int) bool {
		if plan.Actions[i].Type == plan.Actions[j].Type {
			return plan.Actions[i].Path < plan.Actions[j].Path
		}
		return plan.Actions[i].Type < plan.Actions[j].Type
	})

	return plan, nil
}

func mergeHarnessConfigYAML(currentData, defaultData []byte) ([]byte, bool, []string, error) {
	var current map[string]interface{}
	if err := yaml.Unmarshal(currentData, &current); err != nil {
		return nil, false, nil, fmt.Errorf("parse current config.yaml: %w", err)
	}
	var defaults map[string]interface{}
	if err := yaml.Unmarshal(defaultData, &defaults); err != nil {
		return nil, false, nil, fmt.Errorf("parse embedded config.yaml: %w", err)
	}
	if current == nil {
		current = map[string]interface{}{}
	}

	var fields []string
	changed := mergeMissingMapValues(current, defaults, "", &fields)
	if !changed {
		return currentData, false, nil, nil
	}

	data, err := yaml.Marshal(current)
	if err != nil {
		return nil, false, nil, fmt.Errorf("marshal merged config.yaml: %w", err)
	}
	return data, true, fields, nil
}

func mergeMissingMapValues(dst, src map[string]interface{}, prefix string, fields *[]string) bool {
	changed := false
	keys := make([]string, 0, len(src))
	for k := range src {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		srcVal := src[k]
		path := k
		if prefix != "" {
			path = prefix + "." + k
		}

		dstVal, exists := dst[k]
		if !exists || isEmptyYAMLValue(dstVal) {
			dst[k] = srcVal
			*fields = append(*fields, path)
			changed = true
			continue
		}

		dstMap, dstOK := dstVal.(map[string]interface{})
		srcMap, srcOK := srcVal.(map[string]interface{})
		if dstOK && srcOK {
			if mergeMissingMapValues(dstMap, srcMap, path, fields) {
				changed = true
			}
		}
	}
	return changed
}

func isEmptyYAMLValue(v interface{}) bool {
	if v == nil {
		return true
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.String, reflect.Slice, reflect.Map:
		return rv.Len() == 0
	default:
		return false
	}
}

func activateContainerScriptProvisioner(configData []byte) ([]byte, bool, error) {
	var cfg map[string]interface{}
	if err := yaml.Unmarshal(configData, &cfg); err != nil {
		return nil, false, fmt.Errorf("parse merged config.yaml: %w", err)
	}
	if cfg == nil {
		cfg = map[string]interface{}{}
	}
	provisioner, _ := cfg["provisioner"].(map[string]interface{})
	if provisioner == nil {
		provisioner = map[string]interface{}{}
		cfg["provisioner"] = provisioner
	}
	if provisioner["type"] == "container-script" {
		return configData, false, nil
	}
	provisioner["type"] = "container-script"
	if _, ok := provisioner["interface_version"]; !ok {
		provisioner["interface_version"] = 1
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return nil, false, fmt.Errorf("marshal activated config.yaml: %w", err)
	}
	return data, true, nil
}

func addMissingHarnessConfigFiles(targetDir string, embedsFS fs.FS, basePath, configDir string, dryRun bool) ([]string, error) {
	homeDir := filepath.Join(targetDir, "home")
	var added []string
	err := fs.WalkDir(embedsFS, basePath, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		relPath, err := filepath.Rel(basePath, path)
		if err != nil {
			return err
		}
		if relPath == "config.yaml" || relPath == "scion-agent.yaml" {
			return nil
		}
		targetPath := mapEmbedFileToHarnessConfigPath(targetDir, homeDir, configDir, relPath)
		if targetPath == "" || fileExists(targetPath) {
			return nil
		}
		targetRel, err := filepath.Rel(targetDir, targetPath)
		if err != nil {
			return err
		}
		added = append(added, filepath.ToSlash(targetRel))
		if dryRun {
			return nil
		}
		if err := os.MkdirAll(filepath.Dir(targetPath), 0755); err != nil {
			return err
		}
		data, err := fs.ReadFile(embedsFS, path)
		if err != nil {
			return err
		}
		return os.WriteFile(targetPath, data, 0644)
	})
	sort.Strings(added)
	return added, err
}

func backupFile(path string, now time.Time) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read file for backup: %w", err)
	}
	backupPath := fmt.Sprintf("%s.bak.%s", path, now.UTC().Format("20060102T150405Z"))
	if err := os.WriteFile(backupPath, data, 0644); err != nil {
		return "", fmt.Errorf("write backup %s: %w", backupPath, err)
	}
	return backupPath, nil
}

func embeddedFileExists(fsys fs.FS, basePath, relPath string) bool {
	_, err := fs.Stat(fsys, filepath.ToSlash(filepath.Join(basePath, relPath)))
	return err == nil
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func SummarizeHarnessConfigUpgradePlan(plan *HarnessConfigUpgradePlan) string {
	if plan == nil {
		return ""
	}
	if len(plan.Actions) == 0 {
		return fmt.Sprintf("%s: already up to date", plan.Name)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s: %d action(s)", plan.Name, len(plan.Actions))
	for _, action := range plan.Actions {
		if action.Detail != "" {
			fmt.Fprintf(&b, "\n  - %s %s (%s)", action.Type, action.Path, action.Detail)
		} else {
			fmt.Fprintf(&b, "\n  - %s %s", action.Type, action.Path)
		}
	}
	return b.String()
}
