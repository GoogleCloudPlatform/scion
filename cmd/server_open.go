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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/daemon"
	"github.com/GoogleCloudPlatform/scion/pkg/hubsync"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// devLaunchPath is the web server route that mints a dev sign-in code
// (pkg/hub web_devsignin.go).
const devLaunchPath = "/auth/dev-launch"

// devSignInLink is a single-use link that signs the browser in to the web UI
// of a workstation server running with dev auth.
type devSignInLink struct {
	URL       string
	ExpiresAt time.Time
}

// errDevSignInUnavailable means the server does not hand out dev sign-in
// links (dev auth is off).
var errDevSignInUnavailable = errors.New("this server does not use dev sign-in links (dev auth is off)")

// requestDevSignInLink asks the web server at baseURL for a sign-in link,
// authenticating with the dev token. next is the local path the browser
// lands on after signing in ("" for the dashboard).
func requestDevSignInLink(client *http.Client, baseURL, token, next string) (devSignInLink, error) {
	if token == "" {
		return devSignInLink{}, errors.New("no dev token found")
	}
	req, err := http.NewRequest(http.MethodPost, strings.TrimSuffix(baseURL, "/")+devLaunchPath, nil)
	if err != nil {
		return devSignInLink{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req)
	if err != nil {
		return devSignInLink{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return devSignInLink{}, errDevSignInUnavailable
	case http.StatusUnauthorized:
		return devSignInLink{}, errors.New("the server refused the dev token")
	default:
		return devSignInLink{}, fmt.Errorf("the server answered %s", resp.Status)
	}
	var body struct {
		SignInPath string    `json:"signInPath"`
		ExpiresAt  time.Time `json:"expiresAt"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&body); err != nil {
		return devSignInLink{}, fmt.Errorf("unreadable answer: %w", err)
	}
	if !strings.HasPrefix(body.SignInPath, "/") {
		return devSignInLink{}, errors.New("unexpected answer from the server")
	}
	link := strings.TrimSuffix(baseURL, "/") + body.SignInPath
	if next != "" && next != "/" {
		link += "&" + url.Values{"next": {next}}.Encode()
	}
	return devSignInLink{URL: link, ExpiresAt: body.ExpiresAt}, nil
}

// expiresInText describes when a sign-in link stops working.
func (l devSignInLink) expiresInText() string {
	if l.ExpiresAt.IsZero() {
		return "single use"
	}
	d := time.Until(l.ExpiresAt).Round(time.Minute)
	if d < time.Minute {
		return "single use, expires in under a minute"
	}
	return fmt.Sprintf("single use, expires in %d minutes", int(d.Minutes()))
}

// serverDevToken returns the dev token a server with settings cfg uses: the
// settings token if one is set, else the token file's contents ("" if
// none).
func serverDevToken(cfg *config.GlobalConfig, globalDir string) string {
	if cfg == nil {
		cfg = &config.GlobalConfig{}
	}
	if cfg.Auth.Token != "" {
		return cfg.Auth.Token
	}
	return readDevTokenFile(devTokenFilePath(cfg.Auth.TokenFile, globalDir))
}

// readServerDevToken reads the server's dev token (serverDevToken); a test
// seam, so tests can check that a refused request never reads it.
var readServerDevToken = serverDevToken

// stdoutIsTerminal reports whether stdout is a terminal. A sign-in link is
// printed only to a terminal (or on an explicit --print), so a redirected
// stdout or a log does not record the single-use code.
func stdoutIsTerminal() bool {
	return term.IsTerminal(int(os.Stdout.Fd()))
}

// Test seams for runServerOpen.
var (
	serverOpenStdoutIsTerminal = stdoutIsTerminal
	serverOpenBrowserAllowed   = browserAutoOpenAllowed
	serverOpenBrowser          = func(u string) error { return quickstartOpenBrowser(u) }
	serverOpenWebBase          = func(globalDir string, running bool) string {
		webBase, _, _ := serverStatusBaseURLs(globalDir, running)
		return webBase
	}
)

// serverOpenRequestLink sends the server's dev token to baseURL. It does not
// apply the transport rule itself; runServerOpen checks devSignInAllowed
// first (the hub constructor source scan in hub_transport_policy_test.go
// keeps it so).
var serverOpenRequestLink = func(baseURL, token, next string) (devSignInLink, error) {
	return requestDevSignInLink(&http.Client{Timeout: 5 * time.Second}, baseURL, token, next)
}

// devSignInAllowed applies the hub transport rule to a sign-in link request,
// which carries the server's dev token: it may be sent to baseURL only over
// a loopback or TLS connection. It returns the refusal, or nil.
func devSignInAllowed(baseURL string) error {
	if err := hubsync.CheckUserHubTransport(baseURL); err != nil {
		return fmt.Errorf("a sign-in link is requested with the server's dev token, so only over a loopback or TLS connection: %w", err)
	}
	return nil
}

var serverOpenPrintOnly bool

// errNoTerminalForLink is returned when the link would have to be printed
// to a stdout that is not a terminal without --print.
var errNoTerminalForLink = errors.New("stdout is not a terminal, so the single-use sign-in link is not printed; run 'scion server open --print' to print it")

var serverOpenCmd = &cobra.Command{
	Use:   "open",
	Short: "Open the web UI of the local workstation server, signed in",
	Long: `Open the web UI of the local workstation server in your browser, signed in.

A workstation server (dev auth) signs a browser in only from a sign-in link:
a single-use link that stops working after a couple of minutes. 'scion server
start' opens one when the server starts; run this command whenever you need a
new one, for example in another browser or after your session expired.

The link is created with the server's dev token, so only someone who can
read that token can sign in. The link is printed only to a terminal, or
with --print.

Examples:
  # Open the web UI, signed in
  scion server open

  # Print a sign-in link instead of opening a browser
  scion server open --print`,
	Args: cobra.NoArgs,
	RunE: runServerOpen,
}

func init() {
	serverCmd.AddCommand(serverOpenCmd)
	serverOpenCmd.Flags().BoolVar(&serverOpenPrintOnly, "print", false, "Print the sign-in link without opening a browser (also when stdout is not a terminal)")
}

func runServerOpen(cmd *cobra.Command, _ []string) error {
	globalDir, err := config.GetGlobalDir()
	if err != nil {
		return fmt.Errorf("failed to get global directory: %w", err)
	}
	running, _, _ := daemon.StatusComponent(serverDaemonComponent, globalDir)
	webBase := serverOpenWebBase(globalDir, running)
	// The transport rule, before the token is read: the dev token is sent
	// only over a loopback or TLS connection.
	if err := devSignInAllowed(webBase); err != nil {
		return err
	}
	token := readServerDevToken(serverOpenConfig(globalDir, running), globalDir)
	if token == "" {
		return errors.New("no dev token found for the local server; sign-in links need the workstation server's dev token (start it with 'scion server start')")
	}
	tty := serverOpenStdoutIsTerminal()
	// Without a terminal, --print or a browser there is nowhere to put a
	// link: refuse before minting one.
	if !tty && !serverOpenPrintOnly && !serverOpenBrowserAllowed() {
		return errNoTerminalForLink
	}
	link, err := serverOpenRequestLink(webBase, token, "")
	if err != nil {
		if errors.Is(err, errDevSignInUnavailable) {
			return fmt.Errorf("%w; sign in at %s", err, webBase)
		}
		return fmt.Errorf("could not get a sign-in link from %s: %w (is the server running? see 'scion server status')", webBase, err)
	}
	out := cmd.OutOrStdout()
	if !serverOpenPrintOnly && serverOpenBrowserAllowed() {
		if err := serverOpenBrowser(link.URL); err == nil {
			if tty {
				_, _ = fmt.Fprintf(out, "Opened the web UI in your browser. If it did not open, use this link (%s):\n  %s\n", link.expiresInText(), link.URL)
			} else {
				_, _ = fmt.Fprintln(out, "Opened the web UI in your browser.")
			}
			return nil
		}
	}
	// Without a terminal the link is printed only when asked for.
	if !tty && !serverOpenPrintOnly {
		return errNoTerminalForLink
	}
	_, _ = fmt.Fprintf(out, "Sign-in link (%s):\n  %s\n", link.expiresInText(), link.URL)
	return nil
}

// serverOpenConfig returns the settings of the local server, read as
// serverStatusBaseURLs reads them: from the config its saved start args
// name while the daemon runs, else the default config. nil if they cannot
// be loaded.
func serverOpenConfig(globalDir string, daemonRunning bool) *config.GlobalConfig {
	var saved []string
	if daemonRunning {
		if args, err := daemon.LoadArgs(serverDaemonComponent, globalDir); err == nil {
			saved = args
		}
	}
	cfg, err := config.LoadGlobalConfig(serverConfigPathFromArgs(saved))
	if err != nil {
		return nil
	}
	return cfg
}
