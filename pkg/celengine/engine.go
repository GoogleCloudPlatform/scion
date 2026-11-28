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

package celengine

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"cel.dev/cel-go/cel"
	"cel.dev/cel-go/common"
	celast "cel.dev/cel-go/common/ast"
	"cel.dev/cel-go/common/operators"
	"cel.dev/cel-go/common/types"
	"cel.dev/cel-go/ext"
	"cel.dev/cel-go/interpreter"
)

const pinnedStringsExtVersion uint32 = 5

var (
	errEngineTimeout     = errors.New("celengine: eval timeout exceeded")
	errPlanBoundExceeded = errors.New("celengine: plan-time bound exceeded")
)

// Engine compiles, caches, and evaluates CEL expressions for a single immutable
// consumer Environment and ceiling Limits.
type Engine struct {
	env       Environment
	limits    Limits
	celEnv    *cel.Env
	rawFormat cel.Program
	tzCache   *tzLRUCache
	functions []string
	fnSetMap  map[string]struct{}
	fnSetHash [32]byte
	cache     *programCache
}

// New validates env, limits, and cache configuration, and builds the underlying
// cel.Env once. An unknown type, duplicate variable, unknown extension/macro
// set, zero or negative limit ceiling, EvalTimeout > 1s, or CostLimit > 10_000_000
// returns an error.
func New(env Environment, limits Limits, cache CacheConfig) (*Engine, error) {
	if strings.TrimSpace(env.Name) == "" {
		return nil, errors.New("celengine: Environment.Name must not be empty")
	}
	if env.Version < 0 {
		return nil, fmt.Errorf("celengine: Environment.Version must be >= 0 (got %d)", env.Version)
	}
	if err := limits.validateCeiling(); err != nil {
		return nil, err
	}
	limits = limits.normalize()
	normCache, err := cache.normalize()
	if err != nil {
		return nil, err
	}
	if err := ensureEmbeddedTZData(); err != nil {
		return nil, err
	}

	opts := []cel.EnvOption{
		cel.EagerlyValidateDeclarations(true),
		cel.ParserRecursionLimit(limits.RecursionDepth),
		cel.ParserExpressionSizeLimit(limits.MaxExprBytes),
	}

	switch env.Macros {
	case "", MacrosStandard:
		// Standard macros are enabled by default in cel.NewEnv.
	case MacrosNone:
		opts = append(opts, cel.ClearMacros())
	default:
		return nil, fmt.Errorf("celengine: unsupported MacroSet %q", env.Macros)
	}

	seenExt := make(map[Extension]bool, len(env.Extensions))
	for _, extName := range env.Extensions {
		if seenExt[extName] {
			continue
		}
		seenExt[extName] = true
		switch extName {
		case ExtensionStrings:
			opts = append(opts, ext.Strings(ext.StringsVersion(pinnedStringsExtVersion)))
		default:
			return nil, fmt.Errorf("celengine: unsupported Extension %q", extName)
		}
	}

	seenVars := make(map[string]bool, len(env.Variables))
	varsCopy := make([]Variable, len(env.Variables))
	for i, v := range env.Variables {
		if strings.TrimSpace(v.Name) == "" {
			return nil, errors.New("celengine: Variable.Name must not be empty")
		}
		if seenVars[v.Name] {
			return nil, fmt.Errorf("celengine: duplicate Variable.Name %q", v.Name)
		}
		seenVars[v.Name] = true
		ct, err := v.Type.toCELType()
		if err != nil {
			return nil, fmt.Errorf("celengine: variable %q: %w", v.Name, err)
		}
		opts = append(opts, cel.Variable(v.Name, ct))
		varsCopy[i] = v
	}

	celEnv, err := cel.NewEnv(opts...)
	if err != nil {
		return nil, fmt.Errorf("celengine: build cel.Env: %w", err)
	}

	var rawFormat cel.Program
	if seenExt[ExtensionStrings] {
		rawFormat, err = buildRawFormatProgram()
		if err != nil {
			return nil, err
		}
	}

	fns, fnSetMap, fnSetHash := snapshotFunctions(celEnv)
	envCopy := Environment{
		Name:       env.Name,
		Version:    env.Version,
		Variables:  varsCopy,
		Extensions: slices.Clone(env.Extensions),
		Macros:     env.Macros,
	}
	if envCopy.Macros == "" {
		envCopy.Macros = MacrosStandard
	}

	return &Engine{
		env:       envCopy,
		limits:    limits,
		celEnv:    celEnv,
		rawFormat: rawFormat,
		tzCache:   newTZLRUCache(maxCachedTimezones),
		functions: fns,
		fnSetMap:  fnSetMap,
		fnSetHash: fnSetHash,
		cache:     newProgramCache(normCache),
	}, nil
}

