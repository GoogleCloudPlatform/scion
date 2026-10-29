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

// Command fakeagent is the agent side of the port proxy browser e2e suite
// (web/e2e/port-proxy). It is test support, not part of the product.
//
// It runs a standalone Runtime Broker against a running test hub, joined
// with a join token it requests as the hub's dev-auth admin. The broker is
// real except for the agent manager: instead of launching a container, a
// start takes the environment the broker built for the agent (hub URL,
// agent id, agent token) and runs the real sciontool clients in this
// process. Each agent registers one port, the fixture app's, and serves it
// over either the conduit session or the legacy port-forward tunnel.
//
// It creates one project and one agent per transport, waits until each
// agent's port answers through the hub, writes a ready file (JSON, see
// readyState) and runs until SIGINT or SIGTERM.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/brokercredentials"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/runtimebroker"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/conduit"
	scionhub "github.com/GoogleCloudPlatform/scion/pkg/sciontool/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/portforward"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

// Transports the helper can serve a port over.
const (
	transportConduit = "conduit"
	transportLegacy  = "legacy"
)

// agentNamePrefix names each agent after its transport: port-proxy-conduit,
// port-proxy-legacy.
const agentNamePrefix = "port-proxy-"

// readyState is the ready file's content.
type readyState struct {
	BrokerID  string       `json:"brokerId"`
	ProjectID string       `json:"projectId"`
	Port      int          `json:"port"`
	Agents    []readyAgent `json:"agents"`
}

type readyAgent struct {
	Transport string `json:"transport"`
	ID        string `json:"id"`
	Name      string `json:"name"`
}

type options struct {
	hubURL       string
	devTokenFile string
	port         int
	stateDir     string
	readyFile    string
	transports   []string
	probePath    string
	probeText    string
	timeout      time.Duration
}

func main() {
	log.SetFlags(log.LstdFlags | log.LUTC)
	log.SetPrefix("[fakeagent] ")
	opts, err := parseFlags(os.Args[1:])
	if err != nil {
		log.Fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, opts); err != nil {
		// Straight to stderr: the broker may have redirected the log
		// package's output.
		fmt.Fprintf(os.Stderr, "[fakeagent] error: %v\n", err)
		os.Exit(1)
	}
}

func parseFlags(args []string) (options, error) {
	var o options
	var transports string
	fs := flag.NewFlagSet("fakeagent", flag.ContinueOnError)
	fs.StringVar(&o.hubURL, "hub", "", "hub base URL (http://127.0.0.1:<port>)")
	fs.StringVar(&o.devTokenFile, "dev-token-file", "", "file holding the hub's dev-auth token")
	fs.IntVar(&o.port, "port", 0, "loopback port of the fixture app each agent registers")
	fs.StringVar(&o.stateDir, "state-dir", "", "private directory for the broker's state")
	fs.StringVar(&o.readyFile, "ready-file", "", "where to write the ready JSON")
	fs.StringVar(&transports, "transports", "conduit,legacy", "comma-separated transports, one agent each")
	fs.StringVar(&o.probePath, "probe-path", "single.html", "path under the proxy that must answer before ready")
	fs.StringVar(&o.probeText, "probe-text", "", "text the probe answer must contain")
	fs.DurationVar(&o.timeout, "timeout", 2*time.Minute, "setup deadline")
	if err := fs.Parse(args); err != nil {
		return o, err
	}
	switch {
	case o.hubURL == "":
		return o, errors.New("-hub is required")
	case o.devTokenFile == "":
		return o, errors.New("-dev-token-file is required")
	case o.port <= 0 || o.port > 65535:
		return o, errors.New("-port must be a TCP port")
	case o.stateDir == "":
		return o, errors.New("-state-dir is required")
	case o.readyFile == "":
		return o, errors.New("-ready-file is required")
	}
	o.hubURL = strings.TrimRight(o.hubURL, "/")
	seen := map[string]bool{}
	for _, t := range strings.Split(transports, ",") {
		t = strings.TrimSpace(t)
		if t != transportConduit && t != transportLegacy {
			return o, fmt.Errorf("unknown transport %q (want %s or %s)", t, transportConduit, transportLegacy)
		}
		if seen[t] {
			return o, fmt.Errorf("transport %q listed twice", t)
		}
		seen[t] = true
		o.transports = append(o.transports, t)
	}
	return o, nil
}

// transportForAgent returns the transport an agent name stands for.
func transportForAgent(name string) (string, bool) {
	t, ok := strings.CutPrefix(name, agentNamePrefix)
	if !ok || (t != transportConduit && t != transportLegacy) {
		return "", false
	}
	return t, true
}

