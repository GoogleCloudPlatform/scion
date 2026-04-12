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
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
	"gopkg.in/yaml.v3"
)

type Template struct {
	Name  string
	Path  string
	Scope string // "global" or "grove"
}

// ResolveContent resolves a template field value to its content bytes.
// If field is empty, returns nil, nil.
// If a file at t.Path/field exists, reads and returns its content.
// Otherwise, returns field as inline content bytes.
func (t *Template) ResolveContent(field string) ([]byte, error) {
	if field == "" {
		return nil, nil
	}

	filePath := filepath.Join(t.Path, field)
	if _, err := os.Stat(filePath); err == nil {
		data, err := os.ReadFile(filePath)
		if err != nil {
			return nil, fmt.Errorf("failed to read content file %s: %w", filePath, err)
		}
		return data, nil
	}

	return []byte(field), nil
}

// ResolveContentInChain resolves a template field value by searching the
// template chain in reverse order (most specific first). This ensures that
// when a custom template inherits a file reference like "agents.md" from a
// parent template's config, the file is found in the parent template even
// if it doesn't exist in the custom template's directory. Only falls back
// to treating the field as inline content if no template in the chain
// contains the file.
func ResolveContentInChain(chain []*Template, field string) ([]byte, error) {
	if field == "" {
		return nil, nil
	}

	// Walk chain in reverse (most specific first) looking for the file
	for i := len(chain) - 1; i >= 0; i-- {
		filePath := filepath.Join(chain[i].Path, field)
		if _, err := os.Stat(filePath); err == nil {
			data, err := os.ReadFile(filePath)
			if err != nil {
				return nil, fmt.Errorf("failed to read content file %s: %w", filePath, err)
			}
			return data, nil
		}
	}

	// No template had the file; treat as inline content
	return []byte(field), nil
}

func (t *Template) LoadConfig() (*api.ScionConfig, error) {
	// Try YAML first, then JSON
	configPath := GetScionAgentConfigPath(t.Path)
	if configPath == "" {
		// No config file found, return empty config
		return &api.ScionConfig{}, nil
	}

	data, err := os.ReadFile(configPath)
	if err != nil {
		if os.IsNotExist(err) {
			return &api.ScionConfig{}, nil
		}
		return nil, err
	}

	var cfg api.ScionConfig
	ext := filepath.Ext(configPath)
	if ext == ".yaml" || ext == ".yml" {
		if err := unmarshalYAMLNormalized(data, &cfg); err != nil {
			return nil, fmt.Errorf("failed to parse YAML config %s: %w", configPath, err)
		}
	} else {
		if err := json.Unmarshal(data, &cfg); err != nil {
			return nil, fmt.Errorf("failed to parse JSON config %s: %w", configPath, err)
		}
	}

	if err := api.ValidateVolumes(cfg.Volumes); err != nil {
		return nil, fmt.Errorf("invalid volume config in %s: %w", configPath, err)
	}

	if err := api.ValidateServices(cfg.Services); err != nil {
		return nil, fmt.Errorf("invalid services config in %s: %w", configPath, err)
	}

	return &cfg, nil
}

// unmarshalYAMLNormalized parses YAML into a ScionConfig, normalizing
// top-level hyphenated keys to underscored keys. This allows template
// authors to use either `harness-config` or `harness_config` style keys.
func unmarshalYAMLNormalized(data []byte, cfg *api.ScionConfig) error {
	var node yaml.Node
	if err := yaml.Unmarshal(data, &node); err != nil {
		return err
	}
	normalizeYAMLMappingKeys(&node)
	return node.Decode(cfg)
}

// normalizeYAMLMappingKeys replaces hyphens with underscores in the
// top-level mapping keys of a YAML document node. Only the first level
// of mapping keys is normalized to avoid altering map values such as
// env variable names.
func normalizeYAMLMappingKeys(node *yaml.Node) {
	if node.Kind == yaml.DocumentNode {
		for _, child := range node.Content {
			normalizeYAMLMappingKeys(child)
		}
		return
	}
	if node.Kind == yaml.MappingNode {
		for i := 0; i < len(node.Content); i += 2 {
			key := node.Content[i]
			if key.Kind == yaml.ScalarNode {
				key.Value = strings.ReplaceAll(key.Value, "-", "_")
			}
		}
	}
}