func snapshotFunctions(celEnv *cel.Env) ([]string, map[string]struct{}, [32]byte) {
	fnDecls := celEnv.Functions()
	macros := celEnv.Macros()

	nameSet := make(map[string]struct{}, len(fnDecls)+len(macros))
	fnNames := make([]string, 0, len(fnDecls))
	for name := range fnDecls {
		nameSet[name] = struct{}{}
		fnNames = append(fnNames, name)
	}
	slices.Sort(fnNames)

	macroKeys := make([]string, 0, len(macros))
	for _, m := range macros {
		nameSet[m.Function()] = struct{}{}
		macroKeys = append(macroKeys, m.MacroKey())
	}
	slices.Sort(macroKeys)

	allNames := make([]string, 0, len(nameSet))
	for name := range nameSet {
		allNames = append(allNames, name)
	}
	slices.Sort(allNames)

	h := sha256.New()
	for _, fnName := range fnNames {
		_, _ = h.Write([]byte("fn:" + fnName + "\n"))
		decl := fnDecls[fnName]
		ovIDs := make([]string, 0, len(decl.OverloadDecls()))
		for _, ov := range decl.OverloadDecls() {
			ovIDs = append(ovIDs, ov.ID())
		}
		slices.Sort(ovIDs)
		for _, id := range ovIDs {
			_, _ = h.Write([]byte("ov:" + id + "\n"))
		}
	}
	for _, mk := range macroKeys {
		_, _ = h.Write([]byte("macro:" + mk + "\n"))
	}
	var digest [32]byte
	copy(digest[:], h.Sum(nil))
	return allNames, nameSet, digest
}

// Functions returns a sorted copy of the environment's function and macro names
// so consumers can pin the allowed surface against silent cel-go changes.
// Note: matches remains in the function surface, restricted at Compile to
// string literal patterns bounded by MaxRegexPatternBytes and
// MaxRegexFoldedRangeRunes.
func (e *Engine) Functions() []string {
	return slices.Clone(e.functions)
}

// Environment returns a copy of the Engine's Environment declaration.
func (e *Engine) Environment() Environment {
	out := e.env
	out.Variables = slices.Clone(e.env.Variables)
	out.Extensions = slices.Clone(e.env.Extensions)
	return out
}

// Limits returns the Engine's ceiling Limits.
func (e *Engine) Limits() Limits {
	return e.limits
}

// CacheStats returns point-in-time metrics for the compiled-program cache.
func (e *Engine) CacheStats() CacheStats {
	return e.cache.stats()
}

// Invalidate removes scope bindings matching scope and evicts any scoped
// program entries that become unreferenced.
func (e *Engine) Invalidate(scope CacheScope) {
	e.cache.invalidate(scope)
}

// Compile validates expr against the Engine's MaxExprBytes, parses, enforces
// literal-only and pre-scanned regular expressions on matches(), type-checks,
// and verifies the static result type against kind.Result.
func (e *Engine) Compile(expr string, kind Kind) (Program, error) {
	return e.CompileWithOptions(expr, kind)
}

// CompileWithOptions compiles expr like Compile, applying per-call Options (such
// as WithMaxExprBytes) that lower engine ceilings.
func (e *Engine) CompileWithOptions(expr string, kind Kind, opts ...Option) (Program, error) {
	maxBytes := e.effectiveMaxExprBytes(opts...)
	return e.compileInternal(expr, kind, maxBytes)
}

