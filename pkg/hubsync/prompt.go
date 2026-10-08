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

// Package hubsync provides Hub synchronization checks for agent operations.
package hubsync

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/util"
)

// Prompt I/O. Every prompt, its context lines and the auto-confirm notes go
// to stderr, never stdout, so machine-readable stdout (--format json) stays
// clean when a prompt is auto-confirmed. Answers are read from stdin only
// when stdin is a terminal. These are variables so tests can substitute them.
var (
	promptIn        io.Reader = os.Stdin
	promptOut       io.Writer = os.Stderr
	stdinIsTerminal           = util.IsTerminal
)

// ErrAmbiguousProject is returned by ShowMatchingProjectsPrompt under
// --non-interactive when several Hub projects match and there is no
// deterministic answer to pick.
var ErrAmbiguousProject = errors.New("ambiguous project")

// ErrNoTerminal is returned by multiple-choice prompts that need an answer
// but cannot ask for one because stdin is not a terminal.
var ErrNoTerminal = errors.New("stdin is not a terminal")

// noTerminalNote is appended to a yes/no prompt that is answered No because
// stdin is not a terminal.
const noTerminalNote = "answered No (stdin is not a terminal; re-run with --yes to confirm)"

// ConfirmAction prompts user for Y/n confirmation.
// Returns true if confirmed, false otherwise.
// If autoConfirm is true, always confirms Yes (proceeds with the action)
// without reading stdin.
// When stdin is not a terminal it never reads stdin: it answers No, the safe
// answer for every caller, and notes on stderr that --yes confirms. This
// keeps a caller with an idle open stdin (a coding agent, a CI step) from
// hanging, and keeps a closed stdin from silently accepting a Yes default.
// The defaultYes parameter only affects the interactive case: it controls
// whether pressing Enter without input confirms (Y/n) or declines (y/N).
// An end of input at the prompt is No.
func ConfirmAction(prompt string, defaultYes bool, autoConfirm bool) bool {
	if autoConfirm {
		fmt.Fprintf(promptOut, "%s: auto-confirmed Yes\n", prompt)
		return true
	}

	if !stdinIsTerminal() {
		fmt.Fprintf(promptOut, "%s: %s\n", prompt, noTerminalNote)
		return false
	}

	suffix := " (Y/n): "
	if !defaultYes {
		suffix = " (y/N): "
	}

	fmt.Fprint(promptOut, prompt+suffix)

	reader := bufio.NewReader(promptIn)
	input, err := reader.ReadString('\n')
	if err != nil && (input == "" || !errors.Is(err, io.EOF)) {
		// End of input or a read error is not an answer: decline.
		fmt.Fprintln(promptOut)
		return false
	}

	input = strings.TrimSpace(strings.ToLower(input))

	// Empty input returns the default
	if input == "" {
		return defaultYes
	}

	return input == "y" || input == "yes"
}

// ShowSyncPlan displays what will be synced and asks for confirmation.
// Returns true if the user confirms, false otherwise.
func ShowSyncPlan(result *SyncResult, autoConfirm bool) bool {
	if result.IsInSync() {
		return true // Nothing to sync
	}

	fmt.Fprintln(promptOut)
	fmt.Fprintln(promptOut, "Hub Agent Sync Required")
	fmt.Fprintln(promptOut, "=======================")

	if len(result.ToRegister) > 0 {
		fmt.Fprintln(promptOut, "Agents to register on Hub:")
		for _, name := range result.ToRegister {
			fmt.Fprintf(promptOut, "  + %s\n", name)
		}
	}

	if len(result.ToRemove) > 0 {
		fmt.Fprintln(promptOut, "Agents to remove from Hub (not on this broker):")
		for _, ref := range result.ToRemove {
			fmt.Fprintf(promptOut, "  - %s\n", ref.Name)
		}
	}

	// Show pending agents for visibility (they don't require action)
	if len(result.Pending) > 0 {
		fmt.Fprintln(promptOut)
		fmt.Fprintln(promptOut, "Agents pending on Hub (awaiting start):")
		for _, ref := range result.Pending {
			fmt.Fprintf(promptOut, "  ~ %s\n", ref.Name)
		}
	}

	// Show remote-only agents for visibility (they don't require action)
	if len(result.RemoteOnly) > 0 {
		fmt.Fprintln(promptOut)
		fmt.Fprintln(promptOut, "Agents on Hub from other brokers (no action needed):")
		for _, ref := range result.RemoteOnly {
			fmt.Fprintf(promptOut, "  ~ %s\n", ref.Name)
		}
	}

	// Show stale-local agents for visibility (they don't require action)
	if len(result.StaleLocal) > 0 {
		fmt.Fprintln(promptOut)
		fmt.Fprintln(promptOut, "Stale local agent artifacts (no action needed):")
		for _, name := range result.StaleLocal {
			fmt.Fprintf(promptOut, "  ~ %s\n", name)
		}
	}

	fmt.Fprintln(promptOut)
	return ConfirmAction("Proceed with sync?", true, autoConfirm)
}

