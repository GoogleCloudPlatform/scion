// Command mediator is the cr-ci Cloud Run service that turns GitHub
// workflow_job webhooks into Cloud Run Job executions running one ephemeral
// GitHub Actions runner each.
//
// Endpoints: POST /webhook, POST /internal/claim, POST /reconcile and
// GET /health (not /healthz, which Cloud Run reserves). See
// internal/server and cr-ci/docs/architecture.md. Claims are held in memory,
// so the service must be deployed with max-instances=1.
package main

import (
	"context"
	"crypto/rsa"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/GoogleCloudPlatform/scion/cr-ci/mediator/internal/claim"
	"github.com/GoogleCloudPlatform/scion/cr-ci/mediator/internal/gcp"
	"github.com/GoogleCloudPlatform/scion/cr-ci/mediator/internal/githubapp"
	"github.com/GoogleCloudPlatform/scion/cr-ci/mediator/internal/launcher"
	"github.com/GoogleCloudPlatform/scion/cr-ci/mediator/internal/logging"
	"github.com/GoogleCloudPlatform/scion/cr-ci/mediator/internal/oidc"
	"github.com/GoogleCloudPlatform/scion/cr-ci/mediator/internal/server"
)

func main() {
	if err := run(os.Getenv, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// endpoints are the external service origins; tests point them at fakes.
type endpoints struct {
	GitHub        string
	Metadata      string
	SecretManager string
	CloudRun      string
	GoogleCerts   string
}

func defaultEndpoints() endpoints {
	return endpoints{
		GitHub:        githubapp.DefaultBaseURL,
		Metadata:      gcp.DefaultMetadataURL,
		SecretManager: gcp.DefaultSecretManagerURL,
		CloudRun:      launcher.DefaultRunURL,
		GoogleCerts:   oidc.DefaultCertsURL,
	}
}

// newServer wires the mediator from a validated config.
func newServer(cfg *Config, ep endpoints, logger *slog.Logger) *server.Server {
	tokens := &gcp.MetadataTokenSource{BaseURL: ep.Metadata}
	sm := &gcp.SecretManager{BaseURL: ep.SecretManager, Tokens: tokens, Project: cfg.GCPProject}

	repos := make([]githubapp.Repo, len(cfg.TargetRepos))
	for i, r := range cfg.TargetRepos {
		repos[i] = githubapp.Repo{Owner: r.Owner, Name: r.Name}
	}
	scope := githubapp.ScopeRepo
	if cfg.InstallScope == ScopeOrg {
		scope = githubapp.ScopeOrg
	}
	gh := githubapp.New(githubapp.Options{
		BaseURL:    ep.GitHub,
		AppID:      cfg.GitHubAppID,
		Scope:      scope,
		PrivateKey: cachedKey(sm, cfg.AppPrivateKeySecretName),
	})
	return server.New(server.Options{
		TargetRepos:   repos,
		OrgScope:      scope == githubapp.ScopeOrg,
		RunnerLabel:   cfg.RunnerLabel,
		StackLabel:    server.StackLabel(cfg.Region, cfg.RunnerJobName),
		MaxConcurrent: cfg.MaxConcurrentRunners,
		RunnerGroupID: cfg.RunnerGroupID,
		MediatorURL:   cfg.MediatorURL,
		TaskTimeout:   cfg.TaskTimeout,
		WebhookSecret: func(ctx context.Context) ([]byte, error) { return sm.Access(ctx, cfg.WebhookSecretName) },
		GitHub:        gh,
		Launcher: &launcher.JobsLauncher{
			BaseURL: ep.CloudRun, Tokens: tokens,
			Project: cfg.GCPProject, Region: cfg.Region, Job: cfg.RunnerJobName,
			MediatorURL: cfg.MediatorURL,
		},
		Claims: claim.NewStore(cfg.ClaimTTL, nil),
		ReconcileOIDC: &oidc.Verifier{
			CertsURL: ep.GoogleCerts,
			Audience: cfg.MediatorURL,
			Email:    saEmail(cfg.SchedulerSAName, cfg.GCPProject),
		},
		// Claim contract v2: the runner proves it runs as EXEC_SA_NAME.
		RunnerOIDC: &oidc.Verifier{
			CertsURL: ep.GoogleCerts,
			Audience: cfg.MediatorURL,
			Email:    saEmail(cfg.ExecSAName, cfg.GCPProject),
		},
		Logger: logger,
	})
}

func saEmail(name, project string) string {
	return name + "@" + project + ".iam.gserviceaccount.com"
}

// cachedKey loads and parses the App private key once; a failed load is
// retried on the next call. Rotating the key needs a restart (operations.md).
func cachedKey(sm *gcp.SecretManager, name string) func(context.Context) (*rsa.PrivateKey, error) {
	var mu sync.Mutex
	var key *rsa.PrivateKey
	return func(ctx context.Context) (*rsa.PrivateKey, error) {
		mu.Lock()
		defer mu.Unlock()
		if key != nil {
			return key, nil
		}
		pemBytes, err := sm.Access(ctx, name)
		if err != nil {
			return nil, err
		}
		k, err := githubapp.ParsePrivateKey(pemBytes)
		if err != nil {
			return nil, err
		}
		key = k
		return key, nil
	}
}

func run(getenv func(string) string, logOut io.Writer) error {
	logger := logging.New(logOut, slog.LevelInfo)
	cfg, err := Load(getenv)
	if err != nil {
		return err
	}
	if cfg.MediatorURL == "" {
		logger.Warn("MEDIATOR_URL is empty: /webhook and /reconcile answer 503 until it is set and the service is redeployed",
			"event", logging.EventStartup)
	}
	logger.Info("config loaded", "event", logging.EventStartup,
		"target_repos", len(cfg.TargetRepos),
		"install_scope", string(cfg.InstallScope),
		"runner_label", cfg.RunnerLabel,
		"runner_job", cfg.RunnerJobName,
		"max_concurrent_runners", cfg.MaxConcurrentRunners,
		"claim_ttl", cfg.ClaimTTL.String())

	port := getenv("PORT") // set by Cloud Run
	if port == "" {
		port = "8080"
	}
	srv := &http.Server{
		Handler:           newServer(cfg, defaultEndpoints(), logger).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	ln, err := net.Listen("tcp", ":"+port)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return serve(ctx, srv, ln, logger)
}

// serve runs srv until ctx is cancelled, then drains in-flight requests
// (Cloud Run sends SIGTERM and allows 10 s) before returning.
func serve(ctx context.Context, srv *http.Server, ln net.Listener, logger *slog.Logger) error {
	drained := make(chan error, 1)
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 9*time.Second)
		defer cancel()
		drained <- srv.Shutdown(shutdownCtx)
	}()

	logger.Info("listening", "event", logging.EventStartup, "addr", ln.Addr().String())
	if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	// Serve returns as soon as Shutdown starts; wait for the drain.
	return <-drained
}
