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

package logging

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// Request path uses in the hub and in this package are classified from the
// syntax tree. Every read of a request URL's path (X.URL.Path,
// X.URL.RawPath, X.URL.EscapedPath(), X.URL.String(), X.URL.RequestURI(),
// X.RequestURI) and every use of X.URL as a whole value must be listed in
// requestPathSites, per file and function with its exact count and a
// reason, so a path copied into a struct field (or anywhere else) is a
// classified use: a new copy fails as unclassified or over the count.
// Logging a request path goes through RequestPath(r), which reads r itself
// and so is not such a use. Independently of the list, no log call may
// receive a path read, or a local variable that holds one (through
// assignments and path-keeping transforms such as strings.TrimPrefix),
// however it is spelled or split over lines.

// requestPathSite is one allowed (file, function, kind) with its count.
type requestPathSite struct {
	file, fn, kind string
	n              int
	reason         string
}

var requestPathSites = []requestPathSite{
	{"admin_allow_list.go", "Server.handleAdminAllowListByEmail", "URL.Path", 1, "routing: extracts ids and sub-routes"},
	{"admin_invites.go", "Server.handleAdminInviteByID", "URL.Path", 1, "routing: extracts ids and sub-routes"},
	{"admin_maintenance.go", "Server.handleAdminMaintenanceMigrations", "URL.Path", 1, "routing: extracts ids and sub-routes"},
	{"admin_maintenance.go", "Server.handleAdminMaintenanceOps", "URL.Path", 1, "routing: extracts ids and sub-routes"},
	{"admin_mode.go", "WebServer.adminModeWebMiddleware", "URL.Path", 1, "comparison: matches admin-mode exempt paths"},
	{"admin_settings.go", "Server.handleAdminServerConfigSectionReset", "URL.Path", 1, "routing: extracts ids and sub-routes"},
	{"agent_routes.go", "resolveAgentSubRouteForRequest", "URL.EscapedPath", 1, "routing: resolves the agent sub-route from the escaped path"},
	{"agent_run_scope.go", "runScopeRequestFrom", "URL.Path", 1, "classification only: classPath picks the run-scope route class and is never logged (the logged path is logging.RequestPath)"},
	{"artifacts_host.go", "isArtifactCapabilityRequest", "URL.EscapedPath", 1, "comparison: checks the shape of a credential-less artifact read on both path forms"},
	{"artifacts_host.go", "isArtifactCapabilityRequest", "URL.Path", 4, "comparison: checks the shape of a credential-less artifact read on both path forms"},
	{"auth.go", "UnifiedAuthMiddleware", "URL.Path", 1, "comparison: health and unauthenticated endpoint checks"},
	{"auth.go", "isConstraintAuditAuthFailureRoute", "URL.EscapedPath", 1, "comparison: matches the constraint-audit routes on both path forms"},
	{"auth.go", "isConstraintAuditAuthFailureRoute", "URL.Path", 4, "comparison: matches the constraint-audit routes on both path forms"},
	{"auth.go", "isConstraintAuditAuthFailureRoute", "URL.RawPath", 1, "comparison: matches the constraint-audit routes on both path forms"},
	{"brokerauth.go", "BrokerAuthService.buildCanonicalString", "URL.Path", 1, "signature input: the broker request signature covers the path"},
	{"cloud_handler.go", "mapToCloudHTTPRequest", "URL(bare)", 1, "not the served request: rebuilds a request from an already redacted log URL"},
	{"conduit_proxy.go", "Server.serveConduitProxy", "URL.Path", 1, "routing: forwards the path to the proxied conduit target"},
	{"conduit_proxy.go", "Server.serveConduitProxy", "URL.RawPath", 1, "routing: forwards the path to the proxied conduit target"},
	{"devauth.go", "DevAuthMiddlewareWithDebug", "URL.Path", 2, "comparison: health endpoint checks"},
	{"download_signing.go", "Server.signSkillFileDownloadURLs", "URL(bare)", 3, "not a request URL: a signed storage URL field"},
	{"download_signing.go", "isSignedSkillFileRequest", "URL.Path", 3, "comparison: shape check of a signed skill file read"},
	{"google_credential_validator.go", "NewGoogleCredentialValidator", "URL(bare)", 1, "not the served request: an outbound redirect to a pinned Google endpoint"},
	{"handlers_access_constraints.go", "Server.handleAdminAccessConstraintByID", "URL.Path", 1, "routing: extracts ids and sub-routes"},
	{"handlers_access_constraints.go", "Server.handleAdminAccessConstraintPreviews", "URL.Path", 1, "routing: extracts ids and sub-routes"},
	{"handlers_auth.go", "Server.handleTokenByID", "URL.Path", 1, "routing: extracts the token id"},
	{"handlers_brokers.go", "Server.handleBrokerByIDRoutes", "URL.Path", 1, "routing: extracts ids and sub-routes"},
	{"handlers_chat_send.go", "Server.authorizeChatSend", "URL(bare)", 1, "not the served request: a synthetic request carrying a chat-send label path"},
	{"handlers_chat_v2.go", "Server.handleChatAttachmentByID", "URL.Path", 1, "routing: extracts ids and sub-routes"},
	{"handlers_chat_v2.go", "Server.handleChatConversationRoutes", "URL.Path", 1, "routing: extracts ids and sub-routes"},
	{"handlers_chat_v2.go", "Server.handleChatSpaceRoutes", "URL.Path", 1, "routing: extracts ids and sub-routes"},
	{"handlers_chat_v2.go", "Server.handleChatTopicRoutes", "URL.Path", 1, "routing: extracts ids and sub-routes"},
	{"handlers_gcp_identity_scoped.go", "Server.handleGCPServiceAccountByID", "URL.Path", 1, "routing: extracts ids and sub-routes"},
	{"handlers_github_app.go", "parseInstallationIDFromPath", "URL.Path", 1, "routing: extracts the installation id"},
	{"handlers_groups.go", "Server.handleGroupRoutes", "URL.Path", 1, "routing: extracts ids and sub-routes"},
	{"handlers_integrations.go", "Server.handleAdminIntegrationByName", "URL.Path", 1, "routing: extracts ids and sub-routes"},
	{"handlers_principals.go", "Server.handlePrincipalRoutes", "URL.Path", 1, "routing: extracts ids and sub-routes"},
	{"handlers_projects_core.go", "Server.handleProjectRoutes", "URL.Path", 1, "routing: extracts ids and sub-routes"},
	{"handlers_quota.go", "Server.handleAdminLimitByID", "URL.Path", 1, "routing: extracts ids and sub-routes"},
	{"handlers_roles.go", "Server.handleAdminRoleBindingByID", "URL.Path", 1, "routing: extracts ids and sub-routes"},
	{"handlers_runtime_brokers.go", "Server.handleRuntimeBrokerRoutes", "URL.Path", 1, "routing: extracts ids and sub-routes"},
	{"harness_config_handlers.go", "Server.handleHarnessConfigByID", "URL.Path", 1, "routing: extracts ids and sub-routes"},
	{"hub_pre_start_hook_handlers.go", "Server.handleHubPreStartHookByID", "URL.Path", 1, "routing: extracts ids and sub-routes"},
	{"lifecycle_hook_executor.go", "HTTPExecutor.doHTTPRequest", "URL(bare)", 1, "not a request URL: a lifecycle hook action's configured URL"},
	{"lifecycle_hook_executor.go", "HTTPExecutor.recordAudit", "URL(bare)", 2, "not a request URL: a lifecycle hook action's configured URL (only its host is recorded)"},
	{"maintenance_executors.go", "CheckForReleaseUpdates", "URL(bare)", 1, "not a request URL: a release's URL"},
	{"perftrace_middleware.go", "perfHeadersAllowed", "URL.Path", 1, "comparison: decides whether timing headers may be added"},
	{"project_webdav.go", "Server.handleProjectWebDAV", "URL.Path", 3, "routing: maps the WebDAV path onto the project files"},
	{"pty_handlers.go", "Server.handleAgentPTY", "URL.Path", 1, "routing: extracts the agent id"},
	{"redact.go", "IsCredentialURL", "URL(bare)", 1, "the redaction predicate"},
	{"redact.go", "RedactURL", "URL(bare)", 1, "the redaction helper"},
	{"redact.go", "RequestPath", "URL(bare)", 2, "the redaction helper"},
	{"redact.go", "RequestPath", "URL.Path", 1, "the redaction helper itself"},
	{"redact.go", "isArtifactURL", "URL(bare)", 1, "the redaction predicate"},
	{"request_log.go", "RequestLogMiddleware", "URL(bare)", 1, "redaction: passes r.URL to RedactURL"},
	{"request_log.go", "RequestLogMiddleware", "URL.Path", 1, "classification only: extracts project/agent ids for log fields (ids, not the path)"},
	{"server.go", "Server.registerRoutes", "URL.Path", 2, "routing: dispatch inside registered handlers"},
	{"server.go", "extractAction", "URL.Path", 1, "routing: extracts id and action"},
	{"server.go", "extractID", "URL.Path", 1, "routing: extracts an id"},
	{"server.go", "traceableRequest", "URL(bare)", 1, "redaction: passes r.URL to IsCredentialURL"},
	{"skill_dispatch_resolve.go", "rewriteLocalDownloadURLsRelative", "URL(bare)", 2, "not a request URL: a signed storage URL field"},
	{"skill_file_handlers.go", "withSkillVersionDownloadQuery", "URL(bare)", 2, "not a request URL: a signed storage URL field"},
	{"skill_file_handlers.go", "withSkillVersionUploadQuery", "URL(bare)", 2, "not a request URL: a signed storage URL field"},
	{"skill_handlers.go", "Server.handleSkillByID", "URL.Path", 1, "routing: extracts ids and sub-routes"},
	{"skill_handlers.go", "buildResolvedSkillResponse", "URL(bare)", 1, "not a request URL: a signed storage URL field"},
	{"skill_registry_handlers.go", "Server.handleSkillRegistryByID", "URL.Path", 1, "routing: extracts ids and sub-routes"},
	{"storage_helpers.go", "generateDownloadURLs", "URL(bare)", 2, "not a request URL: a signed storage URL field"},
	{"storage_helpers.go", "generateUploadURLs", "URL(bare)", 2, "not a request URL: a signed storage URL field"},
	{"storage_helpers.go", "rewriteLocalDownloadURLs", "URL(bare)", 2, "not a request URL: a signed storage URL field"},
	{"storage_helpers.go", "rewriteLocalUploadURLs", "URL(bare)", 2, "not a request URL: a signed storage URL field"},
	{"suspended_page.go", "WebServer.suspendedUserMiddleware", "URL.Path", 1, "comparison: lets the suspended page and its assets through"},
	{"template_handlers.go", "Server.handleTemplateByIDV2", "URL.Path", 1, "routing: extracts ids and sub-routes"},
	{"web.go", "WebServer.handleOAuthCallback", "URL.Path", 1, "routing: extracts the OAuth provider"},
	{"web.go", "WebServer.handleOAuthLogin", "URL.Path", 1, "routing: extracts the OAuth provider"},
	{"web.go", "WebServer.prefetchPageData", "URL.Path", 2, "routing: maps an SPA page path to its API prefetch"},
	{"web.go", "WebServer.proxyAuthMiddleware", "URL.Path", 1, "comparison: skips auth for public web paths"},
	{"web.go", "WebServer.serveStaticAsset", "URL.Path", 2, "routing: serves the static asset at the path"},
	{"web.go", "WebServer.sessionAuthMiddleware", "URL.Path", 2, "comparison, and the post-login redirect target stored in the session cookie (not logged)"},
	{"web.go", "WebServer.sessionToBearerMiddleware", "URL.Path", 1, "comparison, and the post-login redirect target stored in the session cookie (not logged)"},
	{"web.go", "WebServer.sessionToBearerMiddleware", "URL.RequestURI", 1, "comparison, and the post-login redirect target stored in the session cookie (not logged)"},
	{"web.go", "WebServer.spaHandler", "URL.Path", 3, "routing: SPA page and login-page checks"},
	{"web.go", "WebServer.tryServeStaticFile", "URL.Path", 1, "routing: serves the static file at the path"},
	{"workspace_handlers.go", "Server.handleWorkspaceSyncFrom", "URL(bare)", 1, "not a request URL: a signed storage URL field"},
	{"workspace_handlers.go", "Server.handleWorkspaceSyncTo", "URL(bare)", 1, "not a request URL: a signed storage URL field"},
	{"workspace_handlers.go", "generateWorkspaceUploadURLs", "URL(bare)", 1, "not a request URL: a signed storage URL field"},
}