func LoadProjectKubernetesConfig() (*api.KubernetesConfig, error) {
	path, err := GetProjectKubernetesConfigPath()
	if err != nil {
		return nil, err
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var cfg api.KubernetesConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func FindTemplate(name string) (*Template, error) {
	return FindTemplateWithContext(context.Background(), name)
}

// FindTemplateWithContext finds a template by name, supporting remote URIs.
// Remote templates are fetched and cached locally before being returned.
func FindTemplateWithContext(ctx context.Context, name string) (*Template, error) {
	// 0. Check if name is a remote URI (URL or rclone connection string)
	if IsRemoteURI(name) {
		// Validate the URI format
		if err := ValidateRemoteURI(name); err != nil {
			return nil, fmt.Errorf("invalid remote template URI: %w", err)
		}

		// Fetch the remote template to local cache
		cachedPath, err := FetchRemoteTemplate(ctx, name)
		if err != nil {
			return nil, fmt.Errorf("failed to fetch remote template: %w", err)
		}

		// Derive a short name from the URI for display purposes
		shortName := DeriveTemplateName(name)

		return &Template{Name: shortName, Path: cachedPath}, nil
	}

	// 1. Check if name is an absolute path
	if filepath.IsAbs(name) {
		if info, err := os.Stat(name); err == nil && info.IsDir() {
			return &Template{Name: filepath.Base(name), Path: name}, nil
		}
		return nil, fmt.Errorf("template path %s not found or not a directory", name)
	}

	// 2. Check project-local templates
	projectTemplatesDir, err := GetProjectTemplatesDir()
	if err == nil {
		path := filepath.Join(projectTemplatesDir, name)
		if info, err := os.Stat(path); err == nil && info.IsDir() {
			return &Template{Name: name, Path: path}, nil
		}
	}

	// 3. Check global templates
	globalTemplatesDir, err := GetGlobalTemplatesDir()
	if err == nil {
		path := filepath.Join(globalTemplatesDir, name)
		if info, err := os.Stat(path); err == nil && info.IsDir() {
			return &Template{Name: name, Path: path}, nil
		}
	}

	// TODO: Future enhancement - when operating with a remote hub system,
	// simple template names could also be resolved to remote storage locations:
	// <bucket-name>/<scion-prefix>/<grove-id>/templates/<template-name>
	// This would enable shared templates across teams/organizations.

	return nil, fmt.Errorf("template %s not found", name)
}

// FindTemplateInScope finds a template by name in a specific scope only.
// Scope must be "global" or "grove". Returns nil if not found in that scope.
func FindTemplateInScope(name, scope string) *Template {
	var dir string
	var err error

	switch scope {
	case "global":
		dir, err = GetGlobalTemplatesDir()
	case "grove":
		dir, err = GetProjectTemplatesDir()
	default:
		return nil
	}

	if err != nil {
		return nil
	}

	path := filepath.Join(dir, name)
	if info, err := os.Stat(path); err == nil && info.IsDir() {
		return &Template{Name: name, Path: path, Scope: scope}
	}

	return nil
}

// FriendlyTemplateName converts a raw template reference (cache path, URI, or
// simple name) to a human-friendly short name suitable for display.
// Simple names pass through unchanged; absolute paths return filepath.Base;
// remote URIs are handled by DeriveTemplateName.
func FriendlyTemplateName(ref string) string {
	if ref == "" {
		return ref
	}
	if IsRemoteURI(ref) {
		return DeriveTemplateName(ref)
	}
	if filepath.IsAbs(ref) {
		return filepath.Base(ref)
	}
	return ref
}

// DeriveTemplateName extracts a short template name from a URI for display purposes.
func DeriveTemplateName(uri string) string {
	// For GitHub URLs, extract the folder name
	if parts, err := parseGitHubURL(uri); err == nil {
		if parts.Path != "" {
			// Return the last path component
			pathParts := filepath.SplitList(parts.Path)
			if len(pathParts) > 0 {
				return filepath.Base(parts.Path)
			}
		}
		return parts.Repo
	}

	// For archive URLs, extract filename without extension
	if isArchiveURL(uri) {
		base := filepath.Base(uri)
		// Remove common extensions
		for _, ext := range []string{".tar.gz", ".tgz", ".zip"} {
			if len(base) > len(ext) && base[len(base)-len(ext):] == ext {
				return base[:len(base)-len(ext)]
			}
		}
		return base
	}

	// For rclone paths, use the last path component
	if idx := len(uri) - 1; idx > 0 {
		// Find the last slash
		for i := len(uri) - 1; i >= 0; i-- {
			if uri[i] == '/' {
				if i < len(uri)-1 {
					return uri[i+1:]
				}
				break
			}
		}
	}

	// Fallback: use "remote"
	return "remote"
}

// GetTemplateChain returns a list of templates in inheritance order (base first).
// For non-default templates, the default template is automatically prepended
// to the chain so that common home files and config are inherited.
func GetTemplateChain(name string) ([]*Template, error) {
	var chain []*Template

	// For non-default templates, prepend the default template as a base layer
	if name != "default" {
		defaultTpl, err := FindTemplate("default")
		if err == nil {
			chain = append(chain, defaultTpl)
		}
		// If default template is not found, proceed without it
	}

	tpl, err := FindTemplate(name)
	if err != nil {
		return nil, err
	}
	chain = append(chain, tpl)

	return chain, nil
}

// FindTemplateInGrovePath finds a template by name, using a specific grove path
// for grove-scoped template resolution instead of relying on CWD.
// When grovePath is empty, it falls back to FindTemplate (CWD-based resolution).
func FindTemplateInGrovePath(name, grovePath string) (*Template, error) {
	if grovePath == "" {
		return FindTemplate(name)
	}

	// Remote URIs and absolute paths bypass grove resolution
	if IsRemoteURI(name) {
		return FindTemplateWithContext(context.Background(), name)
	}
	if filepath.IsAbs(name) {
		if info, err := os.Stat(name); err == nil && info.IsDir() {
			return &Template{Name: filepath.Base(name), Path: name}, nil
		}
		return nil, fmt.Errorf("template path %s not found or not a directory", name)
	}

	// Check grove-specific templates directory (in-repo .scion/templates/ for git groves)
	groveTemplatesDir := filepath.Join(grovePath, "templates")
	path := filepath.Join(groveTemplatesDir, name)
	if info, err := os.Stat(path); err == nil && info.IsDir() {
		return &Template{Name: name, Path: path, Scope: "grove"}, nil
	}

	// Fall back to global templates
	globalTemplatesDir, err := GetGlobalTemplatesDir()
	if err == nil {
		path := filepath.Join(globalTemplatesDir, name)
		if info, err := os.Stat(path); err == nil && info.IsDir() {
			return &Template{Name: name, Path: path, Scope: "global"}, nil
		}
	}

	return nil, fmt.Errorf("template %s not found", name)
}

// GetTemplateChainInGrove returns a list of templates in inheritance order,
// using a specific grove path for template resolution instead of CWD.
// For non-default templates, the default template is automatically prepended
// to the chain so that common home files and config are inherited.
func GetTemplateChainInGrove(name, grovePath string) ([]*Template, error) {
	var chain []*Template

	// For non-default templates, prepend the default template as a base layer
	if name != "default" {
		defaultTpl, err := FindTemplateInGrovePath("default", grovePath)
		if err == nil {
			chain = append(chain, defaultTpl)
		}
		// If default template is not found, proceed without it
	}

	tpl, err := FindTemplateInGrovePath(name, grovePath)
	if err != nil {
		return nil, err
	}
	chain = append(chain, tpl)

	return chain, nil
}

func CreateTemplate(name string, global bool) error {
	var templatesDir string
	var err error

	if global {
		templatesDir, err = GetGlobalTemplatesDir()
	} else {
		templatesDir, err = GetProjectTemplatesDir()
	}

	if err != nil {
		return err
	}

	templateDir := filepath.Join(templatesDir, name)
	if _, err := os.Stat(templateDir); err == nil {
		return fmt.Errorf("template %s already exists at %s", name, templateDir)
	}

	return SeedAgnosticTemplate(templateDir, false)
}

func CloneTemplate(srcName, destName string, global bool) error {
	srcTpl, err := FindTemplate(srcName)
	if err != nil {
		return err
	}

	var destTemplatesDir string
	if global {
		destTemplatesDir, err = GetGlobalTemplatesDir()
	} else {
		destTemplatesDir, err = GetProjectTemplatesDir()
	}
	if err != nil {
		return err
	}

	destPath := filepath.Join(destTemplatesDir, destName)
	if _, err := os.Stat(destPath); err == nil {
		return fmt.Errorf("template %s already exists at %s", destName, destPath)
	}

	if err := util.CopyDir(srcTpl.Path, destPath); err != nil {
		return err
	}

	return nil
}

func UpdateDefaultTemplates(force bool, harnesses []api.Harness) error {
	globalDir, err := GetGlobalDir()
	if err != nil {
		return err
	}

	templatesDir, err := GetGlobalTemplatesDir()
	if err != nil {
		return err
	}
	harnessConfigsDir := filepath.Join(globalDir, harnessConfigsDirName)

	defaultDir := filepath.Join(templatesDir, "default")

	// Check if the default template already exists
	if !force {
		if _, err := os.Stat(defaultDir); err == nil {
			return fmt.Errorf("default template already exists at %s; use --force to overwrite", defaultDir)
		}
	}

	// Update default agnostic template
	if err := SeedAgnosticTemplate(defaultDir, true); err != nil {
		return err
	}

	// Update harness-configs
	for _, h := range harnesses {
		if err := SeedHarnessConfig(filepath.Join(harnessConfigsDir, h.Name()), h, true); err != nil {
			return err
		}
	}
	return nil
}

func DeleteTemplate(name string, global bool) error {
	if name == "default" {
		return fmt.Errorf("cannot delete protected template: %s", name)
	}

	var templatesDir string
	var err error

	if global {
		templatesDir, err = GetGlobalTemplatesDir()
	} else {
		templatesDir, err = GetProjectTemplatesDir()
	}

	if err != nil {
		return err
	}

	templateDir := filepath.Join(templatesDir, name)
	if info, err := os.Stat(templateDir); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("template %s not found", name)
		}
		return err
	} else if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", templateDir)
	}

	_ = util.MakeWritableRecursive(templateDir)
	return os.RemoveAll(templateDir)
}