// Cached returns a compiled Program from the content-addressed LRU cache or
// compiles and stores it on miss.
func (e *Engine) Cached(ctx context.Context, scope CacheScope, expr string, kind Kind, opts ...Option) (Program, error) {
	prog, _, err := e.CachedWithMissGate(ctx, scope, expr, kind, nil, opts...)
	return prog, err
}

// CachedWithMissGate returns a compiled Program from the content-addressed LRU
// cache (hit=true) or, on a cache miss (hit=false), invokes beforeCompile (when
// non-nil) before compiling and storing the program. If beforeCompile returns a
// non-nil error, compilation is skipped and that error is returned with hit=false.
func (e *Engine) CachedWithMissGate(
	ctx context.Context,
	scope CacheScope,
	expr string,
	kind Kind,
	beforeCompile func() error,
	opts ...Option,
) (Program, bool, error) {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return Program{}, false, err
		}
	}
	maxBytes := e.effectiveMaxExprBytes(opts...)
	if len(expr) > maxBytes {
		return Program{}, false, &CompileError{
			Line:     1,
			Column:   1,
			Message:  fmt.Sprintf("expression length %d exceeds maximum %d bytes", len(expr), maxBytes),
			Kind:     kind.Name,
			Category: CategorySize,
		}
	}
	if !kind.Result.isSupportedResultKind() {
		return Program{}, false, &CompileError{
			Line:     1,
			Column:   1,
			Message:  fmt.Sprintf("unsupported result type %s for kind %q", kind.Result.String(), kind.Name),
			Kind:     kind.Name,
			Category: CategoryResultType,
		}
	}

	id := computeCacheIdentity(e.env.Name, e.env.Version, e.fnSetHash, kind, expr, maxBytes)
	if prog, ok := e.cache.get(id, scope); ok {
		return prog, true, nil
	}
	if beforeCompile != nil {
		if err := beforeCompile(); err != nil {
			return Program{}, false, err
		}
	}
	prog, err := e.compileInternal(expr, kind, maxBytes)
	if err != nil {
		return Program{}, false, err
	}
	return e.cache.put(id, len(expr), prog, scope), false, nil
}

func (e *Engine) effectiveMaxExprBytes(opts ...Option) int {
	cfg := compileOptions{maxExprBytes: e.limits.MaxExprBytes}
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}
	if cfg.maxExprBytes <= 0 || cfg.maxExprBytes > e.limits.MaxExprBytes {
		return e.limits.MaxExprBytes
	}
	return cfg.maxExprBytes
}

func (e *Engine) compileInternal(expr string, kind Kind, maxBytes int) (Program, error) {
	if len(expr) > maxBytes {
		return Program{}, &CompileError{
			Line:     1,
			Column:   1,
			Message:  fmt.Sprintf("expression length %d exceeds maximum %d bytes", len(expr), maxBytes),
			Kind:     kind.Name,
			Category: CategorySize,
		}
	}
	if !kind.Result.isSupportedResultKind() {
		return Program{}, &CompileError{
			Line:     1,
			Column:   1,
			Message:  fmt.Sprintf("unsupported result type %s for kind %q", kind.Result.String(), kind.Name),
			Kind:     kind.Name,
			Category: CategoryResultType,
		}
	}

	parsed, iss := e.celEnv.Parse(expr)
	if iss != nil && iss.Err() != nil {
		return Program{}, e.classifyParseIssues(iss, kind)
	}

	regexes, cerr := validateAndCompileMatchesAST(parsed, kind, e.limits.MaxRegexProgramSize)
	if cerr != nil {
		return Program{}, cerr
	}

	checked, iss := e.celEnv.Check(parsed)
	if iss != nil && iss.Err() != nil {
		return Program{}, e.classifyCheckIssues(parsed, iss, kind)
	}

	outType := checked.OutputType()
	if !kind.Result.matchesStaticOutput(outType) {
		line, col := rootLocation(checked)
		return Program{}, &CompileError{
			Line:     line,
			Column:   col,
			Message:  fmt.Sprintf("expression evaluates to %s, want %s", cel.FormatCELType(outType), kind.Result.String()),
			Kind:     kind.Name,
			Category: CategoryResultType,
		}
	}

	celProg, err := buildCELProgram(e.celEnv, checked, e.limits, e.rawFormat, e.tzCache, regexes, false)
	if err != nil {
		line, col := rootLocation(checked)
		cat := CategoryType
		switch {
		case errors.Is(err, errRegexLiteralRequired):
			cat = CategoryRegexLiteralRequired
		case errors.Is(err, errPlanBoundExceeded) || strings.Contains(err.Error(), "regex program size "):
			cat = CategorySize
		}
		return Program{}, &CompileError{
			Line:     line,
			Column:   col,
			Message:  err.Error(),
			Kind:     kind.Name,
			Category: cat,
		}
	}

	return Program{
		celEnv:    e.celEnv,
		rawFormat: e.rawFormat,
		tzCache:   e.tzCache,
		regexes:   regexes,
		checked:   checked,
		celProg:   celProg,
		kind:      kind,
		limits:    e.limits,
	}, nil
}

