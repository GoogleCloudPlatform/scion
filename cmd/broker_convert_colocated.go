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
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/pflag"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/brokerconvert"
	"github.com/GoogleCloudPlatform/scion/pkg/brokeridentity"
	"github.com/GoogleCloudPlatform/scion/pkg/brokerregistration"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/daemon"
	entagent "github.com/GoogleCloudPlatform/scion/pkg/ent/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
	entruntimebroker "github.com/GoogleCloudPlatform/scion/pkg/ent/runtimebroker"
	"github.com/GoogleCloudPlatform/scion/pkg/util/cloudrunenv"
	"github.com/GoogleCloudPlatform/scion/pkg/util/privatefs"
)

// The co-located conversion's host seams: whether this host's server runs
// the Hub with an embedded Runtime Broker, whether that server answers, the
// Q7 store topology and probes, and the removal of the unused instance
// credentials files. The phase logic is in pkg/brokerconvert (colocated.go).

// brokerConvertSingleHubReplica is --single-hub-replica: the operator's
// assertion that exactly one Hub process serves an external Hub database.
var brokerConvertSingleHubReplica bool

func init() {
	brokerConvertCmd.Flags().BoolVar(&brokerConvertSingleHubReplica, "single-hub-replica", false,
		"Assert that exactly one Hub process serves this host's external Hub database (co-located '--apply', stopped phase only)")
	brokerConvertCmd.Flags().StringVar(&brokerConvertHostMode, "host-mode", "",
		"Assert the host mode (colocated or remote) when it cannot be detected from a saved daemon launch (--apply and --rollback only; never overrides a saved launch or the journal)")
}

// colocatedProbeTimeout bounds each probe of a server endpoint.
const colocatedProbeTimeout = 2 * time.Second

// errColocatedUndetermined is returned when the server's launch or settings
// cannot be read, so co-located mode cannot be decided.
var errColocatedUndetermined = errors.New(brokerconvert.RefusalText(brokerconvert.RefuseColocatedUnknown))

// errEndpointUnprobeable and errStoreUnreadable are the probes' constant
// errors; Q7Check maps any probe error to its own refusal.
var (
	errEndpointUnprobeable = errors.New("the Hub endpoint could not be probed")
	errStoreUnreadable     = errors.New("the Hub store could not be read")
)

// errCredentialsCleanup is the constant refusal of the credentials cleanup.
var errCredentialsCleanup = errors.New("an instance credentials directory holds a link, a directory or another non-regular entry, or could not be read; nothing further was removed")

// colocatedHost is what the conversion knows about this host's server.
type colocatedHost struct {
	// Colocated: the server runs the Hub and embeds the Runtime Broker, so
	// the legacy embedded registration runs (colocatedBrokerRegisters).
	Colocated bool
	Listeners serverListeners
	Config    *config.GlobalConfig
}

// detectColocatedHost evaluates colocatedBrokerRegisters from the server's
// saved launch arguments (server-args.json, kept across a stop) and its
// server settings: the Hub and the Runtime Broker both enabled (with the
// workstation defaults) and no --simulate-remote-broker. With no saved
// server launch, a saved broker-daemon launch means the remote path, and
// neither means the mode cannot be determined (refused). A store is
// present exactly when the Hub is enabled.
func detectColocatedHost(globalDir string) (colocatedHost, error) {
	// A saved launch file that is a link or not a regular file is refused,
	// never followed.
	for _, c := range []string{serverDaemonComponent, brokerDaemonComponent} {
		fi, err := os.Lstat(filepath.Join(globalDir, daemon.ArgsFileName(c)))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil || !fi.Mode().IsRegular() {
			return colocatedHost{}, errColocatedUndetermined
		}
	}
	args, err := daemon.LoadArgs(serverDaemonComponent, globalDir)
	if err != nil {
		return colocatedHost{}, errColocatedUndetermined
	}
	if args == nil {
		// No saved server launch. A host whose Runtime Broker runs as the
		// separate broker daemon registers to a Hub elsewhere: the remote
		// path. With neither launch saved, the mode is not guessed.
		bargs, berr := daemon.LoadArgs(brokerDaemonComponent, globalDir)
		if berr != nil || bargs == nil {
			return colocatedHost{}, errColocatedUndetermined
		}
		cfg, err := config.LoadGlobalConfig("")
		if err != nil || cfg == nil {
			return colocatedHost{}, errColocatedUndetermined
		}
		return colocatedHost{Colocated: false, Config: cfg}, nil
	}
	cfg, err := config.LoadGlobalConfig(serverConfigPathFromArgs(args))
	if err != nil || cfg == nil {
		return colocatedHost{}, errColocatedUndetermined
	}
	noRegistryCheck := func() error { return nil }
	l := serverListenersFromArgs(args, cfg, config.LoadServerMode(), noRegistryCheck)
	return colocatedHost{
		Colocated: l.Hub && l.Broker && !argsSimulateRemoteBroker(args),
		Listeners: l,
		Config:    cfg,
	}, nil
}

