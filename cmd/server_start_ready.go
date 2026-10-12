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
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/daemon"
	"github.com/spf13/pflag"
)

// serverStartReadyTimeout bounds how long 'server start' and 'server restart'
// wait for the daemon's listeners to answer /healthz: about this long, since
// the last probe can add its 2s client timeout and the settle delay. It covers
// a first start that runs database migrations. A variable so tests can
// shorten it.
var serverStartReadyTimeout = 30 * time.Second

// serverStartReadyPollInterval is how often the start wait polls /healthz.
// A variable so tests can shorten it.
var serverStartReadyPollInterval = 200 * time.Millisecond

// serverStartReadySettle is how long the start wait waits, once every
// listener answered, before checking once more that the started process is
// still running. A stale server on a target port can answer before the new
// process exits on its own port check. A variable so tests can shorten it.
var serverStartReadySettle = 500 * time.Millisecond

// errServerExitedDuringStartup is returned by waitForServerListening when
// the started server process exits before every listener answered.
var errServerExitedDuringStartup = errors.New("server process exited during startup")

// serverReadyTarget is one listener the start wait polls.
type serverReadyTarget struct {
	// Name says which component(s) answer on this listener, e.g. "hub API
	// and web" for the combined web port.
	Name string
	// URL is the full /healthz URL.
	URL string
	// NeedsHub is set when the hub API is mounted on this listener (the
	// combined web port): the answer must then include the hub's health.
	NeedsHub bool
}

// serverNotReadyError reports a listener that did not answer before the
// start wait's deadline.
type serverNotReadyError struct {
	Target  serverReadyTarget
	Timeout time.Duration
	LastErr error
}

func (e *serverNotReadyError) Error() string {
	msg := fmt.Sprintf("server did not become ready within %s: %s at %s did not answer", e.Timeout, e.Target.Name, e.Target.URL)
	if e.LastErr != nil {
		msg += fmt.Sprintf(" (last error: %v)", e.LastErr)
	}
	return msg
}

