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
	"errors"
	"fmt"
	"io"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/brokercredentials"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/harness"
	"github.com/GoogleCloudPlatform/scion/pkg/hubsync"
	"github.com/GoogleCloudPlatform/scion/pkg/projectkeys"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
	"github.com/spf13/cobra"
)

var globalInit bool
var initImageRegistry string

// projectCmd represents the project command
var projectCmd = &cobra.Command{
	Use:     "project",
	Aliases: []string{"group", "projects"},
	Short:   "Manage scion projects",
	Long:    `A project is the grouping construct for a set of agents. The .scion folder represents a project.`,
}

// projectInitCmd represents the init subcommand for project
var projectInitCmd = &cobra.Command{
	Use:   "init",
	Short: "Initialize a new project",
	Long: `Initialize a new project by creating the .scion directory structure
and seeding the default template. 

By default, it initializes in:
- The root of the current git repo if run inside a repo
- The current directory

With --global, it initializes in the user's home folder.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		embedOnlyHarnesses := harness.EmbedOnlyHarnesses()
		remoteHub := isRemoteHubEndpoint(invocationHubEndpoint())

		if globalInit || machineInit {
			if !isJSONOutput() {
				fmt.Println("Initializing global scion directory...")
			}

			// Resolve image registry: flag > existing settings > env
			// (counts as configured, never saved) > prompt > skip
			fileRegistry := ""
			if initImageRegistry == "" {
				// Check if existing global settings already have a registry configured
				if globalDir, err := config.GetGlobalDir(); err == nil {
					if vs, err := config.LoadVersionedSettings(globalDir); err == nil {
						fileRegistry = vs.ImageRegistry
					}
				}
			}
			var prompt func() string
			if !isJSONOutput() && !remoteHub {
				prompt = promptImageRegistry
			}
			registryValue, envRegistry := resolveInitImageRegistry(initImageRegistry, fileRegistry, config.ImageRegistryFromEnv(), prompt)

			opts := config.InitMachineOpts{
				SkipRuntimeCheck: remoteHub,
				ImageRegistry:    registryValue,
				Force:            machineInitForce,
			}
			if err := config.InitMachine(embedOnlyHarnesses, opts); err != nil {
				return fmt.Errorf("failed to initialize global config: %w", appendHubOnlyInitHint(err))
			}

			if isJSONOutput() {
				details := map[string]interface{}{"global": true, "machine": true}
				if registryValue != "" {
					details["image_registry"] = registryValue
				}
				return outputJSON(ActionResult{
					Status:  "success",
					Command: "project init",
					Message: "scion project successfully initialized.",
					Details: details,
				})
			}

			fmt.Println("scion project successfully initialized.")
			if registryValue != "" {
				fmt.Printf("Image registry: %s\n", registryValue)
			} else if envRegistry != "" {
				fmt.Printf("Image registry: %s (from %s; not saved to settings)\n", config.RedactImageRegistry(envRegistry), config.EnvImageRegistry)
			} else if !remoteHub {
				fmt.Println()
				fmt.Println("Note: image_registry is not configured. Agents cannot start without it.")
				fmt.Printf("  Public images: scion config set --global image_registry %s\n", defaultImageRegistrySuggestion)
				fmt.Println("  Custom images: build and push first (see image-build/README.md), then run:")
				fmt.Println("                 scion config set --global image_registry <your-registry>")
			}

			// Prompt for Hub registration if Hub is configured
			if err := promptHubRegistration(true); err != nil {
				// Non-fatal: just log the error
				fmt.Printf("Note: %v\n", err)
			}

			return nil
		}

		if !remoteHub {
			// Check if ~/.scion/ exists; error if not since global project is required
			if globalDir, err := config.GetGlobalDir(); err == nil {
				if _, err := os.Stat(globalDir); os.IsNotExist(err) {
					return fmt.Errorf("global scion directory (~/.scion/) does not exist.\nRun 'scion init --machine' first to set up the global configuration.\n%s", hubOnlyInitHint)
				}
			}
		}

		// Check for existing project at or above current directory
		if _, rootDir, found := config.GetEnclosingProjectPath(); found {
			wd, _ := os.Getwd()
			if filepath.Clean(wd) == filepath.Clean(rootDir) {
				// Re-running init in an existing project is allowed — it ensures
				// the project structure is intact (dirs, gitignore, etc.).
				if !isJSONOutput() {
					fmt.Println("Project already initialized. Ensuring integrity...")
				}
				// Fall through to InitProject which is idempotent
			} else if !isJSONOutput() {
				// Inform user about parent project — nested projects are allowed
				fmt.Printf("Note: parent project exists at %s. Initializing nested project.\n", rootDir)
			}
		}

		// Determine target directory
		targetDir, err := config.GetTargetProjectDir()
		if err != nil {
			return fmt.Errorf("failed to determine project directory: %w", err)
		}

		// Check if we're in a subdirectory of a git repo
		wd, _ := os.Getwd()
		if util.IsGitRepo() {
			repoRoot, err := util.RepoRoot()
			if err == nil && repoRoot != "" {
				expectedTarget := filepath.Join(repoRoot, config.DotScion)
				if targetDir == expectedTarget && wd != repoRoot {
					fmt.Printf("Note: Creating .scion at repository root (%s)\n", repoRoot)
				}
			}
		}

		if !isJSONOutput() {
			fmt.Println("Initializing scion project...")
		}
		if err := config.InitProject("", embedOnlyHarnesses, config.InitProjectOpts{SkipRuntimeCheck: remoteHub}); err != nil {
			return fmt.Errorf("failed to initialize project: %w", appendHubOnlyInitHint(err))
		}

		// Resolve the projectID and save it to settings.
		// For non-git projects, targetDir (.scion) is now a marker file, so we must
		// resolve through it to the external config path. The projectID is already
		// generated during InitProject — read it back rather than generating a new one.
		var projectID string
		markerPath := filepath.Join(filepath.Dir(targetDir), config.DotScion)
		if config.IsProjectMarkerFile(markerPath) {
			// Non-git project: read projectID from marker, save to external settings
			marker, err := config.ReadProjectMarker(markerPath)
			if err == nil {
				projectID = marker.ProjectID
				// projectID is already written during initExternalProject
			}
		} else {
			// Git project: read projectID from file, save to in-repo settings
			projectID, _ = config.ReadProjectID(targetDir)
			if projectID == "" {
				projectID = config.GenerateProjectID()
			}
			if err := config.UpdateSetting(targetDir, projectkeys.ConfigProjectIDKey, projectID, false); err != nil {
				if !isJSONOutput() {
					fmt.Printf("Warning: failed to save project_id: %v\n", err)
				}
			}
		}

		if isJSONOutput() {
			return outputJSON(ActionResult{
				Status:  "success",
				Command: "project init",
				Message: "scion project successfully initialized.",
				Details: map[string]interface{}{
					"projectId": projectID,
					"path":      targetDir,
				},
			})
		}

		fmt.Println("scion project successfully initialized.")
		fmt.Printf("Project ID: %s\n", projectID)

		// Prompt for Hub registration if Hub is configured
		if err := promptHubRegistration(false); err != nil {
			// Non-fatal: just log the error
			fmt.Printf("Note: %v\n", err)
		}

		return nil
	},
}

const hubOnlyInitHint = "To use a remote Hub without a local container runtime, register it with 'scion hub add <name> <url>' and select it with 'scion use --hub <name>' (or run 'scion use <project>' to work without initializing a directory)"

// isRemoteHubEndpoint reports whether endpoint is a validated, genuinely remote
// Hub URL for which 'scion init' and 'scion init --machine' may skip the local
// ~/.scion/ prerequisite and local container-runtime detection.
//
// It uses a fail-closed allowlist: endpoint must pass config.ValidateHubURL and
// config.CheckHubTransport, use https://, have no surrounding whitespace or
// invalid port, and have a host that is either:
//   - a multi-label RFC 1123 DNS name with an alphabetic final label (which
//     excludes all decimal, hex, octal, short, dotted, and mixed-radix IPv4
//     forms and all IPv6 forms), excluding reserved local domains (localhost,
//     *.localhost, *.localdomain, *.local) and container host aliases; or
//   - a globally routable IP literal parsed with net/netip (rejecting loopback,
//     private, link-local, unspecified, multicast, 0.0.0.0/8, IPv4-mapped IPv6,
//     zone-scoped IPv6, and other IANA special-purpose ranges).
//
// Classification is lexical only: it does not perform DNS lookups or read
// /etc/hosts, so custom /etc/hosts aliases and DNS names that resolve to
// loopback are out of scope.
func isRemoteHubEndpoint(endpoint string) bool {
	if endpoint == "" || endpoint != strings.TrimSpace(endpoint) {
		return false
	}
	if config.ValidateHubURL(endpoint) != nil || config.CheckHubTransport(endpoint) != nil {
		return false
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "https" || u.Opaque != "" || u.Host == "" || strings.HasSuffix(u.Host, ":") {
		return false
	}
	if portStr := u.Port(); portStr != "" {
		if !isAllASCIIDigits(portStr) || portStr[0] == '0' {
			return false
		}
		port, err := strconv.Atoi(portStr)
		if err != nil || port < 1 || port > 65535 {
			return false
		}
	}
	rawHost := u.Hostname()
	if rawHost == "" || strings.Contains(rawHost, "%") {
		return false
	}
	if strings.HasPrefix(u.Host, "[") {
		addr, err := netip.ParseAddr(rawHost)
		return err == nil && addr.Is6() && isGloballyRoutableIP(addr)
	}
	if addr, err := netip.ParseAddr(rawHost); err == nil {
		return addr.Is4() && isGloballyRoutableIP(addr)
	}
	return isRemoteHubDNSName(rawHost)
}

// nonGlobalIPPrefixes lists IANA special-purpose IPv4 and IPv6 ranges that are
// not globally routable beyond netip.Addr's built-in loopback, private,
// link-local, unspecified, and multicast predicates.
var nonGlobalIPPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),       // "this network" (RFC 1122; routes to loopback on Linux)
	netip.MustParsePrefix("100.64.0.0/10"),   // shared address space / CGNAT (RFC 6598)
	netip.MustParsePrefix("192.0.0.0/24"),    // IETF protocol assignments (RFC 6890)
	netip.MustParsePrefix("192.0.2.0/24"),    // TEST-NET-1 documentation (RFC 5737)
	netip.MustParsePrefix("192.88.99.0/24"),  // deprecated 6to4 relay anycast (RFC 7526)
	netip.MustParsePrefix("198.18.0.0/15"),   // benchmarking (RFC 2544)
	netip.MustParsePrefix("198.51.100.0/24"), // TEST-NET-2 documentation (RFC 5737)
	netip.MustParsePrefix("203.0.113.0/24"),  // TEST-NET-3 documentation (RFC 5737)
	netip.MustParsePrefix("240.0.0.0/4"),     // reserved for future use + limited broadcast
	netip.MustParsePrefix("::/96"),           // deprecated IPv4-compatible IPv6
	netip.MustParsePrefix("::ffff:0:0/96"),   // IPv4-mapped IPv6
	netip.MustParsePrefix("::ffff:0:0:0/96"), // IPv4-translated (SIIT, RFC 2765)
	netip.MustParsePrefix("64:ff9b::/96"),    // NAT64 well-known prefix (RFC 6052)
	netip.MustParsePrefix("64:ff9b:1::/48"),  // NAT64 local-use prefix (RFC 8215)
	netip.MustParsePrefix("100::/64"),        // discard-only prefix (RFC 6666)
	netip.MustParsePrefix("2001::/32"),       // Teredo tunneling (RFC 4380)
	netip.MustParsePrefix("2001:2::/48"),     // benchmarking (RFC 5180)
	netip.MustParsePrefix("2001:db8::/32"),   // documentation (RFC 3849)
	netip.MustParsePrefix("2002::/16"),       // 6to4 tunneling (RFC 3056)
	netip.MustParsePrefix("fec0::/10"),       // deprecated site-local (RFC 3879)
}

func isGloballyRoutableIP(addr netip.Addr) bool {
	if !addr.IsValid() || addr.Zone() != "" || addr.Is4In6() {
		return false
	}
	if !addr.IsGlobalUnicast() || addr.IsPrivate() ||
		addr.IsLoopback() || addr.IsUnspecified() ||
		addr.IsLinkLocalUnicast() || addr.IsLinkLocalMulticast() ||
		addr.IsInterfaceLocalMulticast() || addr.IsMulticast() {
		return false
	}
	for _, p := range nonGlobalIPPrefixes {
		if p.Contains(addr) {
			return false
		}
	}
	return true
}

func isRemoteHubDNSName(rawHost string) bool {
	h := strings.TrimSuffix(rawHost, ".")
	if h == "" || len(h) > 253 || strings.HasSuffix(h, ".") {
		return false
	}
	if _, err := netip.ParseAddr(h); err == nil {
		return false
	}
	labels := strings.Split(h, ".")
	if len(labels) < 2 {
		return false
	}
	for _, label := range labels {
		if len(label) == 0 || len(label) > 63 || !isValidDNSLabel(label) {
			return false
		}
	}
	last := labels[len(labels)-1]
	if len(last) < 2 || !isAllASCIILetters(last) {
		return false
	}
	canon := strings.ToLower(h)
	switch strings.ToLower(last) {
	case "localhost", "localdomain", "local":
		return false
	}
	return !config.IsAgentHostAliasHost(canon)
}

func isValidDNSLabel(label string) bool {
	for i := 0; i < len(label); i++ {
		c := label[i]
		isAlphaNum := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
		if i == 0 || i == len(label)-1 {
			if !isAlphaNum {
				return false
			}
			continue
		}
		if !isAlphaNum && c != '-' {
			return false
		}
	}
	return true
}

func isAllASCIILetters(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') {
			return false
		}
	}
	return len(s) > 0
}

func isAllASCIIDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return len(s) > 0
}

func appendHubOnlyInitHint(err error) error {
	if errors.Is(err, config.ErrNoLocalRuntime) {
		return fmt.Errorf("%w\n%s", err, hubOnlyInitHint)
	}
	return err
}

// promptHubRegistration links the newly initialised project to the hub,
// through requireHub: on the local workstation hub it links without a
// prompt, on a remote hub it asks first. When no hub is configured or the
// local server is not running, it returns a note-level error (the caller
// prints it as "Note: ..."): the project links on first use once a hub is
// up, so init itself does not fail.
func promptHubRegistration(isGlobal bool) error {
	var gp string
	if isGlobal {
		gp = "global"
	}
	if _, err := requireHub(gp); err != nil {
		var notConfigured *hubsync.HubNotConfiguredError
		if errors.As(err, &notConfigured) {
			return errors.New("no hub is selected yet; this project links on first use. " +
				"Start the local hub with 'scion server start' (first run opens setup), " +
				"or add a remote one with 'scion hub add <name> <url>' and select it with 'scion use --hub <name>'")
		}
		var notRunning *hubsync.LocalServerNotRunningError
		if errors.As(err, &notRunning) {
			return errors.New("the local scion server is not running; this project links on first use. " +
				"Start it with 'scion server start'")
		}
		return err
	}
	return nil
}

// getLocalBrokerInfo returns the local broker ID and name if this host is registered as a broker.
func getLocalBrokerInfo(settings *config.Settings) (brokerID, brokerName string) {
	// First check brokercredentials store
	credStore := brokercredentials.NewStore("")
	creds, err := credStore.Load()
	if err == nil && creds != nil && creds.BrokerID != "" {
		brokerID = creds.BrokerID
	}

	// Fall back to global settings
	if brokerID == "" {
		globalDir, err := config.GetGlobalDir()
		if err == nil {
			globalSettings, err := config.LoadSettings(globalDir)
			if err == nil && globalSettings.Hub != nil && globalSettings.Hub.BrokerID != "" {
				brokerID = globalSettings.Hub.BrokerID
			}
		}
	}

	// Get the broker name for display: the configured name, else the hostname
	brokerName = config.LocalBrokerName("")
	if brokerName == "" {
		if brokerID != "" && len(brokerID) >= 8 {
			brokerName = brokerID[:8]
		} else {
			brokerName = "local-host"
		}
	}

	return brokerID, brokerName
}

// resolveInitImageRegistry decides the image registry 'scion init
// --global/--machine' saves: the flag, else the existing settings value,
// else (when SCION_IMAGE_REGISTRY is unset) the prompt's answer. A node env
// value counts as configured, so it skips the prompt, but it is returned
// separately and never saved: it is a node override, not a setting
// (ptone/scion#3916). prompt may be nil (no prompting).
func resolveInitImageRegistry(flag, fileValue, envValue string, prompt func() string) (save, env string) {
	if flag != "" {
		return flag, envValue
	}
	if fileValue != "" {
		return fileValue, envValue
	}
	if envValue != "" || prompt == nil {
		return "", envValue
	}
	return prompt(), ""
}

// defaultImageRegistrySuggestion is the public registry suggested by
// 'scion init --machine' when no registry is configured, matching
// DEFAULT_REGISTRY_SUGGESTION in the web onboarding wizard.
const defaultImageRegistrySuggestion = "ghcr.io/homebrew-scion"

// promptImageRegistry prompts the user for their container image registry path.
// Returns the entered value (defaulting to defaultImageRegistrySuggestion on
// bare Enter in an interactive terminal) or empty string if skipped or
// non-interactive.
func promptImageRegistry() string {
	if nonInteractive {
		return ""
	}
	return promptImageRegistryFrom(os.Stdin, os.Stderr, util.IsTerminal())
}

func promptImageRegistryFrom(in io.Reader, out io.Writer, isTTY bool) string {
	_, _ = fmt.Fprintln(out)
	_, _ = fmt.Fprintln(out, "Scion runs agents in containers and needs a container image registry.")
	_, _ = fmt.Fprintln(out)
	_, _ = fmt.Fprintf(out, "  Public images:   %s (release builds pull :<version>)\n", defaultImageRegistrySuggestion)
	_, _ = fmt.Fprintln(out, "  Custom registry: build and push images — see image-build/README.md")
	_, _ = fmt.Fprintln(out, "  Local images:    localhost/scion")
	_, _ = fmt.Fprintln(out)

	if !isTTY {
		return ""
	}

	reader := bufio.NewReader(in)
	_, _ = fmt.Fprintf(out, "Image registry path [%s] (or 'skip' to leave unset): ", defaultImageRegistrySuggestion)
	input, err := reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return ""
	}
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		if errors.Is(err, io.EOF) {
			return ""
		}
		return defaultImageRegistrySuggestion
	}
	if strings.EqualFold(trimmed, "skip") {
		return ""
	}
	return trimmed
}

func init() {
	rootCmd.AddCommand(projectCmd)
	projectCmd.AddCommand(projectInitCmd)

	projectInitCmd.Flags().BoolVar(&globalInit, "global", false, "Initialize the global project in the home directory")
	projectInitCmd.Flags().BoolVar(&machineInit, "machine", false, "Perform full machine-level setup (seeds harness-configs, templates, settings)")
	projectInitCmd.Flags().StringVar(&initImageRegistry, "image-registry", "", "Container image registry path (e.g., "+defaultImageRegistrySuggestion+" or ghcr.io/myorg)")
}