func run(ctx context.Context, o options) error {
	raw, err := os.ReadFile(o.devTokenFile)
	if err != nil {
		return fmt.Errorf("read dev token: %w", err)
	}
	devToken := strings.TrimSpace(string(raw))
	admin, err := hubclient.New(o.hubURL, hubclient.WithDevToken(devToken))
	if err != nil {
		return fmt.Errorf("hub client: %w", err)
	}

	setupCtx, cancelSetup := context.WithTimeout(ctx, o.timeout)
	defer cancelSetup()

	// Broker registration: a join token as the admin, then the join.
	reg, err := admin.RuntimeBrokers().Create(setupCtx, &hubclient.CreateBrokerRequest{
		Name:        "port-proxy-e2e",
		AutoProvide: true,
	})
	if err != nil {
		return fmt.Errorf("create broker: %w", err)
	}
	joined, err := admin.RuntimeBrokers().Join(setupCtx, &hubclient.JoinBrokerRequest{
		BrokerID:  reg.BrokerID,
		JoinToken: reg.JoinToken,
		Hostname:  "port-proxy-e2e",
		Version:   "e2e",
		// The broker can host empty-per-agent workspaces (the project's
		// mode, below); the fake manager provisions nothing anyway.
		Capabilities: []string{"empty_per_agent_workspace"},
	})
	if err != nil {
		return fmt.Errorf("join broker: %w", err)
	}
	log.Printf("broker %s joined", joined.BrokerID)

	agents := newAgentSet(o.port)
	defer agents.stopAll()
	rt := agents.runtime()
	mgr := &fakeManager{Manager: agent.NewManager(rt), agents: agents}

	bcfg := runtimebroker.DefaultServerConfig()
	bcfg.Host = "127.0.0.1"
	// The broker's own HTTP listener is unused here (the hub reaches it
	// over the control channel); keep it off the default port.
	if bcfg.Port, err = freePort(); err != nil {
		return err
	}
	bcfg.BrokerID = joined.BrokerID
	bcfg.BrokerName = "port-proxy-e2e"
	bcfg.StateDir = filepath.Join(o.stateDir, "broker-state")
	bcfg.HubEnabled = true
	bcfg.HubEndpoint = o.hubURL
	bcfg.ControlChannelEnabled = true
	// The mock runtime reports a real runtime name ("docker"), and the broker
	// is forced to it: the broker resolves each start's runtime by name from
	// the project's settings and profiles, which name only real runtimes, so
	// any other name would leave the broker looking for a runtime it does not
	// have instead of using this one.
	bcfg.ForceRuntime = rt.Name()
	// The hub's default harness config provisions by script inside the
	// container; nothing is provisioned here, so allow it.
	bcfg.AllowContainerScriptHarnesses = true
	bcfg.InMemoryCredentials = &brokercredentials.BrokerCredentials{
		BrokerID:    joined.BrokerID,
		SecretKey:   joined.SecretKey,
		HubEndpoint: o.hubURL,
	}
	broker := runtimebroker.New(bcfg, mgr, rt, config.RuntimeBuildRecord{})
	brokerCtx, cancelBroker := context.WithCancel(context.Background())
	defer cancelBroker()
	if err := broker.StartServices(brokerCtx); err != nil {
		return fmt.Errorf("start broker: %w", err)
	}
	defer func() {
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = broker.Shutdown(sctx)
	}()

	project, err := admin.Projects().Create(setupCtx, &hubclient.CreateProjectRequest{
		Name: "port-proxy-e2e",
		// Each agent gets an empty private directory, so no workspace
		// has to reach the broker.
		WorkspaceMode: "per-agent",
	})
	if err != nil {
		return fmt.Errorf("create project: %w", err)
	}

	state := readyState{BrokerID: joined.BrokerID, ProjectID: project.ID, Port: o.port}
	for _, t := range o.transports {
		id, err := createAgent(setupCtx, admin, project.ID, joined.BrokerID, agentNamePrefix+t)
		if err != nil {
			return err
		}
		state.Agents = append(state.Agents, readyAgent{Transport: t, ID: id, Name: agentNamePrefix + t})
	}
	for _, a := range state.Agents {
		if err := agents.waitConnected(setupCtx, a.Name); err != nil {
			return fmt.Errorf("agent %s: %w", a.Name, err)
		}
		if err := waitProxy(setupCtx, o, devToken, a.ID); err != nil {
			return fmt.Errorf("agent %s: %w", a.Name, err)
		}
		log.Printf("agent %s (%s) serves port %d", a.Name, a.Transport, o.port)
	}
	if err := writeReady(o.readyFile, state); err != nil {
		return err
	}
	log.Printf("ready")
	<-ctx.Done()
	log.Printf("stopping")
	return nil
}

