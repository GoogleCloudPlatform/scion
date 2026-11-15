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
	"fmt"
	"math"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"cel.dev/cel-go/cel"
	"cel.dev/cel-go/common/functions"
	"cel.dev/cel-go/common/operators"
	"cel.dev/cel-go/common/overloads"
	"cel.dev/cel-go/common/types"
	"cel.dev/cel-go/common/types/ref"
	"cel.dev/cel-go/common/types/traits"
	"cel.dev/cel-go/ext"
	"cel.dev/cel-go/interpreter"
)

const (
	maxPinnedFormatPrec  = 100
	rabinKarpPatMinBytes = 64
	rabinKarpWorkCutoff  = 4 << 20 // 4 MiB-comparisons before switching to linear KMP
)

func abortOutputLimit() {
	panic(interpreter.EvalCancelledError{
		Cause:   interpreter.CostLimitExceeded,
		Message: "operation cancelled: output size limit exceeded",
	})
}

func buildRawFormatProgram() (cel.Program, error) {
	rawEnv, err := cel.NewEnv(
		ext.Strings(ext.StringsVersion(pinnedStringsExtVersion)),
		cel.Variable("_fmt", cel.StringType),
		cel.Variable("_args", cel.ListType(cel.DynType)),
	)
	if err != nil {
		return nil, fmt.Errorf("celengine: build raw format env: %w", err)
	}
	ast, iss := rawEnv.Compile(`_fmt.format(_args)`)
	if iss != nil && iss.Err() != nil {
		return nil, fmt.Errorf("celengine: compile raw format helper: %w", iss.Err())
	}
	prog, err := rawEnv.Program(ast)
	if err != nil {
		return nil, fmt.Errorf("celengine: plan raw format helper: %w", err)
	}
	return prog, nil
}

// outputBoundsDecorator wraps CEL call nodes whose output size, collection
// cardinality, or inner loop work can amplify beyond their inputs within a
// single uninterruptible host call. Exceeding Limits.MaxOutputBytes,
// Limits.MaxCollectionElements, Limits.MaxRegexProgramSize, or
// Limits.MaxRegexWork aborts evaluation immediately with
// interpreter.CostLimitExceeded (-> EvalError{Reason: ReasonCostLimit}) before
// allocating or executing the unbounded work, and cannot be swallowed by
// non-strict ||.
func outputBoundsDecorator(limits Limits, rawFormat cel.Program) interpreter.InterpretableDecoratorV2 {
	maxBytes := limits.MaxOutputBytes
	maxElems := limits.MaxCollectionElements
	maxRegexProgSize := limits.MaxRegexProgramSize
	maxRegexWork := limits.MaxRegexWork
	if maxBytes <= 0 {
		maxBytes = DefaultMaxOutputBytes
	}
	if maxElems <= 0 {
		maxElems = DefaultMaxCollectionElements
	}
	if maxRegexProgSize <= 0 {
		maxRegexProgSize = DefaultMaxRegexProgramSize
	}
	if maxRegexWork <= 0 {
		maxRegexWork = DefaultMaxRegexWork
	}

	return func(node interpreter.InterpretableV2) (interpreter.InterpretableV2, error) {
		call, ok := node.(interpreter.InterpretableCall)
		if !ok {
			return node, nil
		}
		arity := len(call.Args())
		var impl functions.FunctionOp

		switch call.Function() {
		case overloads.Matches:
			if arity == 2 {
				return newBoundedMatchesCall(call, maxBytes, maxRegexProgSize, maxRegexWork)
			}
		case "replace":
			if arity == 3 || arity == 4 {
				impl = boundedReplace(maxBytes)
			}
		case "join":
			if arity == 1 || arity == 2 {
				impl = boundedJoin(maxBytes, maxElems)
			}
		case "split":
			if arity == 2 || arity == 3 {
				impl = boundedSplit(maxBytes, maxElems)
			}
		case "format":
			if arity == 2 && rawFormat != nil {
				impl = boundedFormat(maxBytes, maxElems, rawFormat)
			}
		case "strings.quote":
			if arity == 1 {
				impl = boundedQuote(maxBytes)
			}
		case operators.Add:
			if arity == 2 {
				impl = boundedAdd(maxBytes, maxElems)
			}
		case "indexOf":
			if arity == 2 || arity == 3 {
				impl = boundedIndexOf(maxBytes)
			}
		case "lastIndexOf":
			if arity == 2 || arity == 3 {
				impl = boundedLastIndexOf(maxBytes)
			}
		case "charAt":
			if arity == 2 {
				impl = boundedCharAt(maxBytes)
			}
		case "lowerAscii":
			if arity == 1 {
				impl = boundedLowerASCII(maxBytes)
			}
		case "upperAscii":
			if arity == 1 {
				impl = boundedUpperASCII(maxBytes)
			}
		case "reverse":
			if arity == 1 {
				impl = boundedReverse(maxBytes)
			}
		case "substring":
			if arity == 2 || arity == 3 {
				impl = boundedSubstring(maxBytes)
			}
		case "trim":
			if arity == 1 {
				impl = boundedTrim(maxBytes)
			}
		case overloads.Contains:
			if arity == 2 {
				impl = boundedContains(maxBytes)
			}
		case overloads.StartsWith:
			if arity == 2 {
				impl = boundedStartsWith(maxBytes)
			}
		case overloads.EndsWith:
			if arity == 2 {
				impl = boundedEndsWith(maxBytes)
			}
		case overloads.Size:
			if arity == 1 {
				impl = boundedSize(maxBytes, maxElems)
			}
		case overloads.TypeConvertBytes:
			if arity == 1 {
				impl = boundedBytesConv(maxBytes)
			}
		case overloads.TypeConvertString:
			if arity == 1 {
				impl = boundedStringConv(maxBytes)
			}
		case overloads.TypeConvertBool:
			if arity == 1 {
				impl = boundedScalarStringInputConv(maxBytes, types.BoolType)
			}
		case overloads.TypeConvertDouble:
			if arity == 1 {
				impl = boundedScalarStringInputConv(maxBytes, types.DoubleType)
			}
		case overloads.TypeConvertDuration:
			if arity == 1 {
				impl = boundedScalarStringInputConv(maxBytes, types.DurationType)
			}
		case overloads.TypeConvertInt:
			if arity == 1 {
				impl = boundedScalarStringInputConv(maxBytes, types.IntType)
			}
		case overloads.TypeConvertTimestamp:
			if arity == 1 {
				impl = boundedScalarStringInputConv(maxBytes, types.TimestampType)
			}
		case overloads.TypeConvertUint:
			if arity == 1 {
				impl = boundedScalarStringInputConv(maxBytes, types.UintType)
			}
		case overloads.TimeGetFullYear,
			overloads.TimeGetMonth,
			overloads.TimeGetDayOfYear,
			overloads.TimeGetDayOfMonth,
			overloads.TimeGetDate,
			overloads.TimeGetDayOfWeek,
			overloads.TimeGetHours,
			overloads.TimeGetMinutes,
			overloads.TimeGetSeconds,
			overloads.TimeGetMilliseconds:
			if arity == 2 {
				impl = boundedTimezoneAccessor(call.Function(), call.OverloadID(), maxBytes)
			}
		case operators.Equals:
			if arity == 2 {
				impl = boundedEqNe(true, maxBytes, maxElems)
			}
		case operators.NotEquals:
			if arity == 2 {
				impl = boundedEqNe(false, maxBytes, maxElems)
			}
		case operators.In, operators.OldIn, overloads.DeprecatedIn:
			if arity == 2 {
				impl = boundedIn(maxBytes, maxElems)
			}
		}

		if impl == nil {
			return node, nil
		}
		return interpreter.NewCall(call.ID(), call.Function(), call.OverloadID(), call.Args(), impl), nil
	}
}