// ShowLinkPrompt displays the project link prompt.
// Returns true if the user confirms, false otherwise.
func ShowLinkPrompt(projectName string, autoConfirm bool) bool {
	fmt.Fprintln(promptOut)
	fmt.Fprintf(promptOut, "Project '%s' is not linked to the Hub.\n", projectName)
	return ConfirmAction("Link project with Hub?", true, autoConfirm)
}

// ShowInitLinkPrompt displays the post-init link prompt.
// Returns true if the user confirms, false otherwise.
func ShowInitLinkPrompt(autoConfirm bool) bool {
	return ConfirmAction("Project initialized. Link to Hub?", true, autoConfirm)
}

// ShowInitProvidePrompt displays a confirmation to add this broker as a provider.
// Returns true if the user confirms, false otherwise.
func ShowInitProvidePrompt(brokerName, projectName string, autoConfirm bool) bool {
	fmt.Fprintf(promptOut, "This host (%s) is registered as a broker.\n", brokerName)
	return ConfirmAction(fmt.Sprintf("Add as provider for '%s'?", projectName), true, autoConfirm)
}

// ProjectChoice represents the user's choice when matching projects exist.
type ProjectChoice int

const (
	// ProjectChoiceCancel means the user cancelled the operation.
	ProjectChoiceCancel ProjectChoice = iota
	// ProjectChoiceLink means the user chose to link to an existing project.
	ProjectChoiceLink
	// ProjectChoiceRegisterNew means the user chose to register a new project.
	ProjectChoiceRegisterNew
)

// ProjectMatch holds information about a matching project for display.
type ProjectMatch struct {
	ID        string
	Name      string
	Slug      string
	GitRemote string
}

