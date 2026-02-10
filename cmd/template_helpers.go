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

package cmd

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/ptone/scion-agent/pkg/config"
	"github.com/ptone/scion-agent/pkg/hubclient"
	"github.com/ptone/scion-agent/pkg/util"
)

// TemplateLocation represents where a template was found.
type TemplateLocation string

const (
	LocationLocalGrove  TemplateLocation = "local-grove"
	LocationLocalGlobal TemplateLocation = "local-global"
	LocationHubGrove    TemplateLocation = "hub-grove"
	LocationHubGlobal   TemplateLocation = "hub-global"
)

// TemplateMatch represents a template found during resolution.
type TemplateMatch struct {
	Name        string
	Location    TemplateLocation
	LocalPath   string               // For local templates
	HubTemplate *hubclient.Template  // For hub templates
}

// DisplayLocation returns a human-readable location string.
func (m *TemplateMatch) DisplayLocation() string {
	switch m.Location {
	case LocationLocalGrove:
		return fmt.Sprintf("Local Grove    (%s)", m.LocalPath)
	case LocationLocalGlobal:
		return fmt.Sprintf("Local Global   (%s)", m.LocalPath)
	case LocationHubGrove:
		return fmt.Sprintf("Hub Grove      (scope=grove, ID: %s)", m.HubTemplate.ID)
	case LocationHubGlobal:
		return fmt.Sprintf("Hub Global     (scope=global, ID: %s)", m.HubTemplate.ID)
	default:
		return string(m.Location)
	}
}

// IsLocal returns true if this is a local template.
func (m *TemplateMatch) IsLocal() bool {
	return m.Location == LocationLocalGrove || m.Location == LocationLocalGlobal
}

// IsHub returns true if this is a hub template.
func (m *TemplateMatch) IsHub() bool {
	return m.Location == LocationHubGrove || m.Location == LocationHubGlobal
}

// IsGlobal returns true if this template is in global scope.
func (m *TemplateMatch) IsGlobal() bool {
	return m.Location == LocationLocalGlobal || m.Location == LocationHubGlobal
}

// IsGrove returns true if this template is in grove scope.
func (m *TemplateMatch) IsGrove() bool {
	return m.Location == LocationLocalGrove || m.Location == LocationHubGrove
}

// ResolveOpts controls how template resolution behaves.
type ResolveOpts struct {
	LocalOnly   bool // --local flag: only search local filesystem
	HubOnly     bool // --hub flag: only search Hub
	GroveOnly   bool // --grove flag: only search grove scope
	GlobalOnly  bool // --global flag: only search global scope
	AutoConfirm bool // -y flag: use first match or error if multiple
}

// FindTemplateAllLocations searches all 4 locations for a template by name.
// Returns all matches found. The caller should use PromptTemplateChoice if
// multiple matches are found.
func FindTemplateAllLocations(ctx context.Context, name string, hubCtx *HubContext, opts *ResolveOpts) ([]TemplateMatch, error) {
	if opts == nil {
		opts = &ResolveOpts{}
	}

	var matches []TemplateMatch

	// Search local templates unless HubOnly is set
	if !opts.HubOnly {
		localMatches, err := findLocalTemplates(name, opts)
		if err != nil {
			return nil, err
		}
		matches = append(matches, localMatches...)
	}

	// Search hub templates unless LocalOnly is set or no hubCtx
	if !opts.LocalOnly && hubCtx != nil {
		hubMatches, err := findHubTemplates(ctx, name, hubCtx, opts)
		if err != nil {
			// Don't fail on hub errors if we have local matches
			if len(matches) == 0 {
				return nil, err
			}
			// Log warning but continue with local matches
			fmt.Fprintf(os.Stderr, "Warning: failed to search Hub: %v\n", err)
		}
		matches = append(matches, hubMatches...)
	}

	return matches, nil
}

