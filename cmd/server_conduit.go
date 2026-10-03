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
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/compute/metadata"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hub"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// Conduit flags of `server start`. Each overrides its server.hub.conduit
// setting when set explicitly.
var (
	conduitInternalListen     string
	conduitInternalAdvertise  string
	conduitGrantKeyActivation string
	conduitReconnectWindow    string
	conduitTCPAllowedPorts    []int
)

// registerConduitServerFlags registers the conduit flags of `server start`.
func registerConduitServerFlags(f *pflag.FlagSet) {
	f.StringVar(&conduitInternalListen, "internal-listen", "", "host:port of the internal conduit relay API listener (hub.conduit; multi-node hubs). Must be reachable only inside the cluster/VPC. TLS on this hop is recommended (e.g. a service mesh or TLS-terminating proxy, advertised as https://); plain http:// is accepted")
	f.StringVar(&conduitInternalAdvertise, "internal-advertise", "", "Base URL other hub nodes use to reach the internal listener (default: POD_IP or the listen host). Prefer https:// (TLS on the internal hop); http:// is accepted")
	f.StringVar(&conduitGrantKeyActivation, "conduit-grant-key-activation", "", "Publish-before-sign delay of a new conduit grant key (default 15m, minimum 1m)")
	f.StringVar(&conduitReconnectWindow, "conduit-reconnect-window", "", "Jitter window targets redial in after a planned conduit close (default 5s, 0s-5m)")
	f.IntSliceVar(&conduitTCPAllowedPorts, "conduit-tcp-allowed-ports", nil, "Additional agent-local ports a conduit TCP stream may target besides the agent's exposed ports (comma-separated; reserved ports are always refused; default: exposed ports only)")
}

// applyConduitFlagOverrides copies the explicitly set conduit flags into
// cfg. validateServerPreflight checks the result.
func applyConduitFlagOverrides(cmd *cobra.Command, cfg *config.GlobalConfig) {
	f := cmd.Flags()
	if f.Changed("internal-listen") {
		cfg.Hub.Conduit.InternalListen = conduitInternalListen
	}
	if f.Changed("internal-advertise") {
		cfg.Hub.Conduit.InternalAdvertise = conduitInternalAdvertise
	}
	if f.Changed("conduit-grant-key-activation") {
		cfg.Hub.Conduit.GrantKeyActivation = conduitGrantKeyActivation
	}
	if f.Changed("conduit-reconnect-window") {
		cfg.Hub.Conduit.ReconnectWindow = conduitReconnectWindow
	}
	if f.Changed("conduit-tcp-allowed-ports") {
		cfg.Hub.Conduit.TCPAllowedPorts = append([]int(nil), conduitTCPAllowedPorts...)
	}
}

// appendConduitDaemonArgs forwards the explicitly set conduit flags to the
// --foreground daemon child.
func appendConduitDaemonArgs(cmd *cobra.Command, args []string) []string {
	f := cmd.Flags()
	for _, name := range []string{"internal-listen", "internal-advertise", "conduit-grant-key-activation", "conduit-reconnect-window"} {
		if f.Changed(name) {
			args = append(args, fmt.Sprintf("--%s=%s", name, f.Lookup(name).Value.String()))
		}
	}
	if f.Changed("conduit-tcp-allowed-ports") {
		ports := make([]string, len(conduitTCPAllowedPorts))
		for i, p := range conduitTCPAllowedPorts {
			ports[i] = strconv.Itoa(p)
		}
		args = append(args, "--conduit-tcp-allowed-ports="+strings.Join(ports, ","))
	}
	return args
}

// conduitGrantKeyActivationSetting returns the configured grant key activation
// (0 = hub default). validateServerPreflight has already rejected a
// malformed value.
func conduitGrantKeyActivationSetting(cfg *config.GlobalConfig) time.Duration {
	d, err := cfg.Hub.Conduit.GrantKeyActivationDuration()
	if err != nil {
		return 0
	}
	return d
}

