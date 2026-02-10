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
// If autoConfirm is true, returns defaultYes without prompting.
func ConfirmAction(prompt string, defaultYes bool, autoConfirm bool) bool {
	if autoConfirm {
		return defaultYes
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

// GroveChoice represents the user's choice when matching groves exist.
type GroveChoice int

const (
	// GroveChoiceCancel means the user cancelled the operation.
	GroveChoiceCancel GroveChoice = iota
	// GroveChoiceLink means the user chose to link to an existing grove.
	GroveChoiceLink
	// GroveChoiceRegisterNew means the user chose to register a new grove.
	GroveChoiceRegisterNew
)

// GroveMatch holds information about a matching grove for display.
type GroveMatch struct {
	ID        string
	Name      string
	GitRemote string
}

// ShowMatchingGrovesPrompt displays matching groves and asks the user to choose.
// Returns the choice and the selected grove ID if linking.
func ShowMatchingGrovesPrompt(groveName string, matches []GroveMatch, autoConfirm bool) (GroveChoice, string) {
	fmt.Println()
	fmt.Printf("Found %d existing grove(s) with the name '%s' on the Hub:\n", len(matches), groveName)
	fmt.Println()

	for i, m := range matches {
		if m.GitRemote != "" {
			fmt.Printf("  [%d] %s (ID: %s, remote: %s)\n", i+1, m.Name, m.ID, m.GitRemote)
		} else {
			fmt.Printf("  [%d] %s (ID: %s)\n", i+1, m.Name, m.ID)
		}
	}
	fmt.Printf("  [%d] Register as a new grove (duplicate name)\n", len(matches)+1)
	fmt.Println()

	if autoConfirm {
		// Auto-confirm defaults to linking to the first match
		fmt.Printf("Auto-linking to: %s (ID: %s)\n", matches[0].Name, matches[0].ID)
		return GroveChoiceLink, matches[0].ID
	}

	reader := bufio.NewReader(os.Stdin)
	for {
		fmt.Print("Enter choice (or 'c' to cancel): ")
		input, err := reader.ReadString('\n')
		if err != nil {
			return GroveChoiceCancel, ""
		}

		input = strings.TrimSpace(strings.ToLower(input))
		if input == "c" || input == "cancel" {
			return GroveChoiceCancel, ""
		}

		choice := 0
		if _, err := fmt.Sscanf(input, "%d", &choice); err != nil {
			fmt.Println("Invalid choice. Please enter a number.")
			continue
		}

		if choice < 1 || choice > len(matches)+1 {
			fmt.Printf("Invalid choice. Please enter 1-%d.\n", len(matches)+1)
			continue
		}

		if choice == len(matches)+1 {
			return GroveChoiceRegisterNew, ""
		}

		return GroveChoiceLink, matches[choice-1].ID
	}
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

// ShowGroveLinkPrompt displays the grove link confirmation.
// Returns true if the user confirms, false otherwise.
func ShowGroveLinkPrompt(groveName, endpoint string, autoConfirm bool) bool {
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

// ShowGroveUnlinkPrompt displays the grove unlink confirmation.
// Returns true if the user confirms, false otherwise.
func ShowGroveUnlinkPrompt(groveName string, autoConfirm bool) bool {
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

// ShowLinkOrDisablePrompt displays a prompt when Hub is enabled but grove is not linked.
// Returns the user's choice.
func ShowLinkOrDisablePrompt(groveName string, autoConfirm bool) LinkOrDisableChoice {
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

// ShowGroveProviderPrompt asks if user wants to add the broker as a provider to the grove.
// Returns true if the user confirms, false otherwise.
func ShowGroveProviderPrompt(groveName string, autoConfirm bool) bool {
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

// GroveProviders is an interface to abstract ListProvidersResponse for the delete prompt.
type GroveProviders interface {
	ProviderCount() int
	ProviderNames() []string
}

// ShowGroveDeletePrompt displays the grove deletion confirmation.
// Returns true if the user confirms, false otherwise.
func ShowGroveDeletePrompt(groveName string, agentCount int, providers GroveProviders, autoConfirm bool) bool {
	fmt.Println()
	fmt.Printf("This will permanently delete grove '%s' from the Hub.\n", groveName)
	fmt.Println()
	fmt.Println("The following will be removed:")
	if agentCount > 0 {
		fmt.Printf("  - %d agent(s)\n", agentCount)
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
