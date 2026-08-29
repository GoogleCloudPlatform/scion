// Package main implements the check-security-marker-gates regression guard.
//
// Each gate row asserts that a named security symbol appears a specific number
// of times inside a named enclosing function, using go/ast for exact identifier
// matching and function scoping.
//
// Why go/ast instead of grep/awk:
//   - Identifier matching is exact: ActionAttachment ≠ ActionAttach.
//   - Comments are not identifiers: a trailing comment naming the symbol cannot
//     satisfy a gate row.
//   - Function scope is exact: the AST knows where a function body ends, so
//     braces inside comments, strings, or raw literals cannot shift the boundary.
//
// Gate design principle (DEF-50):
//
//	An exemption records that we looked once; a gate records what must
//	remain true. Prefer the second wherever the thing that makes the
//	path safe can be named. Only fall back to an exemption when safety
//	rests on something the checker cannot see.
//
// See hack/check-security-marker-gates.sh (the invoking wrapper) for full
// documentation of gate categories, exit codes, and the security rationale.
package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

var (
	rc      int
	notices []string
)

// parsedFile associates a parsed AST file with its path.
type parsedFile struct {
	path string
	file *ast.File
}

func main() {
	hamPath := "pkg/hub/handlers_agent_messaging.go"
	hcvPath := "pkg/hub/handlers_chat_v2.go"
	mbPath := "pkg/hub/messagebroker.go"
	hbiPath := "pkg/hub/handlers_broker_inbound.go"
	notifPath := "pkg/hub/notifications.go"
	serverPath := "pkg/hub/server.go"

	// File-existence precheck (exit 2).
	for _, f := range []string{hamPath, hcvPath, mbPath, hbiPath, notifPath, serverPath} {
		if _, err := os.Stat(f); err != nil {
			fmt.Fprintf(os.Stderr, "ABORT: guarded file not found or not readable: %s\n", f)
			fmt.Fprintf(os.Stderr, "  Nothing was analysed. This is an environment/rename issue, not a guard failure.\n")
			os.Exit(2)
		}
	}

	fset := token.NewFileSet()

	mustParse := func(path string) *ast.File {
		f, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if err != nil {
			fmt.Fprintf(os.Stderr, "ABORT: could not parse %s: %v\n", path, err)
			fmt.Fprintf(os.Stderr, "  Nothing was analysed. This is a syntax error, not a guard failure.\n")
			os.Exit(2)
		}
		return f
	}

	ham := mustParse(hamPath)
	hcv := mustParse(hcvPath)
	mb := mustParse(mbPath)
	hbi := mustParse(hbiPath)
	notif := mustParse(notifPath)
	srv := mustParse(serverPath)

	// =========================================================================
	// SECTION 1: Existing security-marker gates (B5, #1322, #1338, #1347, #1371)
	// =========================================================================

	// --- authenticatedSender in handlers_agent_messaging.go ---

	// REQUIRED: 4 call sites + 1 definition
	assertRequired(
		"authenticatedSender in handleAgentMessage (B5 — DM key derivation)",
		ham, hamPath, "handleAgentMessage", "authenticatedSender", 1)

	assertRequired(
		"authenticatedSender in handleGroupMessage (B5 — per-agent and per-user DM resolution)",
		ham, hamPath, "handleGroupMessage", "authenticatedSender", 2)

	assertRequired(
		"authenticatedSender in handleProjectBroadcast (B5 — broadcast self-skip)",
		ham, hamPath, "handleProjectBroadcast", "authenticatedSender", 1)

	assertFuncDef(
		"authenticatedSender function definition (B5 — must exist)",
		ham, hamPath, "authenticatedSender", 1)

	// INFORMATIONAL: 1 doc comment
	assertInformational(
		"authenticatedSender doc comment",
		ham, hamPath, "authenticatedSender", 1)

	// --- validateDefaultAgent in handlers_chat_v2.go ---

	// REQUIRED: 2 call sites + 1 definition
	assertRequired(
		"validateDefaultAgent in handleCreateThread (DEF-31 — topic creation)",
		hcv, hcvPath, "handleCreateThread", "validateDefaultAgent", 1)

	assertRequired(
		"validateDefaultAgent in handleTopicPatch (DEF-31 — topic update)",
		hcv, hcvPath, "handleTopicPatch", "validateDefaultAgent", 1)

	assertFuncDef(
		"validateDefaultAgent function definition (DEF-31 — must exist)",
		hcv, hcvPath, "validateDefaultAgent", 1)

	// INFORMATIONAL: 3 doc comments
	assertInformational(
		"validateDefaultAgent doc comments",
		hcv, hcvPath, "validateDefaultAgent", 3)

	// --- authorizeAgentMessage in handlers_agent_messaging.go ---
	// #1371 replaced ActionAttach-based authorization with authorizeAgentMessage,
	// which provides equivalent-or-stronger protection (mode filtering + ancestry).

	// REQUIRED: 1 call in handleProjectBroadcast (per-recipient pre-filter)
	assertRequired(
		"authorizeAgentMessage in handleProjectBroadcast (#1371 — project broadcast authorization)",
		ham, hamPath, "handleProjectBroadcast", "authorizeAgentMessage", 1)

	// --- authorizeAgentMessage in handlers_chat_v2.go ---
	// #1371 replaced ActionAttach authorize/CheckAccess with authorizeAgentMessage
	// on both primary and mention paths.

	// REQUIRED: 2 calls in sendAgentRouted (primary path + mention fan-out)
	assertRequired(
		"authorizeAgentMessage in sendAgentRouted (#1371 — agent message authorization)",
		hcv, hcvPath, "sendAgentRouted", "authorizeAgentMessage", 2)

	// --- COMPOSITE GATE: handleProjectBroadcast ---
	// This single function carries authenticatedSender (B5) AND authorizeAgentMessage (#1371).
	// messaging-v2 reverts BOTH. A regression here costs sender-identity derivation
	// and project authorization simultaneously.

	compositeAuth := countIdentsInFunc(ham, "handleProjectBroadcast", "authenticatedSender")
	compositeAuthzMsg := countIdentsInFunc(ham, "handleProjectBroadcast", "authorizeAgentMessage")

	if compositeAuth < 1 || compositeAuthzMsg < 1 {
		fmt.Fprintf(os.Stderr, "FAIL [COMPOSITE] handleProjectBroadcast must contain BOTH authenticatedSender AND authorizeAgentMessage\n")
		fmt.Fprintf(os.Stderr, "  authenticatedSender: found x%d (need ≥1)\n", compositeAuth)
		fmt.Fprintf(os.Stderr, "  authorizeAgentMessage: found x%d (need ≥1)\n", compositeAuthzMsg)
		fmt.Fprintf(os.Stderr, "  This function is the highest-value anchor: a single regression costs\n")
		fmt.Fprintf(os.Stderr, "  sender-identity derivation AND project authorization simultaneously.\n")
		rc = 1
	}

	// --- Broadcasted forcing in handlers_agent_messaging.go ---

	// REQUIRED: Broadcasted = true server-side forcing in handleProjectBroadcast.
	// Without this, a client setting Broadcasted=false walks the message through
	// the DM path with a spoofed sender, bypassing broadcast authorization.
	assertRequired(
		"Broadcasted in handleProjectBroadcast (B5 — server-side broadcast forcing)",
		ham, hamPath, "handleProjectBroadcast", "Broadcasted", 1)

	// --- parseDMKeyIDs in handlers_agent_messaging.go ---

	// REQUIRED: DM key ownership verification (#1322).
	// parseDMKeyIDs validates that the DM thread_id matches the resolved sender
	// and agent. Without it, a user can claim a DM key belonging to another
	// user's conversation.
	assertRequired(
		"parseDMKeyIDs in handleAgentOutboundMessage (#1322 — DM key ownership)",
		ham, hamPath, "handleAgentOutboundMessage", "parseDMKeyIDs", 1)

	assertRequired(
		"parseDMKeyIDs in handleAgentMessage (#1322 — DM key ownership)",
		ham, hamPath, "handleAgentMessage", "parseDMKeyIDs", 1)

	// --- parseDMKeyIDs and isDMParticipant definitions in handlers_chat_v2.go ---

	assertFuncDef(
		"parseDMKeyIDs function definition (#1322 — must exist)",
		hcv, hcvPath, "parseDMKeyIDs", 1)

	assertFuncDef(
		"isDMParticipant function definition (#1322 — kind-label tightening, must exist)",
		hcv, hcvPath, "isDMParticipant", 1)

	// --- SenderID in messagebroker.go ---

	// REQUIRED: 3 uses in fanOutToProject (B5/R1 — broadcast self-skip by ID)
	assertRequired(
		"SenderID in fanOutToProject (B5/R1 — broadcast self-skip by canonical ID)",
		mb, mbPath, "fanOutToProject", "SenderID", 3)

	// REQUIRED: 3 uses in fanOutGlobal (B5/R1 — global self-skip by ID)
	assertRequired(
		"SenderID in fanOutGlobal (B5/R1 — global broadcast self-skip by canonical ID)",
		mb, mbPath, "fanOutGlobal", "SenderID", 3)

	// --- handlers_broker_inbound.go ---
	// Parallel entry point to handlers_agent_messaging.go. Same B5 and #1371
	// security patterns: server-derived sender identity, authorizeAgentMessage
	// enforcement, DM key ownership verification.

	// REQUIRED: authorizeAgentMessage x1 in handleBrokerInbound
	// #1371 replaced ActionAttach/CheckAccess with authorizeAgentMessage, which
	// combines policy check with mode filtering and ancestry verification.
	assertRequired(
		"authorizeAgentMessage in handleBrokerInbound (#1371 — broker inbound message authorization)",
		hbi, hbiPath, "handleBrokerInbound", "authorizeAgentMessage", 1)

	// REQUIRED: SenderID x4 in handleBrokerInbound (B5 — canonical sender identity)
	// The 4 idents are: assignment from senderUser.ID, DM ownership check,
	// message persistence, and conversation resolution.
	assertRequired(
		"SenderID in handleBrokerInbound (B5 — canonical sender identity propagation)",
		hbi, hbiPath, "handleBrokerInbound", "SenderID", 4)

	// REQUIRED: NewAuthenticatedUser x1 in handleBrokerInbound
	// Identity constructed from DB-resolved senderUser, not request payload.
	assertRequired(
		"NewAuthenticatedUser in handleBrokerInbound (B5 — server-derived identity construction)",
		hbi, hbiPath, "handleBrokerInbound", "NewAuthenticatedUser", 1)

	// REQUIRED: parseDMKeyIDs x1 in handleBrokerInbound
	// Validates DM thread_id matches the resolved sender and agent.
	assertRequired(
		"parseDMKeyIDs in handleBrokerInbound (B5 — DM key ownership verification)",
		hbi, hbiPath, "handleBrokerInbound", "parseDMKeyIDs", 1)

	// =========================================================================
	// SECTION 2: Validation choke-point gates (DEF-50)
	//
	// DEF-50 ports the file-level checks from hack/check-authz-reachability.sh
	// into the AST checker. The shell script uses grep and treats comment
	// mentions as satisfaction; these gates use go/ast and are immune to
	// comments, strings, and substrings.
	//
	// DEF-56: check-authz-reachability.sh was never wired into CI. These gates
	// are the first mechanical enforcement that actually runs.
	// =========================================================================

	// --- ValidateLegacyMessage in send-path functions ---
	// Every primary send path must call ValidateLegacyMessage for shape/content
	// validation before dispatch (Audit M2).

	assertRequired(
		"ValidateLegacyMessage in handleAgentOutboundMessage (DEF-50 — outbound message validation)",
		ham, hamPath, "handleAgentOutboundMessage", "ValidateLegacyMessage", 1)

	assertRequired(
		"ValidateLegacyMessage in handleAgentMessage (DEF-50 — agent message validation)",
		ham, hamPath, "handleAgentMessage", "ValidateLegacyMessage", 1)

	assertRequired(
		"ValidateLegacyMessage in sendAgentRouted (DEF-50 — chat v2 message validation)",
		hcv, hcvPath, "sendAgentRouted", "ValidateLegacyMessage", 1)

	assertRequired(
		"ValidateLegacyMessage in handleBrokerInbound (DEF-50 — broker inbound validation)",
		hbi, hbiPath, "handleBrokerInbound", "ValidateLegacyMessage", 1)

	// INFORMATIONAL: comment mentions of ValidateLegacyMessage
	assertInformational(
		"ValidateLegacyMessage doc comments in handlers_agent_messaging.go",
		ham, hamPath, "ValidateLegacyMessage", 1)

	// --- ValidateAttributed in send-path functions ---
	// DEF-41: post-attribution validation. Checks ConversationID after
	// attribution has set a real one. Must be present on every path that
	// resolves or creates a conversation.

	assertRequired(
		"ValidateAttributed in handleAgentOutboundMessage (DEF-50 — post-attribution validation)",
		ham, hamPath, "handleAgentOutboundMessage", "ValidateAttributed", 1)

	assertRequired(
		"ValidateAttributed in handleAgentMessage (DEF-50 — post-attribution validation)",
		ham, hamPath, "handleAgentMessage", "ValidateAttributed", 1)

	assertRequired(
		"ValidateAttributed in sendAgentRouted (DEF-50 — post-attribution validation)",
		hcv, hcvPath, "sendAgentRouted", "ValidateAttributed", 1)

	assertRequired(
		"ValidateAttributed in handleBrokerInbound (DEF-50 — post-attribution validation)",
		hbi, hbiPath, "handleBrokerInbound", "ValidateAttributed", 1)

	// =========================================================================
	// SECTION 3: Self-auth gate for handleAgentOutboundMessage (DEF-50)
	//
	// handleAgentOutboundMessage is the agent→user outbound path. It dispatches
	// via PublishUserMessage but does not call authorizeAgentMessage because
	// there is no target agent — the recipient is a user. Authorization is
	// enforced by a self-access check inside the function itself: the caller
	// must be an authenticated agent (GetAgentIdentityFromContext) sending as
	// itself (agentIdent.ID() == route ID).
	//
	// This gate asserts that the self-access check remains present. An
	// exemption would record that we looked once; this gate records what
	// must remain true.
	// =========================================================================

	assertRequired(
		"GetAgentIdentityFromContext in handleAgentOutboundMessage (DEF-50 — agent identity verification)",
		ham, hamPath, "handleAgentOutboundMessage", "GetAgentIdentityFromContext", 1)

	// =========================================================================
	// SECTION 4: Fail-closed dispatch scan (DEF-50)
	//
	// Any function in pkg/hub/handlers_*.go whose body contains a dispatch
	// sink must also contain authorizeAgentMessage — unless it is covered by
	// a separate enforceable gate that binds it more tightly than a bare
	// exemption would.
	//
	// messagebroker.go is excluded: its dispatch paths are downstream of
	// handler-level authorization. fanOutGlobal (E1) is exempt because its
	// admin-only authorization rests on the architectural property that only
	// admin paths publish to TopicGlobalBroadcast — something the checker
	// cannot see. That exemption is recorded, not asserted.
	//
	// Dispatch sinks: dispatchWithBrokerRetry, PublishUserMessage,
	// PublishBroadcast, managedAgentMessage.
	// =========================================================================

	dispatchSinks := []string{
		"dispatchWithBrokerRetry",
		"PublishUserMessage",
		"PublishBroadcast",
		"managedAgentMessage",
	}

	// Functions covered by a separate enforceable gate rather than a bare
	// exemption. Each maps to the gate that binds it — see Section 3 and
	// Section 5 below.
	dispatchGated := map[string]string{
		"handleAgentOutboundMessage": "self-auth gate (Section 3): GetAgentIdentityFromContext",
		"handleAgentMessage":         "caller-side gate (Section 5): all callers must contain authorizeAgentMessage",
		"broadcastDirect":            "caller-side gate (Section 5): all callers must contain authorizeAgentMessage",
		"sendHumanToHuman":           "caller-side gate (Section 5): user-to-user path, callers must contain isDMParticipant",
	}

	handlerFiles := parseGlob(fset, "pkg/hub/handlers_*.go")

	dispatchHits := 0
	for _, hf := range handlerFiles {
		funcsWithDispatch := findFuncsWithAnyIdent(hf.file, dispatchSinks)
		for _, funcName := range funcsWithDispatch {
			dispatchHits++
			if _, gated := dispatchGated[funcName]; gated {
				continue
			}
			if countIdentsInFunc(hf.file, funcName, "authorizeAgentMessage") == 0 {
				fmt.Fprintf(os.Stderr, "FAIL [FAIL-CLOSED] %s in %s contains dispatch calls but no authorizeAgentMessage\n",
					funcName, filepath.Base(hf.path))
				fmt.Fprintf(os.Stderr, "  Dispatch sinks found, but no authorizeAgentMessage. Either add\n")
				fmt.Fprintf(os.Stderr, "  authorizeAgentMessage to the function, or add a named gate entry\n")
				fmt.Fprintf(os.Stderr, "  with a written security assertion in this checker.\n")
				rc = 1
			}
		}
	}

	// Self-test: the dispatch scan must find at least one function.
	// If zero functions matched, the sink list is broken and the scan is inert.
	if dispatchHits == 0 {
		fmt.Fprintf(os.Stderr, "FAIL [SELF-TEST] dispatch scan matched zero functions — sink list is broken\n")
		rc = 1
	}

	// =========================================================================
	// SECTION 5: Caller-side gates (DEF-50)
	//
	// Some functions dispatch messages but do not call authorizeAgentMessage
	// (or the appropriate identity check) in their own body. Authorization
	// happens in their callers.
	//
	// A bare exemption would stay green through both the deletion of an
	// authz block and the addition of an unauthorized route. A caller-side
	// rule fails on both: any function whose body calls the target must
	// also contain the required authorization symbol.
	//
	// handleAgentMessage: callers are handleAgentAction (handlers_agents_core.go)
	//   and the project-scoped action router (handlers_projects_core.go).
	// broadcastDirect: called from handleProjectBroadcast, which pre-filters
	//   recipients through authorizeAgentMessage.
	// sendHumanToHuman: user-to-user path (no target agent), called from
	//   handleConversationSend which verifies isDMParticipant.
	//
	// SCOPE LIMITATION: caller discovery scans only pkg/hub/handlers_*.go.
	// A caller added outside that glob (e.g. in a new top-level file) would
	// be invisible to the rule. Both real callers are inside the glob today,
	// and minCallers guards against silent drift: if a caller moves out of
	// the glob, the count drops and the self-test fires.
	// =========================================================================

	type callerRule struct {
		callee         string
		requiredSymbol string
		desc           string
		minCallers     int
	}

	callerRules := []callerRule{
		{
			callee:         "handleAgentMessage",
			requiredSymbol: "authorizeAgentMessage",
			desc:           "message authorization",
			minCallers:     2,
		},
		{
			callee:         "broadcastDirect",
			requiredSymbol: "authorizeAgentMessage",
			desc:           "broadcast authorization",
			minCallers:     1,
		},
		{
			callee:         "sendHumanToHuman",
			requiredSymbol: "isDMParticipant",
			desc:           "DM participant authorization (user-to-user path, no target agent)",
			minCallers:     1,
		},
	}

	for _, rule := range callerRules {
		callerHits := 0
		for _, hf := range handlerFiles {
			callers := findFuncsWithIdent(hf.file, rule.callee)
			for _, callerName := range callers {
				// Skip the callee itself.
				if callerName == rule.callee {
					continue
				}
				callerHits++
				if countIdentsInFunc(hf.file, callerName, rule.requiredSymbol) == 0 {
					fmt.Fprintf(os.Stderr, "FAIL [CALLER-SIDE] %s in %s calls %s but has no %s\n",
						callerName, filepath.Base(hf.path), rule.callee, rule.requiredSymbol)
					fmt.Fprintf(os.Stderr, "  Every route that invokes %s must perform %s.\n",
						rule.callee, rule.desc)
					rc = 1
				}
			}
		}

		if callerHits < rule.minCallers {
			fmt.Fprintf(os.Stderr, "FAIL [SELF-TEST] caller-side scan found %d callers of %s (need ≥%d) — scan may be broken\n",
				callerHits, rule.callee, rule.minCallers)
			rc = 1
		}
	}

	// =========================================================================
	// SECTION 6: Exempt emitter enforcement (DEF-37)
	//
	// Server-generated message constructors (NewMention, NewNotification,
	// NewSystemMessage) bypass ValidateLegacyMessage by design — they
	// construct messages from server-internal state, not untrusted input.
	// See pkg/messaging/VALIDATION_EXEMPTIONS.md.
	//
	// This gate makes the exempt set enforceable: a contributor adding a
	// new function that constructs server messages without validation must
	// add it to the exempt list here, which changes the checker and is
	// reviewable. Functions that contain BOTH a constructor AND
	// ValidateLegacyMessage (e.g. sendAgentRouted) pass naturally.
	// =========================================================================

	exemptConstructors := []string{
		"NewMention",
		"NewNotification",
		"NewSystemMessage",
	}

	// Exempt emitters: functions that construct server messages without
	// ValidateLegacyMessage. Each entry documents WHY the bypass is safe.
	exemptEmitters := map[string]string{
		"processMentions":     "derivative of validated primary message (mention fan-out)",
		"dispatchToAgent":     "server-internal lifecycle signal (notification dispatch)",
		"dispatchToChannels":  "server-internal lifecycle signal (channel notification)",
		"dispatchToBroker":    "server-internal lifecycle signal (broker notification)",
		"messageEventHandler": "scheduled payload validated at creation time",
	}

	// Files to scan for exempt emitter enforcement.
	exemptScanFiles := []parsedFile{
		{hamPath, ham},
		{hcvPath, hcv},
		{hbiPath, hbi},
		{notifPath, notif},
		{serverPath, srv},
	}

	// Track which exempt emitters were actually found, to detect stale entries.
	exemptSeen := make(map[string]bool)

	for _, pf := range exemptScanFiles {
		for _, decl := range pf.file.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			funcName := fd.Name.Name

			hasConstructor := false
			for _, ctor := range exemptConstructors {
				if countIdentsInFunc(pf.file, funcName, ctor) > 0 {
					hasConstructor = true
					break
				}
			}
			if !hasConstructor {
				continue
			}

			// Function uses a server-generated message constructor.
			// If it also calls ValidateLegacyMessage, it passes naturally.
			if countIdentsInFunc(pf.file, funcName, "ValidateLegacyMessage") > 0 {
				continue
			}

			// Constructor without validation — must be in the exempt list.
			if reason, ok := exemptEmitters[funcName]; ok {
				exemptSeen[funcName] = true
				notices = append(notices, fmt.Sprintf(
					"NOTICE [DEF-37 EXEMPT] %s in %s: %s",
					funcName, filepath.Base(pf.path), reason))
			} else {
				fmt.Fprintf(os.Stderr, "FAIL [DEF-37] %s in %s uses a server-generated message constructor without ValidateLegacyMessage\n",
					funcName, filepath.Base(pf.path))
				fmt.Fprintf(os.Stderr, "  Constructor found but function is not in the exempt emitter list.\n")
				fmt.Fprintf(os.Stderr, "  Either add ValidateLegacyMessage to the function, or add an exempt\n")
				fmt.Fprintf(os.Stderr, "  emitter entry in this checker with a justification for the bypass.\n")
				fmt.Fprintf(os.Stderr, "  See pkg/messaging/VALIDATION_EXEMPTIONS.md.\n")
				rc = 1
			}
		}
	}

	// Verify no exempt emitter entries are stale.
	for name := range exemptEmitters {
		if !exemptSeen[name] {
			fmt.Fprintf(os.Stderr, "FAIL [DEF-37 STALE] exempt emitter %q not found in scanned files\n", name)
			fmt.Fprintf(os.Stderr, "  The function may have been renamed or removed. Update the exempt\n")
			fmt.Fprintf(os.Stderr, "  emitter list in this checker.\n")
			rc = 1
		}
	}

	// --- Print results ---

	for _, n := range notices {
		fmt.Println(n)
	}

	if rc == 0 {
		fmt.Println("check-security-marker-gates: all gates pass")
	} else {
		fmt.Fprintln(os.Stderr)
		fmt.Fprintln(os.Stderr, "check-security-marker-gates: FAILED — see above")
	}

	os.Exit(rc)
}

