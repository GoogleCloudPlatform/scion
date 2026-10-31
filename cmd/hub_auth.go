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

A login never changes settings files. Credentials are stored in
~/.scion/credentials.json, keyed by the hub URL.

--hub accepts a hub nickname ('scion hub list'). A hub URL given with
--hub that is not in the hub list yet is added to it after a successful
login, with a nickname derived from its host. When no hub is current yet,
the hub logged in to becomes the current hub (not one taken from
SCION_HUB_ENDPOINT or SCION_HUB_URL).
A login never switches a hub that is already current.
To make hub <name> the current hub, run 'scion use --hub <name>'.

Example:
  scion hub auth login
  scion --hub https://hub.example.com hub auth login
  scion --hub prod hub auth login
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
	hubAuthLoginCmd.Flags().StringVar(&hubAuthHubURL, "hub-url", "", "Deprecated alias of --hub")
	// --hub-url is kept for one release as a hidden alias of --hub; pflag
	// prints the deprecation on stderr when it is used.
	_ = hubAuthLoginCmd.Flags().MarkDeprecated("hub-url", "use --hub")
	hubAuthLoginCmd.Flags().BoolVar(&hubAuthNoBrowser, "no-browser", false, "Use device flow instead of opening a browser")
	hubAuthLoginCmd.Flags().String("provider", "", "OAuth provider to use (google or github)")
}