// ShowMatchingProjectsPrompt displays matching projects and asks the user to choose.
// The "Register as new project" option is always shown, allowing multiple projects
// per git remote. When nextSlug is non-empty, it is displayed as the proposed
// slug for a new project.
// Returns the choice and the selected project ID if linking.
//
// Without a terminal it does not read stdin:
//   - nonInteractive (--non-interactive) with more than one match returns
//     ErrAmbiguousProject: there is no deterministic answer to pick.
//   - autoConfirm (--yes) links to the first match.
//   - otherwise it returns ErrNoTerminal, naming --yes.
//
// nonInteractive implies autoConfirm in the CLI, so a single match still links
// under --non-interactive.
func ShowMatchingProjectsPrompt(projectName string, matches []ProjectMatch, nextSlug string, autoConfirm, nonInteractive bool) (ProjectChoice, string, error) {
	if nonInteractive && len(matches) > 1 {
		ids := make([]string, 0, len(matches))
		for _, m := range matches {
			ids = append(ids, fmt.Sprintf("%s (ID: %s)", m.Name, m.ID))
		}
		return ProjectChoiceCancel, "", fmt.Errorf("%w: found %d projects named '%s' on the Hub (%s); --non-interactive does not choose between them\n\n"+
			"Pick one interactively with 'scion hub link', or target one hub project directly with --project <project ID>",
			ErrAmbiguousProject, len(matches), projectName, strings.Join(ids, ", "))
	}

	fmt.Fprintln(promptOut)
	fmt.Fprintf(promptOut, "Found %d existing project(s) with the name '%s' on the Hub:\n", len(matches), projectName)
	fmt.Fprintln(promptOut)

	for i, m := range matches {
		if m.GitRemote != "" {
			fmt.Fprintf(promptOut, "  [%d] %s (ID: %s, remote: %s)\n", i+1, m.Name, m.ID, m.GitRemote)
		} else {
			fmt.Fprintf(promptOut, "  [%d] %s (ID: %s)\n", i+1, m.Name, m.ID)
		}
	}
	if nextSlug != "" {
		fmt.Fprintf(promptOut, "  [%d] Register as a new project (will be created as '%s')\n", len(matches)+1, nextSlug)
	} else {
		fmt.Fprintf(promptOut, "  [%d] Register as a new project\n", len(matches)+1)
	}
	fmt.Fprintln(promptOut)

	if autoConfirm {
		// Auto-confirm defaults to linking to the first match
		fmt.Fprintf(promptOut, "Auto-linking to: %s (ID: %s)\n", matches[0].Name, matches[0].ID)
		return ProjectChoiceLink, matches[0].ID, nil
	}

	if !stdinIsTerminal() {
		return ProjectChoiceCancel, "", fmt.Errorf("%w: cannot choose between the projects above; re-run with --yes to link to the first match, or run 'scion hub link' in a terminal", ErrNoTerminal)
	}

	maxChoice := len(matches) + 1

	reader := bufio.NewReader(promptIn)
	for {
		fmt.Fprint(promptOut, "Enter choice (or 'c' to cancel): ")
		input, err := reader.ReadString('\n')
		if err != nil {
			return ProjectChoiceCancel, "", nil
		}

		input = strings.TrimSpace(strings.ToLower(input))
		if input == "c" || input == "cancel" {
			return ProjectChoiceCancel, "", nil
		}

		choice := 0
		if _, err := fmt.Sscanf(input, "%d", &choice); err != nil {
			fmt.Fprintln(promptOut, "Invalid choice. Please enter a number.")
			continue
		}

		if choice < 1 || choice > maxChoice {
			fmt.Fprintf(promptOut, "Invalid choice. Please enter 1-%d.\n", maxChoice)
			continue
		}

		if choice == len(matches)+1 {
			return ProjectChoiceRegisterNew, "", nil
		}

		return ProjectChoiceLink, matches[choice-1].ID, nil
	}
}

// NextSlugFromMatches computes a proposed next serial slug from a list of
// existing project matches. This is a client-side estimate for display purposes;
// the server computes the authoritative slug at creation time.
func NextSlugFromMatches(baseSlug string, matches []ProjectMatch) string {
	maxSerial := 0
	for _, m := range matches {
		if m.Slug == baseSlug {
			if maxSerial < 1 {
				maxSerial = 1
			}
			continue
		}
		// Check for serial-numbered slugs like "base-slug-2"
		prefix := baseSlug + "-"
		if strings.HasPrefix(m.Slug, prefix) {
			suffix := m.Slug[len(prefix):]
			n := 0
			if _, err := fmt.Sscanf(suffix, "%d", &n); err == nil && n >= maxSerial {
				maxSerial = n + 1
			}
		}
	}
	if maxSerial == 0 {
		return ""
	}
	return fmt.Sprintf("%s-%d", baseSlug, maxSerial)
}

// ShowBrokerRegistrationPrompt displays the broker registration confirmation.
// Returns true if the user confirms, false otherwise.
func ShowBrokerRegistrationPrompt(endpoint string, autoConfirm bool) bool {
	fmt.Fprintln(promptOut)
	fmt.Fprintln(promptOut, "This will register this host as a Runtime Broker with the Hub.")
	fmt.Fprintf(promptOut, "Hub endpoint: %s\n", endpoint)
	fmt.Fprintln(promptOut)
	fmt.Fprintln(promptOut, "The broker will be able to:")
	fmt.Fprintln(promptOut, "  - Execute agents on behalf of authorized Hub users")
	fmt.Fprintln(promptOut, "  - Open long lived control channel to receive Hub commands")
	fmt.Fprintln(promptOut, "  - Update agent lifecycle status on the Hub")
	fmt.Fprintln(promptOut)
	return ConfirmAction("Continue with broker registration?", true, autoConfirm)
}