// requestPathSinks are log calls that receive a value derived from a
// request path and are allowed, per file and function with their count and
// a reason. Each logs one route segment cut from a fixed route prefix that
// no artifact route shares.
var requestPathSinks = []requestPathSite{
	{"admin_settings.go", "Server.handleAdminServerConfigSectionReset", "sink", 2,
		"logs the segment after the fixed prefix /api/v1/admin/server-config/sections/ (the settings section name); the handler is mounted only on that admin prefix, so artifact share-link and view URLs, the only paths that carry a token or capability, never reach it"},
	{"web.go", "WebServer.handleOAuthLogin", "sink", 2,
		"logs the segment after the fixed prefix /auth/login/ (the OAuth provider name, checked against the configured providers); the handler is mounted only on that web prefix, so artifact share-link and view URLs never reach it"},
	{"web.go", "WebServer.handleOAuthCallback", "sink", 3,
		"logs the segment after the fixed prefix /auth/callback/ (the OAuth provider name); the handler is mounted only on that web prefix, so artifact share-link and view URLs never reach it"},
}

// requestPathFields are the struct fields ("Type.field") that are given a
// request path somewhere, each with a reason. A struct literal or field
// assignment that stores a path in a field not listed here fails, and no
// log call may receive a listed field, so a path cannot reach a log
// through a struct either.
var requestPathFields = map[string]string{
	"runScopeRequest.classPath": "picks the run-scope route class; the logged path is the separate path field (logging.RequestPath)",
	"pageDataEnvelope.Path":     "the SPA page path rendered into the page's own prefetch JSON, not logged",
}