// ListTemplatesGrouped returns templates grouped by scope (global and grove).
// Unlike ListTemplates, this preserves the scope information and does not merge duplicates.
func ListTemplatesGrouped() (global []*Template, grove []*Template, err error) {
	// Helper to scan a directory for templates
	scan := func(dir string, scope string) []*Template {
		var templates []*Template
		entries, err := os.ReadDir(dir)
		if err != nil {
			return nil
		}
		for _, e := range entries {
			if e.IsDir() {
				templates = append(templates, &Template{
					Name:  e.Name(),
					Path:  filepath.Join(dir, e.Name()),
					Scope: scope,
				})
			}
		}
		return templates
	}

	// Scan global templates
	if globalDir, err := GetGlobalTemplatesDir(); err == nil {
		global = scan(globalDir, "global")
	}

	// Scan grove (project) templates
	if projectDir, err := GetProjectTemplatesDir(); err == nil {
		grove = scan(projectDir, "grove")
	}

	// Sort both lists by name for consistent output
	sortTemplates := func(templates []*Template) {
		sort.Slice(templates, func(i, j int) bool {
			return templates[i].Name < templates[j].Name
		})
	}
	sortTemplates(global)
	sortTemplates(grove)

	return global, grove, nil
}

func ListTemplates() ([]*Template, error) {
	templates := make(map[string]*Template)

	// Helper to scan a directory for templates
	scan := func(dir string, scope string) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, e := range entries {
			if e.IsDir() {
				templates[e.Name()] = &Template{
					Name:  e.Name(),
					Path:  filepath.Join(dir, e.Name()),
					Scope: scope,
				}
			}
		}
	}

	// 1. Scan global templates (lower precedence in map)
	if globalDir, err := GetGlobalTemplatesDir(); err == nil {
		scan(globalDir, "global")
	}

	// 2. Scan project templates (higher precedence)
	if projectDir, err := GetProjectTemplatesDir(); err == nil {
		scan(projectDir, "grove")
	}

	var list []*Template
	for _, t := range templates {
		list = append(list, t)
	}
	return list, nil
}

