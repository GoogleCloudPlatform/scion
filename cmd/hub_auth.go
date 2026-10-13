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
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/credentials"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/auth"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
	"github.com/spf13/cobra"
)

var (
	hubAuthHubURL    string
	hubAuthNoBrowser bool
)

type invalidHubAuthProviderError struct {
	provider string
}

func (e invalidHubAuthProviderError) Error() string {
	return fmt.Sprintf("invalid provider %q: must be one of %s", e.provider, strings.Join(hubclient.OAuthProviderOrder(), ", "))
}

type noHubAuthProvidersConfiguredError struct {
	clientType hubclient.OAuthClientType
}

func (e noHubAuthProvidersConfiguredError) Error() string {
	return fmt.Sprintf("no OAuth providers configured for %s flow", e.clientType)
}

type multipleHubAuthProvidersConfiguredError struct {
	clientType hubclient.OAuthClientType
	providers  []string
}

func (e multipleHubAuthProvidersConfiguredError) Error() string {
	return fmt.Sprintf("multiple OAuth providers configured for %s flow (%s); specify one with --provider", e.clientType, strings.Join(e.providers, ", "))
}

// hubAuthCmd represents the auth subcommand under hub
var hubAuthCmd = &cobra.Command{
	Use:   "auth",
	Short: "Manage Hub authentication",
	Long: `Manage authentication with a Scion Hub.

Commands for logging in and logging out. Use 'scion hub status' to check authentication status.`,
}

// hubAuthLoginCmd authenticates with the Hub
var hubAuthLoginCmd = &cobra.Command{
	Use:   "login",
	Short: "Authenticate with Hub server",
	Long: `Authenticate with a Scion Hub server using browser-based OAuth.

This command will:
1. Start a local callback server
2. Open your browser to authenticate with the Hub
3. Wait for the OAuth callback
4. Store credentials locally

In headless environments (no display server), or when --no-browser is specified,
the device authorization flow is used instead. This displays a URL and code
that you can enter on any device with a browser.

` + hubAuthURLPrecedence + `

After a successful login, when no hub.endpoint is set in the settings that
apply (the current project's own settings, else global), the hub URL is saved
so 'scion hub status' and other hub commands use it. It is saved to global
settings, since credentials are global and project settings are often
tracked in git; it goes to the project's settings only when that project
already has hub settings, or with an explicit --global=false. An existing
hub.endpoint is never overwritten. If hub mode is off, an interactive login
offers to enable it (in the same settings); otherwise run 'scion hub enable'.

--hub-url and --hub accept a hub nickname ('scion hub list'). A hub URL
given with either flag that is not in the hub list yet is added to it after
a successful login, with a nickname derived from its host; it does not
become the current hub.

Example:
  scion hub auth login
  scion hub auth login --hub-url https://hub.example.com
  scion --hub https://hub.example.com hub auth login
  scion hub auth login --no-browser
  scion hub auth login --provider github`,
	RunE: runHubAuthLogin,
}

// hubAuthLogoutCmd clears stored credentials
var hubAuthLogoutCmd = &cobra.Command{
	Use:   "logout",
	Short: "Clear stored credentials",
	Long:  `Log out from the Hub by clearing locally stored credentials.`,
	RunE:  runHubAuthLogout,
}

func init() {
	hubCmd.AddCommand(hubAuthCmd)
	hubAuthCmd.AddCommand(hubAuthLoginCmd)
	hubAuthCmd.AddCommand(hubAuthLogoutCmd)

	// Flags for login command
	hubAuthLoginCmd.Flags().StringVar(&hubAuthHubURL, "hub-url", "", "Hub nickname or server URL; takes precedence over --hub, SCION_HUB_ENDPOINT, hub.endpoint in settings and SCION_HUB_URL")
	hubAuthLoginCmd.Flags().BoolVar(&hubAuthNoBrowser, "no-browser", false, "Use device flow instead of opening a browser")
	hubAuthLoginCmd.Flags().String("provider", "", "OAuth provider to use (google or github)")
}

func runHubAuthLogin(cmd *cobra.Command, args []string) error {
	// Resolve hub URL: --hub-url, --hub, SCION_HUB_ENDPOINT, settings, SCION_HUB_URL.
	// Both flags accept a hub nickname.
	hubURLFlag, err := hubAuthURLFlag(hubAuthHubURL)
	if err != nil {
		return err
	}
	hubURL := resolveHubAuthURL(hubURLFlag, hubEndpoint, os.Getenv, settingsHubEndpoint)
	if hubURL == "" {
		return fmt.Errorf("hub URL not specified, use --hub-url, --hub, SCION_HUB_ENDPOINT, hub.endpoint in settings or SCION_HUB_URL")
	}

	fmt.Printf("Authenticating with Hub at %s\n", hubURL)

	provider, err := cmd.Flags().GetString("provider")
	if err != nil {
		return fmt.Errorf("failed to read provider flag: %w", err)
	}
	tokenResp, err := hubAuthLoginFlow(cmd, hubURL, provider)
	if err != nil {
		return err
	}
	return finishHubAuthLogin(cmd, hubURL, tokenResp)
}

