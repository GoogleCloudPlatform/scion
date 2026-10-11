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
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/credentials"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/transportauth"
	"github.com/GoogleCloudPlatform/scion/pkg/transportauth/adcsource"
	"github.com/GoogleCloudPlatform/scion/pkg/tunnelclient"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
	"github.com/GoogleCloudPlatform/scion/pkg/version"
	"github.com/spf13/cobra"
)

var (
	tunnelPort      int
	tunnelLocalPort int
	tunnelStdio     bool
	tunnelSSH       bool
)

var tunnelCmd = &cobra.Command{
	Use:   "tunnel <agent> (--port N [--local-port L] | --ssh) [--stdio]",
	Short: "Open a tunnel to a port inside an agent through the Hub",
	Long: `Open a tunnel to a port inside an agent through the Hub.

By default the tunnel listens on 127.0.0.1 (a random port, or --local-port)
and forwards each connection to the agent's loopback port. With --stdio the
tunnel carries bytes on stdin and stdout instead, for use as an ssh
ProxyCommand. --ssh asks for the agent's ssh service instead of a port.

The command never prompts. --hub and --project make it independent of the
working directory. Exit codes:
  0 success, 1 other error, 2 usage error, 3 no usable credential or identity
  changed, 4 agent not found, 5 not permitted, 6 hub unreachable or
  unavailable, 7 tunnel closed by policy, 8 tunnel limit reached, 9 agent not
  running, 10 agent unreachable, 11 hub does not support tunnels,
  130 interrupted, 143 terminated.`,
	Example: `  scion tunnel my-agent --port 5173
  scion tunnel my-agent --port 5173 --local-port 8080
  scion tunnel --stdio --ssh --hub https://hub.example.com --project my-project my-agent`,
	Args: func(cmd *cobra.Command, args []string) error {
		if len(args) != 1 {
			return tunnelclient.Errorf(tunnelclient.ExitUsage, "expected exactly one agent, got %d arguments", len(args))
		}
		return nil
	},
	RunE: runTunnel,
}

func init() {
	tunnelCmd.Flags().IntVar(&tunnelPort, "port", 0, "Agent-local TCP port to reach")
	tunnelCmd.Flags().IntVar(&tunnelLocalPort, "local-port", 0, "Local port to listen on (default: a random free port)")
	tunnelCmd.Flags().BoolVar(&tunnelStdio, "stdio", false, "Carry the tunnel on stdin/stdout instead of a local listener")
	tunnelCmd.Flags().BoolVar(&tunnelSSH, "ssh", false, "Reach the agent's ssh service instead of a port")
	tunnelCmd.SetFlagErrorFunc(tunnelFlagError)
	rootCmd.AddCommand(tunnelCmd)
}

// tunnelFlagError marks a flag parse error on the tunnel commands as a
// usage error (exit 2).
func tunnelFlagError(_ *cobra.Command, err error) error {
	return &tunnelclient.Error{Code: tunnelclient.ExitUsage, Msg: err.Error()}
}

// reportPlainError writes a failed command's error as one plain line (no
// colour, no usage block) and returns its exit code: the code of a
// *tunnelclient.Error, otherwise 1. Flag and argument errors are marked
// with exit 2 where they are raised (tunnelFlagError and the commands'
// Args validators).
func reportPlainError(w io.Writer, err error) int {
	code := tunnelclient.ExitError
	var te *tunnelclient.Error
	if errors.As(err, &te) {
		code = te.Code
	}
	_, _ = fmt.Fprintf(w, "Error: %s\n", plainErrorLine(err.Error()))
	return code
}

// plainErrorLine removes ANSI sequences and folds the message onto one line.
func plainErrorLine(s string) string {
	s = util.StripANSI(s)
	return strings.Join(strings.Fields(s), " ")
}

// tunnelSettings loads the settings that supply the hub endpoint and
// transport: the global settings when --project names a hub project,
// otherwise the current directory's project (or the global settings).
// Nothing is created, registered or synced.
func tunnelSettings() *config.Settings {
	path := "global"
	if projectPath == "" && !globalMode {
		if p, _, err := config.ResolveProjectPath(""); err == nil {
			path = p
		}
	}
	settings, err := config.LoadSettings(path)
	if err != nil {
		return nil
	}
	return settings
}

// tunnelEndpoint resolves the hub endpoint through GetHubEndpoint. A nil
// settings is passed as an untyped nil, so a failed settings load falls
// through to the flag and environment.
func tunnelEndpoint(settings *config.Settings) string {
	if settings == nil {
		return GetHubEndpoint(nil)
	}
	return GetHubEndpoint(settings)
}

// tunnelTransportSource is the transport resolver (a test seam).
var tunnelTransportSource = func(ts *transportauth.TransportSettings) (transportauth.TokenSource, transportauth.HeaderMode, error) {
	return transportauth.FromSettings(ts, adcsource.New)
}

// tunnelCredential resolves the credential through the CLI's WebSocket
// credential chain (getHubAccessToken) and labels its kind. The lookup
// uses the endpoint exactly as given.
func tunnelCredential(endpoint string) tunnelclient.Credential {
	tok := getHubAccessToken(endpoint)
	switch {
	case tok == "":
		return tunnelclient.Credential{}
	case tok == credentials.GetAccessToken(endpoint):
		return tunnelclient.Credential{Token: tok, Kind: tunnelclient.CredentialOAuth}
	case tok == os.Getenv("SCION_HUB_TOKEN"):
		return tunnelclient.Credential{Token: tok, Kind: tunnelclient.CredentialToken}
	default:
		return tunnelclient.Credential{Token: tok, Kind: tunnelclient.CredentialDev}
	}
}