// ValidateAgnosticTemplate validates that a ScionConfig is a valid agnostic template.
// It rejects templates that still use the legacy 'harness' field and validates
// that agnostic template fields are properly configured.
func ValidateAgnosticTemplate(cfg *api.ScionConfig) error {
	if cfg.Harness != "" {
		return fmt.Errorf("invalid template: 'harness' field is no longer supported in scion-agent.yaml. Remove it and use --harness-config to specify the harness")
	}
	return nil
}

func MergeScionConfig(base, override *api.ScionConfig) *api.ScionConfig {
	if base == nil {
		base = &api.ScionConfig{}
	}
	if override == nil {
		return base
	}

	result := *base // Shallow copy initially

	if override.Harness != "" {
		result.Harness = override.Harness
	}
	if override.HarnessConfig != "" {
		result.HarnessConfig = override.HarnessConfig
	}
	if override.ConfigDir != "" {
		result.ConfigDir = override.ConfigDir
	}
	if override.Env != nil {
		newEnv := make(map[string]string, len(base.Env)+len(override.Env))
		for k, v := range base.Env {
			newEnv[k] = v
		}
		for k, v := range override.Env {
			newEnv[k] = v
		}
		result.Env = newEnv
	}
	if override.Volumes != nil {
		newVolumes := make([]api.VolumeMount, 0, len(base.Volumes)+len(override.Volumes))
		newVolumes = append(newVolumes, base.Volumes...)
		newVolumes = append(newVolumes, override.Volumes...)
		result.Volumes = newVolumes
	}
	if override.Detached != nil {
		result.Detached = override.Detached
	}
	if len(override.CommandArgs) > 0 {
		result.CommandArgs = override.CommandArgs
	}
	if override.TaskFlag != "" {
		result.TaskFlag = override.TaskFlag
	}
	if override.Model != "" {
		result.Model = override.Model
	}
	if override.Kubernetes != nil {
		if result.Kubernetes == nil {
			result.Kubernetes = override.Kubernetes
		} else {
			if override.Kubernetes.Context != "" {
				result.Kubernetes.Context = override.Kubernetes.Context
			}
			if override.Kubernetes.Namespace != "" {
				result.Kubernetes.Namespace = override.Kubernetes.Namespace
			}
			if override.Kubernetes.RuntimeClassName != "" {
				result.Kubernetes.RuntimeClassName = override.Kubernetes.RuntimeClassName
			}
			if override.Kubernetes.Resources != nil {
				result.Kubernetes.Resources = override.Kubernetes.Resources
			}
		}
	}
	if override.Resources != nil {
		result.Resources = MergeResourceSpec(result.Resources, override.Resources)
	}
	if override.AuthSelectedType != "" {
		result.AuthSelectedType = override.AuthSelectedType
	}
	if override.Image != "" {
		result.Image = override.Image
	}
	if override.Services != nil {
		result.Services = override.Services
	}
	if override.MaxTurns > 0 {
		result.MaxTurns = override.MaxTurns
	}
	if override.MaxDuration != "" {
		result.MaxDuration = override.MaxDuration
	}
	if override.Hub != nil {
		if result.Hub == nil {
			result.Hub = &api.AgentHubConfig{}
		}
		if override.Hub.Endpoint != "" {
			result.Hub.Endpoint = override.Hub.Endpoint
		}
	}
	if override.Telemetry != nil {
		result.Telemetry = mergeTelemetryConfig(result.Telemetry, override.Telemetry)
	}
	if override.AgentInstructions != "" {
		result.AgentInstructions = override.AgentInstructions
	}
	if override.SystemPrompt != "" {
		result.SystemPrompt = override.SystemPrompt
	}
	if override.DefaultHarnessConfig != "" {
		result.DefaultHarnessConfig = override.DefaultHarnessConfig
	}
	if override.User != "" {
		result.User = override.User
	}
	if override.Task != "" {
		result.Task = override.Task
	}
	if override.Branch != "" {
		result.Branch = override.Branch
	}
	if override.MaxModelCalls > 0 {
		result.MaxModelCalls = override.MaxModelCalls
	}
	if override.Info != nil {
		if result.Info == nil {
			infoCopy := *override.Info
			result.Info = &infoCopy
		} else {
			infoCopy := *result.Info
			if override.Info.ID != "" {
				infoCopy.ID = override.Info.ID
			}
			if override.Info.Name != "" {
				infoCopy.Name = override.Info.Name
			}
			if override.Info.Template != "" {
				infoCopy.Template = override.Info.Template
			}
			if override.Info.Grove != "" {
				infoCopy.Grove = override.Info.Grove
			}
			if override.Info.GrovePath != "" {
				infoCopy.GrovePath = override.Info.GrovePath
			}
			if override.Info.ContainerStatus != "" {
				infoCopy.ContainerStatus = override.Info.ContainerStatus
			}
			if override.Info.Phase != "" {
				infoCopy.Phase = override.Info.Phase
			}
			if override.Info.Activity != "" {
				infoCopy.Activity = override.Info.Activity
			}
			if override.Info.Image != "" {
				infoCopy.Image = override.Info.Image
			}
			if override.Info.Runtime != "" {
				infoCopy.Runtime = override.Info.Runtime
			}
			if override.Info.Kubernetes != nil {
				infoCopy.Kubernetes = override.Info.Kubernetes
			}
			result.Info = &infoCopy
		}
	}

	return &result
}

