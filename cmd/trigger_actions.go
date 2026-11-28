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
	"fmt"
	"strconv"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/spf13/pflag"
)

// effectiveTriggerActions returns tr.Actions when populated, or falls back to
// the mirrored scalar tr.Action as a one-element list (action-arrays §2).
func effectiveTriggerActions(tr *hubclient.Trigger) []hubclient.TriggerActionSpec {
	if tr == nil {
		return nil
	}
	if len(tr.Actions) > 0 {
		return tr.Actions
	}
	if strings.TrimSpace(tr.Action.Type) != "" ||
		tr.Action.AgentID != "" ||
		tr.Action.AgentName != "" ||
		tr.Action.Message != "" ||
		tr.Action.Template != "" ||
		tr.Action.Body != "" {
		act := tr.Action
		if strings.TrimSpace(act.Type) == "" {
			act.Type = "message"
		}
		return []hubclient.TriggerActionSpec{act}
	}
	return nil
}

// triggerActionGroup captures the flags belonging to one --action <type> group
// parsed left-to-right from the command line (action-arrays §2).
type triggerActionGroup struct {
	kind          string
	explicit      bool
	agent         string
	message       string
	interrupt     bool
	wake          bool
	plain         bool
	offline       string
	queueTTL      string
	comment       string
	replyInThread bool
	dispatch      dispatchAgentFlagValues
	changed       map[string]bool
}

func newTriggerActionGroup(kind string, explicit bool) *triggerActionGroup {
	return &triggerActionGroup{
		kind:     kind,
		explicit: explicit,
		changed:  make(map[string]bool),
	}
}

func (g *triggerActionGroup) markChanged(name string) {
	if g.changed == nil {
		g.changed = make(map[string]bool)
	}
	g.changed[name] = true
}

// triggerActionGroupCollector accumulates repeatable --action groups in exact
// command-line parse order while also mirroring values into the legacy scalar
// flag variables so single-action helpers and tests continue to work unchanged.
type triggerActionGroupCollector struct {
	groups []*triggerActionGroup
}

func (c *triggerActionGroupCollector) reset() {
	c.groups = nil
}

func (c *triggerActionGroupCollector) current() *triggerActionGroup {
	if len(c.groups) == 0 {
		g := newTriggerActionGroup("", false)
		c.groups = append(c.groups, g)
		return g
	}
	return c.groups[len(c.groups)-1]
}

func (c *triggerActionGroupCollector) startAction(rawKind string) {
	if len(c.groups) == 1 && !c.groups[0].explicit {
		c.groups[0].kind = rawKind
		c.groups[0].explicit = true
		return
	}
	c.groups = append(c.groups, newTriggerActionGroup(rawKind, true))
}

func (c *triggerActionGroupCollector) isMulti() bool {
	return len(c.groups) > 1
}

type groupedStringFlag struct {
	target *string
	onSet  func(string)
}

func (f *groupedStringFlag) String() string {
	if f.target == nil {
		return ""
	}
	return *f.target
}

func (f *groupedStringFlag) Set(val string) error {
	if f.target != nil {
		*f.target = val
	}
	if f.onSet != nil {
		f.onSet(val)
	}
	return nil
}

func (f *groupedStringFlag) Type() string { return "string" }

type groupedBoolFlag struct {
	target *bool
	onSet  func(bool)
}

func (f *groupedBoolFlag) String() string {
	if f.target == nil {
		return "false"
	}
	return strconv.FormatBool(*f.target)
}

func (f *groupedBoolFlag) Set(val string) error {
	b, err := strconv.ParseBool(val)
	if err != nil {
		return err
	}
	if f.target != nil {
		*f.target = b
	}
	if f.onSet != nil {
		f.onSet(b)
	}
	return nil
}

func (f *groupedBoolFlag) Type() string     { return "bool" }
func (f *groupedBoolFlag) IsBoolFlag() bool { return true }

type groupedStringArrayFlag struct {
	target *[]string
	onSet  func(string)
}

func (f *groupedStringArrayFlag) String() string {
	if f.target == nil || len(*f.target) == 0 {
		return "[]"
	}
	return "[" + strings.Join(*f.target, ",") + "]"
}

func (f *groupedStringArrayFlag) Set(val string) error {
	if f.target != nil {
		*f.target = append(*f.target, val)
	}
	if f.onSet != nil {
		f.onSet(val)
	}
	return nil
}

func (f *groupedStringArrayFlag) Type() string { return "stringArray" }

func bindGroupedString(fs *pflag.FlagSet, target *string, name, defVal, usage string, onSet func(string)) {
	if target != nil {
		*target = defVal
	}
	fs.Var(&groupedStringFlag{target: target, onSet: onSet}, name, usage)
}

