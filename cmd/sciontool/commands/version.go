/*
Copyright 2025 The Scion Authors.
*/
package commands

import (
	"fmt"
	"runtime/debug"

	"github.com/spf13/cobra"
)

var (
	// Version is the current version of sciontool.
	// It should be set via ldflags -X.
	Version string

	// Commit is the git commit hash of the build.
	// It should be set via ldflags -X.
	Commit string

	// BuildTime is the timestamp of the build.
	// It should be set via ldflags -X.
	BuildTime string
)

// versionCmd represents the version command
var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print sciontool version information",
	Long:  `Print version, commit, and build time information for sciontool.`,
	Run: func(cmd *cobra.Command, args []string) {
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), getVersionString())
	},
}

func init() {
	rootCmd.AddCommand(versionCmd)
}

// getVersionString returns the formatted version information.
func getVersionString() string {
	v := Version
	c := Commit
	bt := BuildTime

	// Fallback to debug info if ldflags weren't set
	if v == "" || c == "" {
		if info, ok := debug.ReadBuildInfo(); ok {
			if v == "" {
				v = info.Main.Version
				if v == "(devel)" {
					v = "dev"
				}
			}
			for _, setting := range info.Settings {
				if c == "" && setting.Key == "vcs.revision" {
					c = setting.Value
				}
				if bt == "" && setting.Key == "vcs.time" {
					bt = setting.Value
				}
			}
		}
	}

	// Shorten commit hash
	if len(c) > 7 {
		c = c[:7]
	}

	// Apply defaults for missing values
	if v == "" {
		v = "dev"
	}
	if c == "" {
		c = "unknown"
	}
	if bt == "" {
		bt = "unknown"
	}

	return fmt.Sprintf("sciontool version %s\nCommit: %s\nBuild Time: %s", v, c, bt)
}