// pathSource reports whether e reads a request path or the request URL as a
// whole, and which kind. parent is e's parent node.
func pathSource(e ast.Node, parent ast.Node) (string, bool) {
	sel, ok := e.(*ast.SelectorExpr)
	if !ok {
		return "", false
	}
	inner, innerOK := sel.X.(*ast.SelectorExpr)
	switch sel.Sel.Name {
	case "Path", "RawPath", "EscapedPath", "String", "RequestURI":
		if innerOK && inner.Sel.Name == "URL" {
			return "URL." + sel.Sel.Name, true
		}
		if sel.Sel.Name == "RequestURI" {
			return "RequestURI", true
		}
	case "URL":
		if _, isSel := parent.(*ast.SelectorExpr); !isSel {
			return "URL(bare)", true
		}
	}
	return "", false
}

// logMethods are method names that log; logAttrFuncs are slog attribute
// constructors.
var (
	logMethods = map[string]bool{
		"Debug": true, "Info": true, "Warn": true, "Error": true,
		"DebugContext": true, "InfoContext": true, "WarnContext": true, "ErrorContext": true,
		"Log": true, "LogAttrs": true, "With": true, "Printf": true, "Println": true, "Print": true,
	}
	logAttrFuncs = map[string]bool{"String": true, "Any": true, "Group": true, "Attr": true}
	// pathTransforms keep a path a path.
	pathTransforms = map[string]bool{
		"TrimPrefix": true, "TrimSuffix": true, "Trim": true, "TrimLeft": true, "TrimRight": true,
		"ToLower": true, "Clean": true, "Join": true, "Sprintf": true, "Sprint": true,
	}
)

