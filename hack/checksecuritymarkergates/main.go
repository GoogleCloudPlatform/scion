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
// See hack/check-security-marker-gates.sh (the invoking wrapper) for full
// documentation of gate categories, exit codes, and the security rationale.
package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
)

var (
	rc      int
	notices []string
)

func main() {
	hamPath := "pkg/hub/handlers_agent_messaging.go"
	hcvPath := "pkg/hub/handlers_chat_v2.go"
	mbPath := "pkg/hub/messagebroker.go"
	hbiPath := "pkg/hub/handlers_broker_inbound.go"

	// File-existence precheck (exit 2).
	for _, f := range []string{hamPath, hcvPath, mbPath, hbiPath} {
		if _, err := os.Stat(f); err != nil {
			fmt.Fprintf(os.Stderr, "ABORT: guarded file not found or not readable: %s\n", f)
			fmt.Fprintf(os.Stderr, "  Nothing was analysed. This is an environment/rename issue, not a guard failure.\n")
			os.Exit(2)
		}
	}

	fset := token.NewFileSet()

	ham, err := parser.ParseFile(fset, hamPath, nil, parser.ParseComments)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ABORT: could not parse %s: %v\n", hamPath, err)
		fmt.Fprintf(os.Stderr, "  Nothing was analysed. This is a syntax error, not a guard failure.\n")
		os.Exit(2)
	}

	hcv, err := parser.ParseFile(fset, hcvPath, nil, parser.ParseComments)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ABORT: could not parse %s: %v\n", hcvPath, err)
		fmt.Fprintf(os.Stderr, "  Nothing was analysed. This is a syntax error, not a guard failure.\n")
		os.Exit(2)
	}

	mb, err := parser.ParseFile(fset, mbPath, nil, parser.ParseComments)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ABORT: could not parse %s: %v\n", mbPath, err)
		fmt.Fprintf(os.Stderr, "  Nothing was analysed. This is a syntax error, not a guard failure.\n")
		os.Exit(2)
	}

	hbi, err := parser.ParseFile(fset, hbiPath, nil, parser.ParseComments)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ABORT: could not parse %s: %v\n", hbiPath, err)
		fmt.Fprintf(os.Stderr, "  Nothing was analysed. This is a syntax error, not a guard failure.\n")
		os.Exit(2)
	}

	// ===== Gate assertions =====

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
// found or has no body.
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