// hubAuthLoginFlow runs the OAuth login to the hub at hubURL: the browser
// flow, or the device flow with --no-browser or in a headless environment.
// It returns the token and stores nothing. extra adds client options, such
// as the transport auth for a hub the CLI picked itself.
func hubAuthLoginFlow(cmd *cobra.Command, hubURL, provider string, extra ...hubclient.Option) (*hubclient.CLITokenResponse, error) {
	// Create hub client (unauthenticated for initial OAuth)
	opts := append([]hubclient.Option{hubclient.WithTimeout(30 * time.Second)}, extra...)
	client, err := hubclient.New(hubURL, opts...)
	if err != nil {
		return nil, fmt.Errorf("failed to create Hub client: %w", err)
	}

	if hubAuthNoBrowser || util.IsHeadlessEnvironment() {
		provider, err := resolveExplicitDeviceFlowProvider(cmd.Context(), client.Auth(), provider)
		if err != nil {
			return nil, err
		}
		// Device authorization flow for headless environments
		deviceAuth := newDeviceFlowAuth(client.Auth(), provider)
		tokenResp, err := deviceAuth.Authenticate(cmd.Context())
		if err != nil {
			return nil, fmt.Errorf("device flow authentication failed: %w", err)
		}
		return tokenResp, nil
	}
	// Browser-based OAuth flow
	return runBrowserAuthFlow(cmd, client, provider)
}

// finishHubAuthLogin completes a successful login: it stores the
// credential, adds a URL given explicitly with --hub-url or --hub to the
// hub list, and records the endpoint in settings where that applies.
func finishHubAuthLogin(cmd *cobra.Command, hubURL string, tokenResp *hubclient.CLITokenResponse) error {
	if err := storeTokenAndPrintResult(hubURL, tokenResp); err != nil {
		return err
	}
	if loginHubURLGivenExplicitly() {
		registerLoginHub(cmd.OutOrStdout(), cmd.ErrOrStderr(), hubURL)
	}
	recordLoginEndpoint(cmd, hubURL)
	return nil
}

// loginHubURLGivenExplicitly reports whether 'hub auth login' got its hub
// as a URL in --hub-url or --hub (not as a nickname, and not from the
// environment, settings or the current context). Only such a URL is added
// to the hub list.
func loginHubURLGivenExplicitly() bool {
	if hubAuthHubURL != "" {
		return !isHubNickname(hubAuthHubURL)
	}
	return hubFlagEndpoint() != "" && hubFlagName == ""
}

// registerLoginHub adds a hub entry for hubURL, after a successful login,
// when no entry has that endpoint. The nickname is derived from the host
// and printed. It does not change the current hub. A failure is a warning:
// the login itself succeeded.
func registerLoginHub(out, stderr io.Writer, hubURL string) {
	if contextDisabled(resolveMode()) {
		return
	}
	if err := config.ValidateHubURL(hubURL); err != nil {
		_, _ = fmt.Fprintf(stderr, "Warning: could not add %s to the hub list: %v\n", hubURL, err)
		return
	}
	var name string
	var created bool
	err := config.UpdateCLIContext(func(c *config.CLIContext) error {
		name, created = c.EnsureHub(hubURL, useHubBaseName(hubURL))
		if !created {
			return errLoginHubKnown
		}
		return nil
	})
	switch {
	case errors.Is(err, errLoginHubKnown):
	case err != nil:
		_, _ = fmt.Fprintf(stderr, "Warning: could not add %s to the hub list: %v\n", hubURL, err)
	case created:
		printRegisteredHub(out, name, config.NormalizeEndpoint(hubURL))
	}
}

// errLoginHubKnown stops the context update of registerLoginHub when the
// hub already has an entry, so the file is not rewritten.
var errLoginHubKnown = errors.New("hub already registered")

// recordLoginEndpoint makes a successful login usable by the rest of the
// CLI. When the hub came from the current context ('scion use'; no
// --hub-url, --hub or environment variable chose it), or from the link
// entry of the directory project, the settings are left alone: the hub
// list already targets that hub, and writing it to the global hub.endpoint
// would make the workstation server advertise it. Otherwise
// persistLoginEndpointForInvocation saves the endpoint as before.
func recordLoginEndpoint(cmd *cobra.Command, hubURL string) {
	if hubAuthHubURL == "" && hubSource == targetSourceContext {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Logged in to %s (current hub, selected with 'scion use'); settings unchanged.\n", hubURL)
		return
	}
	// The hub of a linked directory comes from its link entry in the hub
	// list, so it is not written to settings either.
	if hubAuthHubURL == "" && hubSource == targetSourceDirectory {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Logged in to %s (the hub this directory is linked to); settings unchanged.\n", hubURL)
		return
	}
	persistLoginEndpointForInvocation(cmd, hubURL)
}

func newDeviceFlowAuth(authSvc hubclient.AuthService, provider string) *auth.DeviceFlowAuth {
	if strings.TrimSpace(provider) == "" {
		return auth.NewDeviceFlowAuth(authSvc)
	}
	return auth.NewDeviceFlowAuth(authSvc, provider)
}

