// Package status provides the vfs status command.
package status

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/rclone/rclone/cmd"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config/flags"
	"github.com/rclone/rclone/lib/atexit"
	"github.com/spf13/cobra"
)

func init() {
	cmd.Root.AddCommand(vfsCommand)
	vfsCommand.AddCommand(commandDefinition)
	flags.StringVarP(commandDefinition.Flags(), &fsFlag, "fs", "", "", "Remote config name of the mount to report on, where a path lies inside more than one registered mount point", "")
}

// vfsCommand is the parent of the commands reporting on VFS mounts.
var vfsCommand = &cobra.Command{
	Use:   "vfs",
	Short: `Commands for interacting with VFS mounts.`,
	Long: `Commands for interacting with VFS mounts.

Each subcommand reports on the state of a mounted remote.`,
	Annotations: map[string]string{
		"versionIntroduced": "v1.76",
	},
}

// commandDefinition is the cobra definition for "rclone vfs status".
var commandDefinition = &cobra.Command{
	Use:   "status <path>",
	Short: `Report the cache and transfer state of a file on a VFS mount.`,
	Long: `Report the cache and transfer state of a file on a VFS mount.

The command reports, for a single file, whether the file is
held in the local cache and the state of its upload to the
remote. It does not read the mount's state itself: it calls
the mount's "vfs/file-status" endpoint over its
remote-control interface, taking the address and credentials
from the same flags and config keys "rclone rc" reads, so the
mount must be started with "--rc".

The path is an absolute file path on the mount host's
filesystem. It is resolved to a mount by its mount point, not
relative to the current directory. One path per invocation.

The output is always a single JSON object.

E.g.

    rclone vfs status /mnt/mydrive/file.txt
`,
	Annotations: map[string]string{
		"versionIntroduced": "v1.76",
		"groups":            "RC",
	},
	Run: func(command *cobra.Command, args []string) {
		cmd.CheckArgs(1, 1, command, args)
		if args[0] == "" {
			_ = command.Usage()
			fmt.Fprintln(os.Stderr, "Command status needs 1 non-empty argument")
			atexit.Run()
			os.Exit(2)
		}
		// TODO: call the mount's "vfs/file-status" endpoint over its
		// remote-control interface, taking the address and credentials
		// from the same flags and config keys "rclone rc" reads.
		out := map[string]any{
			"path": args[0],
			"error": map[string]string{
				"code":    "unimplemented",
				"message": "vfs status is not implemented yet",
			},
		}
		if err := json.NewEncoder(os.Stdout).Encode(out); err != nil {
			fs.Errorf(nil, "vfs status: failed to write output: %v", err)
		}
		atexit.Run()
		os.Exit(1)
	},
}

// fsFlag holds the value of the --fs option.
var fsFlag string