// ShowBrokerDeregistrationPrompt displays the broker deregistration warning.
// Shows list of projects the broker contributes to.
// Returns true if the user confirms, false otherwise.
func ShowBrokerDeregistrationPrompt(brokerID string, projects []string, autoConfirm bool) bool {
	fmt.Fprintln(promptOut)
	fmt.Fprintln(promptOut, "This will remove this host's broker registration from the Hub.")
	fmt.Fprintf(promptOut, "Broker ID: %s\n", brokerID)
	fmt.Fprintln(promptOut)

	if len(projects) > 0 {
		fmt.Fprintf(promptOut, "This broker contributes to %d project(s):\n", len(projects))
		for _, p := range projects {
			fmt.Fprintf(promptOut, "  - %s\n", p)
		}
		fmt.Fprintln(promptOut)
		fmt.Fprintln(promptOut, "The broker will be removed from ALL projects it contributes to.")
	}

	fmt.Fprintln(promptOut)
	// Default NO for safety - destructive operation
	return ConfirmAction("Continue with deregistration?", false, autoConfirm)
}

// ShowProjectLinkPrompt displays the project link confirmation.
// Returns true if the user confirms, false otherwise.
func ShowProjectLinkPrompt(projectName, endpoint string, autoConfirm bool) bool {
	fmt.Fprintln(promptOut)
	fmt.Fprintf(promptOut, "This will link project '%s' to the Hub.\n", projectName)
	fmt.Fprintf(promptOut, "Hub endpoint: %s\n", endpoint)
	fmt.Fprintln(promptOut)
	fmt.Fprintln(promptOut, "When linked:")
	fmt.Fprintln(promptOut, "  - Agent operations will be coordinated through the Hub")
	fmt.Fprintln(promptOut, "  - Agents can be managed from any connected broker")
	fmt.Fprintln(promptOut, "  - Local agents will be synced to the Hub")
	fmt.Fprintln(promptOut)
	return ConfirmAction("Continue with linking?", true, autoConfirm)
}

// ShowProjectUnlinkPrompt displays the project unlink confirmation.
// Returns true if the user confirms, false otherwise.
func ShowProjectUnlinkPrompt(projectName string, autoConfirm bool) bool {
	fmt.Fprintln(promptOut)
	fmt.Fprintf(promptOut, "This will unlink project '%s' from the Hub locally.\n", projectName)
	fmt.Fprintln(promptOut)
	fmt.Fprintln(promptOut, "The project and its agents will remain on the Hub for other brokers.")
	fmt.Fprintln(promptOut, "You can re-link this project later with 'scion hub link'.")
	fmt.Fprintln(promptOut)
	// Default NO - user should be sure they want to unlink
	return ConfirmAction("Continue with unlinking?", false, autoConfirm)
}

// LinkOrDisableChoice represents the user's choice when project is not linked.
type LinkOrDisableChoice int

const (
	// LinkOrDisableCancel means the user cancelled the operation.
	LinkOrDisableCancel LinkOrDisableChoice = iota
	// LinkOrDisableLink means the user chose to link the project.
	LinkOrDisableLink
	// LinkOrDisableDisable means the user chose to disable Hub.
	LinkOrDisableDisable
)

// ShowProjectLinkOrDisablePrompt displays a prompt when Hub is enabled but project is not linked.
// Returns the user's choice. Without a terminal (and without autoConfirm) it
// does not read stdin and returns LinkOrDisableCancel.
func ShowProjectLinkOrDisablePrompt(projectName string, autoConfirm bool) LinkOrDisableChoice {
	fmt.Fprintln(promptOut)
	fmt.Fprintln(promptOut, "Hub is enabled but this project is not linked.")
	fmt.Fprintln(promptOut)
	fmt.Fprintln(promptOut, "Choose an option:")
	fmt.Fprintln(promptOut, "  [1] Link and sync project now")
	fmt.Fprintln(promptOut, "  [2] Disable Hub for this project")
	fmt.Fprintln(promptOut)

	if autoConfirm {
		// Auto-confirm defaults to linking
		fmt.Fprintln(promptOut, "Auto-selecting: Link and sync project")
		return LinkOrDisableLink
	}

	if !stdinIsTerminal() {
		fmt.Fprintln(promptOut, "No choice made (stdin is not a terminal; re-run with --yes to link and sync).")
		return LinkOrDisableCancel
	}

	reader := bufio.NewReader(promptIn)
	for {
		fmt.Fprint(promptOut, "Enter choice (or 'c' to cancel): ")
		input, err := reader.ReadString('\n')
		if err != nil {
			return LinkOrDisableCancel
		}

		input = strings.TrimSpace(strings.ToLower(input))
		if input == "c" || input == "cancel" {
			return LinkOrDisableCancel
		}

		choice := 0
		if _, err := fmt.Sscanf(input, "%d", &choice); err != nil {
			fmt.Fprintln(promptOut, "Invalid choice. Please enter 1 or 2.")
			continue
		}

		switch choice {
		case 1:
			return LinkOrDisableLink
		case 2:
			return LinkOrDisableDisable
		default:
			fmt.Fprintln(promptOut, "Invalid choice. Please enter 1 or 2.")
		}
	}
}