func resolveExplicitDeviceFlowProvider(ctx context.Context, authSvc hubclient.AuthService, requestedProvider string) (string, error) {
	if strings.TrimSpace(requestedProvider) == "" {
		return "", nil
	}
	return resolveHubAuthProvider(ctx, authSvc, hubclient.OAuthClientTypeDevice, requestedProvider)
}

// runBrowserAuthFlow performs the browser-based OAuth flow.
func runBrowserAuthFlow(cmd *cobra.Command, client hubclient.Client, requestedProvider string) (*hubclient.CLITokenResponse, error) {
	provider, err := resolveHubAuthProvider(cmd.Context(), client.Auth(), hubclient.OAuthClientTypeCLI, requestedProvider)
	if err != nil {
		return nil, err
	}

	// Start localhost callback server
	authServer := auth.NewLocalhostAuthServer()
	callbackURL, state, err := authServer.Start(cmd.Context())
	if err != nil {
		return nil, fmt.Errorf("failed to start auth server: %w", err)
	}
	defer func() { _ = authServer.Shutdown() }()

	// Get OAuth URL from Hub
	authResp, err := client.Auth().GetAuthURL(cmd.Context(), callbackURL, state, provider)
	if err != nil {
		return nil, fmt.Errorf("failed to get auth URL: %w", err)
	}

	// Open browser
	fmt.Println("Opening browser for authentication...")
	if err := util.OpenBrowser(authResp.URL); err != nil {
		fmt.Printf("\nCould not open browser automatically.\n")
		fmt.Printf("Please open this URL in your browser:\n\n  %s\n\n", authResp.URL)
	}

	// Wait for callback
	fmt.Println("Waiting for authentication...")
	code, err := authServer.WaitForCode(cmd.Context())
	if err != nil {
		return nil, fmt.Errorf("authentication failed: %w", err)
	}

	// Exchange code for token
	tokenResp, err := client.Auth().ExchangeCode(cmd.Context(), code, callbackURL, provider)
	if err != nil {
		return nil, fmt.Errorf("failed to get token: %w", err)
	}

	return tokenResp, nil
}

// storeTokenAndPrintResult stores the token response as credentials and prints
// the login result to the terminal.
func storeTokenAndPrintResult(hubURL string, tokenResp *hubclient.CLITokenResponse) error {
	credToken := &credentials.TokenResponse{
		AccessToken:  tokenResp.AccessToken,
		RefreshToken: tokenResp.RefreshToken,
		ExpiresIn:    time.Duration(tokenResp.ExpiresIn) * time.Second,
	}
	if tokenResp.User != nil {
		credToken.User = &credentials.User{
			ID:          tokenResp.User.ID,
			Email:       tokenResp.User.Email,
			DisplayName: tokenResp.User.DisplayName,
			Role:        tokenResp.User.Role,
		}
	}

	if err := credentials.Store(hubURL, credToken); err != nil {
		return fmt.Errorf("failed to store credentials: %w", err)
	}

	fmt.Println("\nAuthentication successful!")
	if credToken.User != nil {
		if credToken.User.Role != "" {
			fmt.Printf("Logged in as: %s (%s) [%s]\n", credToken.User.DisplayName, credToken.User.Email, credToken.User.Role)
		} else {
			fmt.Printf("Logged in as: %s (%s)\n", credToken.User.DisplayName, credToken.User.Email)
		}
	}

	return nil
}

func runHubAuthLogout(cmd *cobra.Command, args []string) error {
	hubURL := getDefaultHubURL()
	if hubURL == "" {
		fmt.Println("Hub URL not configured. Nothing to logout from.")
		return nil
	}

	if err := credentials.Remove(hubURL); err != nil {
		return fmt.Errorf("failed to remove credentials: %w", err)
	}

	fmt.Println("Logged out successfully.")
	return nil
}

// getDefaultHubURL returns the hub URL logout uses: the root --hub flag,
// then SCION_HUB_ENDPOINT, then settings, then SCION_HUB_URL (see
// resolveHubAuthURL).
func getDefaultHubURL() string {
	return resolveHubAuthURL("", hubEndpoint, os.Getenv, settingsHubEndpoint)
}

func resolveHubAuthProvider(ctx context.Context, authSvc hubclient.AuthService, clientType hubclient.OAuthClientType, requestedProvider string) (string, error) {
	if requestedProvider != "" {
		provider := strings.ToLower(strings.TrimSpace(requestedProvider))
		if !hubclient.IsKnownOAuthProvider(provider) {
			return "", invalidHubAuthProviderError{provider: requestedProvider}
		}
		return provider, nil
	}

	resp, err := authSvc.GetAuthProviders(ctx, string(clientType))
	if err != nil {
		return "", fmt.Errorf("failed to discover OAuth providers: %w", err)
	}

	switch len(resp.Providers) {
	case 0:
		return "", noHubAuthProvidersConfiguredError{clientType: clientType}
	case 1:
		return resp.Providers[0], nil
	default:
		return "", multipleHubAuthProvidersConfiguredError{clientType: clientType, providers: resp.Providers}
	}
}
