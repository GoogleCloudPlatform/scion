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
	"fmt"
	"os"
	"strings"
)

// ConfirmAction prompts user for Y/n confirmation.
// Returns true if confirmed, false otherwise.
// If autoConfirm is true, always confirms Yes (proceeds with the action).
// The defaultYes parameter only affects the interactive case: it controls
// whether pressing Enter without input confirms (Y/n) or declines (y/N).
func ConfirmAction(prompt string, defaultYes bool, autoConfirm bool) bool {
	if autoConfirm {
		fmt.Printf("%s: auto-confirmed Yes\n", prompt)
		return true
	}

	suffix := " (Y/n): "
	if !defaultYes {
		suffix = " (y/N): "
	}

	fmt.Print(prompt + suffix)

	reader := bufio.NewReader(os.Stdin)
	input, err := reader.ReadString('\n')
	if err != nil {
		// On error, return the default
		return defaultYes
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

	fmt.Println()
	fmt.Println("Hub Agent Sync Required")
	fmt.Println("=======================")

	if len(result.ToRegister) > 0 {
		fmt.Println("Agents to register on Hub:")
		for _, name := range result.ToRegister {
			fmt.Printf("  + %s\n", name)
		}
	}

	if len(result.ToRemove) > 0 {
		fmt.Println("Agents to remove from Hub (not on this broker):")
		for _, ref := range result.ToRemove {
			fmt.Printf("  - %s\n", ref.Name)
		}
	}

	// Show pending agents for visibility (they don't require action)
	if len(result.Pending) > 0 {
		fmt.Println()
		fmt.Println("Agents pending on Hub (awaiting start):")
		for _, ref := range result.Pending {
			fmt.Printf("  ~ %s\n", ref.Name)
		}
	}

	// Show remote-only agents for visibility (they don't require action)
	if len(result.RemoteOnly) > 0 {
		fmt.Println()
		fmt.Println("Agents on Hub from other brokers (no action needed):")
		for _, ref := range result.RemoteOnly {
			fmt.Printf("  ~ %s\n", ref.Name)
		}
	}

	// Show stale-local agents for visibility (they don't require action)
	if len(result.StaleLocal) > 0 {
		fmt.Println()
		fmt.Println("Stale local agent artifacts (no action needed):")
		for _, name := range result.StaleLocal {
			fmt.Printf("  ~ %s\n", name)
		}
	}

	fmt.Println()
	return ConfirmAction("Proceed with sync?", true, autoConfirm)
}

// ShowLinkPrompt displays the grove link prompt.
// Returns true if the user confirms, false otherwise.
func ShowLinkPrompt(groveName string, autoConfirm bool) bool {
	fmt.Println()
	fmt.Printf("Grove '%s' is not linked to the Hub.\n", groveName)
	return ConfirmAction("Link grove with Hub?", true, autoConfirm)
}

// ShowInitLinkPrompt displays the post-init link prompt.
// Returns true if the user confirms, false otherwise.
func ShowInitLinkPrompt(autoConfirm bool) bool {
	return ConfirmAction("Grove initialized. Link to Hub?", true, autoConfirm)
}

// ShowInitProvidePrompt displays a confirmation to add this broker as a provider.
// Returns true if the user confirms, false otherwise.
func ShowInitProvidePrompt(brokerName, groveName string, autoConfirm bool) bool {
	fmt.Printf("This host (%s) is registered as a broker.\n", brokerName)
	return ConfirmAction(fmt.Sprintf("Add as provider for '%s'?", groveName), true, autoConfirm)
}

// ProjectChoice represents the user's choice when matching groves exist.
type ProjectChoice int

const (
	// ProjectChoiceCancel means the user cancelled the operation.
	ProjectChoiceCancel ProjectChoice = iota
	// ProjectChoiceLink means the user chose to link to an existing grove.
	ProjectChoiceLink
	// ProjectChoiceRegisterNew means the user chose to register a new grove.
	ProjectChoiceRegisterNew
)

// ProjectMatch holds information about a matching grove for display.
type ProjectMatch struct {
	ID        string
	Name      string
	Slug      string
	GitRemote string
}

// ShowMatchingProjectsPrompt displays matching groves and asks the user to choose.
// The "Register as new grove" option is always shown, allowing multiple groves
// per git remote. When nextSlug is non-empty, it is displayed as the proposed
// slug for a new grove.
// Returns the choice and the selected grove ID if linking.
func ShowMatchingProjectsPrompt(projectName string, matches []ProjectMatch, nextSlug string, autoConfirm bool) (ProjectChoice, string) {
	fmt.Println()
	fmt.Printf("Found %d existing grove(s) with the name '%s' on the Hub:\n", len(matches), projectName)
	fmt.Println()

	for i, m := range matches {
		if m.GitRemote != "" {
			fmt.Printf("  [%d] %s (ID: %s, remote: %s)\n", i+1, m.Name, m.ID, m.GitRemote)
		} else {
			fmt.Printf("  [%d] %s (ID: %s)\n", i+1, m.Name, m.ID)
		}
	}
	if nextSlug != "" {
		fmt.Printf("  [%d] Register as a new grove (will be created as '%s')\n", len(matches)+1, nextSlug)
	} else {
		fmt.Printf("  [%d] Register as a new grove\n", len(matches)+1)
	}
	fmt.Println()

	if autoConfirm {
		// Auto-confirm defaults to linking to the first match
		fmt.Printf("Auto-linking to: %s (ID: %s)\n", matches[0].Name, matches[0].ID)
		return ProjectChoiceLink, matches[0].ID
	}

	maxChoice := len(matches) + 1

	reader := bufio.NewReader(os.Stdin)
	for {
		fmt.Print("Enter choice (or 'c' to cancel): ")
		input, err := reader.ReadString('\n')
		if err != nil {
			return ProjectChoiceCancel, ""
		}

		input = strings.TrimSpace(strings.ToLower(input))
		if input == "c" || input == "cancel" {
			return ProjectChoiceCancel, ""
		}

		choice := 0
		if _, err := fmt.Sscanf(input, "%d", &choice); err != nil {
			fmt.Println("Invalid choice. Please enter a number.")
			continue
		}

		if choice < 1 || choice > maxChoice {
			fmt.Printf("Invalid choice. Please enter 1-%d.\n", maxChoice)
			continue
		}

		if choice == len(matches)+1 {
			return ProjectChoiceRegisterNew, ""
		}

		return ProjectChoiceLink, matches[choice-1].ID
	}
}

// NextSlugFromMatches computes a proposed next serial slug from a list of
// existing grove matches. This is a client-side estimate for display purposes;
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
	fmt.Println()
	fmt.Println("This will register this host as a Runtime Broker with the Hub.")
	fmt.Printf("Hub endpoint: %s\n", endpoint)
	fmt.Println()
	fmt.Println("The broker will be able to:")
	fmt.Println("  - Execute agents on behalf of authorized Hub users")
	fmt.Println("  - Open long lived control channel to receive Hub commands")
	fmt.Println("  - Update agent lifecycle status on the Hub")
	fmt.Println()
	return ConfirmAction("Continue with broker registration?", true, autoConfirm)
}

