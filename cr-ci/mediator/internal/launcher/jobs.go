package launcher

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// DefaultRunURL is the Cloud Run Admin API origin.
const DefaultRunURL = "https://run.googleapis.com"

const (
	execPageSize = 100
	maxExecPages = 50
	maxBody      = 10 << 20
	errBodySize  = 1024
)

// servedRepoRE is one GH_REPOS entry exactly as cr-ci/runner/entrypoint.sh accepts it
// (its pattern without the list repetition), and maxServedRepos is the entrypoint's cap
// on the list. A launch the entrypoint would refuse (exit 2 at startup) fails here
// instead. TestServedReposMatchEntrypoint keeps both in step with the entrypoint.
var servedRepoRE = regexp.MustCompile(`^[A-Za-z0-9-]+/[A-Za-z0-9._-]+$`)

const maxServedRepos = 100

// TokenSource returns an OAuth2 access token for Google APIs.
type TokenSource interface {
	Token(ctx context.Context) (string, error)
}

// JobsLauncher runs one execution of a pre-created Cloud Run Job per GitHub
// job, overriding only the container env.
type JobsLauncher struct {
	BaseURL     string // default DefaultRunURL
	HTTP        *http.Client
	Tokens      TokenSource
	Project     string
	Region      string
	Job         string
	MediatorURL string // https origin, no trailing slash
	Now         func() time.Time
}

var (
	_ RunnerLauncher  = (*JobsLauncher)(nil)
	_ ExecutionLister = (*JobsLauncher)(nil)
)

func (l *JobsLauncher) jobName() string {
	return "projects/" + l.Project + "/locations/" + l.Region + "/jobs/" + l.Job
}

func (l *JobsLauncher) now() time.Time {
	if l.Now != nil {
		return l.Now()
	}
	return time.Now()
}

type envVar struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// Launch calls jobs.run with env overrides RUNNER_CLAIM_TOKEN, MEDIATOR_URL,
// GH_JOB_ID, RUNNER_NAME and GH_REPOS, and returns the execution resource name.
func (l *JobsLauncher) Launch(ctx context.Context, spec LaunchSpec) (string, error) {
	if spec.ClaimToken == "" || spec.RunnerName == "" || spec.JobID == 0 || len(spec.ServedRepos) == 0 || l.MediatorURL == "" {
		return "", errors.New("launch: incomplete launch spec")
	}
	if len(spec.ServedRepos) > maxServedRepos {
		return "", fmt.Errorf("launch: %d served repositories, at most %d (the runner's GH_REPOS cap)", len(spec.ServedRepos), maxServedRepos)
	}
	for _, r := range spec.ServedRepos {
		if !servedRepoRE.MatchString(r) {
			return "", fmt.Errorf("launch: served repository %q is not owner/name", r)
		}
	}
	body := map[string]any{
		"overrides": map[string]any{
			"taskCount": 1,
			"containerOverrides": []any{map[string]any{
				"env": []envVar{
					{EnvClaimToken, spec.ClaimToken},
					{EnvMediatorURL, l.MediatorURL},
					{EnvJobID, strconv.FormatInt(spec.JobID, 10)},
					{EnvRunnerName, spec.RunnerName},
					{EnvRepos, strings.Join(spec.ServedRepos, ",")},
				},
			}},
		},
	}
	var op struct {
		Name     string `json:"name"`
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
	}
	if err := l.call(ctx, http.MethodPost, "/v2/"+l.jobName()+":run", body, &op); err != nil {
		return "", err
	}
	if op.Metadata.Name == "" {
		// The operation metadata is the Execution; without its name we can
		// still find the execution by GH_JOB_ID later.
		return "", nil
	}
	return op.Metadata.Name, nil
}

// Terminate cancels the execution of runner runnerName.
func (l *JobsLauncher) Terminate(ctx context.Context, runnerName, ref string) error {
	if ref == "" {
		active, err := l.Active(ctx)
		if err != nil {
			return err
		}
		for _, e := range active {
			if e.RunnerName == runnerName {
				ref = e.Ref
				break
			}
		}
		if ref == "" {
			return nil
		}
	}
	return l.cancel(ctx, ref)
}