func bindGroupedBool(fs *pflag.FlagSet, target *bool, name, usage string, onSet func(bool)) {
	if target != nil {
		*target = false
	}
	fl := fs.VarPF(&groupedBoolFlag{target: target, onSet: onSet}, name, "", usage)
	fl.NoOptDefVal = "true"
}

func bindGroupedStringArray(fs *pflag.FlagSet, target *[]string, name, usage string, onSet func(string)) {
	fs.Var(&groupedStringArrayFlag{target: target, onSet: onSet}, name, usage)
}

func bindGroupedTriggerActionFlags(
	fs *pflag.FlagSet,
	c *triggerActionGroupCollector,
	actionTarget *string,
	actionDefault string,
	agentTarget, messageTarget *string,
	interruptTarget, wakeTarget, plainTarget *bool,
	offlineTarget, queueTTLTarget *string,
	dispatchTarget *dispatchAgentFlagValues,
	commentTarget *string,
	replyInThreadTarget *bool,
) {
	bindGroupedString(fs, actionTarget, "action", actionDefault,
		"Action kind (message, dispatch-agent, or github.comment; repeatable with per-action flags grouped after each --action)",
		func(v string) { c.startAction(v) })
	bindGroupedString(fs, agentTarget, "agent", "",
		"Target agent name or ID (message action)",
		func(v string) { g := c.current(); g.agent = v; g.markChanged("agent") })
	bindGroupedString(fs, messageTarget, "message", "",
		"Message delivered to the target agent (message action)",
		func(v string) { g := c.current(); g.message = v; g.markChanged("message") })
	bindGroupedBool(fs, interruptTarget, "interrupt",
		"Interrupt the target agent (message action)",
		func(v bool) { g := c.current(); g.interrupt = v; g.markChanged("interrupt") })
	bindGroupedBool(fs, wakeTarget, "wake",
		"Resume the target agent if suspended when the trigger fires (message action)",
		func(v bool) { g := c.current(); g.wake = v; g.markChanged("wake") })
	bindGroupedBool(fs, plainTarget, "plain",
		"Deliver message text without sender prefix (message action)",
		func(v bool) { g := c.current(); g.plain = v; g.markChanged("plain") })
	bindGroupedString(fs, offlineTarget, "offline", "",
		"Offline target policy for message actions (skip or queue; default skip)",
		func(v string) { g := c.current(); g.offline = v; g.markChanged("offline") })
	bindGroupedString(fs, queueTTLTarget, "queue-ttl", "",
		"Maximum wait duration when --offline queue is set (e.g. 30m, 24h; 60s..48h, default 24h)",
		func(v string) { g := c.current(); g.queueTTL = v; g.markChanged("queue-ttl") })

	bindGroupedString(fs, &dispatchTarget.template, "template", "",
		"Agent template name for dispatch-agent actions (literal only, no placeholders)",
		func(v string) { g := c.current(); g.dispatch.template = v; g.markChanged("template") })
	bindGroupedString(fs, &dispatchTarget.task, "task", "",
		"Task template for dispatch-agent actions (mutually exclusive with --task-file)",
		func(v string) { g := c.current(); g.dispatch.task = v; g.markChanged("task") })
	bindGroupedString(fs, &dispatchTarget.taskFile, "task-file", "",
		"Path to a file containing the task template for dispatch-agent actions (mutually exclusive with --task)",
		func(v string) { g := c.current(); g.dispatch.taskFile = v; g.markChanged("task-file") })
	bindGroupedString(fs, &dispatchTarget.agentName, "agent-name", "",
		"Agent name template for dispatch-agent actions (optional; default <trigger>-<6 base32>)",
		func(v string) { g := c.current(); g.dispatch.agentName = v; g.markChanged("agent-name") })
	bindGroupedString(fs, &dispatchTarget.onNameConflict, "on-name-conflict", "",
		"Name collision policy for dispatch-agent actions (suffix or skip; default suffix)",
		func(v string) { g := c.current(); g.dispatch.onNameConflict = v; g.markChanged("on-name-conflict") })
	bindGroupedString(fs, &dispatchTarget.branch, "branch", "",
		"Git branch template for dispatch-agent actions (optional)",
		func(v string) { g := c.current(); g.dispatch.branch = v; g.markChanged("branch") })
	bindGroupedStringArray(fs, &dispatchTarget.labels, "label",
		"Agent label in key=value template form for dispatch-agent actions (repeatable)",
		func(v string) {
			g := c.current()
			g.dispatch.labels = append(g.dispatch.labels, v)
			g.markChanged("label")
		})
	bindGroupedBool(fs, &dispatchTarget.ephemeral, "ephemeral",
		"Request an ephemeral dispatched agent (currently refused by the Hub until ephemeral agents ship)",
		func(v bool) { g := c.current(); g.dispatch.ephemeral = v; g.markChanged("ephemeral") })
	bindGroupedString(fs, &dispatchTarget.reuseMessage, "reuse-message", "",
		"Message template delivered when reusing a live dispatched agent (Stage 3)",
		func(v string) { g := c.current(); g.dispatch.reuseMessage = v; g.markChanged("reuse-message") })
	bindGroupedString(fs, &dispatchTarget.ackComment, "ack-comment", "",
		"GitHub acknowledgement comment template posted after dispatch-agent succeeds (requires --github)",
		func(v string) { g := c.current(); g.dispatch.ackComment = v; g.markChanged("ack-comment") })
	bindGroupedString(fs, &dispatchTarget.githubToken, "github-token", "",
		"GitHub token precedence for dispatched agents (installation or user_preferred)",
		func(v string) { g := c.current(); g.dispatch.githubToken = v; g.markChanged("github-token") })
	bindGroupedString(fs, &dispatchTarget.agentRole, "agent-role", "",
		"Explicit role for dispatched agents (optional; defaults to project/hub ladder)",
		func(v string) { g := c.current(); g.dispatch.agentRole = v; g.markChanged("agent-role") })
	bindGroupedStringArray(fs, &dispatchTarget.allow, "allow",
		"Optional narrowing list of allowed untrusted event variable paths (or trailing .*) for template rendering (repeatable; derived by the Hub when omitted)",
		func(v string) {
			g := c.current()
			g.dispatch.allow = append(g.dispatch.allow, v)
			g.markChanged("allow")
		})
	if commentTarget != nil {
		bindGroupedString(fs, commentTarget, "comment", "",
			"Post a GitHub comment with this body template (shorthand for --action github.comment; requires --github)",
			func(v string) { g := c.current(); g.comment = v; g.markChanged("comment") })
	}
	if replyInThreadTarget != nil {
		bindGroupedBool(fs, replyInThreadTarget, "reply-in-thread",
			"Reply under the review comment thread instead of the PR/issue conversation (requires --comment)",
			func(v bool) { g := c.current(); g.replyInThread = v; g.markChanged("reply-in-thread") })
	}
}

