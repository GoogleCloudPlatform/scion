package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/ptone/scion-agent/pkg/api"
	"github.com/ptone/scion-agent/pkg/util"
)

type Template struct {
	Name string
	Path string
}

func (t *Template) LoadConfig() (*api.ScionConfig, error) {
	path := filepath.Join(t.Path, "scion-agent.json")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &api.ScionConfig{}, nil
		}
		return nil, err
	}

	var cfg api.ScionConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, err
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
	// 0. Check if name is an absolute path
	if filepath.IsAbs(name) {
		if info, err := os.Stat(name); err == nil && info.IsDir() {
			return &Template{Name: filepath.Base(name), Path: name}, nil
		}
		return nil, fmt.Errorf("template path %s not found or not a directory", name)
	}

	// 1. Check project-local templates
	projectTemplatesDir, err := GetProjectTemplatesDir()
	if err == nil {
		path := filepath.Join(projectTemplatesDir, name)
		if info, err := os.Stat(path); err == nil && info.IsDir() {
			return &Template{Name: name, Path: path}, nil
		}
	}

	// 2. Check global templates
	globalTemplatesDir, err := GetGlobalTemplatesDir()
	if err == nil {
		path := filepath.Join(globalTemplatesDir, name)
		if info, err := os.Stat(path); err == nil && info.IsDir() {
			return &Template{Name: name, Path: path}, nil
		}
	}

	return nil, fmt.Errorf("template %s not found", name)
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

func CreateTemplate(name, harness, embedDir, configDirName string, global bool) error {
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

	return SeedTemplateDir(templateDir, name, harness, embedDir, configDirName, false)
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

func UpdateDefaultTemplates(global bool) error {
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

	if err := SeedTemplateDir(filepath.Join(templatesDir, "gemini"), "gemini", "gemini", "gemini", ".gemini", true); err != nil {
		return err
	}
	return SeedTemplateDir(filepath.Join(templatesDir, "claude"), "claude", "claude", "claude", ".claude", true)
}

func DeleteTemplate(name string, global bool) error {
	if name == "default" || name == "gemini" || name == "claude" {
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

func ListTemplates() ([]*Template, error) {
	templates := make(map[string]*Template)

	// Helper to scan a directory for templates
	scan := func(dir string) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, e := range entries {
			if e.IsDir() {
				templates[e.Name()] = &Template{
					Name: e.Name(),
					Path: filepath.Join(dir, e.Name()),
				}
			}
		}
	}

	// 1. Scan global templates (lower precedence in map)
	if globalDir, err := GetGlobalTemplatesDir(); err == nil {
		scan(globalDir)
	}

	// 2. Scan project templates (higher precedence)
	if projectDir, err := GetProjectTemplatesDir(); err == nil {
		scan(projectDir)
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