func buildCELProgram(celEnv *cel.Env, checked *cel.Ast, limits Limits, rawFormat cel.Program, tzCache *tzLRUCache, regexes map[string]precompiledRegex, forReplan bool) (prog cel.Program, err error) {
	defer func() {
		if r := recover(); r != nil {
			if cancelled, ok := r.(interpreter.EvalCancelledError); ok && cancelled.Cause == interpreter.CostLimitExceeded {
				prog = nil
				err = fmt.Errorf("%w: %s", errPlanBoundExceeded, cancelled.Message)
				return
			}
			prog = nil
			err = fmt.Errorf("celengine: plan failed: %v", r)
		}
	}()
	costEst, costOpts := buildCostTracking(limits)
	evalOpts := cel.OptOptimize | cel.OptTrackCost
	if forReplan && len(regexes) > 0 {
		// Never invoke cel-go's MatchesRegexOptimization regex parser during Eval
		// re-planning; boundedMatchesCall reuses Program.regexes directly.
		evalOpts = cel.OptTrackCost
	}
	return celEnv.Program(
		checked,
		cel.CustomDecoratorV2(outputBoundsDecorator(celEnv, limits, rawFormat, tzCache, regexes)),
		cel.EvalOptions(evalOpts),
		cel.CostTracking(costEst),
		cel.CostTrackerOptions(costOpts...),
		cel.CostLimit(limits.CostLimit),
		cel.InterruptCheckFrequency(limits.InterruptFrequency),
	)
}

func (e *Engine) classifyParseIssues(iss *cel.Issues, kind Kind) *CompileError {
	errs := iss.Errors()
	if len(errs) == 0 {
		return &CompileError{
			Line:     1,
			Column:   1,
			Message:  iss.Err().Error(),
			Kind:     kind.Name,
			Category: CategorySyntax,
		}
	}
	first := errs[0]
	line, col := oneBasedPos(first.Location)
	cat := CategorySyntax
	if strings.Contains(first.Message, "code point size exceeds limit") {
		cat = CategorySize
	}
	return &CompileError{
		Line:     line,
		Column:   col,
		Message:  first.Message,
		Kind:     kind.Name,
		Category: cat,
	}
}

