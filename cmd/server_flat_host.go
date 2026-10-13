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
	"log"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/brokercredentials"
	"github.com/GoogleCloudPlatform/scion/pkg/brokerhost"
	"github.com/GoogleCloudPlatform/scion/pkg/brokeridentity"
	"github.com/GoogleCloudPlatform/scion/pkg/brokerregistration"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/k8s"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/runtimebroker"
	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Multi-instance flat Runtime Broker hosting (ptone/scion#3272). A process
// with server.broker.instances hosts every configured instance through a
// brokerhost.Host: one runtime, identity, credentials and Runtime Broker
// server per instance, one process-wide listener. It never hosts the legacy
// identity, never loads legacy Runtime Broker credentials and never runs the
// legacy embedded registration or orphan reassignment.

// flatHostMode is how this process activates flat instances. Co-located
// exactly when the legacy embedded registration would run
// (colocatedBrokerRegisters); otherwise remote, including
// --simulate-remote-broker with a Hub in the process: a remote instance
// activates only with its instance-scoped credentials after the remote
// activation validation, never through the embedded registration.
func flatHostMode(cfg *config.GlobalConfig, s store.Store) brokerhost.Mode {
	if colocatedBrokerRegisters(cfg, s) {
		return brokerhost.ModeColocated
	}
	return brokerhost.ModeRemote
}

// newFlatInstanceRuntime builds an instance's runtime from its own explicit
// configuration, never from Runtime Broker Profile resolution or a change to
// process-wide settings (KUBECONFIG, HOME, cwd, current context).
//
// Kubernetes: with runtime_target.kubeconfig set, that file is the only
// source (a missing, unreadable or malformed file, or a context it does not
// define, refuses the instance; there is no fallback to the default or
// in-cluster configuration). Without it, the process's normal loading rules
// apply (KUBECONFIG, including path lists, then ~/.kube/config, then
// in-cluster when no context is named). The namespace is the explicit one,
// else the runtime's usual chain.
func newFlatInstanceRuntime(_ context.Context, inst config.V1RuntimeBrokerInstanceConfig) (runtime.Runtime, error) {
	if inst.RuntimeTarget == nil {
		return nil, errors.New("runtime_target is required")
	}
	t := inst.RuntimeTarget
	switch t.Type {
	case brokeridentity.TargetTypeDocker:
		return runtime.NewDockerRuntime(), nil
	case brokeridentity.TargetTypeKubernetes:
		var client *k8s.Client
		var err error
		if t.Kubeconfig != "" {
			// The explicit file is the only source: no default loading
			// rules and no in-cluster fallback.
			client, err = k8s.NewClientFromKubeconfigFile(t.Kubeconfig, t.Context)
		} else {
			client, err = k8s.NewClientWithContext("", t.Context)
		}
		if err != nil {
			return nil, err // a configuration error: the file, its parse or its context
		}
		rt, err := runtime.NewKubernetesRuntimeFromClient(client, config.V1RuntimeConfig{
			Type: "kubernetes", Context: t.Context, Namespace: t.Namespace})
		if err != nil {
			// The connection check failed: the cluster cannot be identified.
			return nil, fmt.Errorf("%w: Cannot identify Kubernetes execution scope: the API server is unreachable or the request failed; "+
				"the instance was not activated: %v", brokeridentity.ErrExecutionScopeUnidentified, err)
		}
		return rt, nil
	default:
		return nil, fmt.Errorf("runtime target type %q is not supported in this release", inst.RuntimeTarget.Type)
	}
}

// flatScopeProber probes an instance's execution scope for the host and for
// 'scion broker register --instance'; a variable so command tests can stand
// in for the Docker daemon.
var flatScopeProber brokerhost.ScopeProber = probeFlatInstanceScope

// probeFlatInstanceScope probes an instance runtime's execution scope.
func probeFlatInstanceScope(ctx context.Context, inst config.V1RuntimeBrokerInstanceConfig, rt runtime.Runtime) (brokeridentity.ExecutionScope, error) {
	switch r := rt.(type) {
	case *runtime.DockerRuntime:
		return probeDockerExecutionScope(ctx, r.Command)
	case *runtime.KubernetesRuntime:
		return probeKubernetesExecutionScope(ctx, r)
	default:
		return brokeridentity.ExecutionScope{}, fmt.Errorf("%w: no scope probe for runtime %q", brokeridentity.ErrExecutionScopeUnidentified, rt.Name())
	}
}

// probeKubernetesExecutionScope identifies a Kubernetes instance's scope
// with the instance's own client (contract section 5, amendment K3): the
// kube-system Namespace object's UID plus the runtime's resolved namespace;
// the API server is informational. Any failure to identify the cluster
// refuses the instance (ErrExecutionScopeUnidentified); there is no fallback
// identity. Messages never include credentials or kubeconfig contents.
func probeKubernetesExecutionScope(ctx context.Context, rt *runtime.KubernetesRuntime) (brokeridentity.ExecutionScope, error) {
	if rt.Client == nil || rt.Client.Clientset == nil {
		return brokeridentity.ExecutionScope{}, fmt.Errorf("%w: the Kubernetes runtime has no client", brokeridentity.ErrExecutionScopeUnidentified)
	}
	probeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	ns, err := rt.Client.Clientset.CoreV1().Namespaces().Get(probeCtx, "kube-system", metav1.GetOptions{})
	switch {
	case apierrors.IsForbidden(err):
		return brokeridentity.ExecutionScope{}, fmt.Errorf("%w: Cannot identify Kubernetes execution scope: access denied reading Namespace kube-system; "+
			"this flat Runtime Broker instance requires get permission on namespaces/kube-system. "+
			"Ask the cluster operator to grant this read permission; the instance was not activated", brokeridentity.ErrExecutionScopeUnidentified)
	case apierrors.IsNotFound(err):
		return brokeridentity.ExecutionScope{}, fmt.Errorf("%w: Cannot identify Kubernetes execution scope: the Namespace object kube-system does not exist in this cluster; "+
			"the instance was not activated", brokeridentity.ErrExecutionScopeUnidentified)
	case err != nil:
		return brokeridentity.ExecutionScope{}, fmt.Errorf("%w: Cannot identify Kubernetes execution scope: reading Namespace kube-system failed (API server unreachable or request failed); "+
			"the instance was not activated: %v", brokeridentity.ErrExecutionScopeUnidentified, err)
	case ns == nil || string(ns.UID) == "":
		return brokeridentity.ExecutionScope{}, fmt.Errorf("%w: Cannot identify Kubernetes execution scope: Namespace kube-system has no UID; the instance was not activated",
			brokeridentity.ErrExecutionScopeUnidentified)
	}
	apiServer := ""
	if rt.Client.Config != nil {
		apiServer = brokeridentity.NormalizeKubernetesAPIServer(rt.Client.Config.Host)
	}
	return brokeridentity.ExecutionScope{
		Type: brokeridentity.TargetTypeKubernetes,
		Kubernetes: &brokeridentity.KubernetesScope{
			ClusterUID: string(ns.UID),
			Namespace:  rt.DefaultNamespace,
			APIServer:  apiServer,
		},
	}, nil
}

// colocatedFlatActivator activates an instance through the Hub's embedded
// flat registration (R1-R8, bound result required) and the in-memory
// credentials of the co-located control channel.
type colocatedFlatActivator struct {
	hubSrv *hub.Server
	// reported holds the instance keys whose refusal the embedded
	// registration already reported to the Hub.
	reportedMu sync.Mutex
	reported   map[string]bool
	// endpoint is the shared listener's base URL. Every flat instance,
	// including a single one, registers <endpoint>/instances/<id>, its
	// instance-qualified route, so its advertised endpoint does not change
	// when siblings are added or removed.
	endpoint    string
	autoProvide bool
	hubEndpoint string
}

func (a *colocatedFlatActivator) Activate(ctx context.Context, c brokerhost.Candidate) (*brokerhost.Activation, error) {
	// RegisterEmbeddedFlatRuntimeBroker reports its own refusals per
	// instance.
	endpoint := strings.TrimSuffix(a.endpoint, "/") + brokerhost.InstancePrefix(c.Identity.RuntimeBrokerID)
	row, err := a.hubSrv.RegisterEmbeddedFlatRuntimeBroker(ctx, c.Identity, c.Instance, hub.EmbeddedFlatRegistrationOptions{
		Endpoint:         endpoint,
		AutoProvide:      a.autoProvide,
		Capabilities:     flatInstanceCapabilities(c.Runtime),
		WorkspaceStorage: loadBrokerRegistrationWorkspaceStorage(),
	})
	if err != nil {
		a.reportedMu.Lock()
		if a.reported == nil {
			a.reported = map[string]bool{}
		}
		a.reported[c.Instance.Key] = true
		a.reportedMu.Unlock()
		var refusal *hub.RuntimeTargetRefusal
		if errors.As(err, &refusal) {
			return nil, brokerhost.WithReason(refusal.Code, err)
		}
		var ack *brokeridentity.AckError
		if errors.As(err, &ack) {
			return nil, err
		}
		return nil, brokerhost.WithReason("registration_failed", err)
	}
	act := &brokerhost.Activation{}
	authSvc := a.hubSrv.GetBrokerAuthService()
	if authSvc == nil {
		log.Printf("Warning: BrokerAuthService not available, skipping credentials for flat Runtime Broker instance %q", c.Instance.Key)
		return act, nil
	}
	secretKeyB64, err := authSvc.GenerateAndStoreSecret(ctx, row.ID)
	if err != nil {
		log.Printf("Warning: failed to generate/retrieve secret for flat Runtime Broker instance %q: %v", c.Instance.Key, err)
		return act, nil
	}
	act.InMemoryCredentials = &brokercredentials.BrokerCredentials{
		Name:         "local",
		BrokerID:     row.ID,
		SecretKey:    secretKeyB64,
		HubEndpoint:  a.hubEndpoint,
		RegisteredAt: time.Now(),
	}
	return act, nil
}

func (a *colocatedFlatActivator) Refused(inst config.V1RuntimeBrokerInstanceConfig, err error) {
	a.reportedMu.Lock()
	already := a.reported[inst.Key]
	delete(a.reported, inst.Key)
	a.reportedMu.Unlock()
	if already {
		return // the embedded registration reported this refusal itself
	}
	a.hubSrv.EmbeddedFlatInstanceFailed(inst.Key, err)
}

// remoteFlatActivator activates an instance with its instance-scoped
// credentials, each validated by brokerregistration.ValidateActivation on
// every start (carrier (a): the HMAC self-read of the instance's own row).
// Legacy credentials are never read and nothing falls back to them.
type remoteFlatActivator struct {
	globalDir string
	newClient func(*brokercredentials.BrokerCredentials) (hubclient.Client, error)
}

func (a *remoteFlatActivator) Activate(ctx context.Context, c brokerhost.Candidate) (*brokerhost.Activation, error) {
	list, err := brokerregistration.LoadInstanceCredentials(a.globalDir, c.Identity)
	if err != nil {
		return nil, err
	}
	newClient := a.newClient
	if newClient == nil {
		newClient = runtimebroker.HubClientForCredentials
	}
	var validated []brokercredentials.BrokerCredentials
	var errs []error
	for i := range list {
		creds := list[i]
		client, err := newClient(&creds)
		if err != nil {
			errs = append(errs, fmt.Errorf("hub %q: %w", creds.Name, err))
			continue
		}
		if err := brokerregistration.ValidateActivation(ctx, client, c.Identity, &creds); err != nil {
			errs = append(errs, fmt.Errorf("hub %q: %w", creds.Name, err))
			continue
		}
		validated = append(validated, creds)
	}
	if len(validated) == 0 {
		return nil, errors.Join(errs...)
	}
	for _, e := range errs {
		slog.Error("Flat Runtime Broker instance not connected to a Hub whose binding failed validation",
			"instance", c.Instance.Key, logging.AttrBrokerID, c.Identity.RuntimeBrokerID, "error", e)
	}
	return &brokerhost.Activation{RemoteCredentials: validated}, nil
}

func (a *remoteFlatActivator) Refused(config.V1RuntimeBrokerInstanceConfig, error) {}

// flatHostParams are the process-wide inputs of the flat host.
type flatHostParams struct {
	cfg            *config.GlobalConfig
	hubSrv         *hub.Server
	webSrv         *hub.WebServer
	store          store.Store
	instances      []config.V1RuntimeBrokerInstanceConfig
	hubEndpoint    string
	hubEndpointSrc hubEndpointSource
	devAuthToken   string
	settings       *config.Settings
	globalDir      string
	autoProvide    bool
	requestLogger  *slog.Logger
	messageLogger  *slog.Logger
	wg             *sync.WaitGroup
	errCh          chan error
}

// startFlatRuntimeBrokerHost hosts the configured flat instances. A
// configuration error refuses the process; a per-instance refusal leaves
// that instance unactivated and its siblings running.
func startFlatRuntimeBrokerHost(ctx context.Context, p flatHostParams) error {
	cfg := p.cfg
	mode := flatHostMode(cfg, p.store)
	if mode == brokerhost.ModeColocated && p.hubSrv == nil {
		return errors.New("flat Runtime Broker host: co-located mode requires the Hub")
	}

	versionedSettings, _, vsErr := config.LoadEffectiveSettings("")
	var vsBroker *config.V1BrokerConfig
	if vsErr == nil && versionedSettings != nil && versionedSettings.Server != nil {
		vsBroker = versionedSettings.Server.Broker
	}
	hubEndpointForRH := resolveHubEndpointForBroker(cfg, p.settings)

	rhEndpoint := fmt.Sprintf("http://%s:%d", cfg.RuntimeBroker.Host, cfg.RuntimeBroker.Port)
	if cfg.RuntimeBroker.Host == "0.0.0.0" {
		rhEndpoint = fmt.Sprintf("http://localhost:%d", cfg.RuntimeBroker.Port)
	}

	// The container Hub endpoint depends on the instance's runtime type, so
	// it is computed per instance (once per type).
	chResByRuntime := map[string]containerHubEndpointResult{}
	containerHub := func(rtName string) containerHubEndpointResult {
		if r, ok := chResByRuntime[rtName]; ok {
			return r
		}
		r := brokerContainerHubConfig(cfg, brokerContainerHubParams{
			RuntimeName:             rtName,
			BrokerHubEndpoint:       hubEndpointForRH,
			PublicHubEndpoint:       p.hubEndpoint,
			PublicHubEndpointSource: p.hubEndpointSrc,
			HostGatewayProbe:        func() bool { return runtime.DockerSupportsHostGateway(ctx, "") },
		}, log.Printf)
		chResByRuntime[rtName] = r
		return r
	}

	var brokerNFS *config.V1NFSConfig
	var workspaceStorageBackend string
	if globalVS, _, gErr := config.LoadGlobalSettings(); gErr != nil {
		log.Printf("WARNING: NFS mount checks disabled: loading global settings: %v", gErr)
	} else {
		workspaceStorageBackend = brokerWorkspaceStorageBackend(globalVS)
		var nfsWarning string
		brokerNFS, nfsWarning = brokerNFSConfig(globalVS)
		if nfsWarning != "" {
			log.Printf("WARNING: %s", nfsWarning)
		}
		if warning := brokerWorkspaceStorageWarning(globalVS); warning != "" {
			log.Printf("WARNING: %s", warning)
		}
	}

	var activator brokerhost.Activator
	if mode == brokerhost.ModeColocated {
		activator = &colocatedFlatActivator{hubSrv: p.hubSrv, endpoint: rhEndpoint, autoProvide: p.autoProvide, hubEndpoint: hubEndpointForRH}
		keys := make([]string, 0, len(p.instances))
		for _, inst := range p.instances {
			keys = append(keys, inst.Key)
		}
		p.hubSrv.ExpectEmbeddedFlatInstances(keys)
	} else {
		activator = &remoteFlatActivator{globalDir: p.globalDir}
	}

	shared := flatServerShared{
		cfg:                     cfg,
		mode:                    mode,
		hubEndpoint:             hubEndpointForRH,
		devAuthToken:            p.devAuthToken,
		nfs:                     brokerNFS,
		workspaceStorageBackend: workspaceStorageBackend,
		workspaceLocks:          runtimebroker.NewWorkspaceLocks(),
		containerHub:            containerHub,
	}
	var hostServices []brokerhost.Service
	if brokerNFS != nil && len(brokerNFS.Shares) > 0 {
		shared.nfsMounter = runtimebroker.NewHostNFSMounter(nil, logging.Subsystem("broker.nfs-mount"))
		hostServices = append(hostServices, shared.nfsMounter)
	}
	// The host's caches: one object per cache directory, the file caches
	// partitioned per instance.
	caches, err := runtimebroker.NewSharedCaches(templateCacheDir, templateCacheMax)
	if err != nil {
		return fmt.Errorf("opening the host's broker caches: %w", err)
	}
	shared.sharedCaches = caches
	hostServices = append(hostServices, caches)
	if p.hubSrv != nil {
		shared.colocatedStorage = p.hubSrv.GetStorage()
	}
	buildServer := func(ic brokerhost.InstanceContext) (*runtimebroker.Server, error) {
		rhCfg := flatInstanceServerConfig(shared, ic)
		srv := runtimebroker.New(rhCfg, ic.Manager, ic.Runtime)
		if p.requestLogger != nil {
			srv.SetRequestLogger(p.requestLogger)
		}
		if p.messageLogger != nil {
			srv.SetMessageLogger(p.messageLogger)
		}
		return srv, nil
	}

	host, err := brokerhost.New(brokerhost.Config{
		GlobalDir:          p.globalDir,
		Instances:          p.instances,
		Mode:               mode,
		LegacyIDs:          legacyRuntimeBrokerIDs(cfg, p.settings, vsBroker, p.globalDir),
		NewRuntime:         newFlatInstanceRuntime,
		ProbeScope:         flatScopeProber,
		Activator:          activator,
		BuildServer:        buildServer,
		OwnershipPreflight: flatOwnershipPreflight,
		OwnershipKeys:      flatOwnershipKeys,
		Services:           hostServices,
		Listener: brokerhost.ListenerConfig{
			Host:         cfg.RuntimeBroker.Host,
			Port:         cfg.RuntimeBroker.Port,
			ReadTimeout:  cfg.RuntimeBroker.ReadTimeout,
			WriteTimeout: cfg.RuntimeBroker.WriteTimeout,
		},
	})
	if err != nil {
		return err
	}
	log.Printf("Runtime broker hosting %d flat Runtime Broker instance(s) (%s); the legacy Runtime Broker identity is not hosted by this process",
		len(p.instances), mode)

	installColocatedSettingsOverlay(cfg, p.hubSrv)

	if err := host.Prepare(ctx); err != nil {
		return err
	}
	for _, st := range host.Status() {
		if st.State == brokerhost.StateActive {
			log.Printf("Flat Runtime Broker instance %q activated: Runtime Broker %s (%s), runtime target %s",
				st.Key, st.RuntimeBrokerID, st.Name, st.RuntimeTargetID)
		}
	}

	active := host.Active()
	if mode == brokerhost.ModeColocated && len(active) > 0 {
		// The Hub's local image checker uses the host's Docker CLI, not
		// whichever instance happened to start last, and only when a Docker
		// instance is active.
		for _, a := range active {
			if _, ok := a.Context.Runtime.(*runtime.DockerRuntime); ok {
				p.hubSrv.SetLocalImageChecker(runtime.NewDockerRuntime())
				break
			}
		}
		for _, a := range active {
			startFlatColocatedHeartbeat(ctx, p.wg, p.store, a.Server, a.Context.Identity.RuntimeBrokerID, a.Context.Instance.Name)
		}
	}

	if p.webSrv != nil {
		p.webSrv.SetBrokerHealthProvider(func(ctx context.Context) interface{} { return flatHostHealth(ctx, host) })
	}

	log.Printf("Starting Runtime Broker API server on %s:%d", cfg.RuntimeBroker.Host, cfg.RuntimeBroker.Port)
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		if err := host.Run(ctx); err != nil {
			p.errCh <- fmt.Errorf("runtime broker server error: %w", err)
		}
	}()
	return nil
}

