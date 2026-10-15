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

package config

import (
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
)

// KubeConnection is how a Kubernetes client connected when it was built.
type KubeConnection int

const (
	// KubeConnUnknown means the connection mode is not known (zero value).
	KubeConnUnknown KubeConnection = iota
	// KubeConnKubeconfig means the client was built from a kubeconfig.
	KubeConnKubeconfig
	// KubeConnInCluster means the client uses in-cluster credentials.
	KubeConnInCluster
)

// String returns a short name for the connection mode.
func (c KubeConnection) String() string {
	switch c {
	case KubeConnKubeconfig:
		return "kubeconfig"
	case KubeConnInCluster:
		return "in-cluster"
	default:
		return "unknown"
	}
}

// RuntimeBuildRecord describes how a runtime was built. The zero value
// (Present false) makes every runtime entry Undetermined.
type RuntimeBuildRecord struct {
	// Present is true when the record was filled at build time.
	Present bool
	// EntryName is the global runtimes key the runtime was built from, or
	// "" when it was not built from a global entry.
	EntryName string
	// KubeconfigContext is the raw kubeconfig current context when the
	// runtime was built ("" when in-cluster). For a client built with an
	// explicit context it can differ from the client's current context.
	KubeconfigContext string
}

// MatchResult is the three-way result of matching a runtime entry against
// a built runtime. The zero value is Undetermined, so an unset result fails
// closed.
type MatchResult int

const (
	// Undetermined means it cannot be decided whether the entry targets
	// the runtime.
	Undetermined MatchResult = iota
	// Match means the entry targets the runtime's cluster and namespace.
	Match
	// NoMatch means the entry does not target the runtime.
	NoMatch
)

// String returns a short name for the match result.
func (m MatchResult) String() string {
	switch m {
	case Match:
		return "match"
	case NoMatch:
		return "no-match"
	default:
		return "undetermined"
	}
}

// Causes returned with an Undetermined result.
const (
	matchCauseNoRecord       = "runtime has no build record or client"
	matchCauseUnknownConn    = "runtime connection mode unknown"
	matchCauseUnreadable     = "kubeconfig current-context could not be read"
	matchCauseContextChanged = "kubeconfig current-context changed since the runtime was built"
)

// MatchKubernetesEntry decides whether a Kubernetes runtime entry targets a
// built runtime. runtimeCtx and runtimeNS are the runtime client's current
// context and default namespace, hasClient whether it has a client, conn
// its connection mode and rec its build record. An entry with no namespace
// uses defaultNS. liveCurrentContext reads the kubeconfig current context
// now; it is called only for a kubeconfig runtime and an entry with no
// context, and a nil callback counts as unreadable.
//
// The cause is set only for Undetermined. The caller filters entries to
// Kubernetes ones.
func MatchKubernetesEntry(entry V1RuntimeConfig, runtimeCtx, runtimeNS string, hasClient bool,
	conn KubeConnection, rec RuntimeBuildRecord, defaultNS string,
	liveCurrentContext func() (string, error)) (MatchResult, string) {
	if !rec.Present || !hasClient {
		return Undetermined, matchCauseNoRecord
	}
	entryNS := entry.Namespace
	if entryNS == "" {
		entryNS = defaultNS
	}
	nsResult := NoMatch
	if entryNS == runtimeNS {
		nsResult = Match
	}
	switch conn {
	case KubeConnInCluster:
		// An explicit context never yields an in-cluster client; with no
		// context only the namespace decides. No kubeconfig is read.
		if entry.Context != "" {
			return NoMatch, ""
		}
		return nsResult, ""
	case KubeConnKubeconfig:
		if entry.Context != "" {
			if entry.Context != runtimeCtx {
				return NoMatch, ""
			}
			return nsResult, ""
		}
		if liveCurrentContext == nil {
			return Undetermined, matchCauseUnreadable
		}
		live, err := liveCurrentContext()
		if err != nil {
			return Undetermined, matchCauseUnreadable
		}
		recordedEq := rec.KubeconfigContext == runtimeCtx
		liveEq := live == runtimeCtx
		switch {
		case recordedEq && liveEq:
			return nsResult, ""
		case !recordedEq && !liveEq:
			return NoMatch, ""
		default:
			return Undetermined, matchCauseContextChanged
		}
	default:
		return Undetermined, matchCauseUnknownConn
	}
}