func boundedReplace(maxBytes int) functions.FunctionOp {
	return func(args ...ref.Val) ref.Val {
		sVal, ok := args[0].(types.String)
		if !ok {
			return types.MaybeNoSuchOverloadErr(args[0])
		}
		oldVal, ok := args[1].(types.String)
		if !ok {
			return types.MaybeNoSuchOverloadErr(args[1])
		}
		newVal, ok := args[2].(types.String)
		if !ok {
			return types.MaybeNoSuchOverloadErr(args[2])
		}
		limit := int64(-1)
		if len(args) == 4 {
			nVal, ok := args[3].(types.Int)
			if !ok {
				return types.MaybeNoSuchOverloadErr(args[3])
			}
			limit = int64(nVal)
		}

		str := string(sVal)
		oldStr := string(oldVal)
		newStr := string(newVal)

		if len(str) > maxBytes {
			abortOutputLimit()
		}
		if limit == 0 {
			return sVal
		}

		var k int64
		baseLen := int64(len(str))
		if oldStr == "" {
			var runeCount int64
			if utf8.ValidString(str) {
				runeCount = int64(utf8.RuneCountInString(str))
			} else {
				baseLen = 0
				for _, r := range str {
					runeCount++
					baseLen += int64(utf8.RuneLen(r))
				}
			}
			k = runeCount + 1
			if limit > 0 && limit < k {
				k = limit
			}
			if baseLen > int64(maxBytes) {
				abortOutputLimit()
			}
			if len(newStr) > 0 && k > (int64(maxBytes)-baseLen)/int64(len(newStr)) {
				abortOutputLimit()
			}
		} else if shouldUseLinearKMPBytes(len(str), len(oldStr)) {
			maxMatches := -1
			if limit > 0 {
				if limit > int64(len(str))+1 {
					maxMatches = len(str) + 1
				} else {
					maxMatches = int(limit)
				}
			}
			matches := kmpFindBytes(str, oldStr, maxMatches, 0)
			if len(matches) == 0 {
				return sVal
			}
			k = int64(len(matches))
			if len(newStr) > len(oldStr) {
				if len(newStr) > maxBytes {
					abortOutputLimit()
				}
				delta := int64(len(newStr) - len(oldStr))
				if k > (int64(maxBytes)-baseLen)/delta {
					abortOutputLimit()
				}
			}
			outLen := int(baseLen + k*int64(len(newStr)-len(oldStr)))
			if outLen > maxBytes {
				abortOutputLimit()
			}
			var sb strings.Builder
			sb.Grow(outLen)
			prev := 0
			for _, pos := range matches {
				sb.WriteString(str[prev:pos])
				sb.WriteString(newStr)
				prev = pos + len(oldStr)
			}
			sb.WriteString(str[prev:])
			return types.String(sb.String())
		} else {
			if limit < 0 {
				k = int64(strings.Count(str, oldStr))
			} else {
				rem := str
				for k < limit {
					idx := strings.Index(rem, oldStr)
					if idx < 0 {
						break
					}
					k++
					rem = rem[idx+len(oldStr):]
				}
			}
			if k == 0 {
				return sVal
			}
			if len(newStr) > len(oldStr) {
				if len(newStr) > maxBytes {
					abortOutputLimit()
				}
				delta := int64(len(newStr) - len(oldStr))
				if k > (int64(maxBytes)-baseLen)/delta {
					abortOutputLimit()
				}
			}
		}

		maxCount := int64(len(str)) + 1
		if limit > maxCount {
			limit = maxCount
		}
		var out string
		if limit < 0 {
			out = strings.ReplaceAll(str, oldStr, newStr)
		} else {
			out = strings.Replace(str, oldStr, newStr, int(limit))
		}
		if len(out) > maxBytes {
			abortOutputLimit()
		}
		return types.String(out)
	}
}