// flatOwnershipPreflight is the pass-1 ownership check of a flat instance
// (ptone/scion#3274): before any Hub activation it reads every scion agent
// object on the instance's scope and refuses when any carries no owner
// label (an unlabeled object belongs to no instance, even for a single
// instance; an operator drains or recreates such agents through the version
// that created them), when the scope cannot be read completely, or when the
// instance's ownership records and slug index contradict each other. Labels
// naming another instance are that instance's and do not refuse this one.
func flatOwnershipPreflight(ctx context.Context, c brokerhost.Candidate) error {
	objects, err := c.Runtime.List(ctx, map[string]string{"scion.agent": "true"})
	if err != nil {
		return fmt.Errorf("cannot read the execution scope to establish ownership: %w", err)
	}
	dir, err := candidateStateDir(c)
	if err != nil {
		return err
	}
	records := runtimebroker.NewOwnershipStore(dir, c.Identity.RuntimeBrokerID)
	var unresolved []string
	for _, o := range objects {
		switch owner := o.Labels[api.LabelRuntimeBrokerID]; owner {
		case "":
			// below: unattributable
		case c.Identity.RuntimeBrokerID:
			// This instance's object: make sure its record exists
			// (reconstruction needs the complete project, agent, run and
			// object identity; anything less is unresolved).
			if err := records.Reconstruct(o); err != nil {
				unresolved = append(unresolved, fmt.Sprintf("%s (%v)", o.Name, err))
			}
			continue
		default:
			continue // another instance's object
		}
		name := o.Name
		if p := o.Labels["scion.project_id"]; p != "" {
			name = p + "/" + name
		}
		unresolved = append(unresolved, name)
	}
	if len(unresolved) > 0 {
		sort.Strings(unresolved)
		return fmt.Errorf("%d agent object(s) on this execution scope have unresolved ownership (no owner label, or labels too incomplete to reconstruct the record): %s; "+
			"drain or recreate them through the Runtime Broker that created them before activating this instance",
			len(unresolved), strings.Join(unresolved, ", "))
	}
	// Per-launch child objects carrying this instance's owner label are
	// recovered on their own (a crash after a child was created but before
	// its handle was recorded, with or without a main object). A listing
	// failure refuses: it is never read as absence.
	if lister, ok := c.Runtime.(runtime.OwnedResourceLister); ok {
		children, err := lister.ListOwnedResources(ctx, c.Identity.RuntimeBrokerID)
		if err != nil {
			return fmt.Errorf("cannot read this instance's child objects to establish ownership: %w", err)
		}
		var unresolvedChildren []string
		for _, ch := range children {
			if err := records.ReconstructChild(ch.Labels, ch.Handle); err != nil {
				unresolvedChildren = append(unresolvedChildren, fmt.Sprintf("%s %s/%s (%v)", ch.Handle.Kind, ch.Handle.Namespace, ch.Handle.Name, err))
			}
		}
		if len(unresolvedChildren) > 0 {
			sort.Strings(unresolvedChildren)
			return fmt.Errorf("%d child object(s) of this instance have unresolved ownership: %s; drain or recreate them through the Runtime Broker that created them before activating this instance",
				len(unresolvedChildren), strings.Join(unresolvedChildren, ", "))
		}
	}
	// The listing above was complete (a failed read refused before this),
	// so recorded main objects it does not show are confirmed gone; other
	// recorded objects are confirmed gone only by the runtime's exact
	// absence check, and what that allows is finished.
	var absent func(api.ResourceHandle) (bool, error)
	if checker, ok := c.Runtime.(runtime.ResourceAbsenceChecker); ok {
		absent = func(h api.ResourceHandle) (bool, error) { return checker.ResourceAbsent(ctx, h) }
	}
	if _, err := records.ReconcileAbsent(objects, absent, slog.Warn); err != nil {
		return fmt.Errorf("ownership records cannot be reconciled with the execution scope: %w", err)
	}
	problems, err := records.RepairSlugIndex()
	if err != nil {
		return fmt.Errorf("ownership records cannot be read: %w", err)
	}
	if len(problems) > 0 {
		return fmt.Errorf("ownership records are inconsistent: %s", strings.Join(problems, "; "))
	}
	return nil
}

