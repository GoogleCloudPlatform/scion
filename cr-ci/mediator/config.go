package main

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Environment variable names. They form the cr-ci config contract and must
// match cr-ci/config.example.env one to one (enforced by a test).
const (
	EnvTargetRepos             = "TARGET_REPOS"
	EnvGitHubAppID             = "GITHUB_APP_ID"
	EnvGitHubInstallScope      = "GITHUB_APP_INSTALLATION_SCOPE"
	EnvGCPProject              = "GCP_PROJECT"
	EnvRegion                  = "REGION"
	EnvRunnerLabel             = "RUNNER_LABEL"
	EnvRunnerJobName           = "RUNNER_JOB_NAME"
	EnvRunnerImageName         = "RUNNER_IMAGE_NAME"
	EnvMediatorImageName       = "MEDIATOR_IMAGE_NAME"
	EnvRunnerCPU               = "RUNNER_CPU"
	EnvRunnerMemory            = "RUNNER_MEMORY"
	EnvRunnerDiskGi            = "RUNNER_DISK_GI"
	EnvMaxConcurrentRunners    = "MAX_CONCURRENT_RUNNERS"
	EnvTaskTimeout             = "TASK_TIMEOUT"
	EnvArtifactRepo            = "ARTIFACT_REPO"
	EnvBuildStagingBucket      = "BUILD_STAGING_BUCKET"
	EnvMediatorServiceName     = "MEDIATOR_SERVICE_NAME"
	EnvReconcilerJobName       = "RECONCILER_JOB_NAME"
	EnvReconcileSchedule       = "RECONCILE_SCHEDULE"
	EnvMediatorURL             = "MEDIATOR_URL"
	EnvRunnerGroupID           = "GITHUB_RUNNER_GROUP_ID"
	EnvRoutingVariableName     = "ROUTING_VARIABLE_NAME"
	EnvBuildSAName             = "BUILD_SA_NAME"
	EnvMediatorSAName          = "MEDIATOR_SA_NAME"
	EnvSchedulerSAName         = "SCHEDULER_SA_NAME"
	EnvExecSAName              = "EXEC_SA_NAME"
	EnvAppPrivateKeySecretName = "GITHUB_APP_PRIVATE_KEY_SECRET_NAME"
	EnvWebhookSecretName       = "GITHUB_WEBHOOK_SECRET_NAME"
	EnvClaimTTL                = "CLAIM_TTL"
	EnvRunnerIdleTimeout       = "RUNNER_IDLE_TIMEOUT"
	EnvMediatorMinInstances    = "MEDIATOR_MIN_INSTANCES"
	EnvRunnerBuildCacheInRAM   = "RUNNER_BUILD_CACHE_IN_RAM"
)

// Cloud Run limits and contract defaults.
const (
	maxOwnerLen            = 39
	maxTargetRepos         = 100 // the runner entrypoint's GH_REPOS cap
	maxCloudRunTaskTimeout = 168 * time.Hour
	// maxEphemeralDiskGi: the per-volume maximum Cloud Run documents. Above
	// 10 GiB per instance needs the per-instance quota
	// run.googleapis.com/max_per_instance_ephemeral_disk raised first (the
	// server rejects the job otherwise); see docs/gcp-setup.md, "Quotas".
	maxEphemeralDiskGi       = 100
	maxCloudRunMemoryMi      = 32 * 1024
	minCloudRunMemoryMi      = 512
	defaultRunnerLabel       = "scion-cr-8c32g"
	defaultRunnerJobName     = "cr-gh-runner-8c32g"
	defaultRunnerImageName   = "gh-runner"
	defaultMediatorImageName = "cr-runner-mediator"
	defaultRunnerCPU         = "8"
	defaultRunnerMemory      = "32Gi"
	defaultRunnerDiskGi      = "10"
	defaultMaxConcurrent     = "5"
	defaultTaskTimeout       = "45m"
	defaultArtifactRepo      = "cr-runners"
	defaultMediatorService   = "cr-runner-mediator"
	defaultReconcilerJobName = "cr-runner-reconciler"
	defaultReconcileSchedule = "*/5 * * * *"
	defaultRunnerGroupID     = "1"
	defaultRoutingVariable   = "SCION_LARGE_RUNNER"
	defaultBuildSA           = "cr-runner-build-sa"
	defaultMediatorSA        = "cr-runner-mediator-sa"
	defaultSchedulerSA       = "cr-runner-scheduler-sa"
	defaultExecSA            = "cr-runner-exec-sa"
	defaultAppKeySecret      = "cr-runner-gh-app-private-key"
	defaultWebhookSecret     = "cr-runner-gh-webhook-secret"
	defaultClaimTTL          = "300s"
	defaultRunnerIdleTimeout = "10m"
	minRunnerIdleTimeout     = time.Minute
	minClaimTTL              = 30 * time.Second
	maxClaimTTL              = time.Hour
)