func boundedJoin(maxBytes, maxElems int) functions.FunctionOp {
	return func(args ...ref.Val) ref.Val {
		list, ok := args[0].(traits.Lister)
		if !ok {
			return types.MaybeNoSuchOverloadErr(args[0])
		}
		sep := ""
		if len(args) == 2 {
			sepVal, ok := args[1].(types.String)
			if !ok {
				return types.MaybeNoSuchOverloadErr(args[1])
			}
			sep = string(sepVal)
		}

		szVal, ok := list.Size().(types.Int)
		if !ok {
			return types.NewErr("join: invalid list size")
		}
		sz := int64(szVal)
		if sz < 0 || sz > int64(maxElems) {
			abortOutputLimit()
		}
		if sz == 0 {
			return types.String("")
		}

		var total int64
		if sz > 1 && len(sep) > 0 {
			if len(sep) > maxBytes || (sz-1) > int64(maxBytes)/int64(len(sep)) {
				abortOutputLimit()
			}
			total = (sz - 1) * int64(len(sep))
		}

		for i := types.Int(0); i < szVal; i++ {
			elem := list.Get(i)
			str, ok := elem.(types.String)
			if !ok {
				return types.NewErr("join: invalid input type %s", elem.Type().TypeName())
			}
			total += int64(len(str))
			if total > int64(maxBytes) {
				abortOutputLimit()
			}
		}

		var sb strings.Builder
		sb.Grow(int(total))
		for i := types.Int(0); i < szVal; i++ {
			if i != 0 {
				sb.WriteString(sep)
			}
			sb.WriteString(string(list.Get(i).(types.String)))
		}
		return types.String(sb.String())
	}
}

func boundedSplit(maxBytes, maxElems int) functions.FunctionOp {
	return func(args ...ref.Val) ref.Val {
		sVal, ok := args[0].(types.String)
		if !ok {
			return types.MaybeNoSuchOverloadErr(args[0])
		}
		sepVal, ok := args[1].(types.String)
		if !ok {
			return types.MaybeNoSuchOverloadErr(args[1])
		}
		limit := int64(-1)
		if len(args) == 3 {
			nVal, ok := args[2].(types.Int)
			if !ok {
				return types.MaybeNoSuchOverloadErr(args[2])
			}
			limit = int64(nVal)
		}

		str := string(sVal)
		sep := string(sepVal)
		if len(str) > maxBytes {
			abortOutputLimit()
		}
		if limit == 0 {
			return types.DefaultTypeAdapter.NativeToValue([]string{})
		}
		if limit == 1 {
			return types.DefaultTypeAdapter.NativeToValue([]string{str})
		}

		if sep != "" && shouldUseLinearKMPBytes(len(str), len(sep)) {
			maxMatches := -1
			if limit > 1 {
				if limit-1 > int64(len(str)) {
					maxMatches = len(str)
				} else {
					maxMatches = int(limit - 1)
				}
			}
			matches := kmpFindBytes(str, sep, maxMatches, maxElems)
			if int64(len(matches))+1 > int64(maxElems) {
				abortOutputLimit()
			}
			out := make([]string, 0, len(matches)+1)
			prev := 0
			for _, pos := range matches {
				out = append(out, str[prev:pos])
				prev = pos + len(sep)
			}
			out = append(out, str[prev:])
			return types.DefaultTypeAdapter.NativeToValue(out)
		}

		var partsCount int64
		if sep == "" {
			if str != "" {
				if utf8.ValidString(str) {
					partsCount = int64(utf8.RuneCountInString(str))
				} else {
					var explodedBytes int64
					for _, r := range str {
						partsCount++
						explodedBytes += int64(utf8.RuneLen(r))
					}
					if explodedBytes > int64(maxBytes) {
						abortOutputLimit()
					}
				}
				if limit > 0 && limit < partsCount {
					partsCount = limit
				}
			}
		} else {
			if limit < 0 {
				// Fast upper bound before scanning; only scan if str could plausibly exceed maxElems.
				if int64(len(str))/int64(len(sep))+1 > int64(maxElems) {
					partsCount = int64(strings.Count(str, sep)) + 1
				}
			} else {
				partsCount = 1
				rem := str
				for partsCount < limit {
					idx := strings.Index(rem, sep)
					if idx < 0 {
						break
					}
					partsCount++
					if partsCount > int64(maxElems) {
						abortOutputLimit()
					}
					rem = rem[idx+len(sep):]
				}
			}
		}
		if partsCount > int64(maxElems) {
			abortOutputLimit()
		}

		maxSplit := int64(len(str)) + 1
		if limit > maxSplit {
			limit = maxSplit
		}
		var out []string
		if limit < 0 {
			out = strings.Split(str, sep)
		} else {
			out = strings.SplitN(str, sep, int(limit))
		}
		return types.DefaultTypeAdapter.NativeToValue(out)
	}
}

func boundedQuote(maxBytes int) functions.FunctionOp {
	return func(args ...ref.Val) ref.Val {
		sVal, ok := args[0].(types.String)
		if !ok {
			return types.MaybeNoSuchOverloadErr(args[0])
		}
		str := string(sVal)
		if len(str) > maxBytes {
			abortOutputLimit()
		}

		outBytes := int64(2) // surrounding double quotes
		for _, c := range str {
			if !utf8.ValidRune(c) {
				c = utf8.RuneError
			}
			switch c {
			case '\a', '\b', '\f', '\n', '\r', '\t', '\v', '\\', '"':
				outBytes += 2
			default:
				outBytes += int64(utf8.RuneLen(c))
			}
			if outBytes > int64(maxBytes) {
				abortOutputLimit()
			}
		}

		var sb strings.Builder
		sb.Grow(int(outBytes))
		sb.WriteByte('"')
		for _, c := range str {
			if !utf8.ValidRune(c) {
				c = utf8.RuneError
			}
			switch c {
			case '\a':
				sb.WriteString("\\a")
			case '\b':
				sb.WriteString("\\b")
			case '\f':
				sb.WriteString("\\f")
			case '\n':
				sb.WriteString("\\n")
			case '\r':
				sb.WriteString("\\r")
			case '\t':
				sb.WriteString("\\t")
			case '\v':
				sb.WriteString("\\v")
			case '\\':
				sb.WriteString("\\\\")
			case '"':
				sb.WriteString("\\\"")
			default:
				sb.WriteRune(c)
			}
		}
		sb.WriteByte('"')
		return types.String(sb.String())
	}
}