// serverReadyTargetsFromArgs returns the listeners the server started with
// the given 'server start --foreground' argv will open, so the parent can
// wait for them. It reads the same argv the child gets (from
// buildDaemonStartArgs, or the saved args on restart) and mirrors the child's
// resolution (loadAndReconcileConfig and the workstation broker check in
// runServerStart):
//   - mode: --hosted/--production, else cfg.Mode (server.mode, including
//     SCION_SERVER_MODE and a --config file), else fallbackMode when cfg is
//     nil.
//   - workstation mode enables every component that was not explicitly
//     disabled, binds the hub and broker to loopback unless --host is given,
//     and skips the broker when registryCheck fails (no image registry) and
//     the hub or web runs.
//   - hosted mode takes --enable-runtime-broker, else cfg.RuntimeBroker.Enabled.
//   - --host sets only the hub (and web) host; the broker keeps
//     cfg.RuntimeBroker.Host, or loopback for a hosted broker without the hub.
//   - ports come from flags, else cfg, else the defaults.
//
// cfg may be nil (config failed to load); the defaults are used then.
//
// Per mode:
//   - web enabled: the web port. With the hub enabled the hub API is mounted
//     there, so that one listener serves both.
//   - hub enabled without web: the hub port.
//   - runtime broker enabled: the broker port, which is its own listener.
//
// registryCheck must answer as the child would (requireImageRegistryForBroker
// run in the child's working directory); it is only called in workstation
// mode with the broker enabled.
func serverReadyTargetsFromArgs(args []string, cfg *config.GlobalConfig, fallbackMode string, registryCheck func() error) []serverReadyTarget {
	fs := pflag.NewFlagSet("server-start-ready", pflag.ContinueOnError)
	fs.ParseErrorsAllowlist.UnknownFlags = true
	fs.SetOutput(io.Discard)
	hosted := fs.Bool("hosted", false, "")
	production := fs.Bool("production", false, "")
	hub := fs.Bool("enable-hub", false, "")
	broker := fs.Bool("enable-runtime-broker", false, "")
	web := fs.Bool("enable-web", false, "")
	host := fs.String("host", "", "")
	hubPortFlag := fs.Int("port", 0, "")
	brokerPortFlag := fs.Int("runtime-broker-port", 0, "")
	webPortFlag := fs.Int("web-port", 8080, "")
	// Parse errors (e.g. a malformed saved value) leave the defaults; the
	// child reports such errors itself.
	_ = fs.Parse(args)

	serverMode := fallbackMode
	if cfg != nil {
		serverMode = cfg.Mode
	} else {
		def := config.DefaultGlobalConfig()
		cfg = &def
	}
	isHosted := *hosted || *production
	if !fs.Changed("hosted") && !fs.Changed("production") {
		isHosted = serverMode == "hosted" || serverMode == "production"
	}
	if isHosted && !fs.Changed("enable-runtime-broker") {
		*broker = cfg.RuntimeBroker.Enabled
	}
	if !isHosted {
		// applyWorkstationDefaults: on unless explicitly disabled.
		if !fs.Changed("enable-hub") {
			*hub = true
		}
		if !fs.Changed("enable-runtime-broker") {
			*broker = true
		}
		if !fs.Changed("enable-web") {
			*web = true
		}
		// Without an image registry the child starts without the broker
		// so the setup wizard is reachable (workstationBrokerRegistryDegrade).
		// When it fails instead (broker only), the child exits and the
		// wait reports that.
		if skip, err := workstationBrokerRegistryDegrade(*broker, *hub || *web, registryCheck); err == nil && skip {
			*broker = false
		}
	}

	hubHostVal, brokerHostVal := cfg.Hub.Host, cfg.RuntimeBroker.Host
	if !isHosted && !fs.Changed("host") {
		hubHostVal, brokerHostVal = "127.0.0.1", "127.0.0.1"
	}
	if fs.Changed("host") {
		// The child applies --host to the hub (and web) only.
		hubHostVal = *host
	}
	if isHosted && !fs.Changed("host") && *broker && !*hub {
		// Standalone hosted broker defaults to loopback.
		brokerHostVal = "127.0.0.1"
	}
	hubPortVal := cfg.Hub.Port
	if fs.Changed("port") {
		hubPortVal = *hubPortFlag
	}
	brokerPortVal := cfg.RuntimeBroker.Port
	if fs.Changed("runtime-broker-port") {
		brokerPortVal = *brokerPortFlag
	}

	var targets []serverReadyTarget
	switch {
	case *web && *hub:
		targets = append(targets, serverReadyTarget{Name: "hub API and web", URL: healthzURL(hubHostVal, *webPortFlag), NeedsHub: true})
	case *web:
		targets = append(targets, serverReadyTarget{Name: "web", URL: healthzURL(hubHostVal, *webPortFlag)})
	case *hub:
		targets = append(targets, serverReadyTarget{Name: "hub API", URL: healthzURL(hubHostVal, hubPortVal)})
	}
	if *broker {
		targets = append(targets, serverReadyTarget{Name: "runtime broker", URL: healthzURL(brokerHostVal, brokerPortVal)})
	}
	return targets
}

// healthzURL builds the /healthz URL for a listener bound to host:port. A
// wildcard or empty bind address is probed on loopback.
func healthzURL(host string, port int) string {
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		host = "127.0.0.1"
	}
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	return "http://" + net.JoinHostPort(host, strconv.Itoa(port)) + "/healthz"
}

// probeServerListening reports whether target answered /healthz: HTTP 200
// with a JSON body that has a status (and, for a listener that serves the
// hub API, the hub's health). Any status counts: the wait is for the
// listeners to serve requests, and 'scion server status' reports health.
func probeServerListening(client *http.Client, target serverReadyTarget) error {
	resp, err := client.Get(target.URL)
	if err != nil {
		return err
	}
	body, readErr := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	if readErr != nil {
		return readErr
	}
	var health healthProbeResponse
	if err := json.Unmarshal(body, &health); err != nil || health.Status == "" {
		return errors.New("response is not a scion health answer")
	}
	if target.NeedsHub && health.Hub == nil {
		return errors.New("hub API not yet mounted")
	}
	return nil
}

