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
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

var (
	lookPlain    bool
	lookFull     bool
	lookNumLines int
)

// buildLookCmd builds the command for tmux capture-pane based on flags.
func buildLookCmd(plain, full bool, numLines int) []string {
	flags := "-p"
	if !plain {
		flags += "e"
	}

	args := []string{"tmux", "capture-pane"}
	if numLines > 0 {
		args = append(args, flags+"S", fmt.Sprintf("-%d", numLines))
	} else if full {
		args = append(args, flags+"S", "-")
	} else {
		args = append(args, flags)
	}
	args = append(args, "-t", "scion")
	return args
}

// lookCmd represents the look command
var lookCmd = &cobra.Command{
	Use:               "look <agent>",
	Short:             "View an agent's current terminal output",
	Args:              cobra.ExactArgs(1),
	ValidArgsFunction: getAgentNames,
	RunE: func(cmd *cobra.Command, args []string) error {
		agentName := api.Slugify(args[0])

		execCmd := buildLookCmd(lookPlain, lookFull, lookNumLines)

		hubCtx, err := requireHub(projectPath)
		if err != nil {
			return err
		}

		return lookViaHub(hubCtx, agentName, execCmd)
	},
}

// printLookOutput prints the captured terminal output, optionally wrapped
// with top/bottom borders sized to the current terminal width.
func printLookOutput(output string) {
	if IsNonInteractive() || lookPlain {
		fmt.Print(output)
		return
	}

	width, _, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil || width <= 0 {
		// Fallback: no border when we can't determine terminal width.
		fmt.Print(output)
		return
	}

	border := func(ch string) string {
		return strings.Repeat(ch, width)
	}

	fmt.Println(border("⌄"))
	fmt.Println()
	fmt.Print(output)
	fmt.Println()
	fmt.Println()
	fmt.Println(border("^"))
}

