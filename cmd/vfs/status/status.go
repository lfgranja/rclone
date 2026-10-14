// Package status provides the vfs status command.
package status

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/rclone/rclone/cmd"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/fshttp"
	"github.com/rclone/rclone/fs/rc"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

var (
	// url is the control server address
	url = "http://localhost:5572/"
	// unixSocket is the unix domain socket path
	unixSocket = ""
	// authUser is the HTTP basic auth username
	authUser = ""
	// authPass is the HTTP basic auth password
	authPass = ""
	// fsFlag is the --fs flag value
	fsFlag = ""
	// noOutput suppresses JSON output
	noOutput = false
)

func init() {
	cmd.Root.AddCommand(vfsCommand)
	vfsCommand.AddCommand(commandDefinition)
	commandDefinition.Flags().StringVar(&fsFlag, "fs", "", "remote config name of the mount to report on, where a path lies inside more than one registered mount point")
	// Add rc flags as alternate flags
	commandDefinition.Flags().StringVarP(&url, "rc-addr", "", "http://localhost:5572/", "URL to connect to rclone remote control")
	commandDefinition.Flags().StringVarP(&unixSocket, "unix-socket", "", "", "Path to a unix domain socket to dial to, instead of opening a TCP connection directly")
	commandDefinition.Flags().StringVarP(&authUser, "rc-user", "", "", "Username to use to rclone remote control")
	commandDefinition.Flags().StringVarP(&authPass, "rc-pass", "", "", "Password to use to connect to rclone remote control")
	commandDefinition.Flags().BoolVarP(&noOutput, "no-output", "", false, "If set, don't output the JSON result")
}

// vfsCommand is the parent of the commands reporting on VFS mounts.
var vfsCommand = &cobra.Command{
	Use:   "vfs",
	Short: `Commands for interacting with VFS mounts.`,
	Long: `Commands for interacting with VFS mounts.

Each subcommand reports on the state of a mounted remote.`,
}

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
		"groups":            "Filter,Listing",
	},
	Run: func(command *cobra.Command, args []string) {
		cmd.CheckArgs(1, 1, command, args)
		ctx := context.Background()
		parseFlags()
		if err := run(ctx, args[0]); err != nil {
			fs.Errorf(nil, "%v", err)
			os.Exit(1)
		}
	},
}

func parseFlags() {
	// set alternates from alternate flags
	setAlternateFlag("rc-addr", &url)
	setAlternateFlag("rc-user", &authUser)
	setAlternateFlag("rc-pass", &authPass)
	// If url is just :port then fix it up
	if strings.HasPrefix(url, ":") {
		url = "localhost" + url
	}
	// if url is just host:port add http://
	if !strings.HasPrefix(url, "http:") && !strings.HasPrefix(url, "https:") {
		url = "http://" + url
	}
	// if url doesn't end with / add it
	if !strings.HasSuffix(url, "/") {
		url += "/"
	}
}

// If the user set flagName set the output to its value
func setAlternateFlag(flagName string, output *string) {
	if rcFlag := pflag.Lookup(flagName); rcFlag != nil && rcFlag.Changed {
		*output = rcFlag.Value.String()
		if sliceValue, ok := rcFlag.Value.(pflag.SliceValue); ok {
			stringSlice := sliceValue.GetSlice()
			for _, value := range stringSlice {
				if value != "" {
					*output = value
					break
				}
			}
		}
	}
}