// isLogCall reports whether c is a logging call.
func isLogCall(c *ast.CallExpr) bool {
	sel, ok := c.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	if logMethods[sel.Sel.Name] {
		return true
	}
	id, ok := sel.X.(*ast.Ident)
	return ok && id.Name == "slog" && logAttrFuncs[sel.Sel.Name]
}

// siteScan is the scan of a set of files.
type siteScan struct {
	uses  map[string]int // "file\tfn\tkind" -> count
	sinks []string       // log calls that receive a path
	// sinkCount counts those log calls per "file\tfn".
	sinkCount map[string]int
	// fields are the "Type.field" names given a request path, with where.
	fields map[string][]string
}

// typeName returns the named type of a type expression (T, *T, pkg.T).
func typeName(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.Ident:
		return x.Name
	case *ast.StarExpr:
		return typeName(x.X)
	case *ast.SelectorExpr:
		return x.Sel.Name
	case *ast.UnaryExpr:
		return typeName(x.X)
	case *ast.CompositeLit:
		if x.Type != nil {
			return typeName(x.Type)
		}
	}
	return ""
}

// localTypes maps the parameters, receiver and composite-literal or var
// declared locals of fd to their named types.
func localTypes(fd *ast.FuncDecl) map[string]string {
	types := map[string]string{}
	add := func(fl *ast.FieldList) {
		if fl == nil {
			return
		}
		for _, f := range fl.List {
			if tn := typeName(f.Type); tn != "" {
				for _, n := range f.Names {
					types[n.Name] = tn
				}
			}
		}
	}
	add(fd.Recv)
	add(fd.Type.Params)
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.AssignStmt:
			for i, lhs := range x.Lhs {
				if id, ok := lhs.(*ast.Ident); ok && i < len(x.Rhs) {
					if tn := typeName(x.Rhs[i]); tn != "" {
						types[id.Name] = tn
					}
				}
			}
		case *ast.ValueSpec:
			tn := ""
			if x.Type != nil {
				tn = typeName(x.Type)
			}
			for i, id := range x.Names {
				if tn != "" {
					types[id.Name] = tn
				} else if i < len(x.Values) {
					if t := typeName(x.Values[i]); t != "" {
						types[id.Name] = t
					}
				}
			}
		}
		return true
	})
	return types
}

