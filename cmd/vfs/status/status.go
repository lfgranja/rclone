// Package status provides the vfs status command.
package status

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	"github.com/rclone/rclone/vfs"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

var (
	// url is the control server address
	rcURL = "http://localhost:5572/"
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
	commandDefinition.Flags().StringVarP(&rcURL, "rc-addr", "", "http://localhost:5572/", "URL to connect to rclone remote control")
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
	setAlternateFlag("rc-addr", &rcURL)
	setAlternateFlag("rc-user", &authUser)
	setAlternateFlag("rc-pass", &authPass)
	// unix:///path handling per existing RC address rules
	// (lib/http/server.go: "unix:///path/to/socket" or absolute path).
	// A unix address is reachable with no further flags: the socket
	// becomes the dial target and the URL a dummy localhost URL,
	// since the unix dialer ignores the host.
	if strings.HasPrefix(rcURL, "unix://") {
		sock := strings.TrimPrefix(rcURL, "unix://")
		if sock != "" {
			unixSocket = sock
		}
		rcURL = "http://localhost/"
		return
	}
	if filepath.IsAbs(rcURL) {
		unixSocket = rcURL
		rcURL = "http://localhost/"
		return
	}
	// A bare :port is a TCP address on localhost.
	if strings.HasPrefix(rcURL, ":") {
		rcURL = "localhost" + rcURL
	}
	// if url is just host:port add http://
	if !strings.HasPrefix(rcURL, "http:") && !strings.HasPrefix(rcURL, "https:") {
		rcURL = "http://" + rcURL
	}
	// if url doesn't end with / add it
	if !strings.HasSuffix(rcURL, "/") {
		rcURL += "/"
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
		// A transport loss is a query failure with the mount_unreachable code.
		if isTransportLoss(err) {
			code := "mount_unreachable"
			emitError(pathArg, code, "mount is not reachable; start it with --rc to allow queries")
			os.Exit(exitCodeForErrorCode(code))
		}
		// A whole-request typed error preserves its code and message.
		if out != nil {
			if errObj, ok := out["error"].(map[string]any); ok {
				code, _ := errObj["code"].(string)
				message, _ := errObj["message"].(string)
				if code != "" {
					if message == "" {
						message = err.Error()
					}
					emitError(pathArg, code, message)
					os.Exit(exitCodeForErrorCode(code))
				}
			}
			if errStr, ok := out["error"].(string); ok && errStr != "" {
				code := "param_invalid"
				emitError(pathArg, code, errStr)
				os.Exit(exitCodeForErrorCode(code))
			}
		}
		code := "param_invalid"
		emitError(pathArg, code, err.Error())
		os.Exit(exitCodeForErrorCode(code))
	}

	statuses, ok := out["statuses"].([]any)
	if !ok || len(statuses) == 0 {
		code := "param_invalid"
		emitError(pathArg, code, "unexpected response format: missing statuses array")
		os.Exit(exitCodeForErrorCode(code))
	}

	status, ok := statuses[0].(map[string]any)
	if !ok || status == nil {
		code := "param_invalid"
		emitError(pathArg, code, "unexpected status format")
		os.Exit(exitCodeForErrorCode(code))
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

// errorf formats err and returns the synthetic RC error response.
func errorf(status int, path string, format string, arg ...any) (out rc.Params, err error) {
	err = fmt.Errorf(format, arg...)
	out = make(rc.Params)
	out["error"] = err.Error()
	out["path"] = path
	out["status"] = status
	return out, err
}

// doCall posts in to the RC path and returns the decoded response.
//
// If err is set, out may be a valid error return or nil.
func doCall(ctx context.Context, path string, in rc.Params) (out rc.Params, err error) {
	var client *http.Client
	if unixSocket == "" {
		client = fshttp.NewClient(ctx)
	} else {
		client = fshttp.NewClientWithUnixSocket(ctx, unixSocket)
	}
	requestURL := rcURL + path
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
		return errorf(http.StatusServiceUnavailable, path, "connection failed: %w", &transportError{err: err})
	}
	defer fs.CheckClose(resp.Body, &err)

	// Read response
	var body []byte
	var bodyString string
	body, err = io.ReadAll(resp.Body)
	bodyString = strings.TrimSpace(string(body))
	if err != nil {
		return errorf(resp.StatusCode, path, "failed to read rc response: %w: %s", &responseReadError{err: err}, bodyString)
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

// responseReadError marks a failure while reading the RC response body.
type responseReadError struct {
	err error
}

func (e *responseReadError) Error() string { return e.err.Error() }

func (e *responseReadError) Unwrap() error { return e.err }

// transportError marks a failure returned by the HTTP client.
type transportError struct {
	err error
}

func (e *transportError) Error() string { return e.err.Error() }

func (e *transportError) Unwrap() error { return e.err }

// isTransportLoss reports whether err is a client.Do or response read failure.
func isTransportLoss(err error) bool {
	if err == nil {
		return false
	}
	var readErr *responseReadError
	if errors.As(err, &readErr) {
		return true
	}
	var transportErr *transportError
	if errors.As(err, &transportErr) {
		return true
	}
	return errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.ErrClosedPipe)
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

// exitCodeForErrorCode maps error codes to their documented exit status.
func exitCodeForErrorCode(code string) int {
	switch code {
	case "empty_path", "param_invalid":
		return 2 // Invocation error.
	case "no_active_mount", "ambiguous_mount", "not_found", "unaccountable_path", "directory_not_file", "mount_unreachable":
		return 1 // Query failed.
	default:
		return 1
	}
}

// exitCodeForState maps reported local and upload states to their exit status.
func exitCodeForState(local, upload string) int {
	return (vfs.FileStatus{
		Local:  vfs.FileLocalState(local),
		Upload: vfs.FileUploadState(upload),
	}).ExitCode(upload == string(vfs.FileUploadNotTracked))
}
