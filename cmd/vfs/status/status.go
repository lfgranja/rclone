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
	"path/filepath"
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
	// unix:///path handling per existing RC address rules
	// (lib/http/server.go: "unix:///path/to/socket" or absolute path).
	// A unix address is reachable with no further flags: the socket
	// becomes the dial target and the URL a dummy localhost URL,
	// since the unix dialer ignores the host.
	if strings.HasPrefix(url, "unix://") {
		sock := strings.TrimPrefix(url, "unix://")
		if sock != "" {
			unixSocket = sock
		}
		url = "http://localhost/"
		return
	}
	if filepath.IsAbs(url) {
		unixSocket = url
		url = "http://localhost/"
		return
	}
	// TCP rules copied from cmd/rc
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
		fmt.Fprintf(os.Stderr, "Usage: rclone vfs status <path> [flags]\n\n")
		fmt.Fprintf(os.Stderr, "Report the cache and transfer state of a file on a VFS mount.\n\n")
		fmt.Fprintf(os.Stderr, "Example:\n")
		fmt.Fprintf(os.Stderr, "    rclone vfs status /mnt/mydrive/file.txt\n")
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
		// Transport loss (dial failure or mid-body disconnect) is
		// mount_unreachable per cli.md.
		if isTransportLoss(err) {
			emitError(pathArg, "mount_unreachable", "mount is not reachable; start it with --rc to allow queries")
			os.Exit(1)
		}
		// Whole-request typed error preserves its code/message.
		if out != nil {
			if errObj, ok := out["error"].(map[string]any); ok {
				code, _ := errObj["code"].(string)
				message, _ := errObj["message"].(string)
				if code != "" {
					if message == "" {
						message = err.Error()
					}
					emitError(pathArg, code, message)
					os.Exit(1)
				}
			}
			// Code-less envelope (rc server string error) is param_invalid.
			if errStr, ok := out["error"].(string); ok && errStr != "" {
				emitError(pathArg, "param_invalid", errStr)
				os.Exit(1)
			}
		}
		// Any other RC/transport failure still emits JSON + exit 1.
		emitError(pathArg, "param_invalid", err.Error())
		os.Exit(1)
	}

	// Extract the single result from statuses array
	statuses, ok := out["statuses"].([]any)
	if !ok || len(statuses) == 0 {
		emitError(pathArg, "param_invalid", "unexpected response format: missing statuses array")
		os.Exit(1)
	}

	status, ok := statuses[0].(map[string]any)
	if !ok || status == nil {
		emitError(pathArg, "param_invalid", "unexpected status format")
		os.Exit(1)
	}

	// Per-path error echoes the single result object.
	if errObj, ok := status["error"].(map[string]any); ok {
		emitJSON(status)
		code, _ := errObj["code"].(string)
		os.Exit(exitCodeForErrorCode(code))
	}

	// Success - emit the single result object and map state to exit.
	local, _ := status["local"].(string)
	upload, _ := status["upload"].(string)
	emitJSON(status)
	os.Exit(exitCodeForState(local, upload))

	return nil
}

// Format an error and create a synthetic server return from it.
// Scaffold copied from cmd/rc/rc.go errorf, then edited by hand.
func errorf(status int, path string, format string, arg ...any) (out rc.Params, err error) {
	err = fmt.Errorf(format, arg...)
	out = make(rc.Params)
	out["error"] = err.Error()
	out["path"] = path
	out["status"] = status
	return out, err
}

// do a call from (path, in) to (out, err).
//
// Scaffold copied from cmd/rc/rc.go doCall (HTTP branch only; no
// loopback flag on this command), then edited by hand: local
// request URL (no global mutation) and fixed read-error errorf
// argument order (status, path, format).
//
// if err is set, out may be a valid error return or it may be nil
func doCall(ctx context.Context, path string, in rc.Params) (out rc.Params, err error) {
	var client *http.Client
	if unixSocket == "" {
		client = fshttp.NewClient(ctx)
	} else {
		client = fshttp.NewClientWithUnixSocket(ctx, unixSocket)
	}
	requestURL := url + path
	data, err := json.Marshal(in)
	if err != nil {
		return errorf(http.StatusBadRequest, path, "failed to encode request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", requestURL, bytes.NewBuffer(data))
	if err != nil {
		return errorf(http.StatusInternalServerError, path, "failed to make request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	if authUser != "" || authPass != "" {
		req.SetBasicAuth(authUser, authPass)
	}

	resp, err := client.Do(req)
	if err != nil {
		return errorf(http.StatusServiceUnavailable, path, "connection failed: %w", err)
	}
	defer fs.CheckClose(resp.Body, &err)

	// Read response
	var body []byte
	var bodyString string
	body, err = io.ReadAll(resp.Body)
	bodyString = strings.TrimSpace(string(body))
	if err != nil {
		return errorf(resp.StatusCode, path, "failed to read rc response: %s: %s", resp.Status, bodyString)
	}

	// Parse output
	out = make(rc.Params)
	err = json.NewDecoder(strings.NewReader(bodyString)).Decode(&out)
	if err != nil {
		return errorf(resp.StatusCode, path, "failed to decode response: %w: %s", err, bodyString)
	}

	// Check we got 200 OK
	if resp.StatusCode != http.StatusOK {
		err = fmt.Errorf("operation %q failed: %v", path, out["error"])
	}

	return out, err
}

// isTransportLoss reports whether err is a transport-level failure:
// the dial itself failed or the response body was lost mid-read.
// Both map to mount_unreachable per cli.md/T080.
func isTransportLoss(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	if strings.Contains(msg, "connection failed") {
		return true
	}
	if strings.Contains(msg, "failed to read rc response") {
		return true
	}
	if strings.Contains(msg, "unexpected EOF") || strings.Contains(msg, "connection reset") || strings.Contains(msg, "broken pipe") {
		return true
	}
	return false
}

// emitJSON writes a single JSON object to stdout.
func emitJSON(v any) {
	_ = json.NewEncoder(os.Stdout).Encode(v)
}

// emitError writes the contract error object for path to stdout.
func emitError(path, code, message string) {
	emitJSON(map[string]any{
		"path": path,
		"error": map[string]string{
			"code":    code,
			"message": message,
		},
	})
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

// exitCodeForState maps local/upload state to exit code per cli.md.
//
// Success (0) only for fully_held with uploaded/not_applicable.
// On cache-off (upload not_tracked) every content state takes 12;
// not_regular_file keeps 11 even there. On cache-keeping mounts any
// non-fully_held local state takes 11 (precedence over 12).
func exitCodeForState(local, upload string) int {
	if upload == "not_tracked" {
		if local == "not_regular_file" {
			return 11
		}
		return 12
	}
	if local != "fully_held" {
		return 11
	}
	if upload == "uploaded" || upload == "not_applicable" {
		return 0
	}
	return 12
}
