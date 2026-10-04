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

package commands

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/homeprep"
)

// Environment the broker sets on the home init containers.
const (
	envHomeAgentID       = "SCION_HOME_AGENT_ID"
	envHomeStartID       = "SCION_HOME_START_ID"
	envHomeLaunchID      = "SCION_HOME_LAUNCH_ID"
	envHomeLinks         = "SCION_HOME_LINKS"
	envHomeSkeletonSrc   = "SCION_HOME_SKELETON_SOURCE"
	envHomeSkeletonMax   = "SCION_HOME_SKELETON_MAX_BYTES"
	envHomeGID           = "SCION_HOME_GID"
	defaultHomeMount     = "/scion-home"
	defaultAgentDirMount = "/scion-agent-dir"
	defaultMemDir        = "/run/scion/mem"
	defaultSkeletonMax   = 256 << 20
	homeLeafUID          = 1000
)

// terminationLogPath is where a failing init container leaves its message
// for the broker (the Kubernetes default termination message path).
var terminationLogPath = "/dev/termination-log"

var homeCmd = &cobra.Command{
	Use:   "home",
	Short: "Prepare a persistent agent home",
	Long: `Commands that prepare an agent home kept on an NFS export across starts.
They run in the agent's pod: "leaf" and "prepare" as init containers, and
"mark-seeded" through the broker after the first transfer.`,
}

var (
	homeDir         string
	homeSeededDir   string
	homeMemDir      string
	homeAgentDir    string
	homeSkeletonSrc string
	homeAgentIDFlag string
	homeStartIDFlag string
)

var homePrepareCmd = &cobra.Command{
	Use:   "prepare",
	Short: "Prepare the agent home for this start",
	Long: `Checks that the home is writable, reads its sentinel and chooses how this
start fills it: "seed" for a new or interrupted home (the image home is
copied first), "seed-over" for a seeded one. Places the links to the staged
secret files and writes the chosen mode and the link result to the pod's
memory directory. Never follows a symbolic link and never leaves the home.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		links, err := homeprep.ParseLinks(os.Getenv(envHomeLinks))
		if err != nil {
			return homeFail(cmd, &homeprep.ClassError{Class: homeprep.ErrClassPrepare, Msg: err.Error()})
		}
		max := int64(defaultSkeletonMax)
		if v := os.Getenv(envHomeSkeletonMax); v != "" {
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil || n < 0 {
				return homeFail(cmd, &homeprep.ClassError{Class: homeprep.ErrClassPrepare, Msg: fmt.Sprintf("invalid %s %q", envHomeSkeletonMax, v)})
			}
			max = n
		}
		agentID := os.Getenv(envHomeAgentID)
		if !homeprep.ValidAgentID(agentID) {
			return homeFail(cmd, &homeprep.ClassError{Class: homeprep.ErrClassUnavailable, Msg: "no agent ID"})
		}
		skeleton := homeSkeletonSrc
		if skeleton == "" {
			skeleton = os.Getenv(envHomeSkeletonSrc)
		}
		mode, err := homeprep.Prepare(homeprep.PrepareOptions{
			Home:             homeDir,
			MemDir:           homeMemDir,
			AgentID:          agentID,
			StartID:          os.Getenv(envHomeStartID),
			LaunchID:         os.Getenv(envHomeLaunchID),
			Links:            links,
			SkeletonSource:   skeleton,
			SkeletonMaxBytes: max,
			Log: func(format string, a ...any) {
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(), format+"\n", a...)
			},
		})
		if err != nil {
			return homeFail(cmd, err)
		}
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Agent home prepared: %s\n", mode)
		return nil
	},
}

var homeMarkSeededCmd = &cobra.Command{
	Use:   "mark-seeded",
	Short: "Mark the agent home seeded after the first transfer",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		dir := homeSeededDir
		if dir == "" {
			h, err := os.UserHomeDir()
			if err != nil {
				return homeFail(cmd, err)
			}
			dir = h
		}
		if err := homeprep.MarkSeeded(dir, homeAgentIDFlag, homeStartIDFlag); err != nil {
			return homeFail(cmd, err)
		}
		return nil
	},
}

var homeLeafCmd = &cobra.Command{
	Use:   "leaf",
	Short: "Create the agent home directory in the agent directory",
	Long: `Creates home-<agent id> in the agent directory with the agent's uid, the
export's group and mode 2771, and normalises a root-owned agent directory to
mode 2775 with that group. Runs as root with only the capabilities to change
ownership and modes. An existing home directory with any other owner, group
or mode fails without being changed.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		agentID := os.Getenv(envHomeAgentID)
		if !homeprep.ValidAgentID(agentID) {
			return homeFail(cmd, &homeprep.ClassError{Class: homeprep.ErrClassUnavailable, Msg: "no agent ID"})
		}
		gid, err := strconv.Atoi(os.Getenv(envHomeGID))
		if err != nil || gid <= 0 {
			return homeFail(cmd, &homeprep.ClassError{Class: homeprep.ErrClassLeafFailed, Msg: fmt.Sprintf("invalid %s %q", envHomeGID, os.Getenv(envHomeGID))})
		}
		if err := homeprep.Leaf(homeprep.LeafOptions{
			AgentDir: homeAgentDir,
			HomeName: homeprep.HomeDirPrefix + agentID,
			UID:      homeLeafUID,
			GID:      gid,
		}); err != nil {
			return homeFail(cmd, err)
		}
		return nil
	},
}

// homeFail prints err, leaves it in the termination log for the broker, and
// returns it so the command exits non-zero.
func homeFail(cmd *cobra.Command, err error) error {
	msg := err.Error()
	var ce *homeprep.ClassError
	if !errors.As(err, &ce) {
		msg = homeprep.ErrClassPrepare + ": " + msg
	}
	_, _ = fmt.Fprintln(cmd.ErrOrStderr(), msg)
	if f, ferr := os.OpenFile(terminationLogPath, os.O_WRONLY|os.O_TRUNC, 0); ferr == nil {
		_, _ = io.WriteString(f, msg)
		_ = f.Close()
	}
	return errors.New(msg)
}

func init() {
	homePrepareCmd.Flags().StringVar(&homeDir, "home", defaultHomeMount, "Agent home on the export, as mounted in this container")
	homePrepareCmd.Flags().StringVar(&homeMemDir, "mem-dir", defaultMemDir, "Pod memory directory for the mode and link result files")
	homePrepareCmd.Flags().StringVar(&homeSkeletonSrc, "skeleton-source", "", "Image home directory to copy into a new home (default "+envHomeSkeletonSrc+")")
	homeMarkSeededCmd.Flags().StringVar(&homeSeededDir, "home", "", "Agent home (default: the current user's home)")
	homeMarkSeededCmd.Flags().StringVar(&homeAgentIDFlag, "agent-id", "", "Hub agent ID the home belongs to")
	homeMarkSeededCmd.Flags().StringVar(&homeStartIDFlag, "start-id", "", "ID of this start")
	homeLeafCmd.Flags().StringVar(&homeAgentDir, "agent-dir", defaultAgentDirMount, "Agent directory on the export, as mounted in this container")
	homeCmd.AddCommand(homePrepareCmd, homeMarkSeededCmd, homeLeafCmd)
	rootCmd.AddCommand(homeCmd)
}