// flatOwnershipKeys returns a flat instance's live ownership keys (its
// records after the preflight reconstructed any missing ones), which the
// host compares across instances: a key two instances claim is conflicting
// and both refuse operations on it.
func flatOwnershipKeys(_ context.Context, c brokerhost.Candidate) ([]string, error) {
	dir, err := candidateStateDir(c)
	if err != nil {
		return nil, err
	}
	return runtimebroker.NewOwnershipStore(dir, c.Identity.RuntimeBrokerID).LiveKeys()
}

// candidateStateDir is the state root the host resolved for the candidate
// (the same directory its server uses), or DefaultStateDir of its Runtime
// Broker ID when called without a host.
func candidateStateDir(c brokerhost.Candidate) (string, error) {
	if c.StateDir != "" {
		return c.StateDir, nil
	}
	return runtimebroker.DefaultStateDir(c.Identity.RuntimeBrokerID)
}

// flatServerShared are the process-wide inputs of every flat instance's
// Runtime Broker server configuration.
type flatServerShared struct {
	cfg                     *config.GlobalConfig
	mode                    brokerhost.Mode
	hubEndpoint             string
	devAuthToken            string
	nfs                     *config.V1NFSConfig
	workspaceStorageBackend string
	// workspaceLocks is the one process-wide workspace lock service every
	// instance shares.
	workspaceLocks *runtimebroker.WorkspaceLocks
	// nfsMounter is the host's single NFS mount owner (nil without NFS).
	nfsMounter *runtimebroker.HostNFSMounter
	// sharedCaches are the host's caches, one object per directory.
	sharedCaches *runtimebroker.SharedCaches
	// containerHub resolves an instance runtime's container Hub settings
	// (nil: none).
	containerHub func(rtName string) containerHubEndpointResult
	// colocatedStorage is the co-located Hub's storage (nil without one);
	// only a co-located (in-memory credentials) instance uses it.
	colocatedStorage storage.Storage
}