// tunnelHub is the REST side of a tunnel, over the hub client.
type tunnelHub struct {
	endpoint string
	src      transportauth.TokenSource
	mode     transportauth.HeaderMode
}

func (h *tunnelHub) client(cred tunnelclient.Credential) (hubclient.Client, error) {
	opts := []hubclient.Option{hubclient.WithBearerToken(cred.Token)}
	if h.src != nil {
		opts = append(opts, hubclient.WithTransportAuth(h.src, h.mode))
	}
	return hubclient.New(h.endpoint, opts...)
}

// Whoami implements tunnelclient.Hub.
func (h *tunnelHub) Whoami(ctx context.Context, cred tunnelclient.Credential) (tunnelclient.User, error) {
	c, err := h.client(cred)
	if err != nil {
		return tunnelclient.User{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	u, err := c.Auth().Me(ctx)
	if err != nil {
		return tunnelclient.User{}, tunnelHTTPError(err)
	}
	return tunnelclient.User{ID: u.ID, Email: u.Email}, nil
}

// ResolveAgent implements tunnelclient.Hub: the project by slug (or id),
// then the agent by slug (or id) within it.
func (h *tunnelHub) ResolveAgent(ctx context.Context, cred tunnelclient.Credential, project, agent string) (tunnelclient.Target, error) {
	c, err := h.client(cred)
	if err != nil {
		return tunnelclient.Target{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	projectID := ""
	list, err := c.Projects().List(ctx, &hubclient.ListProjectsOptions{Slug: project})
	if err != nil {
		return tunnelclient.Target{}, tunnelHTTPError(err)
	}
	if len(list.Projects) == 1 {
		projectID = list.Projects[0].ID
	} else {
		p, err := c.Projects().Get(ctx, project)
		if err != nil {
			return tunnelclient.Target{}, tunnelHTTPError(err)
		}
		projectID = p.ID
	}
	a, err := c.ProjectAgents(projectID).Get(ctx, agent)
	if err != nil {
		return tunnelclient.Target{}, tunnelHTTPError(err)
	}
	if !a.DeletedAt.IsZero() {
		return tunnelclient.Target{}, &tunnelclient.HTTPError{Status: http.StatusNotFound, Code: "agent_not_found", Message: "agent was deleted"}
	}
	return tunnelclient.Target{AgentID: a.ID, ProjectID: projectID}, nil
}

// tunnelHTTPError turns a hub client error into a tunnelclient.HTTPError
// when the hub answered.
func tunnelHTTPError(err error) error {
	var ae *apiclient.APIError
	if errors.As(err, &ae) {
		return &tunnelclient.HTTPError{Status: ae.StatusCode, Code: ae.Code, Message: ae.Message}
	}
	return err
}

// tunnelSignalContext returns a context cancelled by SIGINT or SIGTERM,
// and a function reporting which signal ended it.
func tunnelSignalContext(parent context.Context) (context.Context, func() os.Signal, func()) {
	ctx, cancel := context.WithCancel(parent)
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	var got os.Signal
	done := make(chan struct{})
	go func() {
		select {
		case s := <-ch:
			got = s
			cancel()
		case <-done:
		}
	}()
	stop := func() {
		signal.Stop(ch)
		close(done)
		cancel()
	}
	return ctx, func() os.Signal { return got }, stop
}

func runTunnel(cmd *cobra.Command, args []string) error {
	settings := tunnelSettings()
	endpoint := tunnelEndpoint(settings)
	if endpoint == "" {
		return tunnelclient.Errorf(tunnelclient.ExitUsage, "no hub endpoint: pass --hub or configure hub.endpoint")
	}
	project := projectPath
	if project == "" && settings != nil {
		project = settings.ProjectID
	}
	req := tunnelclient.Request{
		Agent:     args[0],
		Project:   project,
		Port:      tunnelPort,
		LocalPort: tunnelLocalPort,
		SSH:       tunnelSSH,
		Stdio:     tunnelStdio,
		Stdin:     cmd.InOrStdin(),
		Stdout:    cmd.OutOrStdout(),
	}
	if err := req.Validate(); err != nil {
		return err
	}
	var ts *transportauth.TransportSettings
	if settings != nil && settings.Hub != nil && settings.Hub.Transport != nil {
		ts = &transportauth.TransportSettings{Mode: settings.Hub.Transport.Mode, Audience: settings.Hub.Transport.Audience}
	}
	src, mode, err := tunnelTransportSource(ts)
	if err != nil {
		return tunnelclient.Errorf(tunnelclient.ExitError, "resolving transport auth: %v", err)
	}
	hub := &tunnelHub{endpoint: endpoint, src: src, mode: mode}
	var apply func(h http.Header) error
	if src != nil {
		apply = func(h http.Header) error { return transportauth.ApplyHeaders(h, src, mode) }
	}
	cfg := tunnelclient.Config{
		Endpoint:          endpoint,
		ResolveCredential: func() tunnelclient.Credential { return tunnelCredential(endpoint) },
		Hub:               hub,
		ApplyTransport:    apply,
		ClientVersion:     version.Short(),
		Stderr:            cmd.ErrOrStderr(),
	}
	ctx, signalled, stop := tunnelSignalContext(cmd.Context())
	defer stop()
	err = tunnelclient.Run(ctx, cfg, req)
	if s := signalled(); s != nil {
		return tunnelclient.Errorf(tunnelclient.SignalExit(s), "%s", "interrupted")
	}
	if err == nil {
		return nil
	}
	var te *tunnelclient.Error
	if errors.As(err, &te) {
		return te
	}
	return tunnelclient.Errorf(tunnelclient.ExitError, "%v", err)
}