// ShowBrokerDeregistrationPrompt displays the broker deregistration warning.
// Shows list of groves the broker contributes to.
// Returns true if the user confirms, false otherwise.
func ShowBrokerDeregistrationPrompt(brokerID string, groves []string, autoConfirm bool) bool {
	fmt.Println()
	fmt.Println("This will remove this host's broker registration from the Hub.")
	fmt.Printf("Broker ID: %s\n", brokerID)
	fmt.Println()

	if len(groves) > 0 {
		fmt.Printf("This broker contributes to %d grove(s):\n", len(groves))
		for _, g := range groves {
			fmt.Printf("  - %s\n", g)
		}
		fmt.Println()
		fmt.Println("The broker will be removed from ALL groves it contributes to.")
	}

	fmt.Println()
	// Default NO for safety - destructive operation
	return ConfirmAction("Continue with deregistration?", false, autoConfirm)
}

// ShowProjectLinkPrompt displays the grove link confirmation.
// Returns true if the user confirms, false otherwise.
func ShowProjectLinkPrompt(groveName, endpoint string, autoConfirm bool) bool {
	fmt.Println()
	fmt.Printf("This will link grove '%s' to the Hub.\n", groveName)
	fmt.Printf("Hub endpoint: %s\n", endpoint)
	fmt.Println()
	fmt.Println("When linked:")
	fmt.Println("  - Agent operations will be coordinated through the Hub")
	fmt.Println("  - Agents can be managed from any connected broker")
	fmt.Println("  - Local agents will be synced to the Hub")
	fmt.Println()
	return ConfirmAction("Continue with linking?", true, autoConfirm)
}