func boundedFormat(maxBytes, maxElems int, rawFormat cel.Program) functions.FunctionOp {
	return func(args ...ref.Val) ref.Val {
		fmtVal, ok := args[0].(types.String)
		if !ok {
			return types.MaybeNoSuchOverloadErr(args[0])
		}
		argList, ok := args[1].(traits.Lister)
		if !ok {
			return types.MaybeNoSuchOverloadErr(args[1])
		}
		fmtStr := string(fmtVal)
		if len(fmtStr) > maxBytes {
			abortOutputLimit()
		}

		checkFormatOutputBound(fmtStr, argList, maxBytes, maxElems)

		out, _, err := rawFormat.Eval(map[string]any{
			"_fmt":  fmtVal,
			"_args": argList,
		})
		if err != nil {
			if types.IsError(out) {
				return out
			}
			return types.WrapErr(err)
		}
		if s, ok := out.(types.String); ok && len(s) > maxBytes {
			abortOutputLimit()
		}
		return out
	}
}

type formatBudget struct {
	remBytes int64
	remElems int64
}

func (b *formatBudget) chargeBytes(n int64) {
	b.remBytes -= n
	if b.remBytes < 0 {
		abortOutputLimit()
	}
}

func (b *formatBudget) chargeElems(n int64) {
	b.remElems -= n
	if b.remElems < 0 {
		abortOutputLimit()
	}
}

func checkFormatOutputBound(fmtStr string, argList traits.Lister, maxBytes, maxElems int) {
	szVal, ok := argList.Size().(types.Int)
	if !ok {
		return
	}
	argCount := int64(szVal)
	if argCount < 0 {
		abortOutputLimit()
	}

	b := formatBudget{
		remBytes: int64(maxBytes),
		remElems: int64(maxElems),
	}

	var argIdx int64
	for i := 0; i < len(fmtStr); {
		if fmtStr[i] != '%' {
			b.chargeBytes(1)
			i++
			continue
		}
		if i+1 < len(fmtStr) && fmtStr[i+1] == '%' {
			b.chargeBytes(1)
			i += 2
			continue
		}
		if i+1 >= len(fmtStr) || argIdx >= argCount {
			// Malformed format string or missing arg; let rawFormat produce the exact CEL error.
			return
		}
		j := i + 1
		prec := 6
		if fmtStr[j] == '.' {
			j++
			startDigits := j
			precVal := 0
			for j < len(fmtStr) && fmtStr[j] >= '0' && fmtStr[j] <= '9' {
				if precVal <= maxPinnedFormatPrec {
					precVal = precVal*10 + int(fmtStr[j]-'0')
				}
				j++
			}
			if j >= len(fmtStr) || j == startDigits || precVal > maxPinnedFormatPrec {
				return
			}
			prec = precVal
		}
		verb := fmtStr[j]
		j++
		arg := argList.Get(types.Int(argIdx))
		argIdx++
		i = j

		switch verb {
		case 's':
			measureFormatStringArg(&b, arg, 0)
		case 'd':
			b.chargeBytes(upperBoundDecimalBytes(arg))
		case 'f':
			b.chargeBytes(upperBoundDecimalBytes(arg) + 1 + int64(prec))
		case 'e':
			b.chargeBytes(int64(prec) + 16)
		case 'b':
			b.chargeBytes(65)
		case 'x', 'X':
			switch v := arg.(type) {
			case types.String:
				b.chargeBytes(2 * int64(len(v)))
			case types.Bytes:
				b.chargeBytes(2 * int64(len(v)))
			default:
				b.chargeBytes(17)
			}
		case 'o':
			b.chargeBytes(23)
		default:
			return
		}
	}
}

func upperBoundDecimalBytes(arg ref.Val) int64 {
	if d, ok := arg.(types.Double); ok {
		fv := float64(d)
		if !math.IsNaN(fv) && !math.IsInf(fv, 0) && fv != 0 {
			abs := math.Abs(fv)
			if abs < 1e-9 || abs >= 1e20 {
				return 340
			}
		}
		return 32
	}
	return 20
}

func measureFormatStringArg(b *formatBudget, arg ref.Val, depth int) {
	if depth > MaxContainerDepth {
		abortOutputLimit()
	}
	switch v := arg.(type) {
	case types.String:
		b.chargeBytes(int64(len(v)))
	case types.Bytes:
		b.chargeBytes(int64(len(v)))
	case types.Bool:
		b.chargeBytes(5)
	case types.Int, types.Uint:
		b.chargeBytes(20)
	case types.Double:
		b.chargeBytes(upperBoundDecimalBytes(arg))
	case types.Duration, types.Timestamp:
		b.chargeBytes(35)
	case traits.Lister:
		szVal, ok := v.Size().(types.Int)
		if !ok || szVal < 0 {
			abortOutputLimit()
		}
		sz := int64(szVal)
		b.chargeElems(sz)
		b.chargeBytes(2) // "[" and "]"
		if sz > 1 {
			b.chargeBytes((sz - 1) * 2) // ", "
		}
		for idx := types.Int(0); idx < szVal; idx++ {
			measureFormatStringArg(b, v.Get(idx), depth+1)
		}
	case traits.Mapper:
		szVal, ok := v.Size().(types.Int)
		if !ok || szVal < 0 {
			abortOutputLimit()
		}
		sz := int64(szVal)
		b.chargeElems(sz)
		b.chargeBytes(2 + sz*2) // "{", "}", and ": " per entry
		if sz > 1 {
			b.chargeBytes((sz - 1) * 2) // ", "
		}
		it := v.Iterator()
		for it.HasNext() == types.True {
			k := it.Next()
			measureFormatStringArg(b, k, depth+1)
			if mv, found := v.Find(k); found {
				measureFormatStringArg(b, mv, depth+1)
			}
		}
	default:
		b.chargeBytes(32)
	}
}