// findLocalTemplates searches local filesystem for templates.
func findLocalTemplates(name string, opts *ResolveOpts) ([]TemplateMatch, error) {
	var matches []TemplateMatch

	// Get templates directory paths
	globalDir, globalErr := config.GetGlobalTemplatesDir()
	groveDir, groveErr := config.GetProjectTemplatesDir()

	// Search grove (project) templates unless GlobalOnly is set
	if !opts.GlobalOnly && groveErr == nil {
		tpl := config.FindTemplateInScope(name, "grove")
		if tpl != nil {
			matches = append(matches, TemplateMatch{
				Name:      tpl.Name,
				Location:  LocationLocalGrove,
				LocalPath: tpl.Path,
			})
		}
	}

	// Search global templates unless GroveOnly is set
	if !opts.GroveOnly && globalErr == nil {
		tpl := config.FindTemplateInScope(name, "global")
		if tpl != nil {
			// Avoid duplicates if grove and global point to same template
			// (this shouldn't happen, but be safe)
			isDuplicate := false
			for _, m := range matches {
				if m.LocalPath == globalDir+"/"+name {
					isDuplicate = true
					break
				}
			}
			if !isDuplicate {
				matches = append(matches, TemplateMatch{
					Name:      tpl.Name,
					Location:  LocationLocalGlobal,
					LocalPath: tpl.Path,
				})
			}
		}
	}

	// Suppress unused variable warnings
	_ = globalDir
	_ = groveDir

	return matches, nil
}

// findHubTemplates searches Hub for templates.
func findHubTemplates(ctx context.Context, name string, hubCtx *HubContext, opts *ResolveOpts) ([]TemplateMatch, error) {
	var matches []TemplateMatch

	listCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	// Get grove ID for grove-scoped lookups
	groveID, _ := GetGroveID(hubCtx)

	// Search grove scope unless GlobalOnly is set
	if !opts.GlobalOnly && groveID != "" {
		resp, err := hubCtx.Client.Templates().List(listCtx, &hubclient.ListTemplatesOptions{
			Name:    name,
			Scope:   "grove",
			GroveID: groveID,
			Status:  "active",
		})
		if err != nil {
			return nil, fmt.Errorf("failed to search Hub grove templates: %w", err)
		}

		for i := range resp.Templates {
			t := &resp.Templates[i]
			if t.Name == name || t.Slug == name {
				matches = append(matches, TemplateMatch{
					Name:        t.Name,
					Location:    LocationHubGrove,
					HubTemplate: t,
				})
			}
		}
	}

	// Search global scope unless GroveOnly is set
	if !opts.GroveOnly {
		resp, err := hubCtx.Client.Templates().List(listCtx, &hubclient.ListTemplatesOptions{
			Name:   name,
			Scope:  "global",
			Status: "active",
		})
		if err != nil {
			return nil, fmt.Errorf("failed to search Hub global templates: %w", err)
		}

		for i := range resp.Templates {
			t := &resp.Templates[i]
			if t.Name == name || t.Slug == name {
				matches = append(matches, TemplateMatch{
					Name:        t.Name,
					Location:    LocationHubGlobal,
					HubTemplate: t,
				})
			}
		}
	}

	return matches, nil
}

// PromptTemplateChoice presents an interactive selection when multiple matches are found.
// Returns the selected match, or an error if cancelled.
func PromptTemplateChoice(matches []TemplateMatch, action string) (*TemplateMatch, error) {
	if len(matches) == 0 {
		return nil, fmt.Errorf("no templates to choose from")
	}

	if len(matches) == 1 {
		return &matches[0], nil
	}

	// Check if we're in an interactive terminal
	if !util.IsTerminal() {
		return nil, fmt.Errorf("multiple templates found but running non-interactively; use --local, --hub, --grove, or --global flags to specify")
	}

	fmt.Printf("\nTemplate '%s' found in multiple locations:\n", matches[0].Name)
	for i, m := range matches {
		fmt.Printf("  [%d] %s\n", i+1, m.DisplayLocation())
	}
	fmt.Println()

	reader := bufio.NewReader(os.Stdin)
	for {
		fmt.Printf("Select a template to %s (or 'c' to cancel): ", action)
		input, err := reader.ReadString('\n')
		if err != nil {
			return nil, fmt.Errorf("failed to read input: %w", err)
		}

		input = strings.TrimSpace(strings.ToLower(input))
		if input == "c" || input == "cancel" {
			return nil, fmt.Errorf("operation cancelled")
		}

		choice, err := strconv.Atoi(input)
		if err != nil || choice < 1 || choice > len(matches) {
			fmt.Printf("Invalid choice. Please enter 1-%d.\n", len(matches))
			continue
		}

		return &matches[choice-1], nil
	}
}

