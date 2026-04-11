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
	"fmt"
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

Example:
  scion hub auth login
  scion hub auth login --hub-url https://hub.example.com
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
	hubAuthLoginCmd.Flags().StringVar(&hubAuthHubURL, "hub-url", "", "Hub server URL (defaults to configured endpoint)")
	hubAuthLoginCmd.Flags().BoolVar(&hubAuthNoBrowser, "no-browser", false, "Use device flow instead of opening a browser")
	hubAuthLoginCmd.Flags().String("provider", "", "OAuth provider to use (google or github)")
}

func runHubAuthLogin(cmd *cobra.Command, args []string) error {
	// Resolve hub URL
	hubURL := hubAuthHubURL
	if hubURL == "" {
		hubURL = getDefaultHubURL()
	}
	if hubURL == "" {
		return fmt.Errorf("Hub URL not specified. Use --hub-url or configure hub.endpoint in settings")
	}

	fmt.Printf("Authenticating with Hub at %s\n", hubURL)

	// Create hub client (unauthenticated for initial OAuth)
	client, err := hubclient.New(hubURL, hubclient.WithTimeout(30*time.Second))
	if err != nil {
		return fmt.Errorf("failed to create Hub client: %w", err)
	}

	var tokenResp *hubclient.CLITokenResponse
	provider, err := cmd.Flags().GetString("provider")
	if err != nil {
		return fmt.Errorf("failed to read provider flag: %w", err)
	}

	if hubAuthNoBrowser || util.IsHeadlessEnvironment() {
		provider, err := resolveExplicitDeviceFlowProvider(cmd.Context(), client.Auth(), provider)
		if err != nil {
			return err
		}
		// Device authorization flow for headless environments
		deviceAuth := newDeviceFlowAuth(client.Auth(), provider)
		tokenResp, err = deviceAuth.Authenticate(cmd.Context())
		if err != nil {
			return fmt.Errorf("device flow authentication failed: %w", err)
		}
	} else {
		// Browser-based OAuth flow
		tokenResp, err = runBrowserAuthFlow(cmd, client, provider)
		if err != nil {
			return err
		}
	}

	return storeTokenAndPrintResult(hubURL, tokenResp)
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
	defer authServer.Shutdown()

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

// getDefaultHubURL returns the default Hub URL from settings or environment.
func getDefaultHubURL() string {
	// Check environment first
	if env := os.Getenv("SCION_HUB_ENDPOINT"); env != "" {
		return env
	}

	// Try to load from settings
	grovePath, _, err := config.ResolveGrovePath("")
	if err != nil {
		return ""
	}

	settings, err := config.LoadSettings(grovePath)
	if err != nil {
		return ""
	}

	return settings.GetHubEndpoint()
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