func (e *Engine) classifyCheckIssues(parsed *cel.Ast, iss *cel.Issues, kind Kind) *CompileError {
	errs := iss.Errors()
	if len(errs) == 0 {
		return &CompileError{
			Line:     1,
			Column:   1,
			Message:  iss.Err().Error(),
			Kind:     kind.Name,
			Category: CategoryType,
		}
	}
	first := errs[0]
	line, col := oneBasedPos(first.Location)
	cat := CategoryType

	msg := first.Message
	switch {
	case strings.HasPrefix(msg, "regex program size "):
		cat = CategorySize
	case strings.HasPrefix(msg, "undeclared reference to '"):
		if exprNodeIsCall(parsed, first.ExprID) {
			cat = CategoryUnknownFunction
		} else {
			cat = CategoryUnknownVariable
		}
	case strings.HasPrefix(msg, "found no matching overload for '"):
		if fnName, ok := extractSingleQuoted(msg, "found no matching overload for '"); ok {
			if _, known := e.fnSetMap[fnName]; !known {
				cat = CategoryUnknownFunction
			}
		}
	}

	return &CompileError{
		Line:     line,
		Column:   col,
		Message:  msg,
		Kind:     kind.Name,
		Category: cat,
	}
}

func exprNodeIsCall(parsed *cel.Ast, exprID int64) bool {
	if parsed == nil || exprID == 0 {
		return false
	}
	rep := parsed.NativeRep()
	if rep == nil {
		return false
	}
	nav := celast.NavigateAST(rep)
	matched := celast.MatchDescendants(nav, func(n celast.NavigableExpr) bool {
		return n.ID() == exprID
	})
	for _, m := range matched {
		if m.Kind() == celast.CallKind {
			return true
		}
	}
	return false
}

func extractSingleQuoted(s, prefix string) (string, bool) {
	after, ok := strings.CutPrefix(s, prefix)
	if !ok {
		return "", false
	}
	idx := strings.IndexByte(after, '\'')
	if idx < 0 {
		return "", false
	}
	return after[:idx], true
}

func oneBasedPos(loc common.Location) (int, int) {
	if loc == nil || loc.Line() <= 0 {
		return 1, 1
	}
	col := loc.Column() + 1
	if col <= 0 {
		col = 1
	}
	return loc.Line(), col
}

func rootLocation(checked *cel.Ast) (int, int) {
	if checked == nil || checked.NativeRep() == nil || checked.NativeRep().SourceInfo() == nil {
		return 1, 1
	}
	loc := checked.NativeRep().SourceInfo().GetStartLocation(checked.NativeRep().Expr().ID())
	return oneBasedPos(loc)
}

// Program is an immutable compiled CEL expression bound to its Engine's
// Environment, Kind, and ceiling Limits.
type Program struct {
	celEnv    *cel.Env
	rawFormat cel.Program
	tzCache   *tzLRUCache
	regexes   map[string]precompiledRegex
	checked   *cel.Ast
	celProg   cel.Program
	kind      Kind
	limits    Limits
}

// IsZero reports whether p is an uninitialized Program value.
func (p Program) IsZero() bool {
	return p.celProg == nil
}

func (p Program) regexCacheWeight() int {
	if len(p.regexes) == 0 {
		return 0
	}
	total := 0
	for _, r := range p.regexes {
		if r.progSize > 0 {
			total += r.progSize * RegexInstructionWeightBytes
		}
	}
	return total
}

// Kind returns the Kind this Program was compiled for.
func (p Program) Kind() Kind {
	return p.kind
}

// ReferencesIdent reports whether p's checked AST contains an identifier
// reference matching any of names. It is conservative: a missing checked AST,
// empty identifier name, or malformed identifier node returns (true, err).
func (p Program) ReferencesIdent(names ...string) (bool, error) {
	if p.checked == nil {
		return true, errors.New("celengine: program has no checked AST")
	}
	rep := p.checked.NativeRep()
	if rep == nil || rep.Expr() == nil {
		return true, errors.New("celengine: program has empty checked AST")
	}
	if len(names) == 0 {
		return false, nil
	}
	wanted := make(map[string]struct{}, len(names))
	for _, name := range names {
		if name == "" {
			return true, errors.New("celengine: empty identifier name")
		}
		wanted[name] = struct{}{}
	}
	found := false
	var walkErr error
	celast.MatchDescendants(celast.NavigateAST(rep), func(n celast.NavigableExpr) bool {
		if found || walkErr != nil {
			return false
		}
		if n.Kind() == celast.IdentKind {
			ident := n.AsIdent()
			if ident == "" {
				walkErr = errors.New("celengine: empty identifier node in checked AST")
				return false
			}
			if _, ok := wanted[ident]; ok {
				found = true
				return true
			}
		}
		return false
	})
	if walkErr != nil {
		return true, walkErr
	}
	return found, nil
}