func funcName(fd *ast.FuncDecl) string {
	if fd.Recv != nil && len(fd.Recv.List) > 0 {
		t := fd.Recv.List[0].Type
		if s, ok := t.(*ast.StarExpr); ok {
			t = s.X
		}
		if ix, ok := t.(*ast.IndexExpr); ok {
			t = ix.X
		}
		if id, ok := t.(*ast.Ident); ok {
			return id.Name + "." + fd.Name.Name
		}
	}
	return fd.Name.Name
}

// scanSources parses every non-test Go file of dirs (or the given files).
func scanSources(t *testing.T, files []string) siteScan {
	t.Helper()
	fset := token.NewFileSet()
	type parsed struct {
		name string
		f    *ast.File
	}
	var all []parsed
	for _, name := range files {
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		all = append(all, parsed{filepath.Base(name), f})
	}
	scan := siteScan{uses: map[string]int{}, sinkCount: map[string]int{}, fields: map[string][]string{}}
	var curTypes map[string]string
	containsPath := func(e ast.Node, tainted map[string]bool) bool {
		found := false
		var stack []ast.Node
		ast.Inspect(e, func(n ast.Node) bool {
			if n == nil {
				stack = stack[:len(stack)-1]
				return true
			}
			var parent ast.Node
			if len(stack) > 0 {
				parent = stack[len(stack)-1]
			}
			stack = append(stack, n)
			if found {
				return false
			}
			if k, ok := pathSource(n, parent); ok && k != "URL(bare)" {
				found = true
				return false
			}
			switch x := n.(type) {
			case *ast.FuncLit:
				// A function value is not a path; its body is scanned as
				// part of the enclosing function.
				return false
			case *ast.BinaryExpr:
				// A comparison yields a bool, not the path.
				switch x.Op {
				case token.EQL, token.NEQ, token.LSS, token.GTR, token.LEQ, token.GEQ, token.LAND, token.LOR:
					return false
				}
			case *ast.Ident:
				if tainted[x.Name] {
					found = true
				}
			case *ast.SelectorExpr:
				if id, ok := x.X.(*ast.Ident); ok && curTypes != nil {
					if _, listed := requestPathFields[curTypes[id.Name]+"."+x.Sel.Name]; listed {
						found = true
					}
				}
			case *ast.CallExpr:
				// Only path-keeping transforms carry a path through a call;
				// the result of any other call (an id parsed from the path)
				// is not the path.
				switch f := x.Fun.(type) {
				case *ast.SelectorExpr:
					if _, isSrc := pathSource(f, x); isSrc {
						found = true
						return false
					}
					if !pathTransforms[f.Sel.Name] {
						return false
					}
				default:
					return false
				}
			}
			return true
		})
		return found
	}
	for _, p := range all {
		for _, d := range p.f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			fn := funcName(fd)
			curTypes = localTypes(fd)
			// Struct fields given a path here.
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.CompositeLit:
					tn := typeName(x)
					for _, el := range x.Elts {
						if kv, ok := el.(*ast.KeyValueExpr); ok {
							if key, ok := kv.Key.(*ast.Ident); ok && tn != "" && containsPath(kv.Value, nil) {
								scan.fields[tn+"."+key.Name] = append(scan.fields[tn+"."+key.Name], fmt.Sprintf("%s:%d", p.name, fset.Position(kv.Pos()).Line))
							}
						}
					}
				case *ast.AssignStmt:
					for i, lhs := range x.Lhs {
						sel, ok := lhs.(*ast.SelectorExpr)
						if !ok || i >= len(x.Rhs) || !containsPath(x.Rhs[i], nil) {
							continue
						}
						if id, ok := sel.X.(*ast.Ident); ok && curTypes[id.Name] != "" {
							k := curTypes[id.Name] + "." + sel.Sel.Name
							scan.fields[k] = append(scan.fields[k], fmt.Sprintf("%s:%d", p.name, fset.Position(x.Pos()).Line))
						}
					}
				}
				return true
			})
			var stack []ast.Node
			ast.Inspect(fd, func(n ast.Node) bool {
				if n == nil {
					stack = stack[:len(stack)-1]
					return true
				}
				var parent ast.Node
				if len(stack) > 0 {
					parent = stack[len(stack)-1]
				}
				stack = append(stack, n)
				if k, ok := pathSource(n, parent); ok {
					scan.uses[p.name+"\t"+fn+"\t"+k]++
				}
				return true
			})
			// Local variables that hold a path, to a fixed point.
			tainted := map[string]bool{}
			for range 3 {
				ast.Inspect(fd.Body, func(n ast.Node) bool {
					switch x := n.(type) {
					case *ast.AssignStmt:
						for i, lhs := range x.Lhs {
							id, ok := lhs.(*ast.Ident)
							if !ok {
								continue
							}
							rhs := x.Rhs[min(i, len(x.Rhs)-1)]
							if containsPath(rhs, tainted) {
								tainted[id.Name] = true
							}
						}
					case *ast.ValueSpec:
						for i, id := range x.Names {
							if i < len(x.Values) && containsPath(x.Values[i], tainted) {
								tainted[id.Name] = true
							}
						}
					}
					return true
				})
			}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				c, ok := n.(*ast.CallExpr)
				if !ok || !isLogCall(c) {
					return true
				}
				for _, a := range c.Args {
					if containsPath(a, tainted) {
						scan.sinks = append(scan.sinks, fmt.Sprintf("%s:%d (%s)", p.name, fset.Position(c.Pos()).Line, fn))
						scan.sinkCount[p.name+"\t"+fn]++
						break
					}
				}
				return true
			})
		}
	}
	return scan
}

