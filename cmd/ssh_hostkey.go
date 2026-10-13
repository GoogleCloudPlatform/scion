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
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/transportauth"
	"github.com/GoogleCloudPlatform/scion/pkg/tunnelclient"
)

// sshHubRequestTimeout bounds each hub request made by ssh, ssh-config and
// the host-key check, so a ProxyCommand never hangs on an unresponsive hub.
const sshHubRequestTimeout = 15 * time.Second

// sshRecreatedHint is printed when an agent's host key no longer matches
// the pinned known-hosts line.
const sshRecreatedHint = "agent was recreated; run scion ssh-config --write"

// sshHubConn is one hub's REST access for the ssh commands. The hub client
// and the raw transport share one HTTP client and one user bearer token.
// The token comes from getHubAccessToken, which never yields an agent token.
type sshHubConn struct {
	endpoint string
	client   hubclient.Client
	raw      *apiclient.Transport
}

// sshTransportResolver resolves transport-layer auth (for example IAP) for
// the hub requests; tests replace it.
var sshTransportResolver = func() (transportauth.TokenSource, transportauth.HeaderMode, error) {
	var ts *transportauth.TransportSettings
	if s := tunnelSettings(); s != nil && s.Hub != nil && s.Hub.Transport != nil {
		ts = &transportauth.TransportSettings{Mode: s.Hub.Transport.Mode, Audience: s.Hub.Transport.Audience}
	}
	return tunnelTransportSource(ts)
}

// newSSHHubConn builds the hub access for endpoint with a user token.
func newSSHHubConn(endpoint, token string) (*sshHubConn, error) {
	hc := &http.Client{Timeout: sshHubRequestTimeout}
	src, mode, err := sshTransportResolver()
	if err != nil {
		return nil, tunnelclient.Errorf(tunnelclient.ExitError, "transport auth for %s: %v", endpoint, err)
	}
	if src != nil {
		hc.Transport = transportauth.Wrap(http.DefaultTransport, src, mode)
	}
	client, err := hubclient.New(endpoint,
		hubclient.WithHTTPClient(hc),
		hubclient.WithBearerToken(token),
		// hc already carries any transport auth; stop New wrapping it again.
		hubclient.WithTransportAuth(nil, mode),
	)
	if err != nil {
		return nil, tunnelclient.Errorf(tunnelclient.ExitError, "hub client for %s: %v", endpoint, err)
	}
	raw := apiclient.NewTransport(endpoint,
		apiclient.WithHTTPClient(hc),
		apiclient.WithAuth(&apiclient.BearerAuth{Token: token}),
	)
	return &sshHubConn{endpoint: endpoint, client: client, raw: raw}, nil
}

// sshAgentHostKeyResponse is the part of GET agent the host-key accessor
// reads. The hub serves sshHostKey on both the GET by id and the
// project-scoped GET by slug, and omits it when the agent has no key.
type sshAgentHostKeyResponse struct {
	SSHHostKey string `json:"sshHostKey"`
}

// agentSSHHostKey returns the agent's SSH host public key in authorized-keys
// form, or "" when the hub reports none. agentRef is an agent id, or a slug
// when projectID is set. A missing agent maps to exit 4.
func (h *sshHubConn) agentSSHHostKey(ctx context.Context, projectID, agentRef string) (string, error) {
	path := "/api/v1/agents/" + url.PathEscape(agentRef)
	if projectID != "" {
		path = "/api/v1/projects/" + url.PathEscape(projectID) + "/agents/" + url.PathEscape(agentRef)
	}
	resp, err := h.raw.Get(ctx, path, nil)
	if err != nil {
		return "", classifySSHHubError(h.endpoint, err)
	}
	body, err := apiclient.DecodeRequired[sshAgentHostKeyResponse](resp)
	if err != nil {
		var apiErr *apiclient.APIError
		if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
			return "", tunnelclient.Errorf(tunnelclient.ExitAgentNotFound, "agent not found or deleted: %s", agentRef)
		}
		return "", classifySSHHubError(h.endpoint, err)
	}
	return strings.TrimSpace(body.SSHHostKey), nil
}

// classifySSHHubError maps a hub request failure to the exit-code table and
// a one-line message.
func classifySSHHubError(endpoint string, err error) *tunnelclient.Error {
	var e *tunnelclient.Error
	if errors.As(err, &e) {
		return e
	}
	var apiErr *apiclient.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.StatusCode {
		case http.StatusUnauthorized:
			return tunnelclient.Errorf(tunnelclient.ExitCredential, "credential refused by hub %s; run scion hub auth login", endpoint)
		case http.StatusForbidden:
			return tunnelclient.Errorf(tunnelclient.ExitForbidden, "not permitted by hub %s", endpoint)
		case http.StatusBadGateway,
			http.StatusServiceUnavailable,
			http.StatusGatewayTimeout:
			return tunnelclient.Errorf(tunnelclient.ExitHubUnavailable, "hub unreachable or unavailable: %s (status %d)", endpoint, apiErr.StatusCode)
		default:
			return tunnelclient.Errorf(tunnelclient.ExitError, "hub %s: %s", endpoint, sshOneLine(apiErr.Error()))
		}
	}
	var netErr net.Error
	var urlErr *url.Error
	if errors.Is(err, context.DeadlineExceeded) || errors.As(err, &netErr) || errors.As(err, &urlErr) {
		return tunnelclient.Errorf(tunnelclient.ExitHubUnavailable, "hub unreachable or unavailable: %s", endpoint)
	}
	return tunnelclient.Errorf(tunnelclient.ExitError, "hub %s: %s", endpoint, sshOneLine(err.Error()))
}