// InstallScope is where the GitHub App is installed and, therefore, which
// runner endpoints (repo or org) the mediator calls.
type InstallScope string

const (
	ScopeRepo InstallScope = "repo"
	ScopeOrg  InstallScope = "org"
)

// Repo is an owner/name pair from TARGET_REPOS.
type Repo struct {
	Owner string
	Name  string
}

func (r Repo) String() string { return r.Owner + "/" + r.Name }

// Config is the validated cr-ci configuration. It holds resource NAMES only;
// secret values are never part of the configuration.
type Config struct {
	TargetRepos             []Repo
	GitHubAppID             int64
	InstallScope            InstallScope
	GCPProject              string
	Region                  string
	RunnerLabel             string
	RunnerJobName           string
	RunnerImageName         string
	MediatorImageName       string
	RunnerCPU               int
	RunnerMemory            string
	RunnerMemoryMi          int
	RunnerDiskGi            int
	MaxConcurrentRunners    int
	TaskTimeout             time.Duration
	ArtifactRepo            string
	BuildStagingBucket      string // empty: <GCP_PROJECT>-cr-ci-staging (used by the deploy scripts only)
	MediatorServiceName     string
	ReconcilerJobName       string
	ReconcileSchedule       string
	MediatorURL             string // empty until the first deploy assigns one
	RunnerGroupID           int64
	RoutingVariableName     string
	BuildSAName             string
	MediatorSAName          string
	SchedulerSAName         string
	ExecSAName              string
	AppPrivateKeySecretName string
	WebhookSecretName       string
	ClaimTTL                time.Duration
	RunnerIdleTimeout       time.Duration // used by deploy-job.sh and the runner entrypoint only
	MediatorMinInstances    int           // used by deploy-mediator.sh only: 0 or 1
	RunnerBuildCacheInRAM   bool          // used by deploy-job.sh and the runner entrypoint only
}

// Defaults returns the default value of every optional variable. Required
// variables (no safe default) map to "".
func Defaults() map[string]string {
	return map[string]string{
		EnvTargetRepos:             "",
		EnvGitHubAppID:             "",
		EnvGitHubInstallScope:      string(ScopeRepo),
		EnvGCPProject:              "",
		EnvRegion:                  "",
		EnvRunnerLabel:             defaultRunnerLabel,
		EnvRunnerJobName:           defaultRunnerJobName,
		EnvRunnerImageName:         defaultRunnerImageName,
		EnvMediatorImageName:       defaultMediatorImageName,
		EnvRunnerCPU:               defaultRunnerCPU,
		EnvRunnerMemory:            defaultRunnerMemory,
		EnvRunnerDiskGi:            defaultRunnerDiskGi,
		EnvMaxConcurrentRunners:    defaultMaxConcurrent,
		EnvTaskTimeout:             defaultTaskTimeout,
		EnvArtifactRepo:            defaultArtifactRepo,
		EnvBuildStagingBucket:      "",
		EnvMediatorServiceName:     defaultMediatorService,
		EnvReconcilerJobName:       defaultReconcilerJobName,
		EnvReconcileSchedule:       defaultReconcileSchedule,
		EnvMediatorURL:             "",
		EnvRunnerGroupID:           defaultRunnerGroupID,
		EnvRoutingVariableName:     defaultRoutingVariable,
		EnvBuildSAName:             defaultBuildSA,
		EnvMediatorSAName:          defaultMediatorSA,
		EnvSchedulerSAName:         defaultSchedulerSA,
		EnvExecSAName:              defaultExecSA,
		EnvAppPrivateKeySecretName: defaultAppKeySecret,
		EnvWebhookSecretName:       defaultWebhookSecret,
		EnvClaimTTL:                defaultClaimTTL,
		EnvRunnerIdleTimeout:       defaultRunnerIdleTimeout,
		EnvMediatorMinInstances:    "0",
		EnvRunnerBuildCacheInRAM:   "false",
	}
}