// conduitReconnectWindowSetting returns the configured GoAway jitter window (0 =
// relay default). validateServerPreflight has already rejected a malformed
// value.
func conduitReconnectWindowSetting(cfg *config.GlobalConfig) time.Duration {
	d, err := cfg.Hub.Conduit.ReconnectWindowDuration()
	if err != nil {
		return 0
	}
	return d
}

// conduitAdvertiseEndpoint derives the internal endpoint other hub nodes
// use to reach this node: the configured internal_advertise, else
// http://<host>:<port> where host is the listen host when it is a specific
// address, else POD_IP. It returns "" (unaddressable) when no address is
// known.
func conduitAdvertiseEndpoint(advertise string, bound net.Addr, podIP string) string {
	if advertise != "" {
		return advertise
	}
	if bound == nil {
		return ""
	}
	host, port, err := net.SplitHostPort(bound.String())
	if err != nil {
		return ""
	}
	if ip := net.ParseIP(host); host == "" || (ip != nil && ip.IsUnspecified()) {
		host = podIP
	}
	if host == "" {
		return ""
	}
	return "http://" + net.JoinHostPort(host, port)
}

// conduitPeerAuthOptions builds the relay-peer auth options from config
// and the environment.
func conduitPeerAuthOptions(ctx context.Context, cfg *config.GlobalConfig, selfID string, onGCP bool) hub.ConduitPeerAuthOptions {
	o := hub.ConduitPeerAuthOptions{
		Mode:            cfg.Hub.Conduit.PeerAuthMode(),
		OnGCP:           onGCP,
		SelfID:          selfID,
		SharedSecret:    resolveSessionSecret(),
		Audience:        cfg.Hub.Conduit.PeerAudience,
		ServiceAccounts: cfg.Hub.Conduit.PeerServiceAccounts,
	}
	if onGCP {
		if email, err := metadata.EmailWithContext(ctx, "default"); err == nil {
			o.OwnServiceAccount = email
		}
	}
	return o
}

// startConduit opens the internal listener (if configured) and starts the
// hub's conduit relay when hub.conduit is on. With the flag off it does
// nothing. In hosted-HA mode every failure is returned and the server
// exits non-zero (C8); elsewhere a failure is logged and the hub serves
// without a relay. A relay that stops on its own (superseded) reports on
// errCh so the process exits and restarts.
func startConduit(ctx context.Context, cfg *config.GlobalConfig, hubSrv *hub.Server, hubEndpoint string, wg *sync.WaitGroup, errCh chan<- error) error {
	if hubSrv == nil || !hubSrv.ConduitEnabled() {
		return nil
	}
	requireHA := hostedHAGuardsRequired(cfg)
	err := startConduitRelay(ctx, cfg, hubSrv, hubEndpoint, wg, errCh, requireHA)
	if err == nil {
		return nil
	}
	if requireHA {
		return fmt.Errorf("conduit relay startup failed (hosted HA): %w", err)
	}
	slog.Error("Conduit relay not started; agent conduit sessions are unavailable on this node", "error", err)
	return nil
}