// flatInstanceServerConfig is one activated flat instance's Runtime Broker
// server configuration (without the container Hub endpoint and co-located
// storage, which the caller adds). Co-located instances are HubInProcess
// with in-memory credentials; remote instances carry their validated
// instance credentials, which also enable the Hub integration, control
// channel and heartbeat. With more than one instance configured (never the
// active count), every instance's NFS reconciler is verify-only.
func flatInstanceServerConfig(sh flatServerShared, ic brokerhost.InstanceContext) runtimebroker.ServerConfig {
	cfg := sh.cfg
	remote := len(ic.Activation.RemoteCredentials) > 0
	hubOn := sh.hubEndpoint != "" || remote
	rhCfg := runtimebroker.ServerConfig{
		Port:                          cfg.RuntimeBroker.Port,
		Host:                          cfg.RuntimeBroker.Host,
		ReadTimeout:                   cfg.RuntimeBroker.ReadTimeout,
		WriteTimeout:                  cfg.RuntimeBroker.WriteTimeout,
		HubEndpoint:                   sh.hubEndpoint,
		BrokerID:                      ic.Identity.RuntimeBrokerID,
		StateDir:                      ic.StateDir,
		BrokerName:                    ic.Instance.Name,
		CORSEnabled:                   cfg.RuntimeBroker.CORSEnabled,
		CORSAllowedOrigins:            cfg.RuntimeBroker.CORSAllowedOrigins,
		CORSAllowedMethods:            cfg.RuntimeBroker.CORSAllowedMethods,
		CORSAllowedHeaders:            cfg.RuntimeBroker.CORSAllowedHeaders,
		CORSMaxAge:                    cfg.RuntimeBroker.CORSMaxAge,
		AllowContainerScriptHarnesses: cfg.RuntimeBroker.AllowContainerScriptHarnesses,
		NFSConfig:                     sh.nfs,
		StorageBucket:                 brokerStorageBucket(cfg.Storage),
		WorkspaceStorageBackend:       sh.workspaceStorageBackend,
		WorkspaceLocks:                sh.workspaceLocks,
		Debug:                         enableDebug,
		SlowRequestThreshold:          cfg.SlowRequestThreshold,

		HubEnabled:           hubOn,
		HubToken:             sh.devAuthToken,
		TemplateCacheDir:     templateCacheDir,
		TemplateCacheMaxSize: templateCacheMax,

		ControlChannelEnabled: hubOn,
		HeartbeatEnabled:      hubOn,

		InMemoryCredentials:  ic.Activation.InMemoryCredentials,
		BrokerAuthEnabled:    true,
		BrokerAuthStrictMode: true,

		FlatInstance: &runtimebroker.FlatInstanceConfig{
			Identity:          ic.Identity,
			Instance:          ic.Instance,
			HubInProcess:      sh.mode == brokerhost.ModeColocated,
			RemoteCredentials: ic.Activation.RemoteCredentials,

			ConflictingOwnershipKeys: ic.ConflictingKeys,
		},
	}
	if sh.containerHub != nil && ic.Runtime != nil {
		sh.containerHub(ic.Runtime.Name()).applyTo(&rhCfg)
	}
	if ic.Activation.InMemoryCredentials != nil && sh.colocatedStorage != nil {
		rhCfg.ColocatedStorage = sh.colocatedStorage
	}
	// The host's single NFS mount owner: host-bind instances
	// register with it instead of mounting themselves.
	rhCfg.NFSHostMounter = sh.nfsMounter
	rhCfg.SharedCaches = sh.sharedCaches
	return rhCfg
}