func boundedAdd(maxBytes, maxElems int) functions.FunctionOp {
	return func(args ...ref.Val) ref.Val {
		lhs, rhs := args[0], args[1]
		switch l := lhs.(type) {
		case types.String:
			if r, ok := rhs.(types.String); ok {
				if len(l) > maxBytes || len(r) > maxBytes-len(l) {
					abortOutputLimit()
				}
			}
		case types.Bytes:
			if r, ok := rhs.(types.Bytes); ok {
				if len(l) > maxBytes || len(r) > maxBytes-len(l) {
					abortOutputLimit()
				}
			}
		case traits.Lister:
			if r, ok := rhs.(traits.Lister); ok {
				lSz, ok1 := l.Size().(types.Int)
				rSz, ok2 := r.Size().(types.Int)
				if !ok1 || !ok2 || lSz < 0 || rSz < 0 || int64(lSz) > int64(maxElems) || int64(rSz) > int64(maxElems)-int64(lSz) {
					abortOutputLimit()
				}
			}
		}
		adder, ok := lhs.(traits.Adder)
		if !ok {
			return types.MaybeNoSuchOverloadErr(lhs)
		}
		return adder.Add(rhs)
	}
}

func boundedIndexOf(maxBytes int) functions.FunctionOp {
	return func(args ...ref.Val) ref.Val {
		sVal, ok := args[0].(types.String)
		if !ok {
			return types.MaybeNoSuchOverloadErr(args[0])
		}
		subVal, ok := args[1].(types.String)
		if !ok {
			return types.MaybeNoSuchOverloadErr(args[1])
		}
		offset := int64(0)
		if len(args) == 3 {
			offVal, ok := args[2].(types.Int)
			if !ok {
				return types.MaybeNoSuchOverloadErr(args[2])
			}
			offset = int64(offVal)
		}

		str := string(sVal)
		sub := string(subVal)
		if len(str) > maxBytes || len(sub) > maxBytes {
			abortOutputLimit()
		}
		if offset < 0 {
			return types.NewErr("index out of range: %d", offset)
		}

		runes := []rune(str)
		if sub == "" {
			if offset > int64(len(runes)) {
				return types.Int(len(runes))
			}
			return types.Int(offset)
		}
		if offset >= int64(len(runes)) {
			return types.Int(-1)
		}
		subRunes := []rune(sub)
		off := int(offset)
		if len(subRunes) > len(runes)-off {
			return types.Int(-1)
		}
		idx := kmpSearchRunes(runes[off:], subRunes, false)
		if idx < 0 {
			return types.Int(-1)
		}
		return types.Int(int64(off + idx))
	}
}

func boundedLastIndexOf(maxBytes int) functions.FunctionOp {
	return func(args ...ref.Val) ref.Val {
		sVal, ok := args[0].(types.String)
		if !ok {
			return types.MaybeNoSuchOverloadErr(args[0])
		}
		subVal, ok := args[1].(types.String)
		if !ok {
			return types.MaybeNoSuchOverloadErr(args[1])
		}
		str := string(sVal)
		sub := string(subVal)
		if len(str) > maxBytes || len(sub) > maxBytes {
			abortOutputLimit()
		}

		hasOffset := len(args) == 3
		var offset int64
		if hasOffset {
			offVal, ok := args[2].(types.Int)
			if !ok {
				return types.MaybeNoSuchOverloadErr(args[2])
			}
			offset = int64(offVal)
			if offset < 0 {
				return types.NewErr("index out of range: %d", offset)
			}
		}

		runes := []rune(str)
		if sub == "" {
			if !hasOffset || offset > int64(len(runes)) {
				return types.Int(len(runes))
			}
			return types.Int(offset)
		}
		if len(runes) == 0 {
			return types.Int(-1)
		}
		if !hasOffset {
			if len(str) < len(sub) {
				return types.Int(-1)
			}
			offset = int64(len(runes) - 1)
		} else if offset >= int64(len(runes)) {
			return types.Int(-1)
		}

		subRunes := []rune(sub)
		maxStart := int(offset)
		if maxStart > len(runes)-len(subRunes) {
			maxStart = len(runes) - len(subRunes)
		}
		if maxStart < 0 {
			return types.Int(-1)
		}
		idx := kmpSearchRunes(runes[:maxStart+len(subRunes)], subRunes, true)
		return types.Int(int64(idx))
	}
}

// kmpSearchRunes finds either the first (findLast=false) or last (findLast=true)
// occurrence of pattern in text in O(len(text) + len(pattern)) time.
func kmpSearchRunes(text, pattern []rune, findLast bool) int {
	m := len(pattern)
	if m == 0 {
		if findLast {
			return len(text)
		}
		return 0
	}
	if len(text) < m {
		return -1
	}

	lps := make([]int32, m)
	for i, length := 1, int32(0); i < m; {
		if pattern[i] == pattern[length] {
			length++
			lps[i] = length
			i++
		} else if length != 0 {
			length = lps[length-1]
		} else {
			lps[i] = 0
			i++
		}
	}

	lastMatch := -1
	for i, j := 0, int32(0); i < len(text); {
		if text[i] == pattern[j] {
			i++
			j++
			if int(j) == m {
				matchIdx := i - m
				if !findLast {
					return matchIdx
				}
				lastMatch = matchIdx
				j = lps[j-1]
			}
		} else if j != 0 {
			j = lps[j-1]
		} else {
			i++
		}
	}
	return lastMatch
}

func boundedCharAt(maxBytes int) functions.FunctionOp {
	return func(args ...ref.Val) ref.Val {
		sVal, ok := args[0].(types.String)
		if !ok {
			return types.MaybeNoSuchOverloadErr(args[0])
		}
		indVal, ok := args[1].(types.Int)
		if !ok {
			return types.MaybeNoSuchOverloadErr(args[1])
		}
		str := string(sVal)
		if len(str) > maxBytes {
			abortOutputLimit()
		}
		ind := int64(indVal)
		runes := []rune(str)
		if ind < 0 || ind > int64(len(runes)) {
			return types.NewErr("index out of range: %d", ind)
		}
		if ind == int64(len(runes)) {
			return types.String("")
		}
		return types.String(runes[ind])
	}
}