// createAgent creates an agent on the broker. It retries transport errors
// and 5xx answers (the broker's control channel may still be connecting);
// any 4xx answer is final.
func createAgent(ctx context.Context, c hubclient.Client, projectID, brokerID, name string) (string, error) {
	var lastErr error
	for {
		resp, err := c.Agents().Create(ctx, &hubclient.CreateAgentRequest{
			Name:            name,
			ProjectID:       projectID,
			RuntimeBrokerID: brokerID,
			NoAuth:          true,
		})
		if err == nil && resp != nil && resp.Agent != nil {
			return resp.Agent.ID, nil
		}
		if err == nil {
			err = errors.New("no agent in the create response")
		}
		if !retryable(err) {
			return "", fmt.Errorf("create agent %s: %w", name, err)
		}
		if lastErr == nil || errorKey(lastErr) != errorKey(err) {
			fmt.Fprintf(os.Stderr, "[fakeagent] create agent %s: %v (retrying)\n", name, err)
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("create agent %s: %w", name, lastErr)
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// retryable reports whether a create failure may pass on its own: no HTTP
// answer at all, or a 5xx answer.
func retryable(err error) bool {
	var apiErr *apiclient.APIError
	if errors.As(err, &apiErr) {
		return apiErr.StatusCode >= 500
	}
	return true
}

// errorKey is an error's text without the per-request suffix, so a
// repeated failure is logged once.
func errorKey(err error) string {
	key, _, _ := strings.Cut(err.Error(), " (status")
	return key
}

// waitProxy waits until the agent's port answers the probe path through
// the hub (as the admin, with a bearer: this is setup, not a journey).
func waitProxy(ctx context.Context, o options, devToken, agentID string) error {
	u := fmt.Sprintf("%s/api/v1/agents/%s/ports/%d/proxy/%s", o.hubURL, agentID, o.port, o.probePath)
	var last string
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+devToken)
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK && strings.Contains(string(body), o.probeText) {
				return nil
			}
			last = fmt.Sprintf("status %d", resp.StatusCode)
		} else {
			last = err.Error()
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("port %d did not answer through the hub: %s", o.port, last)
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// freePort returns a loopback port that was free a moment ago.
func freePort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("free port: %w", err)
	}
	defer func() { _ = ln.Close() }()
	return ln.Addr().(*net.TCPAddr).Port, nil
}

func writeReady(path string, state readyState) error {
	b, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// fakeManager is the real agent manager except for the calls that would
// provision or launch a container.
type fakeManager struct {
	agent.Manager
	agents *agentSet
}

func (m *fakeManager) Preflight(context.Context, api.StartOptions) error { return nil }

func (m *fakeManager) Provision(context.Context, api.StartOptions) (*api.ScionConfig, error) {
	return &api.ScionConfig{}, nil
}

func (m *fakeManager) Start(_ context.Context, opts api.StartOptions) (*api.AgentInfo, error) {
	return m.agents.start(opts)
}

// agentSet holds the agents this process runs.
type agentSet struct {
	port int

	mu     sync.Mutex
	agents map[string]*fakeAgent
}

type fakeAgent struct {
	info      api.AgentInfo
	cancel    context.CancelFunc
	done      chan struct{}
	connected chan struct{}
	once      sync.Once
}

func newAgentSet(port int) *agentSet {
	return &agentSet{port: port, agents: map[string]*fakeAgent{}}
}

// start runs the sciontool clients for one agent, from the environment the
// broker built for it.
func (s *agentSet) start(opts api.StartOptions) (*api.AgentInfo, error) {
	transport, ok := transportForAgent(opts.Name)
	if !ok {
		return nil, fmt.Errorf("fakeagent: agent name %q names no transport", opts.Name)
	}
	env := opts.Env
	hubURL := env[scionhub.EnvHubEndpoint]
	if hubURL == "" {
		hubURL = env[scionhub.EnvHubURL]
	}
	// The agent's hub credential is the token the hub issued for this
	// start. The runtime broker passes it only in HubIssuedToken and never
	// in the start environment (pkg/runtimebroker/start_context.go), as for
	// a real agent in broker mode.
	token := opts.HubIssuedToken
	agentID := env[scionhub.EnvAgentID]
	if hubURL == "" || token == "" || agentID == "" {
		return nil, fmt.Errorf("fakeagent: the start lacks the hub URL, agent id or hub-issued token")
	}

	s.mu.Lock()
	if old := s.agents[opts.Name]; old != nil {
		s.mu.Unlock()
		return nil, fmt.Errorf("fakeagent: agent %s is already running", opts.Name)
	}
	ctx, cancel := context.WithCancel(context.Background())
	a := &fakeAgent{
		info: api.AgentInfo{
			ID:          agentID,
			ContainerID: "fakeagent-" + opts.Name,
			Name:        opts.Name,
			Phase:       "running",
			// The run the hub minted for this start, as a real container
			// carries it (label and launch id).
			RunID: opts.RunID,
			Labels: map[string]string{
				"scion.name":   opts.Name,
				"agent_id":     agentID,
				api.LabelRunID: opts.RunID,
			},
		},
		cancel:    cancel,
		done:      make(chan struct{}),
		connected: make(chan struct{}),
	}
	s.agents[opts.Name] = a
	s.mu.Unlock()

	client := scionhub.NewClientWithConfig(hubURL, token, agentID)
	go func() {
		defer close(a.done)
		s.serve(ctx, a, client, transport, env, opts.RunID)
	}()
	info := a.info
	return &info, nil
}

// serve registers the port and keeps the transport up until ctx ends.
//
// The conduit launch id is the run id: the broker drops SCION_LAUNCH_ID
// from the start environment and the real agent manager sets it from the
// run, so this does the same.
func (s *agentSet) serve(ctx context.Context, a *fakeAgent, client *scionhub.Client, transport string, env map[string]string, runID string) {
	for {
		_, err := client.RegisterPort(ctx, scionhub.RegisterPortRequest{Port: s.port, Label: "fixture"})
		if err == nil {
			break
		}
		log.Printf("agent %s: register port %d: %v", a.info.Name, s.port, err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
	switch transport {
	case transportLegacy:
		// The tunnel manager reports no connect event; the proxy probe in
		// run() is the readiness check.
		a.markConnected()
		portforward.NewManager(client).Run(ctx)
	case transportConduit:
		ag, err := conduit.New(conduit.Options{
			HubURL:     client.HubURL(),
			AgentID:    client.AgentID(),
			ProjectID:  env["SCION_PROJECT_ID"],
			LaunchID:   runID,
			Token:      client.AuthToken,
			HTTPClient: client.HTTPClient(),
			NoPTY:      true,
			OnSession:  func(*conduitv1.Welcome) { a.markConnected() },
		})
		if err != nil {
			log.Printf("agent %s: conduit: %v", a.info.Name, err)
			return
		}
		if err := ag.Run(ctx); err != nil && ctx.Err() == nil {
			log.Printf("agent %s: conduit stopped: %v", a.info.Name, err)
		}
	}
}

func (a *fakeAgent) markConnected() { a.once.Do(func() { close(a.connected) }) }

func (s *agentSet) waitConnected(ctx context.Context, name string) error {
	for {
		s.mu.Lock()
		a := s.agents[name]
		s.mu.Unlock()
		if a != nil {
			select {
			case <-a.connected:
				return nil
			case <-a.done:
				return errors.New("the agent stopped before its transport connected")
			case <-ctx.Done():
				return errors.New("no transport session")
			}
		}
		select {
		case <-ctx.Done():
			return errors.New("the hub never started the agent")
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func (s *agentSet) stop(name string) {
	s.mu.Lock()
	a := s.agents[name]
	delete(s.agents, name)
	s.mu.Unlock()
	if a != nil {
		a.cancel()
		<-a.done
	}
}

func (s *agentSet) stopAll() {
	s.mu.Lock()
	names := make([]string, 0, len(s.agents))
	for n := range s.agents {
		names = append(names, n)
	}
	s.mu.Unlock()
	for _, n := range names {
		s.stop(n)
	}
}

func (s *agentSet) list(filter map[string]string) []api.AgentInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []api.AgentInfo{}
	for _, a := range s.agents {
		match := true
		for k, v := range filter {
			if a.info.Labels[k] != v {
				match = false
				break
			}
		}
		if match {
			out = append(out, a.info)
		}
	}
	return out
}

// runtime is the broker's container runtime: it lists the running fake
// agents and stops them; it never launches anything.
func (s *agentSet) runtime() *runtime.MockRuntime {
	stopByRef := func(ref runtime.RunRef) error {
		s.mu.Lock()
		var name string
		for n, a := range s.agents {
			if a.info.ContainerID == ref.ID || a.info.Name == ref.ID || a.info.ID == ref.ID {
				name = n
			}
		}
		s.mu.Unlock()
		if name != "" {
			s.stop(name)
		}
		return nil
	}
	return &runtime.MockRuntime{
		// A real runtime name; see ForceRuntime in run().
		NameFunc: func() string { return "docker" },
		ListFunc: func(_ context.Context, filter map[string]string) ([]api.AgentInfo, error) {
			return s.list(filter), nil
		},
		StopFunc:   func(_ context.Context, ref runtime.RunRef) error { return stopByRef(ref) },
		DeleteFunc: func(_ context.Context, ref runtime.RunRef) error { return stopByRef(ref) },
		RunFunc: func(context.Context, runtime.RunConfig) (string, error) {
			return "", errors.New("fakeagent: the runtime launches nothing")
		},
	}
}