func runHubAuthLogin(cmd *cobra.Command, args []string) error {
	// The hub of this invocation (see hubAuthURLPrecedence); the
	// deprecated --hub-url wins, and like --hub accepts a nickname.
	hubURL, err := hubAuthURL(hubAuthHubURL)
	if err != nil {
		return err
	}
	if hubURL == "" {
		return errHubNotConfigured
	}

	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Authenticating with Hub at %s\n", config.RedactURL(hubURL))

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
	// The login receives the user's new token: the transport rule applies
	// before the flow starts.
	client, err := newUserHubClient(hubURL, opts...)
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
// credential and then updates the hub list (recordLoginHub). No settings
// file is written.
func finishHubAuthLogin(cmd *cobra.Command, hubURL string, tokenResp *hubclient.CLITokenResponse) error {
	if err := storeTokenAndPrintResult(cmd.OutOrStdout(), hubURL, tokenResp); err != nil {
		return err
	}
	recordLoginHub(cmd.OutOrStdout(), cmd.ErrOrStderr(), hubURL)
	return nil
}

// loginHubURLGivenExplicitly reports whether 'hub auth login' got its hub
// as a URL in --hub-url or --hub (not as a nickname, and not from the
// environment, a link entry or the current context). Only such a URL is
// added to the hub list.
func loginHubURLGivenExplicitly() bool {
	if hubAuthHubURL != "" {
		return !isHubNickname(hubAuthHubURL)
	}
	return hubFlagEndpoint() != "" && hubFlagName == ""
}

// registerHubInList adds a hub entry for hubURL, after a successful
// command against that hub (today: a login), when no entry has that
// endpoint. The nickname is derived from the host and printed. It does not
// change the current hub. A failure is a warning: the command itself
// succeeded. Inside an agent the hub list is not used.
func registerHubInList(out, stderr io.Writer, hubURL string) {
	if contextDisabled(resolveMode()) {
		return
	}
	if err := config.ValidateHubURL(hubURL); err != nil {
		_, _ = fmt.Fprintf(stderr, "Warning: could not add %s to the hub list: %s\n", config.RedactURL(hubURL), config.RedactURLsIn(err.Error()))
		return
	}
	var name string
	var created bool
	err := config.UpdateCLIContext(func(c *config.CLIContext) error {
		name, created = c.EnsureHub(hubURL, useHubBaseName(hubURL))
		if !created {
			return errHubAlreadyListed
		}
		return nil
	})
	switch {
	case errors.Is(err, errHubAlreadyListed):
	case err != nil:
		_, _ = fmt.Fprintf(stderr, "Warning: could not add %s to the hub list: %v\n", config.RedactURL(hubURL), err)
	case created:
		printRegisteredHub(out, name, config.NormalizeEndpoint(hubURL))
	}
}

// errHubAlreadyListed stops the context update of registerHubInList when
// the hub already has an entry, so the file is not rewritten.
var errHubAlreadyListed = errors.New("hub already registered")

// recordLoginHub is everything a successful login does with the hub list.
// It never writes settings: the hub list is the CLI's only hub
// configuration.
//
//   - The current hub ('scion use') or the hub of the linked directory:
//     one line saying which hub that was. The hub of the linked directory
//     becomes the current hub when none is (selectLoginHubIfNoneCurrent),
//     and one more line says so.
//   - A URL given with --hub or --hub-url: added to the hub list when it
//     has no entry (registerHubInList).
//   - A hub given with --hub or --hub-url (URL or nickname): when no hub
//     is current, it becomes the current hub (selectLoginHubIfNoneCurrent)
//     and one line says so. A current hub is never switched; when the hub
//     is not the current hub, a hint says how to select it.
//   - A hub from the environment: nothing (it is a one-off override).
func recordLoginHub(out, stderr io.Writer, hubURL string) {
	if contextDisabled(resolveMode()) {
		return
	}
	switch {
	case hubAuthHubURL == "" && hubSource == targetSourceContext:
		_, _ = fmt.Fprintf(out, "Logged in to %s (current hub, selected with 'scion use').\n", config.RedactURL(hubURL))
		return
	case hubAuthHubURL == "" && hubSource == targetSourceDirectory:
		_, _ = fmt.Fprintf(out, "Logged in to %s (the hub this directory is linked to).\n", config.RedactURL(hubURL))
		printLoginHubSelected(out, stderr, hubURL)
		return
	case hubAuthHubURL == "" && hubFlagEndpoint() == "":
		return
	}
	if loginHubURLGivenExplicitly() {
		registerHubInList(out, stderr, hubURL)
	}
	if printLoginHubSelected(out, stderr, hubURL) {
		return
	}
	c, err := config.LoadCLIContext()
	if err != nil {
		return
	}
	if name, h := c.HubByEndpoint(hubURL); h != nil && name != c.CurrentHub {
		_, _ = fmt.Fprintf(out, "To make hub '%s' the current hub, run 'scion use --hub %s'.\n", name, name)
	}
}

// printLoginHubSelected makes the hub at hubURL the current hub when none
// is (selectLoginHubIfNoneCurrent) and prints one line when it did.
func printLoginHubSelected(out, stderr io.Writer, hubURL string) bool {
	name, endpoint, ok := selectLoginHubIfNoneCurrent(stderr, hubURL)
	if ok {
		_, _ = fmt.Fprintf(out, "Hub '%s' (%s) is now the current hub.\n", name, config.RedactURL(endpoint))
	}
	return ok
}

// selectLoginHubIfNoneCurrent makes the hub list entry for hubURL the
// current hub when no hub is current, and returns the entry's name and
// endpoint. It changes nothing, and returns false, when a hub is already
// current or hubURL has no entry (for example when registerHubInList
// refused it). A failure to update the hub list is a warning: the login
// itself succeeded. A read-only look comes first, so a hub that is not
// listed takes no lock and an unreadable file is not reported twice.
func selectLoginHubIfNoneCurrent(stderr io.Writer, hubURL string) (name, endpoint string, ok bool) {
	listed, err := config.LoadCLIContext()
	if err != nil {
		return "", "", false
	}
	if _, h := listed.HubByEndpoint(hubURL); h == nil {
		return "", "", false
	}
	err = config.UpdateCLIContext(func(c *config.CLIContext) error {
		if c.CurrentHub != "" {
			return errLoginHubNotSelected
		}
		n, h := c.HubByEndpoint(hubURL)
		if h == nil {
			return errLoginHubNotSelected
		}
		name, endpoint = n, h.Endpoint
		c.CurrentHub = n
		return nil
	})
	switch {
	case errors.Is(err, errLoginHubNotSelected):
		return "", "", false
	case err != nil:
		_, _ = fmt.Fprintf(stderr, "Warning: could not make %s the current hub: %v\n", config.RedactURL(hubURL), err)
		return "", "", false
	}
	return name, endpoint, true
}

// errLoginHubNotSelected stops the context update of
// selectLoginHubIfNoneCurrent when there is nothing to change, so the file
// is not rewritten.
var errLoginHubNotSelected = errors.New("login hub not selected")

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
	out := cmd.OutOrStdout()
	_, _ = fmt.Fprintln(out, "Opening browser for authentication...")
	if err := util.OpenBrowser(authResp.URL); err != nil {
		_, _ = fmt.Fprintf(out, "\nCould not open browser automatically.\n")
		_, _ = fmt.Fprintf(out, "Please open this URL in your browser:\n\n  %s\n\n", authResp.URL)
	}

	// Wait for callback
	_, _ = fmt.Fprintln(out, "Waiting for authentication...")
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
// the login result to out.
func storeTokenAndPrintResult(out io.Writer, hubURL string, tokenResp *hubclient.CLITokenResponse) error {
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

	_, _ = fmt.Fprintln(out, "\nAuthentication successful!")
	if credToken.User != nil {
		if credToken.User.Role != "" {
			_, _ = fmt.Fprintf(out, "Logged in as: %s (%s) [%s]\n", credToken.User.DisplayName, credToken.User.Email, credToken.User.Role)
		} else {
			_, _ = fmt.Fprintf(out, "Logged in as: %s (%s)\n", credToken.User.DisplayName, credToken.User.Email)
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

// getDefaultHubURL returns the hub URL logout uses: the hub of this
// invocation (see hubAuthURLPrecedence).
func getDefaultHubURL() string {
	return invocationHubEndpoint()
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