// argsSimulateRemoteBroker reports whether a saved server argv sets
// --simulate-remote-broker.
func argsSimulateRemoteBroker(args []string) bool {
	flags := pflag.NewFlagSet("convert-colocated", pflag.ContinueOnError)
	flags.ParseErrorsAllowlist.UnknownFlags = true
	flags.SetOutput(io.Discard)
	sim := flags.Bool("simulate-remote-broker", false, "")
	_ = flags.Parse(args)
	return *sim
}

// newProbeClient is an HTTP client for unauthenticated probes: a short
// timeout, no proxy, no redirects followed, no cookies and no credential.
// connected is set once any TCP connection is made, so a server that
// accepts and then stalls or fails the TLS handshake counts as an answer.
func newProbeClient(connected *atomic.Bool) *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil
	dialer := &net.Dialer{Timeout: colocatedProbeTimeout}
	tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		c, err := dialer.DialContext(ctx, network, addr)
		if err == nil {
			connected.Store(true)
		}
		return c, err
	}
	return &http.Client{
		Timeout:   colocatedProbeTimeout,
		Transport: tr,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// localServerAnswers reports whether this host's server is up: its daemon
// is running, or anything answers on any of its listeners (Hub, web or
// Runtime Broker port, on loopback for a wildcard bind). Any answer counts,
// whatever its status or body, so the stopped phase never runs beside a
// live server.
func localServerAnswers(ctx context.Context, globalDir string, l serverListeners) bool {
	if running, err := serverDaemonMayRun(globalDir); err != nil || running {
		return true // an undecidable daemon state counts as up (fail closed)
	}
	for _, u := range []string{
		healthzURL(l.HubHost, l.WebPort),
		healthzURL(l.HubHost, l.HubPort),
		healthzURL(l.BrokerHost, l.BrokerPort),
	} {
		if answered, err := probeAnswers(ctx, u); err != nil || answered {
			return true
		}
	}
	return false
}

// darkEndpointProbe probes each Hub endpoint (Q7 (b)): GET <endpoint>/healthz
// with no credential and no redirect followed. It reports answered when any
// endpoint gives any HTTP response or any reply at the TLS layer. Only a
// refused or timed-out connection is dark. No endpoint, an unparsable one,
// or an address that does not resolve is an error: the probe cannot tell.
func darkEndpointProbe(ctx context.Context, endpoints []string) (bool, error) {
	probed := 0
	seen := map[string]bool{}
	for _, ep := range endpoints {
		ep = strings.TrimSuffix(strings.TrimSpace(ep), "/")
		if ep == "" || seen[ep] {
			continue
		}
		seen[ep] = true
		u, err := url.Parse(ep)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
			return false, errEndpointUnprobeable
		}
		answered, err := probeAnswers(ctx, ep+"/healthz")
		if err != nil {
			return false, err
		}
		if answered {
			return true, nil
		}
		probed++
	}
	if probed == 0 {
		return false, errEndpointUnprobeable
	}
	return false, nil
}

// probeAnswers sends one unauthenticated GET. It returns answered for any
// response, and for any failure after a TCP connection was made (a TLS
// reply, a reset or a stall after connect). A connection that is refused
// or times out is not an answer. A name that does not resolve is an error.
func probeAnswers(ctx context.Context, target string) (bool, error) {
	var connected atomic.Bool
	client := newProbeClient(&connected)
	ctx, cancel := context.WithTimeout(ctx, colocatedProbeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return false, errEndpointUnprobeable
	}
	resp, err := client.Do(req)
	if err == nil {
		_ = resp.Body.Close()
		return true, nil
	}
	return classifyProbeError(err, connected.Load())
}

// classifyProbeError maps a failed probe to answered (true, nil: a
// connection was made), cannot tell (error: the name did not resolve) or
// dark (false, nil: refused, unreachable or timed out before connecting).
func classifyProbeError(err error, connected bool) (bool, error) {
	if connected {
		return true, nil
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return false, errEndpointUnprobeable
	}
	return false, nil
}

// colocatedStoreTopology classifies the Hub store from the effective
// server settings (Q7). The replica markers are those of isHADeployment,
// except that an external database alone is not taken as a replica setup:
// a single Hub on Postgres is allowed with the operator's assertion.
func colocatedStoreTopology(cfg *config.GlobalConfig, isCloudRunService func() bool) brokerconvert.StoreTopology {
	if cfg == nil {
		return brokerconvert.TopologyUndetermined
	}
	if isCloudRunService == nil {
		isCloudRunService = cloudrunenv.IsService
	}
	if isCloudRunService() || (strings.EqualFold(cfg.Storage.Provider, "gcs") && cfg.Auth.Mode == "proxy") {
		return brokerconvert.TopologyReplica
	}
	switch strings.ToLower(cfg.Database.Driver) {
	case "", "sqlite":
		if strings.TrimSpace(cfg.Database.URL) == "" {
			return brokerconvert.TopologyUndetermined
		}
		return brokerconvert.TopologySingleSQLite
	case "postgres":
		return brokerconvert.TopologySingleExternal
	}
	return brokerconvert.TopologyUndetermined
}