func boundedLowerASCII(maxBytes int) functions.FunctionOp {
	return func(args ...ref.Val) ref.Val {
		sVal, ok := args[0].(types.String)
		if !ok {
			return types.MaybeNoSuchOverloadErr(args[0])
		}
		str := string(sVal)
		if len(str) > maxBytes {
			abortOutputLimit()
		}
		runes := []rune(str)
		for i, r := range runes {
			if r <= unicode.MaxASCII {
				runes[i] = unicode.ToLower(r)
			}
		}
		out := string(runes)
		if len(out) > maxBytes {
			abortOutputLimit()
		}
		return types.String(out)
	}
}

func boundedUpperASCII(maxBytes int) functions.FunctionOp {
	return func(args ...ref.Val) ref.Val {
		sVal, ok := args[0].(types.String)
		if !ok {
			return types.MaybeNoSuchOverloadErr(args[0])
		}
		str := string(sVal)
		if len(str) > maxBytes {
			abortOutputLimit()
		}
		runes := []rune(str)
		for i, r := range runes {
			if r <= unicode.MaxASCII {
				runes[i] = unicode.ToUpper(r)
			}
		}
		out := string(runes)
		if len(out) > maxBytes {
			abortOutputLimit()
		}
		return types.String(out)
	}
}

func boundedReverse(maxBytes int) functions.FunctionOp {
	return func(args ...ref.Val) ref.Val {
		sVal, ok := args[0].(types.String)
		if !ok {
			return types.MaybeNoSuchOverloadErr(args[0])
		}
		str := string(sVal)
		if len(str) > maxBytes {
			abortOutputLimit()
		}
		chars := []rune(str)
		for i, j := 0, len(chars)-1; i < j; i, j = i+1, j-1 {
			chars[i], chars[j] = chars[j], chars[i]
		}
		out := string(chars)
		if len(out) > maxBytes {
			abortOutputLimit()
		}
		return types.String(out)
	}
}

func boundedSubstring(maxBytes int) functions.FunctionOp {
	return func(args ...ref.Val) ref.Val {
		sVal, ok := args[0].(types.String)
		if !ok {
			return types.MaybeNoSuchOverloadErr(args[0])
		}
		startVal, ok := args[1].(types.Int)
		if !ok {
			return types.MaybeNoSuchOverloadErr(args[1])
		}
		str := string(sVal)
		if len(str) > maxBytes {
			abortOutputLimit()
		}
		runes := []rune(str)
		l := int64(len(runes))
		start := int64(startVal)
		if len(args) == 2 {
			if start < 0 || start > l {
				return types.NewErr("index out of range: %d", start)
			}
			out := string(runes[start:])
			if len(out) > maxBytes {
				abortOutputLimit()
			}
			return types.String(out)
		}
		endVal, ok := args[2].(types.Int)
		if !ok {
			return types.MaybeNoSuchOverloadErr(args[2])
		}
		end := int64(endVal)
		if start > end {
			return types.NewErr("invalid substring range. start: %d, end: %d", start, end)
		}
		if start < 0 || start > l {
			return types.NewErr("index out of range: %d", start)
		}
		if end < 0 || end > l {
			return types.NewErr("index out of range: %d", end)
		}
		out := string(runes[start:end])
		if len(out) > maxBytes {
			abortOutputLimit()
		}
		return types.String(out)
	}
}

func boundedBytesConv(maxBytes int) functions.FunctionOp {
	return func(args ...ref.Val) ref.Val {
		switch v := args[0].(type) {
		case types.String:
			if len(v) > maxBytes {
				abortOutputLimit()
			}
		case types.Bytes:
			if len(v) > maxBytes {
				abortOutputLimit()
			}
		}
		return args[0].ConvertToType(types.BytesType)
	}
}

func boundedStringConv(maxBytes int) functions.FunctionOp {
	return func(args ...ref.Val) ref.Val {
		switch v := args[0].(type) {
		case types.String:
			if len(v) > maxBytes {
				abortOutputLimit()
			}
		case types.Bytes:
			if len(v) > maxBytes {
				abortOutputLimit()
			}
		}
		out := args[0].ConvertToType(types.StringType)
		if s, ok := out.(types.String); ok && len(s) > maxBytes {
			abortOutputLimit()
		}
		return out
	}
}

func isContainerVal(v ref.Val) bool {
	switch v.(type) {
	case traits.Lister, traits.Mapper:
		return true
	default:
		return false
	}
}

func checkContainerWalkBound(v ref.Val, b *formatBudget, depth int) {
	if depth > MaxContainerDepth {
		abortOutputLimit()
	}
	switch c := v.(type) {
	case types.String:
		b.chargeBytes(int64(len(c)))
	case types.Bytes:
		b.chargeBytes(int64(len(c)))
	case traits.Lister:
		szVal, ok := c.Size().(types.Int)
		if !ok || szVal < 0 {
			abortOutputLimit()
		}
		b.chargeElems(int64(szVal))
		for i := types.Int(0); i < szVal; i++ {
			checkContainerWalkBound(c.Get(i), b, depth+1)
		}
	case traits.Mapper:
		szVal, ok := c.Size().(types.Int)
		if !ok || szVal < 0 {
			abortOutputLimit()
		}
		b.chargeElems(int64(szVal))
		it := c.Iterator()
		for it.HasNext() == types.True {
			k := it.Next()
			checkContainerWalkBound(k, b, depth+1)
			if mv, found := c.Find(k); found {
				checkContainerWalkBound(mv, b, depth+1)
			}
		}
	}
}

func boundedEqNe(wantEq bool, maxBytes, maxElems int) functions.FunctionOp {
	return func(args ...ref.Val) ref.Val {
		lhs, rhs := args[0], args[1]
		if isContainerVal(lhs) || isContainerVal(rhs) {
			b := formatBudget{
				remBytes: int64(maxBytes) * 2,
				remElems: int64(maxElems) * 2,
			}
			checkContainerWalkBound(lhs, &b, 0)
			checkContainerWalkBound(rhs, &b, 0)
		}
		eq := types.Equal(lhs, rhs)
		if wantEq {
			return eq
		}
		return types.Bool(eq != types.True)
	}
}