// LiteralSiblingGroups groups Kubernetes runtime entries whose context and
// namespace are literally equal (both empty counts as equal). Only groups
// of two or more are returned; names are sorted within each group and the
// groups are sorted.
func LiteralSiblingGroups(runtimes map[string]V1RuntimeConfig) [][]string {
	byKey := make(map[[2]string][]string)
	for name, rt := range runtimes {
		if !isKubernetesRuntimeEntry(name, rt) {
			continue
		}
		key := [2]string{rt.Context, rt.Namespace}
		byKey[key] = append(byKey[key], name)
	}
	var groups [][]string
	for _, names := range byKey {
		if len(names) >= 2 {
			groups = append(groups, names)
		}
	}
	return sortGroups(groups)
}

// IdentityGroups groups Kubernetes runtime entries that resolve to the same
// cluster identity and namespace on this host. An entry with a context is
// identified by it. An entry with no context resolves through
// currentContext, called at most once: an in-cluster connection gives the
// in-cluster identity, and a kubeconfig connection gives its current
// context. An entry with no namespace uses defaultNS.
//
// When an entry with no context cannot be resolved (callback error,
// unknown connection, or a kubeconfig connection with an empty current
// context), it fails closed: it forms a group with every entry in the same
// namespace, and a warning names it. Groups can therefore overlap. Only
// groups of two or more are returned, sorted as in LiteralSiblingGroups.
func IdentityGroups(runtimes map[string]V1RuntimeConfig, currentContext func() (string, KubeConnection, error),
	defaultNS string) (groups [][]string, warnings []string) {
	names := make([]string, 0, len(runtimes))
	for name, rt := range runtimes {
		if isKubernetesRuntimeEntry(name, rt) {
			names = append(names, name)
		}
	}
	sort.Strings(names)

	var (
		resolved  bool
		hostCtx   string
		hostConn  KubeConnection
		hostErr   error
		nsOf      = make(map[string]string, len(names))
		byID      = make(map[[2]string][]string)
		unresolve []string
	)
	for _, name := range names {
		rt := runtimes[name]
		ns := rt.Namespace
		if ns == "" {
			ns = defaultNS
		}
		nsOf[name] = ns
		if rt.Context != "" {
			id := [2]string{"context:" + rt.Context, ns}
			byID[id] = append(byID[id], name)
			continue
		}
		if !resolved {
			resolved = true
			if currentContext == nil {
				hostErr = errors.New("no current-context reader")
			} else {
				hostCtx, hostConn, hostErr = currentContext()
			}
		}
		var reason string
		switch {
		case hostErr != nil:
			reason = fmt.Sprintf("the current context could not be read: %v", hostErr)
		case hostConn == KubeConnInCluster:
			id := [2]string{"in-cluster", ns}
			byID[id] = append(byID[id], name)
			continue
		case hostConn == KubeConnKubeconfig && hostCtx != "":
			id := [2]string{"context:" + hostCtx, ns}
			byID[id] = append(byID[id], name)
			continue
		case hostConn == KubeConnKubeconfig:
			reason = "the kubeconfig has no current context"
		default:
			reason = "the connection mode is unknown"
		}
		unresolve = append(unresolve, name)
		warnings = append(warnings, fmt.Sprintf("runtimes.%s: cluster identity could not be resolved (%s); treating it as targeting the same cluster as every entry in namespace %q", name, reason, ns))
	}

	for _, members := range byID {
		if len(members) >= 2 {
			groups = append(groups, members)
		}
	}
	for _, u := range unresolve {
		group := []string{u}
		for _, other := range names {
			if other != u && nsOf[other] == nsOf[u] {
				group = append(group, other)
			}
		}
		if len(group) >= 2 {
			groups = append(groups, group)
		}
	}
	return sortGroups(groups), warnings
}

// sortGroups sorts the names in each group, sorts the groups, and drops
// duplicate groups.
func sortGroups(groups [][]string) [][]string {
	for _, g := range groups {
		sort.Strings(g)
	}
	sort.Slice(groups, func(i, j int) bool {
		return strings.Join(groups[i], "\x00") < strings.Join(groups[j], "\x00")
	})
	return slices.CompactFunc(groups, func(a, b []string) bool { return slices.Equal(a, b) })
}