// Keys returns every variable name in the config contract, sorted.
func Keys() []string {
	d := Defaults()
	keys := make([]string, 0, len(d))
	for k := range d {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

var (
	// GitHub user/org names: alphanumerics and single hyphens, no leading or
	// trailing hyphen. The 39-character limit is checked separately. The runner's
	// shell checks (entrypoint.sh GH_REPOS, cr-ci-job-guard.sh) use the looser owner
	// part [A-Za-z0-9-]+: config is the strict side, so every owner it accepts passes
	// them (TestOwnerRuleWithinRunnerPattern pins this).
	ownerRE = regexp.MustCompile(`^[A-Za-z0-9](?:-?[A-Za-z0-9])*$`)
	repoRE  = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)
	// GCP project IDs: 6-30 chars, lowercase letter first, no trailing hyphen.
	projectRE = regexp.MustCompile(`^[a-z][a-z0-9-]{4,28}[a-z0-9]$`)
	regionRE  = regexp.MustCompile(`^[a-z]+-[a-z]+[0-9]+$`)
	// Runner labels: no whitespace or commas (runs-on lists are comma separated).
	labelRE = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)
	// Cloud Run service and job names.
	cloudRunNameRE = regexp.MustCompile(`^[a-z](?:[a-z0-9-]{0,47}[a-z0-9])?$`)
	// Artifact Registry repository IDs and Cloud Scheduler job IDs (kept to
	// the stricter lowercase form so one rule covers both).
	resourceIDRE = regexp.MustCompile(`^[a-z](?:[a-z0-9-]{0,61}[a-z0-9])?$`)
	// Cloud Storage bucket names (3-63 chars, no dots: dotted names need
	// domain verification).
	bucketRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{1,61}[a-z0-9]$`)
	// Docker image names (single path component).
	imageNameRE = regexp.MustCompile(`^[a-z0-9]+(?:[._-][a-z0-9]+)*$`)
	// Service account IDs: 6-30 chars.
	saRE = regexp.MustCompile(`^[a-z][a-z0-9-]{4,28}[a-z0-9]$`)
	// Secret Manager secret IDs.
	secretRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,255}$`)
	// GitHub Actions configuration variable names.
	variableRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	memoryRE   = regexp.MustCompile(`^([0-9]+)(Mi|Gi)$`)
	// Unix cron: five fields of digits and * , - / (no names or macros).
	cronRE = regexp.MustCompile(`^[0-9*,/-]+( [0-9*,/-]+){4}$`)
)

// reservedLabels are the default labels every self-hosted runner carries.
// Routing on one of them would let any self-hosted runner pick up the job.
var reservedLabels = map[string]bool{"self-hosted": true, "linux": true, "x64": true, "arm64": true}

// maxMemoryMiByCPU is the Cloud Run Jobs memory ceiling per CPU count
// (https://docs.cloud.google.com/run/docs/configuring/jobs/cpu).
var maxMemoryMiByCPU = map[int]int{1: 4 * 1024, 2: 8 * 1024, 4: 16 * 1024, 6: 24 * 1024, 8: 32 * 1024}

// minMemoryMiByCPU is the Cloud Run Jobs memory floor per CPU count.
var minMemoryMiByCPU = map[int]int{1: 512, 2: 512, 4: 2 * 1024, 6: 4 * 1024, 8: 4 * 1024}