func boundedIn(maxBytes, maxElems int) functions.FunctionOp {
	return func(args ...ref.Val) ref.Val {
		lhs, rhs := args[0], args[1]
		if !rhs.Type().HasTrait(traits.ContainerType) {
			return types.ValOrErr(rhs, "no such overload")
		}
		// Map key lookup is O(1) for primitive keys; list membership or nested container
		// comparison walks the container.
		if _, isList := rhs.(traits.Lister); isList || isContainerVal(lhs) {
			b := formatBudget{
				remBytes: int64(maxBytes) * 2,
				remElems: int64(maxElems) * 2,
			}
			checkContainerWalkBound(lhs, &b, 0)
			checkContainerWalkBound(rhs, &b, 0)
		}
		return rhs.(traits.Container).Contains(lhs)
	}
}

func shouldUseLinearKMPBytes(textLen, patLen int) bool {
	return patLen > rabinKarpPatMinBytes && int64(textLen)*int64(patLen) > rabinKarpWorkCutoff
}

// kmpFindBytes returns non-overlapping start byte offsets of pattern in text
// using O(len(text) + len(pattern)) KMP so adversarial patterns longer than 64
// bytes cannot trigger Go stdlib IndexRabinKarp's fixed-PrimeRK hash-collision
// O(N*M) fallback. When limit >= 0, at most limit matches are returned; when
// abortAfter > 0, exceeding abortAfter matches aborts immediately.
func kmpFindBytes(text, pattern string, limit int, abortAfter int) []int {
	m := len(pattern)
	if m == 0 || len(text) < m || limit == 0 {
		return nil
	}
	lps := make([]int32, m)
	for i, length := 1, int32(0); i < m; {
		if pattern[i] == pattern[length] {
			length++
			lps[i] = length
			i++
		} else if length != 0 {
			length = lps[length-1]
		} else {
			lps[i] = 0
			i++
		}
	}

	var matches []int
	for i, j := 0, int32(0); i < len(text); {
		if text[i] == pattern[j] {
			i++
			j++
			if int(j) == m {
				matches = append(matches, i-m)
				if abortAfter > 0 && len(matches) > abortAfter {
					abortOutputLimit()
				}
				if limit > 0 && len(matches) >= limit {
					return matches
				}
				j = 0 // non-overlapping matches
			}
		} else if j != 0 {
			j = lps[j-1]
		} else {
			i++
		}
	}
	return matches
}

func indexStringLinear(s, sub string) int {
	if sub == "" {
		return 0
	}
	if len(s) < len(sub) {
		return -1
	}
	if !shouldUseLinearKMPBytes(len(s), len(sub)) {
		return strings.Index(s, sub)
	}
	matches := kmpFindBytes(s, sub, 1, 0)
	if len(matches) == 0 {
		return -1
	}
	return matches[0]
}

func boundedTrim(maxBytes int) functions.FunctionOp {
	return func(args ...ref.Val) ref.Val {
		sVal, ok := args[0].(types.String)
		if !ok {
			return types.MaybeNoSuchOverloadErr(args[0])
		}
		str := string(sVal)
		if len(str) > maxBytes {
			abortOutputLimit()
		}
		return types.String(strings.TrimSpace(str))
	}
}

func boundedContains(maxBytes int) functions.FunctionOp {
	return func(args ...ref.Val) ref.Val {
		sVal, ok := args[0].(types.String)
		if !ok {
			return types.MaybeNoSuchOverloadErr(args[0])
		}
		subVal, ok := args[1].(types.String)
		if !ok {
			return types.MaybeNoSuchOverloadErr(args[1])
		}
		str := string(sVal)
		sub := string(subVal)
		if len(str) > maxBytes || len(sub) > maxBytes {
			abortOutputLimit()
		}
		return types.Bool(indexStringLinear(str, sub) >= 0)
	}
}

func boundedStartsWith(maxBytes int) functions.FunctionOp {
	return func(args ...ref.Val) ref.Val {
		sVal, ok := args[0].(types.String)
		if !ok {
			return types.MaybeNoSuchOverloadErr(args[0])
		}
		preVal, ok := args[1].(types.String)
		if !ok {
			return types.MaybeNoSuchOverloadErr(args[1])
		}
		str := string(sVal)
		pre := string(preVal)
		if len(str) > maxBytes || len(pre) > maxBytes {
			abortOutputLimit()
		}
		return types.Bool(strings.HasPrefix(str, pre))
	}
}

func boundedEndsWith(maxBytes int) functions.FunctionOp {
	return func(args ...ref.Val) ref.Val {
		sVal, ok := args[0].(types.String)
		if !ok {
			return types.MaybeNoSuchOverloadErr(args[0])
		}
		sufVal, ok := args[1].(types.String)
		if !ok {
			return types.MaybeNoSuchOverloadErr(args[1])
		}
		str := string(sVal)
		suf := string(sufVal)
		if len(str) > maxBytes || len(suf) > maxBytes {
			abortOutputLimit()
		}
		return types.Bool(strings.HasSuffix(str, suf))
	}
}

func boundedSize(maxBytes, maxElems int) functions.FunctionOp {
	return func(args ...ref.Val) ref.Val {
		switch v := args[0].(type) {
		case types.String:
			if len(v) > maxBytes {
				abortOutputLimit()
			}
			return types.Int(utf8.RuneCountInString(string(v)))
		case types.Bytes:
			if len(v) > maxBytes {
				abortOutputLimit()
			}
			return types.Int(len(v))
		case traits.Lister:
			szVal, ok := v.Size().(types.Int)
			if !ok || szVal < 0 || int64(szVal) > int64(maxElems) {
				abortOutputLimit()
			}
			return szVal
		case traits.Mapper:
			szVal, ok := v.Size().(types.Int)
			if !ok || szVal < 0 || int64(szVal) > int64(maxElems) {
				abortOutputLimit()
			}
			return szVal
		case traits.Sizer:
			return v.Size()
		default:
			return types.MaybeNoSuchOverloadErr(args[0])
		}
	}
}

