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
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/transportauth"
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
var sshTransportResolver = resolveAttachTransport

// newSSHHubConn builds the hub access for endpoint with a user token.
func newSSHHubConn(endpoint, token string) (*sshHubConn, error) {
	hc := &http.Client{Timeout: sshHubRequestTimeout}
	src, mode, err := sshTransportResolver()
	if err != nil {
		return nil, Errorf(ExitError, "transport auth for %s: %v", endpoint, err)
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
		return nil, Errorf(ExitError, "hub client for %s: %v", endpoint, err)
	}
	raw := apiclient.NewTransport(endpoint,
		apiclient.WithHTTPClient(hc),
		apiclient.WithAuth(&apiclient.BearerAuth{Token: token}),
	)
	return &sshHubConn{endpoint: endpoint, client: client, raw: raw}, nil
}

// sshAgentHostKeyResponse is the part of GET agent the host-key accessor
// reads.
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
			return "", Errorf(ExitAgentNotFound, "agent not found or deleted: %s", agentRef)
		}
		return "", classifySSHHubError(h.endpoint, err)
	}
	return strings.TrimSpace(body.SSHHostKey), nil
}

// normalizeSSHHostKey validates an authorized-keys form public key and
// returns "<type> <base64>" without any comment.
func normalizeSSHHostKey(key string) (string, error) {
	pub, _, _, _, err := ssh.ParseAuthorizedKey([]byte(key))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub))), nil
}

// classifySSHHubError maps a hub request failure to the exit-code table and
// a one-line message.
func classifySSHHubError(endpoint string, err error) *Error {
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	var apiErr *apiclient.APIError
	if errors.As(err, &apiErr) {
		switch {
		case apiErr.StatusCode == http.StatusUnauthorized:
			return Errorf(ExitCredential, "credential refused by hub %s; run scion hub auth login", endpoint)
		case apiErr.StatusCode == http.StatusForbidden:
			return Errorf(ExitForbidden, "not permitted by hub %s", endpoint)
		case apiErr.StatusCode == http.StatusBadGateway,
			apiErr.StatusCode == http.StatusServiceUnavailable,
			apiErr.StatusCode == http.StatusGatewayTimeout:
			return Errorf(ExitHubUnavailable, "hub unreachable or unavailable: %s (status %d)", endpoint, apiErr.StatusCode)
		default:
			return Errorf(ExitError, "hub %s: %s", endpoint, sshOneLine(apiErr.Error()))
		}
	}
	var netErr net.Error
	var urlErr *url.Error
	if errors.Is(err, context.DeadlineExceeded) || errors.As(err, &netErr) || errors.As(err, &urlErr) {
		return Errorf(ExitHubUnavailable, "hub unreachable or unavailable: %s", endpoint)
	}
	return Errorf(ExitError, "hub %s: %s", endpoint, sshOneLine(err.Error()))
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
	return "", "", Errorf(ExitAgentNotFound, "project not found: %s", project)
}

// checkSSHHostKeyPin is the host-key check for scion tunnel --ssh. It fetches
// the agent's current host key and compares it with the line ssh-config
// pinned for this hub, project and agent. On a mismatch it prints the
// recreated hint to stderr and returns nil, so ssh goes on to report the
// mismatch itself. A missing agent returns exit 4 promptly.
func checkSSHHostKeyPin(ctx context.Context, stderr io.Writer, home, endpoint, token, project, agent string) error {
	normalized, err := NormalizeEndpoint(endpoint)
	if err != nil {
		return Errorf(ExitUsage, "%v", err)
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
	fmt.Fprintln(stderr, sshRecreatedHint)
	return true
}