func lookViaHub(hubCtx *HubContext, agentName string, execCmd []string) error {
	PrintUsingHub(hubCtx.Endpoint)

	projectID, err := GetProjectID(hubCtx)
	if err != nil {
		return wrapHubError(err)
	}

	agentSvc := hubCtx.Client.ProjectAgents(projectID)

	// Fetch the identity header alongside Exec rather than after it, so the
	// header's round trips do not delay the terminal output. The channel is
	// buffered so the fetch never blocks when Exec fails and nobody reads it.
	headerCtx, cancelHeader := context.WithTimeout(context.Background(), lookIdentityTimeout)
	defer cancelHeader()
	headerCh := make(chan string, 1)
	showAuthority := cliShowsAgentAuthority(resolveMode())
	go func() {
		headerCh <- fetchLookIdentityHeader(headerCtx, hubCtx.Client, agentSvc, projectID, agentName, showAuthority)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	resp, err := agentSvc.Exec(ctx, agentName, execCmd, 10)
	if err != nil {
		if isAgentNotRunningError(err) {
			return lookSavedScreenFallback(ctx, agentSvc, agentName, headerCh, headerCtx, err)
		}
		return wrapHubError(fmt.Errorf("failed to capture terminal output for agent '%s': %w", agentName, err))
	}
	if resp.ExitCode != 0 {
		// The capture failed inside the agent (no tmux session, ssh exit 255,
		// ...). The output is the error text, not pane content, so keep it
		// off stdout.
		return lookCaptureError(agentName, resp.ExitCode, resp.Output)
	}

	// The header goes to stderr, so stdout keeps only the captured terminal
	// output that scripts parse. It is skipped silently once its timeout
	// passes.
	printLookIdentityHeader(headerCh, headerCtx)
	printLookOutput(resp.Output)
	return nil
}

func printLookIdentityHeader(headerCh <-chan string, headerCtx context.Context) {
	select {
	case header := <-headerCh:
		if header != "" {
			fmt.Fprintln(os.Stderr, header)
		}
	case <-headerCtx.Done():
	}
}

func isAgentNotRunningError(err error) bool {
	var apiErr *apiclient.APIError
	return errors.As(err, &apiErr) && apiErr.Code == "agent_not_running"
}

// ansiEscapeRe strips ANSI CSI, OSC, and simple 2-byte ESC sequences for
// `scion look --plain` when rendering a saved ANSI screen.
var ansiEscapeRe = regexp.MustCompile(`\x1b(?:\[[0-9;?]*[ -/]*[@-~]|\][^\x07\x1b]*(?:\x07|\x1b\\)|[@-Z\\-_])`)

func stripANSI(s string) string {
	return ansiEscapeRe.ReplaceAllString(s, "")
}

func formatLookRelativeAge(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// lookNow is a hook for deterministic relative-age formatting in tests.
var lookNow = time.Now

func lookSavedScreenFallback(ctx context.Context, agentSvc hubclient.AgentService, agentName string, headerCh <-chan string, headerCtx context.Context, execErr error) error {
	screenCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = ctx

	screen, err := agentSvc.GetScreen(screenCtx, agentName)
	if err != nil {
		if apiclient.IsNotFoundError(err) {
			return wrapHubError(fmt.Errorf("failed to capture terminal output for agent '%s': %w\nno saved screen for agent '%s' (it was not captured at suspend)", agentName, execErr, agentName))
		}
		return wrapHubError(fmt.Errorf("failed to capture terminal output for agent '%s': %w", agentName, execErr))
	}

	printLookIdentityHeader(headerCh, headerCtx)

	phase := screen.Phase
	if phase == "" {
		phase = "not running"
	}
	capturedLocal := screen.CapturedAt.Local()
	age := formatLookRelativeAge(lookNow().Sub(screen.CapturedAt))
	banner := fmt.Sprintf("Agent '%s' is %s. Showing its screen saved at %s (%s ago), before it was suspended.",
		agentName, phase, capturedLocal.Format("2006-01-02 15:04:05"), age)
	if screen.Truncated {
		banner += " (truncated to the last 64 KiB)"
	}
	fmt.Fprintln(os.Stderr, banner)

	if lookFull || lookNumLines > 0 {
		fmt.Fprintln(os.Stderr, "scrollback is not available for a saved screen; showing the saved visible screen")
	}

	content := screen.Content
	if lookPlain {
		content = stripANSI(content)
	}
	printLookOutput(content)
	return nil
}

// lookCaptureError reports a capture that exited non-zero, quoting the first
// non-empty line of its output.
func lookCaptureError(agentName string, exitCode int, output string) error {
	msg := fmt.Sprintf("capturing terminal for agent '%s' failed (exit %d)", agentName, exitCode)
	for _, line := range strings.Split(output, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return fmt.Errorf("%s: %s", msg, line)
		}
	}
	return errors.New(msg)
}

// lookIdentityTimeout bounds the identity header fetch. It runs alongside
// Exec, so it delays the terminal output only when it outlasts Exec. A var
// so tests can shorten it.
var lookIdentityTimeout = 3 * time.Second

// fetchLookIdentityHeader returns the header lines for scion look: the
// agent's GCP identity and, with showAuthority, the cause line of an
// orphaned or revoked agent (authorityCauseLine). It is best effort: when
// the agent cannot be read, or there is nothing to show, it returns "".
func fetchLookIdentityHeader(ctx context.Context, client hubclient.Client, agentSvc hubclient.AgentService, projectID, agentName string, showAuthority bool) string {
	agent, err := agentSvc.Get(ctx, agentName)
	if err != nil || agent == nil {
		return ""
	}
	var authorityLine string
	if showAuthority {
		authorityLine = authorityCauseLine(agent.Authority)
	}
	id := agentGCPIdentity(*agent)
	if id == nil {
		return authorityLine
	}
	if id.ServiceAccountID != "" {
		pid := agent.ProjectID
		if pid == "" {
			pid = projectID
		}
		id.DisplayName = serviceAccountDisplayNames(ctx, client, []string{pid})[id.ServiceAccountID]
	}
	profile := ""
	if agent.AppliedConfig != nil {
		profile = agent.AppliedConfig.Profile
	}
	header := lookIdentityHeader(id, profile)
	switch {
	case authorityLine == "":
		return header
	case header == "":
		return authorityLine
	}
	return header + "\n" + authorityLine
}

func init() {
	lookCmd.Flags().BoolVar(&lookPlain, "plain", false, "Strip ANSI escape sequences from output")
	lookCmd.Flags().BoolVar(&lookFull, "full", false, "Capture the full scrollback history")
	lookCmd.Flags().IntVarP(&lookNumLines, "num-lines", "n", 0, "Number of scrollback lines to capture (tail)")
	lookCmd.MarkFlagsMutuallyExclusive("full", "num-lines")
	rootCmd.AddCommand(lookCmd)
}