func buildActionSpecFromGroup(g *triggerActionGroup, isGitHubSource bool) (hubclient.TriggerActionSpec, []string, error) {
	commentSet := g.changed["comment"] || g.comment != ""
	ackCommentExplicit := g.changed["ack-comment"]
	ackCommentSet := ackCommentExplicit || g.dispatch.ackComment != ""
	if g.replyInThread && !commentSet {
		return hubclient.TriggerActionSpec{}, nil, newUsageError("--reply-in-thread requires --comment")
	}

	rawAction := strings.TrimSpace(g.kind)
	if commentSet {
		if g.explicit && rawAction != store.TriggerActionGitHubComment {
			return hubclient.TriggerActionSpec{}, nil, newUsageError("--comment cannot be used with --action %s", triggerTerminalText(rawAction, false))
		}
		rawAction = store.TriggerActionGitHubComment
	}

	actionKind, err := normalizeTriggerActionKind(rawAction)
	if err != nil {
		return hubclient.TriggerActionSpec{}, nil, err
	}
	if actionKind == store.TriggerActionGitHubComment && !isGitHubSource {
		return hubclient.TriggerActionSpec{}, nil, newUsageError("--comment requires --github")
	}
	if ackCommentSet && !isGitHubSource {
		return hubclient.TriggerActionSpec{}, nil, newUsageError("--ack-comment requires --github")
	}

	allowExplicit := g.changed["allow"]
	switch actionKind {
	case "message":
		if badFlag, ok := hasDispatchAgentFlagValues(&g.dispatch); ok {
			return hubclient.TriggerActionSpec{}, nil, newUsageError("%s cannot be used with --action message", badFlag)
		}
		if ackCommentExplicit {
			return hubclient.TriggerActionSpec{}, nil, newUsageError("--ack-comment cannot be used with --action message")
		}
		agentRef := strings.TrimSpace(g.agent)
		if agentRef == "" {
			return hubclient.TriggerActionSpec{}, nil, newUsageError("--agent is required")
		}
		if strings.TrimSpace(g.message) == "" {
			return hubclient.TriggerActionSpec{}, nil, newUsageError("--message is required")
		}
		offline := strings.TrimSpace(g.offline)
		if offline != "" && offline != "skip" && offline != "queue" {
			return hubclient.TriggerActionSpec{}, nil, newUsageError("invalid --offline %q (supported: skip, queue)", triggerTerminalText(offline, false))
		}
		var queueTTLSeconds int
		if rawTTL := strings.TrimSpace(g.queueTTL); rawTTL != "" {
			if offline != "queue" {
				return hubclient.TriggerActionSpec{}, nil, newUsageError("--queue-ttl requires --offline queue")
			}
			secs, tErr := parseTriggerQueueTTL(rawTTL)
			if tErr != nil {
				return hubclient.TriggerActionSpec{}, nil, tErr
			}
			queueTTLSeconds = secs
		}
		action := hubclient.TriggerActionSpec{
			Type:            "message",
			Message:         g.message,
			Interrupt:       g.interrupt,
			Wake:            g.wake,
			Plain:           g.plain,
			Offline:         offline,
			QueueTTLSeconds: queueTTLSeconds,
		}
		if allowExplicit || len(g.dispatch.allow) > 0 {
			action.UntrustedVars = append([]string(nil), g.dispatch.allow...)
		}
		if uuid.Validate(agentRef) == nil {
			action.AgentID = agentRef
		} else {
			action.AgentName = agentRef
		}
		return action, nil, nil

	case "dispatch_agent":
		if badFlag, ok := hasMessageFlagValues(g.agent, g.message, g.interrupt, g.wake, g.plain); ok {
			return hubclient.TriggerActionSpec{}, nil, newUsageError("%s cannot be used with --action dispatch-agent", badFlag)
		}
		if strings.TrimSpace(g.offline) != "" {
			return hubclient.TriggerActionSpec{}, nil, newUsageError("--offline cannot be used with --action dispatch-agent")
		}
		if strings.TrimSpace(g.queueTTL) != "" {
			return hubclient.TriggerActionSpec{}, nil, newUsageError("--queue-ttl cannot be used with --action dispatch-agent")
		}
		if ackCommentExplicit && strings.TrimSpace(g.dispatch.ackComment) == "" {
			return hubclient.TriggerActionSpec{}, nil, newUsageError("--ack-comment cannot be empty")
		}
		return buildDispatchAgentActionSpec(&g.dispatch, allowExplicit)

	case store.TriggerActionGitHubComment:
		if badFlag, ok := hasMessageFlagValues(g.agent, g.message, g.interrupt, g.wake, g.plain); ok {
			return hubclient.TriggerActionSpec{}, nil, newUsageError("%s cannot be used with --action github.comment", badFlag)
		}
		if strings.TrimSpace(g.offline) != "" {
			return hubclient.TriggerActionSpec{}, nil, newUsageError("--offline cannot be used with --action github.comment")
		}
		if strings.TrimSpace(g.queueTTL) != "" {
			return hubclient.TriggerActionSpec{}, nil, newUsageError("--queue-ttl cannot be used with --action github.comment")
		}
		if badFlag, ok := hasDispatchAgentFlagValues(&g.dispatch); ok {
			return hubclient.TriggerActionSpec{}, nil, newUsageError("%s cannot be used with --action github.comment", badFlag)
		}
		if ackCommentExplicit {
			return hubclient.TriggerActionSpec{}, nil, newUsageError("--ack-comment cannot be used with --action github.comment")
		}
		return buildGitHubCommentActionSpec(g.comment, g.replyInThread, g.dispatch.allow, allowExplicit)

	default:
		return hubclient.TriggerActionSpec{}, nil, newUsageError("unsupported --action %q", triggerTerminalText(g.kind, false))
	}
}

func buildActionSpecsFromGroups(groups []*triggerActionGroup, isGitHubSource bool) ([]hubclient.TriggerActionSpec, []string, error) {
	if len(groups) > store.MaxTriggerActions {
		return nil, nil, newUsageError("at most %d --action entries are allowed per trigger (got %d)", store.MaxTriggerActions, len(groups))
	}
	actions := make([]hubclient.TriggerActionSpec, 0, len(groups))
	var derivedAll []string
	seen := make(map[string]bool)
	for i, g := range groups {
		spec, derived, err := buildActionSpecFromGroup(g, isGitHubSource)
		if err != nil {
			return nil, nil, fmt.Errorf("action %d/%d: %w", i+1, len(groups), err)
		}
		actions = append(actions, spec)
		for _, p := range derived {
			if !seen[p] {
				seen[p] = true
				derivedAll = append(derivedAll, p)
			}
		}
	}
	return actions, derivedAll, nil
}