// legacyStoreSQLite reads the local SQLite Hub store: whether it holds this
// host's legacy Runtime Broker row (store identity), and the non-terminal
// agents on that row. It opens the
// store read-only (mode=ro, query_only; a missing file is an error, never
// created; no migration) inside one read-only snapshot. An agent with no
// phase counts as non-terminal. Any error is returned as a constant text.
// Only IDs, names, project IDs and phases are read; no other column.
func legacyStoreSQLite(ctx context.Context, dsn, legacyID string) (brokerconvert.LegacyStoreFacts, error) {
	var facts brokerconvert.LegacyStoreFacts
	client, err := entc.OpenSQLiteReadOnly(preflightSQLiteDSN(dsn))
	if err != nil {
		return facts, errStoreUnreadable
	}
	defer func() { _ = client.Close() }()
	tx, err := client.BeginTx(ctx, snapshotTxOptions(client.Driver().Dialect()))
	if err != nil {
		return facts, errStoreUnreadable
	}
	defer func() { _ = tx.Rollback() }() // read only: never committed
	// Store identity: the store must hold this host's legacy row.
	if uid, perr := uuid.Parse(legacyID); perr == nil {
		present, qerr := tx.Client().RuntimeBroker.Query().Where(entruntimebroker.IDEQ(uid)).Exist(ctx)
		if qerr != nil {
			return facts, errStoreUnreadable
		}
		facts.RowPresent = present
	}
	if !facts.RowPresent {
		return facts, nil
	}
	rows, err := tx.Client().Agent.Query().
		Where(
			entagent.RuntimeBrokerIDEQ(legacyID),
			entagent.DeletedAtIsNil(),
			entagent.Or(
				entagent.PhaseIsNil(),
				entagent.PhaseNotIn(string(state.PhaseStopped), string(state.PhaseError), string(state.PhaseHalted)),
			),
		).
		Select(entagent.FieldID, entagent.FieldName, entagent.FieldProjectID, entagent.FieldPhase).
		All(ctx)
	if err != nil {
		return facts, errStoreUnreadable
	}
	for _, a := range rows {
		facts.Agents = append(facts.Agents, brokerconvert.AgentEntry{ID: a.ID.String(), Name: a.Name, ProjectID: a.ProjectID.String(), Phase: a.Phase})
	}
	return facts, nil
}

// removeInstanceCredentials removes the instance's unused Hub credentials
// files (<global>/runtime-brokers/<key>/hub-credentials/*) and then the
// emptied directory, without following a link. The instance directory and
// the credentials directory must be real directories; every entry must be
// a regular file. Anything else refuses before that entry is touched. An
// absent credentials directory is done. Used by the cleanup after the
// first co-located start and before the stopped-phase rollback moves the
// instance directory aside.
func removeInstanceCredentials(globalDir, key string) error {
	instDir := brokeridentity.InstanceDir(globalDir, key)
	fi, err := os.Lstat(instDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil || !fi.IsDir() {
		return errCredentialsCleanup
	}
	tree, err := privatefs.OpenTree(instDir, privatefs.TreeOptions{})
	if err != nil {
		return errCredentialsCleanup
	}
	defer func() { _ = tree.Close() }()
	d, err := tree.OpenDir(brokerregistration.HubCredentialsDirName)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return errCredentialsCleanup
	}
	entries, err := d.ReadDir(-1)
	_ = d.Close()
	if err != nil {
		return errCredentialsCleanup
	}
	for _, e := range entries {
		if !e.Type().IsRegular() {
			return errCredentialsCleanup
		}
	}
	for _, e := range entries {
		if err := tree.Remove(path.Join(brokerregistration.HubCredentialsDirName, e.Name())); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return errCredentialsCleanup
		}
	}
	if err := tree.Remove(brokerregistration.HubCredentialsDirName); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return errCredentialsCleanup
	}
	return nil
}

// instanceCredentialsRemain reports whether any instance credentials
// directory still exists for keys (the AC12 "none remains" check).
func instanceCredentialsRemain(globalDir string, keys []string) bool {
	for _, k := range keys {
		if _, err := os.Lstat(brokerregistration.InstanceCredentialsDir(globalDir, k)); !errors.Is(err, fs.ErrNotExist) {
			return true
		}
	}
	return false
}