// countIdentsInFunc returns the number of *ast.Ident nodes with the exact given
// name inside the body of the named function. Returns 0 if the function is not
// found or has no body. ast.Inspect descends into closures (FuncLit nodes), so
// identifiers inside anonymous functions returned from the named function are
// included — this is load-bearing for messageEventHandler, which returns a
// closure containing the security-relevant calls.
func countIdentsInFunc(file *ast.File, funcName, symbol string) int {
	for _, decl := range file.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || fd.Name.Name != funcName || fd.Body == nil {
			continue
		}
		count := 0
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			if ident, ok := n.(*ast.Ident); ok && ident.Name == symbol {
				count++
			}
			return true
		})
		return count
	}
	return 0
}

// countFuncDefs returns the number of top-level function declarations whose
// Name.Name exactly matches the given symbol.
func countFuncDefs(file *ast.File, symbol string) int {
	count := 0
	for _, decl := range file.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		if fd.Name.Name == symbol {
			count++
		}
	}
	return count
}

// countCommentMentions returns the number of individual comment lines in the
// file that contain the given symbol as a substring. Used for INFORMATIONAL
// gates where the check is about documentation presence, not code behavior.
func countCommentMentions(file *ast.File, symbol string) int {
	count := 0
	for _, cg := range file.Comments {
		for _, c := range cg.List {
			if strings.Contains(c.Text, symbol) {
				count++
			}
		}
	}
	return count
}