// sshOneLine collapses s onto a single line for stderr output that lands in ssh
// and editor logs.
func sshOneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// resolveSSHProjectID returns the hub project id for a project slug or id.
func (h *sshHubConn) resolveSSHProjectID(ctx context.Context, project string) (id, slug string, err error) {
	if p, gerr := h.client.Projects().Get(ctx, project); gerr == nil && p != nil && p.ID != "" {
		return p.ID, p.Slug, nil
	} else if gerr != nil {
		var apiErr *apiclient.APIError
		if !errors.As(gerr, &apiErr) || apiErr.StatusCode != http.StatusNotFound {
			return "", "", classifySSHHubError(h.endpoint, gerr)
		}
	}
	resp, err := h.client.Projects().List(ctx, &hubclient.ListProjectsOptions{Slug: project})
	if err != nil {
		return "", "", classifySSHHubError(h.endpoint, err)
	}
	for _, p := range resp.Projects {
		if p.Slug == project {
			return p.ID, p.Slug, nil
		}
	}
	return "", "", tunnelclient.Errorf(tunnelclient.ExitAgentNotFound, "project not found: %s", project)
}

// checkSSHHostKeyPin is the host-key check for scion tunnel --ssh. It fetches
// the agent's current host key and compares it with the line ssh-config
// pinned for this hub, project and agent. On a mismatch it prints the
// recreated hint to stderr and returns nil, so ssh goes on to report the
// mismatch itself. A missing agent returns exit 4 promptly.
func checkSSHHostKeyPin(ctx context.Context, stderr io.Writer, home, endpoint, token, project, agent string) error {
	// The endpoint as given must be a plain origin before it is normalized.
	if _, err := sshEndpointURL(endpoint); err != nil {
		return tunnelclient.Errorf(tunnelclient.ExitUsage, "%v", err)
	}
	normalized := tunnelclient.NormalizeEndpoint(endpoint)
	if _, err := sshEndpointURL(normalized); err != nil {
		return tunnelclient.Errorf(tunnelclient.ExitUsage, "%v", err)
	}
	conn, err := newSSHHubConn(endpoint, token)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, sshHubRequestTimeout)
	defer cancel()
	projectID, projectSlug, err := conn.resolveSSHProjectID(ctx, project)
	if err != nil {
		return err
	}
	current, err := conn.agentSSHHostKey(ctx, projectID, agent)
	if err != nil {
		return err
	}
	if current == "" {
		return nil
	}
	checkPinnedSSHHostKey(stderr, sshPaths{home: home}, normalized, projectSlug, agent, current)
	return nil
}

// checkPinnedSSHHostKey compares current with the pinned known-hosts line
// for the agent and prints the recreated hint when they differ. It reports
// whether it printed the hint. Missing files or pins are not a mismatch.
func checkPinnedSSHHostKey(stderr io.Writer, p sshPaths, endpoint, projectSlug, agentSlug, current string) bool {
	files, err := listSSHIncludeFiles(p)
	if err != nil {
		return false
	}
	label := ""
	for _, f := range files {
		if f.endpoint == endpoint {
			label = f.label
			break
		}
	}
	if label == "" {
		return false
	}
	pins, err := readKnownHosts(p.knownHostsFile(label))
	if err != nil {
		return false
	}
	pinned, ok := pins[sshHostName(label, projectSlug, agentSlug)]
	if !ok {
		return false
	}
	want, err := normalizeSSHHostKey(current)
	if err != nil || want == pinned {
		return false
	}
	_, _ = fmt.Fprintln(stderr, sshRecreatedHint)
	return true
}

// sshHostKeyHintTimeout bounds the hub requests of the host-key hint, so
// it never delays scion tunnel --ssh by more than this.
const sshHostKeyHintTimeout = 5 * time.Second

// tunnelSSHHostKeyHint prints the recreated-agent hint for scion tunnel
// --ssh when the agent's current host key differs from the pinned one. It
// is a hint only: every error (no project, no credential, a failed hub
// request, a missing known-hosts file) is ignored, and the tunnel always
// continues, so ssh itself refuses a changed key.
func tunnelSSHHostKeyHint(cmd *cobra.Command, ssh bool, endpoint, project, agent string) {
	if !ssh || project == "" {
		return
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	token := getHubAccessToken(endpoint)
	if token == "" {
		return
	}
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, sshHostKeyHintTimeout)
	defer cancel()
	_ = checkSSHHostKeyPin(ctx, cmd.ErrOrStderr(), home, endpoint, token, project, agent)
}
