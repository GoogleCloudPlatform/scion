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

package commands

import (
	"context"
	"errors"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/log"
	scionportforward "github.com/GoogleCloudPlatform/scion/pkg/sciontool/portforward"
)

// envProjectID is the agent's project id, set by the broker.
const envProjectID = "SCION_PROJECT_ID"

// portForwarding chooses how the hub reaches the agent's ports.
type portForwarding struct {
	getenv         func(string) string
	disableConduit bool
	// conduit builds the conduit dialer; legacy runs the port-forward
	// tunnel until ctx ends.
	conduit func() (conduitRunner, error)
	legacy  func(ctx context.Context)
}

type conduitRunner interface {
	Run(ctx context.Context) error
}

// run dials the conduit endpoint only when the hub advertises it
// (hub.conduit in SCION_HUB_EXPERIMENTS) and the caller has not disabled it. Without the
// capability it runs exactly the legacy tunnel and never calls
// /api/v1/conduit. A hub that answers the conduit endpoint with 404 falls
// back to the legacy tunnel.
func (p portForwarding) run(ctx context.Context) {
	if p.disableConduit || !conduit.HubServesConduit(p.getenv) {
		p.legacy(ctx)
		return
	}
	a, err := p.conduit()
	if err != nil {
		log.Error("Conduit: cannot start (%v); using the port-forward tunnel", err)
		p.legacy(ctx)
		return
	}
	log.Info("Started conduit dialer")
	err = a.Run(ctx)
	switch {
	case errors.Is(err, conduit.ErrUnsupported):
		log.Info("Conduit: hub has no conduit endpoint; using the port-forward tunnel")
		p.legacy(ctx)
	case err != nil && ctx.Err() == nil:
		log.Error("Conduit dialer stopped: %v", err)
	}
}

// newPortForwarding wires portForwarding to the hub client; getenv reads
// the agent environment (os.Getenv outside tests). ptyUser is the agent
// user that PTY streams' tmux clients run as.
func newPortForwarding(c *hub.Client, disableConduit bool, ptyUser conduit.PTYUser, getenv func(string) string) portForwarding {
	return portForwarding{
		getenv:         getenv,
		disableConduit: disableConduit,
		conduit: func() (conduitRunner, error) {
			return conduit.New(conduit.Options{
				HubURL:                c.HubURL(),
				AgentID:               c.AgentID(),
				ProjectID:             getenv(envProjectID),
				LaunchID:              getenv(conduit.EnvLaunchID),
				Token:                 c.AuthToken,
				ApplyTransportHeaders: c.ApplyTransportHeaders,
				HTTPClient:            c.HTTPClient(),
				PTYUser:               ptyUser,
				RefreshCredential: func(ctx context.Context) error {
					_, _, err := c.RefreshToken(ctx)
					return err
				},
			})
		},
		legacy: func(ctx context.Context) {
			log.Info("Started port-forward tunnel manager")
			scionportforward.NewManager(c).Run(ctx)
		},
	}
}

// conduitPTYUser is the identity PTY streams' tmux clients run as: the
// harness's (see harnessSupervisorConfig), which owns the tmux server.
func conduitPTYUser(targetUID, targetGID int, rootless, requirePrivilegeDrop bool) conduit.PTYUser {
	u := conduit.PTYUser{UID: targetUID, GID: targetGID, RequirePrivilegeDrop: requirePrivilegeDrop}
	if targetUID > 0 || rootless {
		u.Username = "scion"
	}
	return u
}