func boundedScalarStringInputConv(maxBytes int, targetType ref.Type) functions.FunctionOp {
	return func(args ...ref.Val) ref.Val {
		if s, ok := args[0].(types.String); ok && len(s) > maxBytes {
			abortOutputLimit()
		}
		return args[0].ConvertToType(targetType)
	}
}

func boundedTimezoneAccessor(fn, overload string, maxBytes int) functions.FunctionOp {
	return func(args ...ref.Val) ref.Val {
		if tz, ok := args[1].(types.String); ok && len(tz) > maxBytes {
			abortOutputLimit()
		}
		if recv, ok := args[0].(traits.Receiver); ok {
			return recv.Receive(fn, overload, args[1:])
		}
		return types.MaybeNoSuchOverloadErr(args[0])
	}
}

// boundedMatchesCall wraps CEL's matches(string, string) in an InterpretableV2
// that deliberately does NOT implement interpreter.InterpretableCall. In
// cel-go, CustomDecoratorV2 runs before CompileRegexConstants (decRegexOptimizer,
// which type-asserts InterpretableCall and replaces constant-pattern matches
// nodes with a fresh NewCall). Returning a non-InterpretableCall node preserves
// both constant-pattern pre-compilation and pre-call input * program-size work
// bounds at evaluation time.
type boundedMatchesCall struct {
	id               int64
	origCall         interpreter.InterpretableCall
	lhs              interpreter.InterpretableV2
	rhs              interpreter.InterpretableV2
	compiled         *regexp.Regexp
	compiledProgSize int
	maxBytes         int
	maxRegexProgSize int
	maxRegexWork     int64
}

func newBoundedMatchesCall(
	call interpreter.InterpretableCall,
	maxBytes int,
	maxRegexProgSize int,
	maxRegexWork int64,
) (interpreter.InterpretableV2, error) {
	args := call.Args()
	node := &boundedMatchesCall{
		id:               call.ID(),
		origCall:         call,
		lhs:              args[0],
		rhs:              args[1],
		maxBytes:         maxBytes,
		maxRegexProgSize: maxRegexProgSize,
		maxRegexWork:     maxRegexWork,
	}
	if constPat, isConst := args[1].(interpreter.InterpretableConst); isConst {
		if patStr, ok := constPat.Value().(types.String); ok {
			pat := string(patStr)
			if len(pat) > maxBytes {
				abortOutputLimit()
			}
			progSize, err := types.RegexProgramSize(pat)
			if err != nil {
				return nil, err
			}
			if progSize > maxRegexProgSize {
				abortOutputLimit()
			}
			re, err := regexp.Compile(pat)
			if err != nil {
				return nil, err
			}
			node.compiled = re
			node.compiledProgSize = progSize
		}
	}
	return node, nil
}

func (m *boundedMatchesCall) ID() int64 {
	return m.id
}

func (m *boundedMatchesCall) Eval(ctx interpreter.Activation) ref.Val {
	return interpreter.EvalActivation(ctx, m.Exec)
}

func (m *boundedMatchesCall) Exec(frame *interpreter.ExecutionFrame) ref.Val {
	lVal := m.lhs.Exec(frame)
	switch l := lVal.(type) {
	case *types.Err:
		return lVal
	case *types.Unknown:
		rVal := m.rhs.Exec(frame)
		if rUnk, ok := rVal.(*types.Unknown); ok {
			merged := types.MergeUnknowns(l, rUnk)
			if costs := frame.CostTracker(); costs != nil {
				costs.EvalBinary(frame, m.id, m.origCall, lVal, rVal, merged)
			}
			return merged
		}
		if costs := frame.CostTracker(); costs != nil {
			costs.EvalBinary(frame, m.id, m.origCall, lVal, rVal, lVal)
		}
		return lVal
	}

	rVal := m.rhs.Exec(frame)
	switch rVal.(type) {
	case *types.Err:
		return rVal
	case *types.Unknown:
		if costs := frame.CostTracker(); costs != nil {
			costs.EvalBinary(frame, m.id, m.origCall, lVal, rVal, rVal)
		}
		return rVal
	}

	lStr, okL := lVal.(types.String)
	if !okL {
		res := types.MaybeNoSuchOverloadErr(lVal)
		if costs := frame.CostTracker(); costs != nil {
			costs.EvalBinary(frame, m.id, m.origCall, lVal, rVal, res)
		}
		return res
	}
	rStr, okR := rVal.(types.String)
	if !okR {
		res := types.MaybeNoSuchOverloadErr(rVal)
		if costs := frame.CostTracker(); costs != nil {
			costs.EvalBinary(frame, m.id, m.origCall, lVal, rVal, res)
		}
		return res
	}

	inStr := string(lStr)
	if len(inStr) > m.maxBytes {
		abortOutputLimit()
	}

	re := m.compiled
	progSize := m.compiledProgSize
	if re == nil {
		pat := string(rStr)
		if len(pat) > m.maxBytes {
			abortOutputLimit()
		}
		var err error
		progSize, err = types.RegexProgramSize(pat)
		if err != nil {
			res := types.WrapErr(err)
			if costs := frame.CostTracker(); costs != nil {
				costs.EvalBinary(frame, m.id, m.origCall, lVal, rVal, res)
			}
			return res
		}
		if progSize > m.maxRegexProgSize {
			abortOutputLimit()
		}
		if progSize > 0 && int64(len(inStr)) > m.maxRegexWork/int64(progSize) {
			abortOutputLimit()
		}
		re, err = regexp.Compile(pat)
		if err != nil {
			res := types.WrapErr(err)
			if costs := frame.CostTracker(); costs != nil {
				costs.EvalBinary(frame, m.id, m.origCall, lVal, rVal, res)
			}
			return res
		}
	} else if progSize > 0 && int64(len(inStr)) > m.maxRegexWork/int64(progSize) {
		abortOutputLimit()
	}

	res := types.Bool(re.MatchString(inStr))
	if costs := frame.CostTracker(); costs != nil {
		costs.EvalBinary(frame, m.id, m.origCall, lVal, rVal, res)
	}
	return res
}