func goFiles(t *testing.T, dirs ...string) []string {
	t.Helper()
	var out []string
	for _, dir := range dirs {
		names, err := filepath.Glob(filepath.Join(dir, "*.go"))
		if err != nil {
			t.Fatal(err)
		}
		for _, n := range names {
			if !strings.HasSuffix(n, "_test.go") {
				out = append(out, n)
			}
		}
	}
	return out
}

// TestRequestPathSites: every request path use in the hub and this package
// is classified, and no log call receives a request path except through
// RequestPath.
func TestRequestPathSites(t *testing.T) {
	files := goFiles(t, ".", "../../hub")
	if len(files) < 50 {
		t.Fatalf("scanned only %d files; is the hub package where this test expects it?", len(files))
	}
	scan := scanSources(t, files)
	for f, where := range scan.fields {
		if _, ok := requestPathFields[f]; !ok {
			t.Errorf("struct field %s is given a request path (%v): log paths with logging.RequestPath, or list the field in requestPathFields with a reason", f, where)
		}
	}
	for f := range requestPathFields {
		if len(scan.fields[f]) == 0 {
			t.Errorf("stale requestPathFields entry %s", f)
		}
	}
	sinkOK := map[string]requestPathSite{}
	for _, s := range requestPathSinks {
		sinkOK[s.file+"\t"+s.fn] = s
	}
	for k, n := range scan.sinkCount {
		if s, ok := sinkOK[k]; !ok || s.n != n {
			t.Errorf("%d log calls in %s receive a request path not via logging.RequestPath (allowed: %d); sinks: %v",
				n, strings.ReplaceAll(k, "\t", " "), s.n, scan.sinks)
		}
	}
	for k, s := range sinkOK {
		if scan.sinkCount[k] == 0 {
			t.Errorf("stale requestPathSinks entry %s %s", s.file, s.fn)
		}
	}

	allowed := map[string]requestPathSite{}
	for _, s := range requestPathSites {
		if s.reason == "" {
			t.Errorf("site %s %s %s has no reason", s.file, s.fn, s.kind)
		}
		allowed[s.file+"\t"+s.fn+"\t"+s.kind] = s
	}
	var keys []string
	for k := range scan.uses {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		s, ok := allowed[k]
		switch {
		case !ok:
			t.Errorf("unclassified request path use %q (x%d): log it with logging.RequestPath, or add it to requestPathSites with a reason", strings.ReplaceAll(k, "\t", " "), scan.uses[k])
		case s.n != scan.uses[k]:
			t.Errorf("request path use %q: %d uses, %d classified (%s)", strings.ReplaceAll(k, "\t", " "), scan.uses[k], s.n, s.reason)
		}
	}
	for k, s := range allowed {
		if scan.uses[k] == 0 {
			t.Errorf("stale requestPathSites entry %s %s %s", s.file, s.fn, s.kind)
		}
	}
}