// MapKeyAccesses inspects p's checked AST for all references to the top-level
// map identifier rootName (e.g. "chain"). It returns sorted unique literal
// string keys accessed via rootName.key, has(rootName.key), rootName["key"],
// or "key" in rootName. If rootName is accessed with a non-literal key (e.g.
// rootName[x] or x in rootName) or used directly as a value (e.g. size(chain),
// chain == {}), dynamic is true. It is conservative: a missing checked AST or
// empty rootName returns an error.
func (p Program) MapKeyAccesses(rootName string) ([]string, bool, error) {
	if p.checked == nil {
		return nil, true, errors.New("celengine: program has no checked AST")
	}
	if rootName == "" {
		return nil, true, errors.New("celengine: empty root identifier name")
	}
	rep := p.checked.NativeRep()
	if rep == nil || rep.Expr() == nil {
		return nil, true, errors.New("celengine: program has empty checked AST")
	}
	keySet := make(map[string]struct{})
	dynamic := false
	celast.MatchDescendants(celast.NavigateAST(rep), func(n celast.NavigableExpr) bool {
		if n.Kind() != celast.IdentKind || n.AsIdent() != rootName {
			return false
		}
		parent, hasParent := n.Parent()
		if !hasParent {
			dynamic = true
			return false
		}
		switch parent.Kind() {
		case celast.SelectKind:
			sel := parent.AsSelect()
			if sel.Operand() != nil && sel.Operand().ID() == n.ID() {
				if field := sel.FieldName(); field != "" {
					keySet[field] = struct{}{}
					return false
				}
			}
			dynamic = true
		case celast.CallKind:
			call := parent.AsCall()
			args := call.Args()
			switch call.FunctionName() {
			case operators.Index:
				if len(args) == 2 && args[0].ID() == n.ID() && args[1].Kind() == celast.LiteralKind {
					if s, ok := args[1].AsLiteral().Value().(string); ok {
						keySet[s] = struct{}{}
						return false
					}
				}
				dynamic = true
			case operators.In, operators.OldIn:
				if len(args) == 2 && args[1].ID() == n.ID() && args[0].Kind() == celast.LiteralKind {
					if s, ok := args[0].AsLiteral().Value().(string); ok {
						keySet[s] = struct{}{}
						return false
					}
				}
				dynamic = true
			default:
				dynamic = true
			}
		default:
			dynamic = true
		}
		return false
	})
	keys := make([]string, 0, len(keySet))
	for k := range keySet {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys, dynamic, nil
}

// nowFunc is the clock used to measure Program.Eval elapsed wall-clock time;
// internal tests override it to exercise the post-eval elapsed > EvalTimeout
// backstop deterministically.
var nowFunc = time.Now

// Eval executes p against activation act under the engine's ceiling limits
// (optionally lowered by per-call overrides). Lowered per-call plan limits
// (CostLimit, InterruptFrequency, MaxOutputBytes, MaxCollectionElements,
// MaxRegexProgramSize, or MaxRegexWork) re-plan the already-typechecked AST on
// that evaluation; lowered EvalTimeout reuses the cached plan. Every failure
// returns a non-nil *EvalError and a nil Result; a failure or an evaluation
// whose wall-clock Elapsed exceeds the effective EvalTimeout never produces a
// decision.
func (p Program) Eval(ctx context.Context, act map[string]any, limits ...Limits) (Result, EvalStats, error) {
	if p.celProg == nil {
		return nil, EvalStats{}, &EvalError{Reason: ReasonRuntime}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if ctx.Err() != nil {
		return nil, EvalStats{}, &EvalError{Reason: ReasonCanceled}
	}

	eff := p.limits.clampEval(limits...)
	progToRun := p.celProg
	if eff.CostLimit != p.limits.CostLimit ||
		eff.InterruptFrequency != p.limits.InterruptFrequency ||
		eff.MaxOutputBytes != p.limits.MaxOutputBytes ||
		eff.MaxCollectionElements != p.limits.MaxCollectionElements ||
		eff.MaxRegexProgramSize != p.limits.MaxRegexProgramSize ||
		eff.MaxRegexWork != p.limits.MaxRegexWork {
		lowered, err := buildCELProgram(p.celEnv, p.checked, eff, p.rawFormat, p.tzCache, p.regexes, true)
		if err != nil {
			if errors.Is(err, errPlanBoundExceeded) || strings.Contains(err.Error(), "regex program size ") {
				return nil, EvalStats{}, &EvalError{Reason: ReasonCostLimit}
			}
			return nil, EvalStats{}, &EvalError{Reason: ReasonRuntime}
		}
		progToRun = lowered
	}

	if act == nil {
		act = map[string]any{}
	}

	evalCtx, cancel := context.WithTimeoutCause(ctx, eff.EvalTimeout, errEngineTimeout)
	defer cancel()

	start := nowFunc()
	val, det, err := progToRun.ContextEval(evalCtx, act)
	elapsed := nowFunc().Sub(start)

	var stats EvalStats
	stats.Elapsed = elapsed
	if det != nil {
		if cost := det.ActualCost(); cost != nil {
			stats.Cost = *cost
		}
	}

	if err != nil {
		return nil, stats, classifyEvalError(ctx, evalCtx, err)
	}
	if ctx.Err() != nil {
		return nil, stats, &EvalError{Reason: ReasonCanceled}
	}
	if elapsed > eff.EvalTimeout {
		return nil, stats, &EvalError{Reason: ReasonTimeout}
	}
	if val == nil || types.IsUnknownOrError(val) {
		return nil, stats, &EvalError{Reason: ReasonRuntime}
	}
	if stats.Cost > eff.CostLimit {
		return nil, stats, &EvalError{Reason: ReasonCostLimit}
	}

	switch p.kind.Result.kind {
	case typeBool:
		if val.Type() != types.BoolType {
			return nil, stats, &EvalError{Reason: ReasonType}
		}
		b, ok := val.Value().(bool)
		if !ok {
			return nil, stats, &EvalError{Reason: ReasonType}
		}
		return boolResult{kind: p.kind, val: b}, stats, nil
	case typeString:
		if val.Type() != types.StringType {
			return nil, stats, &EvalError{Reason: ReasonType}
		}
		s, ok := val.Value().(string)
		if !ok {
			return nil, stats, &EvalError{Reason: ReasonType}
		}
		return stringResult{kind: p.kind, val: s}, stats, nil
	default:
		return nil, stats, &EvalError{Reason: ReasonType}
	}
}

func classifyEvalError(callerCtx, evalCtx context.Context, err error) *EvalError {
	var cancelled interpreter.EvalCancelledError
	if errors.As(err, &cancelled) && cancelled.Cause == interpreter.CostLimitExceeded {
		return &EvalError{Reason: ReasonCostLimit}
	}

	if callerCtx.Err() != nil {
		return &EvalError{Reason: ReasonCanceled}
	}
	if errors.Is(err, errEngineTimeout) || (evalCtx.Err() != nil && errors.Is(context.Cause(evalCtx), errEngineTimeout)) {
		return &EvalError{Reason: ReasonTimeout}
	}
	if (errors.As(err, &cancelled) && cancelled.Cause == interpreter.ContextCancelled) ||
		errors.Is(err, interpreter.InterruptError{}) ||
		errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded) {
		return &EvalError{Reason: ReasonCanceled}
	}

	msg := err.Error()
	if strings.HasPrefix(msg, "no such attribute(s):") ||
		strings.HasPrefix(msg, "no such key:") ||
		strings.HasPrefix(msg, "no such field:") ||
		strings.HasPrefix(msg, "no such field '") {
		return &EvalError{Reason: ReasonMissingInput}
	}

	return &EvalError{Reason: ReasonRuntime}
}