// ShowSyncAfterLinkPrompt asks if user wants to sync agents after linking.
// Returns true if the user confirms, false otherwise.
func ShowSyncAfterLinkPrompt(autoConfirm bool) bool {
	return ConfirmAction("Project linked. Sync agents now?", true, autoConfirm)
}

// ShowLinkBeforeRegisterPrompt asks if user wants to link project before registering broker.
// Returns true if the user confirms, false otherwise.
func ShowLinkBeforeRegisterPrompt(projectName string, autoConfirm bool) bool {
	fmt.Fprintln(promptOut)
	fmt.Fprintf(promptOut, "Project '%s' is not linked to the Hub.\n", projectName)
	return ConfirmAction("Link it first?", true, autoConfirm)
}

// ShowProjectProviderPrompt asks if user wants to add the broker as a provider to the project.
// Returns true if the user confirms, false otherwise.
func ShowProjectProviderPrompt(projectName string, autoConfirm bool) bool {
	fmt.Fprintln(promptOut)
	fmt.Fprintf(promptOut, "Add this broker as a provider to project '%s'?\n", projectName)
	fmt.Fprintln(promptOut, "This will allow the broker to execute agents for this project.")
	return ConfirmAction("Continue?", true, autoConfirm)
}

// ShowCheckHubAnywayPrompt asks if user wants to check Hub even though it's disabled.
// Returns true if the user wants to check, false otherwise.
func ShowCheckHubAnywayPrompt(autoConfirm bool) bool {
	fmt.Fprintln(promptOut)
	fmt.Fprintln(promptOut, "Hub integration is disabled for this project.")
	return ConfirmAction("Check Hub status anyway?", false, autoConfirm)
}

// ShowCleanUnlinkPrompt asks if user wants to unlink from Hub before cleaning.
// Returns true if the user confirms, false otherwise.
func ShowCleanUnlinkPrompt(projectName string, autoConfirm bool) bool {
	fmt.Fprintln(promptOut)
	fmt.Fprintln(promptOut, "The project will be unlinked from the Hub locally.")
	fmt.Fprintln(promptOut, "The project and its agents will remain on the Hub for other brokers.")
	return ConfirmAction("Unlink from Hub before cleaning?", true, autoConfirm)
}

// ShowCleanConfirmPrompt displays the final confirmation for cleaning a project.
// Returns true if the user confirms, false otherwise.
func ShowCleanConfirmPrompt(projectName, projectPath string, isGlobal bool, autoConfirm bool) bool {
	fmt.Fprintln(promptOut)
	fmt.Fprintln(promptOut, "This will permanently remove the scion configuration:")
	fmt.Fprintf(promptOut, "  Project: %s\n", projectName)
	fmt.Fprintf(promptOut, "  Path:    %s\n", projectPath)
	if isGlobal {
		fmt.Fprintln(promptOut, "  Type:  global")
	} else {
		fmt.Fprintln(promptOut, "  Type:  project")
	}
	fmt.Fprintln(promptOut)
	fmt.Fprintln(promptOut, "This action cannot be undone. Agent configurations will be lost.")
	fmt.Fprintln(promptOut)
	// Default NO for safety - destructive operation
	return ConfirmAction("Remove scion project?", false, autoConfirm)
}

// ShowProvidePrompt asks if user wants to add the broker as a provider for a project.
// Returns true if the user confirms, false otherwise.
func ShowProvidePrompt(projectName, brokerName string, autoConfirm bool) bool {
	fmt.Fprintln(promptOut)
	fmt.Fprintf(promptOut, "Add broker '%s' as a provider for project '%s'?\n", brokerName, projectName)
	fmt.Fprintln(promptOut)
	fmt.Fprintln(promptOut, "This will allow the broker to execute agents for this project.")
	return ConfirmAction("Continue?", true, autoConfirm)
}