// waitForServerListening polls each target's /healthz, in order, until it
// answers (see probeServerListening), all within about one timeout (a probe
// in flight at the deadline can add up to the client timeout). alive reports
// whether the started server process is still running; when it stops, the
// wait ends with errServerExitedDuringStartup instead of running out the
// timeout. Once every listener answered it waits settle and checks alive
// again, so a stale server answering on a port the new process then fails to
// bind is not reported ready. On timeout it returns a *serverNotReadyError
// naming the first listener that did not answer. It never stops the server.
func waitForServerListening(client *http.Client, targets []serverReadyTarget, timeout, interval, settle time.Duration, alive func() bool) error {
	deadline := time.Now().Add(timeout)
	for _, target := range targets {
		for {
			err := probeServerListening(client, target)
			if err == nil {
				break
			}
			if !alive() {
				return errServerExitedDuringStartup
			}
			remaining := time.Until(deadline)
			if remaining <= 0 {
				return &serverNotReadyError{Target: target, Timeout: timeout, LastErr: err}
			}
			time.Sleep(min(interval, remaining))
		}
	}
	// Something answered; make sure it is still our process.
	time.Sleep(settle)
	if !alive() {
		return errServerExitedDuringStartup
	}
	return nil
}

// waitForDaemonServerReady waits after 'server start' or 'server restart'
// launched the daemon with daemonArgs until its listeners answer /healthz,
// for about serverStartReadyTimeout. On a timeout the server is left
// running: it may only be slow (for example a long first-start migration),
// and stopping it could interrupt that work. The error says so and points
// at the log.
func waitForDaemonServerReady(globalDir string, daemonArgs []string) error {
	cfg, err := config.LoadGlobalConfig(serverConfigPathFromArgs(daemonArgs))
	if err != nil {
		cfg = nil
	}
	targets := serverReadyTargetsFromArgs(daemonArgs, cfg, config.LoadServerMode(), daemonRegistryCheck(globalDir))
	if len(targets) == 0 {
		return nil
	}
	names := make([]string, 0, len(targets))
	for _, t := range targets {
		names = append(names, fmt.Sprintf("%s at %s", t.Name, strings.TrimSuffix(t.URL, "/healthz")))
	}
	fmt.Printf("Waiting for the server to answer (%s)...\n", strings.Join(names, ", "))

	alive := func() bool {
		running, _, _ := daemon.StatusComponent(serverDaemonComponent, globalDir)
		return running
	}
	client := &http.Client{Timeout: 2 * time.Second}
	logPath := daemon.GetLogPathComponent(serverDaemonComponent, globalDir)
	err = waitForServerListening(client, targets, serverStartReadyTimeout, serverStartReadyPollInterval, serverStartReadySettle, alive)
	return daemonReadyError(err, logPath, alive)
}

// daemonRegistryCheck returns the image registry check as the daemon child
// runs it. The child's working directory is the global directory
// (daemon.StartComponent), so it resolves settings from there, not from the
// project of the directory 'server start' was run in: a registry set only in
// that project does not keep the child's broker on.
func daemonRegistryCheck(globalDir string) func() error {
	return func() error { return requireImageRegistryForBrokerAt(globalDir) }
}

// daemonReadyError turns a waitForServerListening result into the error
// 'server start' and 'server restart' return.
func daemonReadyError(err error, logPath string, alive func() bool) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, errServerExitedDuringStartup):
		return fmt.Errorf("the server exited during startup before it became ready. Check log at: %s", logPath)
	default:
		state := "The server process is still running"
		if !alive() {
			state = "The server process has exited"
		}
		return fmt.Errorf("%w\n\n%s. Check the log at %s and 'scion server status'; use 'scion server stop' to stop it", err, state, logPath)
	}
}

// serverConfigPathFromArgs returns the --config value in a daemon argv, or
// "" when it is not set.
func serverConfigPathFromArgs(args []string) string {
	for i, a := range args {
		switch {
		case strings.HasPrefix(a, "--config="):
			return strings.TrimPrefix(a, "--config=")
		case (a == "--config" || a == "-c") && i+1 < len(args):
			return args[i+1]
		}
	}
	return ""
}