// TestRequestPathSitesCatchIndirection: the scan catches a path logged
// through a local variable, a struct field, a call split over lines, and
// an unclassified read.
func TestRequestPathSitesCatchIndirection(t *testing.T) {
	dir := t.TempDir()
	src := `package x

import (
	"log/slog"
	"net/http"
	"strings"
)

type req struct{ path string }

func viaVar(r *http.Request) {
	p := r.URL.Path
	slog.Info("x", "path", p)
}

func viaTransform(r *http.Request) {
	p := strings.TrimPrefix(r.URL.EscapedPath(), "/api")
	slog.Warn("x", "path", p)
}

func build(r *http.Request) req { return req{path: r.URL.Path} }

func viaField(q req) {
	slog.Info("x", "path", q.path)
}

func split(r *http.Request) {
	slog.Info("x",
		"path",
		r.URL.Path)
}

func viaAttr(r *http.Request) {
	slog.Info("x", slog.String("path", r.RequestURI))
}

func clean(r *http.Request) {
	id := extract(r.URL.Path)
	slog.Info("x", "id", id)
}

func extract(string) string { return "" }
`
	name := filepath.Join(dir, "x.go")
	if err := os.WriteFile(name, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	scan := scanSources(t, []string{name})
	got := map[string]bool{}
	for _, s := range scan.sinks {
		got[s[strings.Index(s, "(")+1:len(s)-1]] = true
	}
	for _, fn := range []string{"viaVar", "viaTransform", "split", "viaAttr"} {
		if !got[fn] {
			t.Errorf("sink in %s not caught (sinks %v)", fn, scan.sinks)
		}
	}
	if got["clean"] {
		t.Errorf("an id extracted from the path was taken for the path")
	}
	// The struct-field copy is a use in build and a field given a path,
	// both of which TestRequestPathSites fails on unless listed.
	if len(scan.fields["req.path"]) != 1 {
		t.Errorf("struct field given a path not found: %v", scan.fields)
	}
	if scan.uses["x.go\tbuild\tURL.Path"] != 1 || scan.uses["x.go\tclean\tURL.Path"] != 1 {
		t.Errorf("uses = %v", scan.uses)
	}
}