// ShowChangeDefaultBrokerPrompt asks if user wants to change the default broker for a project.
// Returns true if the user confirms, false otherwise.
func ShowChangeDefaultBrokerPrompt(projectName, currentBrokerName, newBrokerName string, autoConfirm bool) bool {
	fmt.Fprintln(promptOut)
	fmt.Fprintf(promptOut, "Project '%s' already has a default broker: '%s'\n", projectName, currentBrokerName)
	return ConfirmAction(fmt.Sprintf("Change default broker to '%s'?", newBrokerName), false, autoConfirm)
}

// ShowWithdrawPrompt asks if user wants to remove the broker as a provider from a project.
// Returns true if the user confirms, false otherwise.
func ShowWithdrawPrompt(projectName, brokerName string, autoConfirm bool) bool {
	fmt.Fprintln(promptOut)
	fmt.Fprintf(promptOut, "Remove broker '%s' as a provider from project '%s'?\n", brokerName, projectName)
	fmt.Fprintln(promptOut)
	fmt.Fprintln(promptOut, "The broker will no longer be able to execute agents for this project.")
	fmt.Fprintln(promptOut, "Existing agents on this broker will continue running but cannot be")
	fmt.Fprintln(promptOut, "managed through the Hub until the broker is re-added as a provider.")
	fmt.Fprintln(promptOut)
	// Default NO for safety - could disrupt running agents
	return ConfirmAction("Continue?", false, autoConfirm)
}

// ProjectProviders is an interface to abstract ListProvidersResponse for the delete prompt.
type ProjectProviders interface {
	ProviderCount() int
	ProviderNames() []string
}

// ShowProjectDeletePrompt displays the project deletion confirmation.
// Returns true if the user confirms, false otherwise.
func ShowProjectDeletePrompt(projectName string, agentCount int, providers ProjectProviders, autoConfirm bool) bool {
	fmt.Fprintln(promptOut)
	fmt.Fprintf(promptOut, "This will permanently delete project '%s' from the Hub.\n", projectName)
	fmt.Fprintln(promptOut)
	fmt.Fprintln(promptOut, "The following will be removed:")
	if agentCount > 0 {
		fmt.Fprintf(promptOut, "  - %d agent(s) (will be stopped and deleted)\n", agentCount)
	} else {
		fmt.Fprintln(promptOut, "  - 0 agents")
	}
	if providers != nil && providers.ProviderCount() > 0 {
		fmt.Fprintf(promptOut, "  - %d broker provider association(s):\n", providers.ProviderCount())
		for _, name := range providers.ProviderNames() {
			fmt.Fprintf(promptOut, "      %s\n", name)
		}
	}
	fmt.Fprintln(promptOut)
	fmt.Fprintln(promptOut, "This action cannot be undone.")
	fmt.Fprintln(promptOut)
	// Default NO for safety - destructive operation
	return ConfirmAction("Delete this project?", false, autoConfirm)
}

// ShowBrokerDeletePrompt displays the broker deletion confirmation.
// projectNames is a list of project names the broker provides for.
// Returns true if the user confirms, false otherwise.
func ShowBrokerDeletePrompt(brokerName string, projectNames []string, autoConfirm bool) bool {
	fmt.Fprintln(promptOut)
	fmt.Fprintf(promptOut, "This will permanently delete broker '%s' from the Hub.\n", brokerName)
	fmt.Fprintln(promptOut)

	if len(projectNames) > 0 {
		fmt.Fprintf(promptOut, "This broker provides for %d project(s):\n", len(projectNames))
		for _, name := range projectNames {
			fmt.Fprintf(promptOut, "  - %s\n", name)
		}
		fmt.Fprintln(promptOut)
		fmt.Fprintln(promptOut, "The broker will be removed as a provider from all projects.")
	}

	fmt.Fprintln(promptOut)
	fmt.Fprintln(promptOut, "This action cannot be undone.")
	fmt.Fprintln(promptOut)
	// Default NO for safety - destructive operation
	return ConfirmAction("Delete this broker?", false, autoConfirm)
}