// Load reads and validates the config contract via getenv (os.Getenv in
// production). Unset or empty variables take their default. All problems are
// reported together.
func Load(getenv func(string) string) (*Config, error) {
	defaults := Defaults()
	get := func(key string) string {
		if v := strings.TrimSpace(getenv(key)); v != "" {
			return v
		}
		return defaults[key]
	}

	var errs []error
	fail := func(key, format string, args ...any) {
		errs = append(errs, fmt.Errorf("%s: %s", key, fmt.Sprintf(format, args...)))
	}
	required := func(key string) string {
		v := get(key)
		if v == "" {
			fail(key, "required")
		}
		return v
	}
	match := func(key string, re *regexp.Regexp, what string) string {
		v := get(key)
		if v != "" && !re.MatchString(v) {
			fail(key, "%q is not a valid %s", v, what)
		}
		return v
	}
	posInt := func(key string) int {
		v := get(key)
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			fail(key, "%q is not a positive integer", v)
			return 0
		}
		return n
	}

	c := &Config{}

	if raw := required(EnvTargetRepos); raw != "" {
		c.TargetRepos = parseRepos(raw, func(format string, args ...any) { fail(EnvTargetRepos, format, args...) })
	}

	if raw := required(EnvGitHubAppID); raw != "" {
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || id < 1 {
			fail(EnvGitHubAppID, "%q is not a positive integer", raw)
		}
		c.GitHubAppID = id
	}

	switch scope := InstallScope(get(EnvGitHubInstallScope)); scope {
	case ScopeRepo, ScopeOrg:
		c.InstallScope = scope
	default:
		fail(EnvGitHubInstallScope, "%q must be %q or %q", scope, ScopeRepo, ScopeOrg)
	}
	if c.InstallScope == ScopeOrg && len(c.TargetRepos) > 0 {
		owner := c.TargetRepos[0].Owner
		for _, r := range c.TargetRepos[1:] {
			if !strings.EqualFold(r.Owner, owner) {
				fail(EnvGitHubInstallScope, "org scope uses org-level runner endpoints for a single org, but %s has owner %q, not %q; use repo scope or a separate deployment per org", r, r.Owner, owner)
			}
		}
	}

	required(EnvGCPProject)
	c.GCPProject = match(EnvGCPProject, projectRE, "GCP project ID")
	required(EnvRegion)
	c.Region = match(EnvRegion, regionRE, "GCP region")

	c.RunnerLabel = match(EnvRunnerLabel, labelRE, "runner label")
	if reservedLabels[strings.ToLower(c.RunnerLabel)] {
		fail(EnvRunnerLabel, "%q is a default self-hosted runner label; use a unique label", c.RunnerLabel)
	}
	c.RunnerJobName = match(EnvRunnerJobName, cloudRunNameRE, "Cloud Run job name")
	c.RunnerImageName = match(EnvRunnerImageName, imageNameRE, "image name")
	c.MediatorImageName = match(EnvMediatorImageName, imageNameRE, "image name")
	checkDistinct(fail, "image", map[string]string{
		EnvRunnerImageName: c.RunnerImageName, EnvMediatorImageName: c.MediatorImageName,
	})

	c.RunnerCPU = posInt(EnvRunnerCPU)
	if _, ok := maxMemoryMiByCPU[c.RunnerCPU]; c.RunnerCPU != 0 && !ok {
		fail(EnvRunnerCPU, "%d is not a Cloud Run Jobs CPU value (1, 2, 4, 6 or 8)", c.RunnerCPU)
	}
	c.RunnerMemory = get(EnvRunnerMemory)
	if mi, err := parseMemoryMi(c.RunnerMemory); err != nil {
		fail(EnvRunnerMemory, "%v", err)
	} else {
		c.RunnerMemoryMi = mi
		if lo, hi, ok := memoryBounds(c.RunnerCPU); ok && (mi < lo || mi > hi) {
			fail(EnvRunnerMemory, "%s is outside the %d-CPU range %dMi-%dMi", c.RunnerMemory, c.RunnerCPU, lo, hi)
		}
	}

	c.RunnerDiskGi = posInt(EnvRunnerDiskGi)
	if c.RunnerDiskGi > maxEphemeralDiskGi {
		fail(EnvRunnerDiskGi, "%d exceeds the %d GiB ephemeral-disk maximum", c.RunnerDiskGi, maxEphemeralDiskGi)
	}
	c.MaxConcurrentRunners = posInt(EnvMaxConcurrentRunners)
	switch raw := get(EnvRunnerBuildCacheInRAM); raw {
	case "true":
		c.RunnerBuildCacheInRAM = true
	case "false":
	default:
		fail(EnvRunnerBuildCacheInRAM, "%q must be true or false", raw)
	}
	// max-instances is always 1 (claims live in memory), so min-instances is 0
	// (scale to zero) or 1 (always warm: no cold-start 429s on webhooks).
	switch raw := get(EnvMediatorMinInstances); raw {
	case "0":
	case "1":
		c.MediatorMinInstances = 1
	default:
		fail(EnvMediatorMinInstances, "%q must be 0 or 1 (max-instances is always 1)", raw)
	}
	// The in-RAM cache (~9.5 GB) comes on top of the lint's ~21 GB compile (P6 PoC
	// run 4: 27.9 GB of 32 GiB), so only the 32 GiB shape has room for it.
	if c.RunnerBuildCacheInRAM && c.RunnerMemoryMi != 0 && c.RunnerMemoryMi < maxCloudRunMemoryMi {
		fail(EnvRunnerBuildCacheInRAM, "true needs RUNNER_MEMORY=32Gi (the in-RAM build cache plus the compile need ~28 GB), got %s", c.RunnerMemory)
	}

	if raw := get(EnvTaskTimeout); raw != "" {
		d, err := time.ParseDuration(raw)
		switch {
		case err != nil:
			fail(EnvTaskTimeout, "%q is not a duration (for example 45m)", raw)
		case d < time.Minute || d > maxCloudRunTaskTimeout:
			fail(EnvTaskTimeout, "%s must be between 1m and %s", d, maxCloudRunTaskTimeout)
		default:
			c.TaskTimeout = d
		}
	}

	// An idle runner (no job started) stops after RUNNER_IDLE_TIMEOUT, so a job that
	// GitHub hands to an already-running runner still inherits at least
	// TASK_TIMEOUT - RUNNER_IDLE_TIMEOUT of the execution's budget.
	if raw := get(EnvRunnerIdleTimeout); raw != "" {
		d, err := time.ParseDuration(raw)
		switch {
		case err != nil:
			fail(EnvRunnerIdleTimeout, "%q is not a duration (for example 10m)", raw)
		case d < minRunnerIdleTimeout:
			fail(EnvRunnerIdleTimeout, "%s must be at least %s", d, minRunnerIdleTimeout)
		case c.TaskTimeout > 0 && d > c.TaskTimeout/2:
			fail(EnvRunnerIdleTimeout, "%s must be at most half of TASK_TIMEOUT (%s), so a job taken by an idle runner keeps most of the budget", d, c.TaskTimeout)
		default:
			c.RunnerIdleTimeout = d
		}
	}

	if raw := get(EnvClaimTTL); raw != "" {
		d, err := time.ParseDuration(raw)
		switch {
		case err != nil:
			fail(EnvClaimTTL, "%q is not a duration (for example 300s)", raw)
		case d < minClaimTTL || d > maxClaimTTL:
			fail(EnvClaimTTL, "%s must be between %s and %s", d, minClaimTTL, maxClaimTTL)
		default:
			c.ClaimTTL = d
		}
	}

	c.ArtifactRepo = match(EnvArtifactRepo, resourceIDRE, "Artifact Registry repository ID")
	c.BuildStagingBucket = match(EnvBuildStagingBucket, bucketRE, "Cloud Storage bucket name")
	// Cloud Storage forbids the "goog" prefix and "google" anywhere in a name.
	if strings.HasPrefix(c.BuildStagingBucket, "goog") || strings.Contains(c.BuildStagingBucket, "google") ||
		strings.Contains(c.BuildStagingBucket, "..") {
		fail(EnvBuildStagingBucket, "%q is not a valid Cloud Storage bucket name", c.BuildStagingBucket)
	}
	c.MediatorServiceName = match(EnvMediatorServiceName, cloudRunNameRE, "Cloud Run service name")
	c.ReconcilerJobName = match(EnvReconcilerJobName, resourceIDRE, "Cloud Scheduler job ID")
	c.ReconcileSchedule = match(EnvReconcileSchedule, cronRE, "five-field cron schedule")
	if raw := get(EnvMediatorURL); raw != "" {
		if u, err := url.Parse(raw); err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil ||
			(u.Path != "" && u.Path != "/") || strings.ContainsAny(raw, "?#") {
			fail(EnvMediatorURL, "%q must be a bare https:// origin such as https://cr-runner-mediator-abc123-uc.a.run.app", raw)
		} else {
			c.MediatorURL = strings.TrimSuffix(raw, "/")
		}
	}
	if raw := get(EnvRunnerGroupID); raw != "" {
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || id < 1 {
			fail(EnvRunnerGroupID, "%q is not a positive integer", raw)
		}
		c.RunnerGroupID = id
	}
	c.RoutingVariableName = match(EnvRoutingVariableName, variableRE, "GitHub Actions variable name")
	if strings.HasPrefix(strings.ToUpper(c.RoutingVariableName), "GITHUB_") {
		fail(EnvRoutingVariableName, "%q: names starting with GITHUB_ are reserved by GitHub", c.RoutingVariableName)
	}

	c.BuildSAName = match(EnvBuildSAName, saRE, "service account ID")
	c.MediatorSAName = match(EnvMediatorSAName, saRE, "service account ID")
	c.SchedulerSAName = match(EnvSchedulerSAName, saRE, "service account ID")
	c.ExecSAName = match(EnvExecSAName, saRE, "service account ID")
	checkDistinct(fail, "service account", map[string]string{
		EnvBuildSAName: c.BuildSAName, EnvMediatorSAName: c.MediatorSAName,
		EnvSchedulerSAName: c.SchedulerSAName, EnvExecSAName: c.ExecSAName,
	})

	c.AppPrivateKeySecretName = match(EnvAppPrivateKeySecretName, secretRE, "Secret Manager secret name")
	c.WebhookSecretName = match(EnvWebhookSecretName, secretRE, "Secret Manager secret name")
	checkDistinct(fail, "secret", map[string]string{
		EnvAppPrivateKeySecretName: c.AppPrivateKeySecretName, EnvWebhookSecretName: c.WebhookSecretName,
	})

	if len(errs) > 0 {
		return nil, fmt.Errorf("invalid cr-ci config: %w", errors.Join(errs...))
	}
	return c, nil
}