func run(ctx context.Context, pathArg string) error {
	// Validate path argument - empty path is a usage error
	if pathArg == "" {
		// Print usage to stderr and exit 2 (usage error)
		// Use cobra's usage mechanism
		fmt.Fprintf(os.Stderr, "Usage: rclone vfs status <path> [flags]\n\n")
		fmt.Fprintf(os.Stderr, "Report the cache and transfer state of a file on a VFS mount.\n\n")
		fmt.Fprintf(os.Stderr, "Example:\n")
		fmt.Fprintf(os.Stderr, "    rclone vfs status /mnt/mydrive/file.txt\n\n")
		fmt.Fprintf(os.Stderr, "Flags:\n")
		fmt.Fprintf(os.Stderr, "  -h, --help               help for status\n")
		fmt.Fprintf(os.Stderr, "      --fs string          remote config name of the mount to report on\n")
		fmt.Fprintf(os.Stderr, "      --no-output          If set, don't output the JSON result\n")
		fmt.Fprintf(os.Stderr, "      --rc-addr string     URL to connect to rclone remote control (default \"http://localhost:5572/\")\n")
		fmt.Fprintf(os.Stderr, "      --rc-pass string     Password to use to connect to rclone remote control\n")
		fmt.Fprintf(os.Stderr, "      --rc-user string     Username to use to rclone remote control\n")
		fmt.Fprintf(os.Stderr, "      --unix-socket string Path to a unix domain socket to dial to\n")
		fmt.Fprintf(os.Stderr, "  -v, --verbose            Enable verbose output on standard error\n")
		os.Exit(2)
	}

	// Build request
	in := rc.Params{
		"paths": []string{pathArg},
	}
	if fsFlag != "" {
		in["fs"] = fsFlag
	}

	// Do the call
	out, err := doCall(ctx, "vfs/file-status", in)
	if err != nil {
		// Check if it's a connection error (mount unreachable)
		if strings.Contains(err.Error(), "connection failed") {
			// Print error JSON to stdout and exit 1
			errOut := map[string]any{
				"path": pathArg,
				"error": map[string]string{
					"code":    "mount_unreachable",
					"message": "mount is not reachable; start it with --rc to allow queries",
				},
			}
			if !noOutput {
				_ = json.NewEncoder(os.Stdout).Encode(errOut)
			}
			return fmt.Errorf("mount unreachable")
		}
		return err
	}

	// Extract the single result from statuses array
	statuses, ok := out["statuses"].([]any)
	if !ok || len(statuses) == 0 {
		return fmt.Errorf("unexpected response format: missing statuses array")
	}

	status := statuses[0]
	statusMap, ok := status.(map[string]any)
	if !ok {
		return fmt.Errorf("unexpected status format")
	}

	// Print JSON to stdout
	if !noOutput {
		if err := json.NewEncoder(os.Stdout).Encode(statusMap); err != nil {
			return fmt.Errorf("failed to write output: %w", err)
		}
	}

	// Determine exit code
	if errObj, ok := statusMap["error"].(map[string]any); ok {
		// Per-path error
		code, _ := errObj["code"].(string)
		exitCode := exitCodeForErrorCode(code)
		os.Exit(exitCode)
	}

	// Success - determine exit code from state
	local, _ := statusMap["local"].(string)
	upload, _ := statusMap["upload"].(string)
	exitCode := exitCodeForState(local, upload)
	os.Exit(exitCode)

	return nil
}

func doCall(ctx context.Context, path string, in rc.Params) (rc.Params, error) {
	var client *http.Client
	if unixSocket == "" {
		client = fshttp.NewClient(ctx)
	} else {
		client = fshttp.NewClientWithUnixSocket(ctx, unixSocket)
	}

	data, err := json.Marshal(in)
	if err != nil {
		return nil, fmt.Errorf("failed to encode request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", url+path, bytes.NewBuffer(data))
	if err != nil {
		return nil, fmt.Errorf("failed to make request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	if authUser != "" || authPass != "" {
		req.SetBasicAuth(authUser, authPass)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("connection failed: %w", err)
	}
	defer fs.CheckClose(resp.Body, nil)

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	// Parse response
	var out rc.Params
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w: %s", err, string(body))
	}

	// Check status code
	if resp.StatusCode != http.StatusOK {
		// Handle whole-request errors
		if errObj, ok := out["error"].(map[string]any); ok {
			code, _ := errObj["code"].(string)
			message, _ := errObj["message"].(string)
			return nil, fmt.Errorf("%s: %s", code, message)
		}
		return nil, fmt.Errorf("operation %q failed with status %d: %s", path, resp.StatusCode, string(body))
	}

	return out, nil
}

// exitCodeForErrorCode maps error codes to exit codes per cli.md
func exitCodeForErrorCode(code string) int {
	switch code {
	case "empty_path", "param_invalid", "no_active_mount", "ambiguous_mount", "not_found", "unaccountable_path", "directory_not_file", "mount_unreachable":
		return 1 // Query failed
	default:
		return 1
	}
}

// exitCodeForState maps local/upload state to exit code per cli.md
func exitCodeForState(local, upload string) int {
	// Success (0): fully_held + uploaded/not_applicable
	if local == "fully_held" && (upload == "uploaded" || upload == "not_applicable") {
		return 0
	}
	// Not fully held (11): any other local state on cache-keeping mount
	// On cache-off mount, non-regular files return 11, others return 12
	// We can't determine cache mode from here, so default to 11 for not fully held
	if local != "fully_held" {
		// Check if it's a non-regular file (would be 11 even on cache-off)
		if local == "not_regular_file" {
			return 11
		}
		return 11
	}
	// Fully held but upload not complete
	if upload == "awaiting_upload" || upload == "uploading" || upload == "upload_failed" || upload == "not_tracked" {
		return 12
	}
	// fully_held + uploaded/not_applicable already handled above
	return 0
}