// flatHostHealth is the broker health the Hub's public /healthz shows for a
// flat host: the single configured instance's own health while it is
// active, otherwise readiness plus each instance's state and reason code
// (never the refusal text, which stays in the logs).
func flatHostHealth(ctx context.Context, host *brokerhost.Host) interface{} {
	if !host.MultiInstance() {
		if a := host.Active(); len(a) == 1 {
			return a[0].Server.GetHealthInfo(ctx)
		}
	}
	return map[string]interface{}{"ready": host.Ready(), "instances": host.Status()}
}

// installColocatedSettingsOverlay installs the global settings overlay for a
// co-located Hub with a Postgres database, as startRuntimeBroker does for
// the legacy broker. Flat instances never read profiles or runtimes from it.
func installColocatedSettingsOverlay(cfg *config.GlobalConfig, hubSrv *hub.Server) {
	if hubSrv == nil || cfg.Database.Driver != "postgres" {
		return
	}
	overlay := config.NewSettingsOverlay()
	config.SetGlobalSettingsOverlay(overlay)
	if ops := hubSrv.GetOperationalSettings(); ops != nil {
		snap := ops.Snapshot()
		overlay.Update(snap.Runtimes, snap.Profiles, snap.HarnessConfigs, snap.ImageRegistry)
	}
}

// startFlatColocatedHeartbeat runs the co-located internal heartbeat for one
// active instance: every 30s it writes the instance's status from its own
// control channel state.
func startFlatColocatedHeartbeat(ctx context.Context, wg *sync.WaitGroup, s store.Store, srv *runtimebroker.Server, brokerID, brokerName string) {
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		prevOnline := srv.IsControlChannelConnected()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				ccUp := srv.IsControlChannelConnected()
				status := store.BrokerStatusOnline
				if !ccUp {
					status = store.BrokerStatusOffline
				}
				if ccUp != prevOnline {
					log.Printf("Co-located heartbeat: control channel %s for flat Runtime Broker %s (%s)",
						map[bool]string{true: "restored", false: "down"}[ccUp], brokerName, brokerID)
					prevOnline = ccUp
				}
				if err := s.UpdateRuntimeBrokerHeartbeat(ctx, brokerID, status); err != nil {
					log.Printf("Warning: failed to update internal heartbeat for %s: %v", brokerName, err)
				}
			}
		}
	}()
}
