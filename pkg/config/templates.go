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

	"github.com/ptone/scion-agent/pkg/api"
	"github.com/ptone/scion-agent/pkg/util"
	"gopkg.in/yaml.v3"
)

type Template struct {
	Name  string
	Path  string
	Scope string // "global" or "grove"
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
		if err := yaml.Unmarshal(data, &cfg); err != nil {
			return nil, fmt.Errorf("failed to parse YAML config %s: %w", configPath, err)
		}
	} else {
		if err := json.Unmarshal(data, &cfg); err != nil {
			return nil, fmt.Errorf("failed to parse JSON config %s: %w", configPath, err)
		}
	}
	return &cfg, nil
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
		shortName := deriveTemplateName(name)

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

// deriveTemplateName extracts a short template name from a URI for display purposes.
func deriveTemplateName(uri string) string {
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

// GetTemplateChain returns a list of templates in inheritance order (base first)
func GetTemplateChain(name string) ([]*Template, error) {
	var chain []*Template

	tpl, err := FindTemplate(name)
	if err != nil {
		return nil, err
	}
	chain = append(chain, tpl)

	return chain, nil
}

func CreateTemplate(name string, h api.Harness, global bool) error {
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

	return h.SeedTemplateDir(templateDir, false)
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

func UpdateDefaultTemplates(global bool, harnesses []api.Harness) error {
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

	for _, h := range harnesses {
		if err := h.SeedTemplateDir(filepath.Join(templatesDir, h.Name()), true); err != nil {
			return err
		}
	}
	return nil
}

func DeleteTemplate(name string, global bool) error {
	if name == "default" || name == "gemini" || name == "claude" || name == "opencode" || name == "codex" {
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
	if override.Gemini != nil {
		if result.Gemini == nil {
			result.Gemini = &api.GeminiConfig{}
		}
		if override.Gemini.AuthSelectedType != "" {
			result.Gemini.AuthSelectedType = override.Gemini.AuthSelectedType
		}
	}
	if override.Image != "" {
		result.Image = override.Image
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
			if override.Info.Status != "" {
				infoCopy.Status = override.Info.Status
			}
			if override.Info.SessionStatus != "" {
				infoCopy.SessionStatus = override.Info.SessionStatus
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