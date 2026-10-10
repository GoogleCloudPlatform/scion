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

// Command buildstats measures Go compile and test cost the same way every
// time, so that before/after comparisons (refactor gates) are like for like.
//
// It runs the measured command as its own direct child and reads the child's
// resource usage from wait4, so peak RSS works on hosts without /usr/bin/time
// and when buildstats itself is the command handed to a job queue.
//
// See README.md for the subcommands and the gate invocations.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
)

const usage = `usage: buildstats <subcommand> [flags] [-- command...]

Measuring subcommands (run a command as a child, report rusage):
  compile  [flags] -- go build|test -c ...   compile wall/user/sys/peak RSS,
                                             actiongraph and -bench summaries
  test     [flags] -- <command emitting test2json>
                                             rusage plus test2json summary
  run      [flags] -- <command>              rusage only

Analysis subcommands (no child process):
  actiongraph [-top N] [-json F] FILE        summarise a -debug-actiongraph file
  bench       [-json F] FILE                 summarise a compiler -bench file
  tests       [-top N] [-json F] FILE...     summarise test2json output
  deps        [-test] [-json F] PKG...       go list -deps counts (total, non-std)
  diff        OLD.json NEW.json              compare two records

Run "buildstats <subcommand> -h" for flags.
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// errUsage signals that usage was already printed.
var errUsage = errors.New("usage")

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	var (
		code int
		err  error
	)
	sub, rest := args[0], args[1:]
	switch sub {
	case "compile":
		code, err = cmdCompile(rest, stdout, stderr)
	case "test":
		code, err = cmdTest(rest, stdout, stderr)
	case "run":
		code, err = cmdRun(rest, stdout, stderr)
	case "actiongraph":
		err = cmdActiongraph(rest, stdout, stderr)
	case "bench":
		err = cmdBench(rest, stdout, stderr)
	case "tests":
		err = cmdTests(rest, stdout, stderr)
	case "deps":
		err = cmdDeps(rest, stdout, stderr)
	case "diff":
		err = cmdDiff(rest, stdout, stderr)
	case "-h", "-help", "--help", "help":
		fmt.Fprint(stdout, usage)
		return 0
	default:
		fmt.Fprintf(stderr, "buildstats: unknown subcommand %q\n\n%s", sub, usage)
		return 2
	}
	if err != nil {
		if errors.Is(err, errUsage) {
			return 2
		}
		fmt.Fprintf(stderr, "buildstats %s: %v\n", sub, err)
		if code == 0 {
			code = 1
		}
	}
	return code
}