// mergeTelemetryConfig merges override telemetry settings on top of base.
// Non-nil override fields replace base values (last write wins).
func mergeTelemetryConfig(base, override *api.TelemetryConfig) *api.TelemetryConfig {
	if base == nil {
		cp := *override
		return &cp
	}
	result := *base

	if override.Enabled != nil {
		result.Enabled = override.Enabled
	}
	if override.Cloud != nil {
		if result.Cloud == nil {
			result.Cloud = &api.TelemetryCloudConfig{}
		}
		if override.Cloud.Enabled != nil {
			result.Cloud.Enabled = override.Cloud.Enabled
		}
		if override.Cloud.Endpoint != "" {
			result.Cloud.Endpoint = override.Cloud.Endpoint
		}
		if override.Cloud.Protocol != "" {
			result.Cloud.Protocol = override.Cloud.Protocol
		}
		if override.Cloud.Headers != nil {
			result.Cloud.Headers = mergeMaps(result.Cloud.Headers, override.Cloud.Headers)
		}
		if override.Cloud.TLS != nil {
			if result.Cloud.TLS == nil {
				result.Cloud.TLS = &api.TelemetryTLS{}
			}
			if override.Cloud.TLS.Enabled != nil {
				result.Cloud.TLS.Enabled = override.Cloud.TLS.Enabled
			}
			if override.Cloud.TLS.InsecureSkipVerify != nil {
				result.Cloud.TLS.InsecureSkipVerify = override.Cloud.TLS.InsecureSkipVerify
			}
			if override.Cloud.TLS.CAFile != "" {
				result.Cloud.TLS.CAFile = override.Cloud.TLS.CAFile
			}
			if override.Cloud.TLS.CertFile != "" {
				result.Cloud.TLS.CertFile = override.Cloud.TLS.CertFile
			}
			if override.Cloud.TLS.KeyFile != "" {
				result.Cloud.TLS.KeyFile = override.Cloud.TLS.KeyFile
			}
		}
		if override.Cloud.Batch != nil {
			if result.Cloud.Batch == nil {
				result.Cloud.Batch = &api.TelemetryBatch{}
			}
			if override.Cloud.Batch.MaxSize > 0 {
				result.Cloud.Batch.MaxSize = override.Cloud.Batch.MaxSize
			}
			if override.Cloud.Batch.Timeout != "" {
				result.Cloud.Batch.Timeout = override.Cloud.Batch.Timeout
			}
		}
		if override.Cloud.Provider != "" {
			result.Cloud.Provider = override.Cloud.Provider
		}
	}
	if override.Hub != nil {
		if result.Hub == nil {
			result.Hub = &api.TelemetryHubConfig{}
		}
		if override.Hub.Enabled != nil {
			result.Hub.Enabled = override.Hub.Enabled
		}
		if override.Hub.ReportInterval != "" {
			result.Hub.ReportInterval = override.Hub.ReportInterval
		}
	}
	if override.Local != nil {
		if result.Local == nil {
			result.Local = &api.TelemetryLocalConfig{}
		}
		if override.Local.Enabled != nil {
			result.Local.Enabled = override.Local.Enabled
		}
		if override.Local.File != "" {
			result.Local.File = override.Local.File
		}
		if override.Local.Console != nil {
			result.Local.Console = override.Local.Console
		}
	}
	if override.Filter != nil {
		if result.Filter == nil {
			result.Filter = &api.TelemetryFilterConfig{}
		}
		if override.Filter.Enabled != nil {
			result.Filter.Enabled = override.Filter.Enabled
		}
		if override.Filter.RespectDebugMode != nil {
			result.Filter.RespectDebugMode = override.Filter.RespectDebugMode
		}
		if override.Filter.Events != nil {
			if result.Filter.Events == nil {
				result.Filter.Events = &api.TelemetryEventsConfig{}
			}
			if override.Filter.Events.Include != nil {
				result.Filter.Events.Include = override.Filter.Events.Include
			}
			if override.Filter.Events.Exclude != nil {
				result.Filter.Events.Exclude = override.Filter.Events.Exclude
			}
		}
		if override.Filter.Attributes != nil {
			if result.Filter.Attributes == nil {
				result.Filter.Attributes = &api.TelemetryAttributesConfig{}
			}
			if override.Filter.Attributes.Redact != nil {
				result.Filter.Attributes.Redact = override.Filter.Attributes.Redact
			}
			if override.Filter.Attributes.Hash != nil {
				result.Filter.Attributes.Hash = override.Filter.Attributes.Hash
			}
		}
		if override.Filter.Sampling != nil {
			if result.Filter.Sampling == nil {
				result.Filter.Sampling = &api.TelemetrySamplingConfig{}
			}
			if override.Filter.Sampling.Default != nil {
				result.Filter.Sampling.Default = override.Filter.Sampling.Default
			}
			if override.Filter.Sampling.Rates != nil {
				if result.Filter.Sampling.Rates == nil {
					result.Filter.Sampling.Rates = make(map[string]float64)
				}
				for k, v := range override.Filter.Sampling.Rates {
					result.Filter.Sampling.Rates[k] = v
				}
			}
		}
	}
	if override.Resource != nil {
		result.Resource = mergeMaps(result.Resource, override.Resource)
	}

	return &result
}