// findFuncsWithIdent returns the names of all top-level functions whose body
// contains at least one *ast.Ident with the given name.
func findFuncsWithIdent(file *ast.File, symbol string) []string {
	var result []string
	for _, decl := range file.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || fd.Body == nil {
			continue
		}
		if countIdentsInFunc(file, fd.Name.Name, symbol) > 0 {
			result = append(result, fd.Name.Name)
		}
	}
	return result
}

// findFuncsWithAnyIdent returns the names of all top-level functions whose body
// contains at least one *ast.Ident matching any of the given symbols.
func findFuncsWithAnyIdent(file *ast.File, symbols []string) []string {
	var result []string
	for _, decl := range file.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || fd.Body == nil {
			continue
		}
		for _, sym := range symbols {
			if countIdentsInFunc(file, fd.Name.Name, sym) > 0 {
				result = append(result, fd.Name.Name)
				break
			}
		}
	}
	return result
}

// parseGlob parses all Go files matching the glob pattern, excluding test
// files. Returns the parsed files sorted by path for deterministic output.
func parseGlob(fset *token.FileSet, pattern string) []parsedFile {
	matches, err := filepath.Glob(pattern)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ABORT: glob %q failed: %v\n", pattern, err)
		os.Exit(2)
	}

	sort.Strings(matches)

	var result []parsedFile
	for _, path := range matches {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if err != nil {
			fmt.Fprintf(os.Stderr, "ABORT: could not parse %s: %v\n", path, err)
			fmt.Fprintf(os.Stderr, "  Nothing was analysed. This is a syntax error, not a guard failure.\n")
			os.Exit(2)
		}
		result = append(result, parsedFile{path: path, file: f})
	}

	if len(result) == 0 {
		fmt.Fprintf(os.Stderr, "ABORT: glob %q matched zero non-test files\n", pattern)
		os.Exit(2)
	}

	return result
}