// ShowProjectUnlinkPrompt displays the grove unlink confirmation.
// Returns true if the user confirms, false otherwise.
func ShowProjectUnlinkPrompt(groveName string, autoConfirm bool) bool {
	fmt.Println()
	fmt.Printf("This will unlink grove '%s' from the Hub locally.\n", groveName)
	fmt.Println()
	fmt.Println("The grove and its agents will remain on the Hub for other brokers.")
	fmt.Println("You can re-link this grove later with 'scion hub link'.")
	fmt.Println()
	// Default NO - user should be sure they want to unlink
	return ConfirmAction("Continue with unlinking?", false, autoConfirm)
}

// LinkOrDisableChoice represents the user's choice when grove is not linked.
type LinkOrDisableChoice int

const (
	// LinkOrDisableCancel means the user cancelled the operation.
	LinkOrDisableCancel LinkOrDisableChoice = iota
	// LinkOrDisableLink means the user chose to link the grove.
	LinkOrDisableLink
	// LinkOrDisableDisable means the user chose to disable Hub.
	LinkOrDisableDisable
)

// ShowProjectLinkOrDisablePrompt displays a prompt when Hub is enabled but grove is not linked.
// Returns the user's choice.
func ShowProjectLinkOrDisablePrompt(groveName string, autoConfirm bool) LinkOrDisableChoice {
	fmt.Println()
	fmt.Println("Hub is enabled but this grove is not linked.")
	fmt.Println()
	fmt.Println("Choose an option:")
	fmt.Println("  [1] Link and sync grove now")
	fmt.Println("  [2] Disable Hub for this grove")
	fmt.Println()

	if autoConfirm {
		// Auto-confirm defaults to linking
		fmt.Println("Auto-selecting: Link and sync grove")
		return LinkOrDisableLink
	}

	reader := bufio.NewReader(os.Stdin)
	for {
		fmt.Print("Enter choice (or 'c' to cancel): ")
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
			fmt.Println("Invalid choice. Please enter 1 or 2.")
			continue
		}

		switch choice {
		case 1:
			return LinkOrDisableLink
		case 2:
			return LinkOrDisableDisable
		default:
			fmt.Println("Invalid choice. Please enter 1 or 2.")
		}
	}
}

// ShowSyncAfterLinkPrompt asks if user wants to sync agents after linking.
// Returns true if the user confirms, false otherwise.
func ShowSyncAfterLinkPrompt(autoConfirm bool) bool {
	return ConfirmAction("Grove linked. Sync agents now?", true, autoConfirm)
}

// ShowLinkBeforeRegisterPrompt asks if user wants to link grove before registering broker.
// Returns true if the user confirms, false otherwise.
func ShowLinkBeforeRegisterPrompt(groveName string, autoConfirm bool) bool {
	fmt.Println()
	fmt.Printf("Grove '%s' is not linked to the Hub.\n", groveName)
	return ConfirmAction("Link it first?", true, autoConfirm)
}

// ShowProjectProviderPrompt asks if user wants to add the broker as a provider to the grove.
// Returns true if the user confirms, false otherwise.
func ShowProjectProviderPrompt(groveName string, autoConfirm bool) bool {
	fmt.Println()
	fmt.Printf("Add this broker as a provider to grove '%s'?\n", groveName)
	fmt.Println("This will allow the broker to execute agents for this grove.")
	return ConfirmAction("Continue?", true, autoConfirm)
}

// ShowCheckHubAnywayPrompt asks if user wants to check Hub even though it's disabled.
// Returns true if the user wants to check, false otherwise.
func ShowCheckHubAnywayPrompt(autoConfirm bool) bool {
	fmt.Println()
	fmt.Println("Hub integration is disabled for this grove.")
	return ConfirmAction("Check Hub status anyway?", false, autoConfirm)
}

// ShowCleanUnlinkPrompt asks if user wants to unlink from Hub before cleaning.
// Returns true if the user confirms, false otherwise.
func ShowCleanUnlinkPrompt(groveName string, autoConfirm bool) bool {
	fmt.Println()
	fmt.Println("The grove will be unlinked from the Hub locally.")
	fmt.Println("The grove and its agents will remain on the Hub for other brokers.")
	return ConfirmAction("Unlink from Hub before cleaning?", true, autoConfirm)
}