// PromptTemplateChoiceWithAll is like PromptTemplateChoice but also offers an "all" option.
// Returns the selected matches (could be one or all), or an error if cancelled.
func PromptTemplateChoiceWithAll(matches []TemplateMatch, action string) ([]TemplateMatch, error) {
	if len(matches) == 0 {
		return nil, fmt.Errorf("no templates to choose from")
	}

	if len(matches) == 1 {
		return matches, nil
	}

	// Check if we're in an interactive terminal
	if !util.IsTerminal() {
		return nil, fmt.Errorf("multiple templates found but running non-interactively; use --local, --hub, --grove, or --global flags to specify")
	}

	fmt.Printf("\nTemplate '%s' found in multiple locations:\n", matches[0].Name)
	for i, m := range matches {
		fmt.Printf("  [%d] %s\n", i+1, m.DisplayLocation())
	}
	fmt.Printf("  [a] All of the above\n")
	fmt.Println()

	reader := bufio.NewReader(os.Stdin)
	for {
		fmt.Printf("Select template(s) to %s (or 'c' to cancel): ", action)
		input, err := reader.ReadString('\n')
		if err != nil {
			return nil, fmt.Errorf("failed to read input: %w", err)
		}

		input = strings.TrimSpace(strings.ToLower(input))
		if input == "c" || input == "cancel" {
			return nil, fmt.Errorf("operation cancelled")
		}

		if input == "a" || input == "all" {
			return matches, nil
		}

		choice, err := strconv.Atoi(input)
		if err != nil || choice < 1 || choice > len(matches) {
			fmt.Printf("Invalid choice. Please enter 1-%d or 'a' for all.\n", len(matches))
			continue
		}

		return []TemplateMatch{matches[choice-1]}, nil
	}
}

// ResolveTemplate combines search + prompt, returning a single match.
// If multiple matches are found, prompts the user to choose (unless AutoConfirm is set).
func ResolveTemplate(ctx context.Context, name string, hubCtx *HubContext, opts *ResolveOpts, action string) (*TemplateMatch, error) {
	if opts == nil {
		opts = &ResolveOpts{}
	}

	matches, err := FindTemplateAllLocations(ctx, name, hubCtx, opts)
	if err != nil {
		return nil, err
	}

	if len(matches) == 0 {
		return nil, fmt.Errorf("template '%s' not found", name)
	}

	if len(matches) == 1 {
		return &matches[0], nil
	}

	// Multiple matches found
	if opts.AutoConfirm {
		// In auto-confirm mode with multiple matches, we need a deterministic choice.
		// Use priority: local-grove > local-global > hub-grove > hub-global
		// This matches the existing FindTemplate behavior.
		for _, loc := range []TemplateLocation{LocationLocalGrove, LocationLocalGlobal, LocationHubGrove, LocationHubGlobal} {
			for i := range matches {
				if matches[i].Location == loc {
					return &matches[i], nil
				}
			}
		}
		// Fallback to first match
		return &matches[0], nil
	}

	return PromptTemplateChoice(matches, action)
}

// ResolveTemplateForDelete combines search + prompt for delete operations.
// Returns all matches the user wants to delete.
func ResolveTemplateForDelete(ctx context.Context, name string, hubCtx *HubContext, opts *ResolveOpts) ([]TemplateMatch, error) {
	if opts == nil {
		opts = &ResolveOpts{}
	}

	matches, err := FindTemplateAllLocations(ctx, name, hubCtx, opts)
	if err != nil {
		return nil, err
	}

	if len(matches) == 0 {
		return nil, fmt.Errorf("template '%s' not found", name)
	}

	if len(matches) == 1 {
		return matches, nil
	}

	// Multiple matches found
	if opts.AutoConfirm {
		// In auto-confirm mode, delete all matches
		return matches, nil
	}

	return PromptTemplateChoiceWithAll(matches, "delete")
}

// FilterMatchesBySource filters matches to only local or only hub sources.
func FilterMatchesBySource(matches []TemplateMatch, localOnly, hubOnly bool) []TemplateMatch {
	if !localOnly && !hubOnly {
		return matches
	}

	var filtered []TemplateMatch
	for _, m := range matches {
		if localOnly && m.IsLocal() {
			filtered = append(filtered, m)
		} else if hubOnly && m.IsHub() {
			filtered = append(filtered, m)
		}
	}
	return filtered
}

// FilterMatchesByScope filters matches to only grove or only global scope.
func FilterMatchesByScope(matches []TemplateMatch, groveOnly, globalOnly bool) []TemplateMatch {
	if !groveOnly && !globalOnly {
		return matches
	}

	var filtered []TemplateMatch
	for _, m := range matches {
		if groveOnly && m.IsGrove() {
			filtered = append(filtered, m)
		} else if globalOnly && m.IsGlobal() {
			filtered = append(filtered, m)
		}
	}
	return filtered
}
