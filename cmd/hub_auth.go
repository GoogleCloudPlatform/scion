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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
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
	hubAuthHubURL string
	// hubAuthNoBrowser is a test seam for forcing device-flow login when
	// --no-browser is not explicitly set on cmd (including commands without
	// that flag, such as 'scion use').
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
var hubAuthLoginCmd = newHubLoginCommand("login")

// newHubLoginCommand builds a login command ('scion hub auth login' or the
// top-level 'scion login' alias) sharing runHubAuthLogin and the --no-browser
// and --provider flags. The deprecated --hub-url flag is attached only to
// hubAuthLoginCmd.
func newHubLoginCommand(use string) *cobra.Command {
	c := &cobra.Command{
		Use:   use,
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
  scion login
  scion hub auth login
  scion --hub https://hub.example.com login
  scion --hub prod login
  scion login --no-browser
  scion login --provider github`,
		RunE: runHubAuthLogin,
	}
	c.Flags().Bool("no-browser", false, "Use device flow instead of opening a browser")
	c.Flags().String("provider", "", "OAuth provider to use (google or github)")
	return c
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

	// Hidden deprecated --hub-url stays only on 'scion hub auth login' (not
	// carried onto the top-level 'scion login' alias); pflag prints the
	// deprecation on stderr when it is used.
	hubAuthLoginCmd.Long = strings.Replace(
		hubAuthLoginCmd.Long,
		hubAuthURLPrecedence,
		hubAuthURLPrecedence+" --hub-url is a deprecated alias of --hub.",
		1,
	)
	hubAuthLoginCmd.Flags().StringVar(&hubAuthHubURL, "hub-url", "", "Deprecated alias of --hub")
	_ = hubAuthLoginCmd.Flags().MarkDeprecated("hub-url", "use --hub")
}

// loginHubURLFlagValue returns the deprecated --hub-url flag value when cmd
// defines it (i.e. 'scion hub auth login'), and "" otherwise (e.g. top-level
// 'scion login').
func loginHubURLFlagValue(cmd *cobra.Command) string {
	if cmd == nil {
		return ""
	}
	v, _ := cmd.Flags().GetString("hub-url")
	return v
}

// loginNoBrowser reports whether the login flow should skip opening a browser.
// If cmd defines --no-browser and the caller explicitly set it, that flag value
// wins; otherwise it falls back to the hubAuthNoBrowser default (used by tests
// and by callers whose command has no --no-browser flag, such as 'scion use').
func loginNoBrowser(cmd *cobra.Command) bool {
	if cmd != nil {
		if f := cmd.Flags().Lookup("no-browser"); f != nil && f.Changed {
			if v, err := cmd.Flags().GetBool("no-browser"); err == nil {
				return v
			}
		}
	}
	return hubAuthNoBrowser
}

func runHubAuthLogin(cmd *cobra.Command, args []string) error {
	// The hub of this invocation (see hubAuthURLPrecedence); the
	// deprecated --hub-url wins when present on this command, and like
	// --hub accepts a nickname.
	hubURL, err := hubAuthURL(loginHubURLFlagValue(cmd))
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

	if loginNoBrowser(cmd) || util.IsHeadlessEnvironment() {
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
	return runBrowserAuthFlow(cmd, client, hubURL, provider)
}

// finishHubAuthLogin completes a successful login: it stores the
// credential and then updates the hub list (recordLoginHub). No settings
// file is written.
func finishHubAuthLogin(cmd *cobra.Command, hubURL string, tokenResp *hubclient.CLITokenResponse) error {
	if err := storeTokenAndPrintResult(cmd.OutOrStdout(), hubURL, tokenResp); err != nil {
		return err
	}
	recordLoginHubWithFlag(cmd.OutOrStdout(), cmd.ErrOrStderr(), hubURL, loginHubURLFlagValue(cmd))
	return nil
}

// loginHubURLGivenExplicitly is a test seam delegating to
// loginHubURLGivenExplicitlyFor with the package-level hubAuthHubURL.
func loginHubURLGivenExplicitly() bool {
	return loginHubURLGivenExplicitlyFor(hubAuthHubURL)
}

func loginHubURLGivenExplicitlyFor(hubURLFlag string) bool {
	if hubURLFlag != "" {
		return !isHubNickname(hubURLFlag)
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
//
// recordLoginHub is a test seam delegating to recordLoginHubWithFlag with the
// package-level hubAuthHubURL.
func recordLoginHub(out, stderr io.Writer, hubURL string) {
	recordLoginHubWithFlag(out, stderr, hubURL, hubAuthHubURL)
}

func recordLoginHubWithFlag(out, stderr io.Writer, hubURL, hubURLFlag string) {
	if contextDisabled(resolveMode()) {
		return
	}
	switch {
	case hubURLFlag == "" && hubSource == targetSourceContext:
		_, _ = fmt.Fprintf(out, "Logged in to %s (current hub, selected with 'scion use').\n", config.RedactURL(hubURL))
		return
	case hubURLFlag == "" && hubSource == targetSourceDirectory:
		_, _ = fmt.Fprintf(out, "Logged in to %s (the hub this directory is linked to).\n", config.RedactURL(hubURL))
		printLoginHubSelected(out, stderr, hubURL)
		return
	case hubURLFlag == "" && hubFlagEndpoint() == "":
		return
	}
	if loginHubURLGivenExplicitlyFor(hubURLFlag) {
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

const callbackBrandingFetchTimeout = 1500 * time.Millisecond
const maxCallbackBrandingBodyBytes = 256 << 10

// fetchHubCallbackBranding performs a best-effort unauthenticated GET to
// /api/v1/settings/public with a 1.5s timeout to theme the localhost OAuth
// callback page. Any network, timeout, HTTP, or parse error silently falls
// back to nil (stock Scion defaults).
func fetchHubCallbackBranding(ctx context.Context, hubURL string) *auth.CallbackBranding {
	base := strings.TrimRight(strings.TrimSpace(hubURL), "/")
	if base == "" {
		return nil
	}
	fetchCtx, cancel := context.WithTimeout(ctx, callbackBrandingFetchTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(fetchCtx, http.MethodGet, base+"/api/v1/settings/public", nil)
	if err != nil {
		return nil
	}
	req.Header.Set("Accept", "application/json")

	httpClient := &http.Client{
		Timeout: callbackBrandingFetchTimeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 2 {
				return http.ErrUseLastResponse
			}
			return nil
		},
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil
	}

	var payload struct {
		Branding *config.PublicBranding `json:"branding"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxCallbackBrandingBodyBytes)).Decode(&payload); err != nil {
		return nil
	}
	if payload.Branding == nil {
		return nil
	}
	b := payload.Branding
	cb := &auth.CallbackBranding{
		Name:        b.Name,
		Attribution: b.Attribution,
		Corners:     b.Corners,
		FontSans:    b.FontSans,
		LogoLight:   resolveCallbackLogoURL(base, b.LogoLight),
		LogoDark:    resolveCallbackLogoURL(base, b.LogoDark),
	}
	if b.Light != nil {
		cb.Light = auth.CallbackColors{
			Bg:        b.Light.Bg,
			Surface:   b.Light.Surface,
			Text:      b.Light.Text,
			TextMuted: b.Light.TextMuted,
			Border:    b.Light.Border,
			Accent:    b.Light.Primary,
		}
	}
	if b.Dark != nil {
		cb.Dark = auth.CallbackColors{
			Bg:        b.Dark.Bg,
			Surface:   b.Dark.Surface,
			Text:      b.Dark.Text,
			TextMuted: b.Dark.TextMuted,
			Border:    b.Dark.Border,
			Accent:    b.Dark.Primary,
		}
	}
	return cb
}

func resolveCallbackLogoURL(hubBaseURL, raw string) string {
	if raw == "" {
		return ""
	}
	if strings.HasPrefix(raw, "/") && !strings.HasPrefix(raw, "//") {
		return hubBaseURL + raw
	}
	return raw
}

// runBrowserAuthFlow performs the browser-based OAuth flow.
func runBrowserAuthFlow(cmd *cobra.Command, client hubclient.Client, hubURL, requestedProvider string) (*hubclient.CLITokenResponse, error) {
	provider, err := resolveHubAuthProvider(cmd.Context(), client.Auth(), hubclient.OAuthClientTypeCLI, requestedProvider)
	if err != nil {
		return nil, err
	}

	// Best-effort branding fetch for the localhost callback page (1.5s cap).
	callbackBranding := fetchHubCallbackBranding(cmd.Context(), hubURL)

	// Start localhost callback server
	authServer := auth.NewLocalhostAuthServer().WithBranding(callbackBranding)
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