func parseRepos(raw string, fail func(string, ...any)) []Repo {
	var repos []Repo
	seen := map[string]bool{}
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			fail("empty entry in %q", raw)
			continue
		}
		owner, name, ok := strings.Cut(part, "/")
		if !ok || len(owner) > maxOwnerLen || !ownerRE.MatchString(owner) || !repoRE.MatchString(name) || name == "." || name == ".." {
			fail("%q is not owner/repo", part)
			continue
		}
		key := strings.ToLower(part)
		if seen[key] {
			fail("%q is listed twice", part)
			continue
		}
		seen[key] = true
		repos = append(repos, Repo{Owner: owner, Name: name})
	}
	if len(repos) > maxTargetRepos {
		fail("%d repositories listed, at most %d (each runner's GH_REPOS allowlist carries them all in org scope)", len(repos), maxTargetRepos)
	}
	return repos
}

func parseMemoryMi(v string) (int, error) {
	m := memoryRE.FindStringSubmatch(v)
	if m == nil {
		return 0, fmt.Errorf("%q must be an integer followed by Mi or Gi (for example 32Gi)", v)
	}
	n, err := strconv.Atoi(m[1])
	if m[2] == "Gi" && err == nil {
		// Bound before multiplying so huge values cannot overflow into range.
		if n > maxCloudRunMemoryMi/1024 {
			n = maxCloudRunMemoryMi + 1
		} else {
			n *= 1024
		}
	}
	if err != nil || n < minCloudRunMemoryMi || n > maxCloudRunMemoryMi {
		return 0, fmt.Errorf("%q must be between 512Mi and 32Gi", v)
	}
	return n, nil
}

func memoryBounds(cpu int) (lo, hi int, ok bool) {
	hi, ok = maxMemoryMiByCPU[cpu]
	return minMemoryMiByCPU[cpu], hi, ok
}

func checkDistinct(fail func(string, string, ...any), what string, byKey map[string]string) {
	keys := make([]string, 0, len(byKey))
	for k := range byKey {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	owner := map[string]string{}
	for _, k := range keys {
		v := byKey[k]
		if v == "" {
			continue
		}
		if prev, dup := owner[v]; dup {
			fail(k, "%s name %q is also used by %s; each must be distinct", what, v, prev)
			continue
		}
		owner[v] = k
	}
}