func (l *JobsLauncher) cancel(ctx context.Context, ref string) error {
	if !strings.HasPrefix(ref, l.jobName()+"/executions/") || strings.ContainsAny(ref[len(l.jobName()):], "?#:") {
		return fmt.Errorf("cancel: %q is not an execution of %s", ref, l.jobName())
	}
	err := l.call(ctx, http.MethodPost, "/v2/"+ref+":cancel", map[string]any{}, nil)
	var ae *apiError
	if errors.As(err, &ae) && (ae.status == http.StatusNotFound ||
		(ae.status == http.StatusBadRequest && strings.Contains(ae.body, "FAILED_PRECONDITION"))) {
		// Gone, or already finished (Cloud Run answers FAILED_PRECONDITION).
		return nil
	}
	return err
}

// ReapOrphans cancels active executions created more than maxAge ago.
func (l *JobsLauncher) ReapOrphans(ctx context.Context, maxAge time.Duration) (int, error) {
	active, err := l.Active(ctx)
	if err != nil {
		return 0, err
	}
	cutoff := l.now().Add(-maxAge)
	n := 0
	var errs []error
	for _, e := range active {
		if e.Created.IsZero() || !e.Created.Before(cutoff) {
			continue
		}
		if err := l.cancel(ctx, e.Ref); err != nil {
			errs = append(errs, err)
			continue
		}
		n++
	}
	return n, errors.Join(errs...)
}

// Active lists the job's executions that have no completion time.
func (l *JobsLauncher) Active(ctx context.Context) ([]Execution, error) {
	all, err := l.Executions(ctx)
	if err != nil {
		return nil, err
	}
	var active []Execution
	for _, e := range all {
		if !e.Done {
			active = append(active, e)
		}
	}
	return active, nil
}

// Executions lists all of the job's executions.
func (l *JobsLauncher) Executions(ctx context.Context) ([]Execution, error) {
	var active []Execution
	pageToken := ""
	for page := 0; page < maxExecPages; page++ {
		q := url.Values{"pageSize": {strconv.Itoa(execPageSize)}}
		if pageToken != "" {
			q.Set("pageToken", pageToken)
		}
		var out struct {
			Executions []struct {
				Name           string    `json:"name"`
				CreateTime     time.Time `json:"createTime"`
				CompletionTime string    `json:"completionTime"`
				Template       struct {
					Containers []struct {
						Env []envVar `json:"env"`
					} `json:"containers"`
				} `json:"template"`
			} `json:"executions"`
			NextPageToken string `json:"nextPageToken"`
		}
		if err := l.call(ctx, http.MethodGet, "/v2/"+l.jobName()+"/executions?"+q.Encode(), nil, &out); err != nil {
			return nil, err
		}
		for _, e := range out.Executions {
			ex := Execution{Ref: e.Name, Created: e.CreateTime, Done: e.CompletionTime != ""}
			for _, c := range e.Template.Containers {
				for _, v := range c.Env {
					switch v.Name {
					case EnvJobID:
						ex.JobID, _ = strconv.ParseInt(v.Value, 10, 64)
					case EnvRunnerName:
						ex.RunnerName = v.Value
					}
				}
			}
			active = append(active, ex)
		}
		if out.NextPageToken == "" {
			return active, nil
		}
		pageToken = out.NextPageToken
	}
	// Fail closed: an incomplete list could exceed capacity.
	return nil, fmt.Errorf("cloud run: more than %d pages of executions for %s", maxExecPages, l.jobName())
}

type apiError struct {
	method, path string
	status       int
	body         string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("cloud run %s %s: HTTP %d: %s", e.method, e.path, e.status, e.body)
}

func (l *JobsLauncher) call(ctx context.Context, method, path string, body, out any) error {
	tok, err := l.Tokens.Token(ctx)
	if err != nil {
		return err
	}
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	base := l.BaseURL
	if base == "" {
		base = DefaultRunURL
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimSuffix(base, "/")+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	hc := l.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	short := path
	if i := strings.IndexByte(short, '?'); i >= 0 {
		short = short[:i]
	}
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("cloud run %s %s: %w", method, short, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, errBodySize))
		return &apiError{method: method, path: short, status: resp.StatusCode, body: strings.TrimSpace(string(b))}
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBody))
		return nil
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBody)).Decode(out); err != nil {
		return fmt.Errorf("cloud run %s %s: decoding response failed", method, short)
	}
	return nil
}