func assertRequired(desc string, file *ast.File, filename, funcName, symbol string, expected int) {
	actual := countIdentsInFunc(file, funcName, symbol)
	if actual < expected {
		fmt.Fprintf(os.Stderr, "FAIL [REQUIRED] %s\n", desc)
		fmt.Fprintf(os.Stderr, "  expected %s at least x%d in %s (%s), found x%d\n",
			symbol, expected, funcName, filename, actual)
		rc = 1
	}
}

func assertFuncDef(desc string, file *ast.File, filename, symbol string, expected int) {
	actual := countFuncDefs(file, symbol)
	if actual < expected {
		fmt.Fprintf(os.Stderr, "FAIL [REQUIRED] %s\n", desc)
		fmt.Fprintf(os.Stderr, "  expected func %s definition at least x%d in %s, found x%d\n",
			symbol, expected, filename, actual)
		rc = 1
	}
}

func assertAudit(desc string, file *ast.File, filename, funcName, symbol string, expected int) {
	actual := countIdentsInFunc(file, funcName, symbol)
	if actual < expected {
		fmt.Fprintf(os.Stderr, "FAIL [AUDIT] %s\n", desc)
		fmt.Fprintf(os.Stderr, "  expected %s at least x%d in %s (%s), found x%d\n",
			symbol, expected, funcName, filename, actual)
		fmt.Fprintf(os.Stderr, "  This is a silent-denial path — logAuthzDenial is the ONLY record of the denial.\n")
		rc = 1
	}
}

func assertInformational(desc string, file *ast.File, filename, symbol string, expected int) {
	actual := countCommentMentions(file, symbol)
	if actual < expected {
		notices = append(notices, fmt.Sprintf(
			"NOTICE [INFORMATIONAL] %s: expected %s in ≥%d doc comments in %s, found %d",
			desc, symbol, expected, filename, actual))
	}
}