// ShowCleanConfirmPrompt displays the final confirmation for cleaning a grove.
// Returns true if the user confirms, false otherwise.
func ShowCleanConfirmPrompt(groveName, grovePath string, isGlobal bool, autoConfirm bool) bool {
	fmt.Println()
	fmt.Println("This will permanently remove the scion configuration:")
	fmt.Printf("  Grove: %s\n", groveName)
	fmt.Printf("  Path:  %s\n", grovePath)
	if isGlobal {
		fmt.Println("  Type:  global")
	} else {
		fmt.Println("  Type:  project")
	}
	fmt.Println()
	fmt.Println("This action cannot be undone. Agent configurations will be lost.")
	fmt.Println()
	// Default NO for safety - destructive operation
	return ConfirmAction("Remove scion grove?", false, autoConfirm)
}

// ShowProvidePrompt asks if user wants to add the broker as a provider for a grove.
// Returns true if the user confirms, false otherwise.
func ShowProvidePrompt(groveName, brokerName string, autoConfirm bool) bool {
	fmt.Println()
	fmt.Printf("Add broker '%s' as a provider for grove '%s'?\n", brokerName, groveName)
	fmt.Println()
	fmt.Println("This will allow the broker to execute agents for this grove.")
	return ConfirmAction("Continue?", true, autoConfirm)
}

// ShowChangeDefaultBrokerPrompt asks if user wants to change the default broker for a grove.
// Returns true if the user confirms, false otherwise.
func ShowChangeDefaultBrokerPrompt(groveName, currentBrokerName, newBrokerName string, autoConfirm bool) bool {
	fmt.Println()
	fmt.Printf("Grove '%s' already has a default broker: '%s'\n", groveName, currentBrokerName)
	return ConfirmAction(fmt.Sprintf("Change default broker to '%s'?", newBrokerName), false, autoConfirm)
}

// ShowWithdrawPrompt asks if user wants to remove the broker as a provider from a grove.
// Returns true if the user confirms, false otherwise.
func ShowWithdrawPrompt(groveName, brokerName string, autoConfirm bool) bool {
	fmt.Println()
	fmt.Printf("Remove broker '%s' as a provider from grove '%s'?\n", brokerName, groveName)
	fmt.Println()
	fmt.Println("The broker will no longer be able to execute agents for this grove.")
	fmt.Println("Existing agents on this broker will continue running but cannot be")
	fmt.Println("managed through the Hub until the broker is re-added as a provider.")
	fmt.Println()
	// Default NO for safety - could disrupt running agents
	return ConfirmAction("Continue?", false, autoConfirm)
}

// ProjectProviders is an interface to abstract ListProvidersResponse for the delete prompt.
type ProjectProviders interface {
	ProviderCount() int
	ProviderNames() []string
}

// ShowProjectDeletePrompt displays the grove deletion confirmation.
// Returns true if the user confirms, false otherwise.
func ShowProjectDeletePrompt(groveName string, agentCount int, providers ProjectProviders, autoConfirm bool) bool {
	fmt.Println()
	fmt.Printf("This will permanently delete grove '%s' from the Hub.\n", groveName)
	fmt.Println()
	fmt.Println("The following will be removed:")
	if agentCount > 0 {
		fmt.Printf("  - %d agent(s) (will be stopped and deleted)\n", agentCount)
	} else {
		fmt.Println("  - 0 agents")
	}
	if providers != nil && providers.ProviderCount() > 0 {
		fmt.Printf("  - %d broker provider association(s):\n", providers.ProviderCount())
		for _, name := range providers.ProviderNames() {
			fmt.Printf("      %s\n", name)
		}
	}
	fmt.Println()
	fmt.Println("This action cannot be undone.")
	fmt.Println()
	// Default NO for safety - destructive operation
	return ConfirmAction("Delete this grove?", false, autoConfirm)
}

// ShowBrokerDeletePrompt displays the broker deletion confirmation.
// groveNames is a list of grove names the broker provides for.
// Returns true if the user confirms, false otherwise.
func ShowBrokerDeletePrompt(brokerName string, groveNames []string, autoConfirm bool) bool {
	fmt.Println()
	fmt.Printf("This will permanently delete broker '%s' from the Hub.\n", brokerName)
	fmt.Println()

	if len(groveNames) > 0 {
		fmt.Printf("This broker provides for %d grove(s):\n", len(groveNames))
		for _, name := range groveNames {
			fmt.Printf("  - %s\n", name)
		}
		fmt.Println()
		fmt.Println("The broker will be removed as a provider from all groves.")
	}

	fmt.Println()
	fmt.Println("This action cannot be undone.")
	fmt.Println()
	// Default NO for safety - destructive operation
	return ConfirmAction("Delete this broker?", false, autoConfirm)
}