func startConduitRelay(ctx context.Context, cfg *config.GlobalConfig, hubSrv *hub.Server, hubEndpoint string, wg *sync.WaitGroup, errCh chan<- error, requireHA bool) (err error) {
	// C8: checked first, before any listener opens or peer auth is built,
	// so the operator sees this cause rather than a later symptom.
	if requireHA && !hubSrv.ConduitGrantRingShared() {
		return hub.ErrConduitNoAtRestKey
	}
	if requireHA && os.Getenv("K_SERVICE") != "" {
		return errConduitRelayOnCloudRun
	}
	if err := checkConduitAdvertiseHost(cfg.Hub.Conduit.InternalAdvertise, hubEndpoint); err != nil {
		return err
	}
	id := hub.ConduitInstanceID(cfg.Hub.Conduit.InstanceID)
	auth, mode, err := hub.NewConduitPeerAuth(conduitPeerAuthOptions(ctx, cfg, id, metadata.OnGCE()))
	if err != nil {
		return err
	}

	var endpoint string
	if listen := cfg.Hub.Conduit.InternalListen; listen != "" {
		ln, lerr := net.Listen("tcp", listen)
		if lerr != nil {
			return fmt.Errorf("conduit internal listener %s: %w", listen, lerr)
		}
		srv := &http.Server{Handler: hubSrv.ConduitInternalHandler(), ReadHeaderTimeout: 10 * time.Second}
		// A relay that does not start must not keep the port.
		defer func() {
			if err != nil {
				_ = srv.Close()
				_ = ln.Close() // Serve may not have taken it over yet
			}
		}()
		wg.Add(2)
		go func() {
			defer wg.Done()
			if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				sendConduitErr(errCh, fmt.Errorf("conduit internal listener: %w", err))
			}
		}()
		go func() {
			defer wg.Done()
			<-ctx.Done()
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_ = srv.Shutdown(shutdownCtx)
		}()
		endpoint = conduitAdvertiseEndpoint(cfg.Hub.Conduit.InternalAdvertise, ln.Addr(), os.Getenv("POD_IP"))
		slog.Info("Conduit internal listener started", "addr", ln.Addr().String(), "advertise", endpoint)
	} else if cfg.Hub.Conduit.InternalAdvertise != "" {
		return errors.New("server.hub.conduit.internal_advertise is set but internal_listen is not")
	}

	if err := hubSrv.StartConduitRelay(ctx, hub.ConduitRelayOptions{
		InstanceID:       id,
		InternalEndpoint: endpoint,
		RequireHA:        requireHA,
		PeerAuth:         auth,
		ReconnectWindow:  conduitReconnectWindowSetting(cfg),
	}); err != nil {
		return err
	}
	slog.Info("Conduit relay started", "instance_id", id, "peer_auth", mode, "internal_endpoint", endpoint, "hosted_ha", strconv.FormatBool(requireHA))

	if fatal := hubSrv.ConduitRelayFatal(); fatal != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case err, ok := <-fatal:
				if !ok || err == nil {
					err = errors.New("relay stopped serving")
				}
				sendConduitErr(errCh, fmt.Errorf("conduit relay: %w", err))
			case <-ctx.Done():
			}
		}()
	}
	return nil
}

// errConduitRelayOnCloudRun refuses the in-process relay on Cloud Run in
// hosted HA: Cloud Run instances are not individually addressable, so other
// hub nodes cannot route a session to a specific instance. A separate relay
// role will cover this deployment later.
var errConduitRelayOnCloudRun = errors.New("the in-process conduit relay is not supported on Cloud Run in hosted HA " +
	"(K_SERVICE is set): Cloud Run instances are not individually addressable; run the hub where each node has " +
	"its own internal address, or turn hub.conduit off (a separate relay role will support this deployment later)")

// checkConduitAdvertiseHost refuses an internal_advertise whose host is the
// public hub endpoint's host or a Cloud Run (*.run.app) host: those names
// reach an arbitrary instance behind a load balancer, not this node.
func checkConduitAdvertiseHost(advertise, hubEndpoint string) error {
	if advertise == "" {
		return nil
	}
	au, err := url.Parse(advertise)
	if err != nil {
		return fmt.Errorf("server.hub.conduit.internal_advertise %q: %w", advertise, err)
	}
	host := strings.ToLower(strings.TrimSuffix(au.Hostname(), "."))
	if host == "run.app" || strings.HasSuffix(host, ".run.app") {
		return fmt.Errorf("server.hub.conduit.internal_advertise %q is a Cloud Run host; it must address this node directly, not a load-balanced service", advertise)
	}
	if hubEndpoint == "" {
		return nil
	}
	hu, err := url.Parse(hubEndpoint)
	if err != nil {
		return nil
	}
	if hubHost := strings.ToLower(strings.TrimSuffix(hu.Hostname(), ".")); hubHost != "" && hubHost == host {
		return fmt.Errorf("server.hub.conduit.internal_advertise %q uses the public hub host %q; it must address this node directly, not the load-balanced hub endpoint", advertise, hubHost)
	}
	return nil
}

// sendConduitErr reports a fatal conduit error without blocking: any error
// already queued ends the server just the same.
func sendConduitErr(errCh chan<- error, err error) {
	select {
	case errCh <- err:
	default:
		slog.Error("Conduit fatal error (server already stopping)", "error", err)
	}
}
