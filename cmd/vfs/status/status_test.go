package status

import (
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"testing/quick"
	"time"
)

// rcloneBin is the rclone binary these tests invoke, built once in TestMain.
var rcloneBin string

// TestMain builds the rclone binary the tests exercise: they assert on the
// command's observable process behaviour (exit status and the two output
// streams) rather than on package internals.
func TestMain(m *testing.M) {
	// This file lives at <repo root>/cmd/vfs/status/status_test.go.
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		fmt.Fprintln(os.Stderr, "status: cannot locate this test file")
		os.Exit(1)
	}
	repoRoot := filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(thisFile))))

	goBin, err := exec.LookPath("go")
	if err != nil {
		// "go" is not always on PATH; the toolchain that built this test
		// binary knows where its own GOROOT is.
		// Use `go env GOROOT` instead of deprecated runtime.GOROOT().
		cmd := exec.Command("go", "env", "GOROOT")
		output, err := cmd.Output()
		if err != nil {
			fmt.Fprintf(os.Stderr, "go env GOROOT failed: %v\n", err)
			os.Exit(1)
		}
		goRoot := strings.TrimSpace(string(output))
		goBin = filepath.Join(goRoot, "bin", "go")
	}

	binDir, err := os.MkdirTemp("", "rclone-vfs-status-test")
	if err != nil {
		fmt.Fprintf(os.Stderr, "status: %v\n", err)
		os.Exit(1)
	}
	defer func() { _ = os.RemoveAll(binDir) }()
	rcloneBin = filepath.Join(binDir, "rclone")

	build := exec.Command(goBin, "build", "-o", rcloneBin, ".")
	build.Dir = repoRoot
	if out, err := build.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "status: building rclone: %v: %s\n", err, out)
		os.Exit(1)
	}

	code := m.Run()
	os.Exit(code)
}

// runVFSStatus runs the built rclone binary's "vfs status" command with the
// given arguments, returning its exit code and the two output streams.
func runVFSStatus(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	cmd := exec.Command(rcloneBin, args...)
	var out, errOut strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	runErr := cmd.Run()
	code = 0
	if runErr != nil {
		exitErr, ok := runErr.(*exec.ExitError)
		if !ok {
			t.Fatalf("running %v: %v", args, runErr)
		}
		code = exitErr.ExitCode()
	}
	return code, out.String(), errOut.String()
}

type capturedStatusRequest struct {
	Method        string
	URLPath       string
	FS            string
	Paths         []string
	Authorization string
	AuthUser      string
	AuthPass      string
	HasAuth       bool
}

type statusRequestBody struct {
	FS    string   `json:"fs"`
	Paths []string `json:"paths"`
}

type statusResponder func(http.ResponseWriter, *http.Request, capturedStatusRequest)

func newStatusTestHandler(respond statusResponder) (http.Handler, <-chan capturedStatusRequest) {
	requests := make(chan capturedStatusRequest, 8)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body statusRequestBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "invalid test request", http.StatusBadRequest)
			return
		}
		user, pass, hasAuth := r.BasicAuth()
		request := capturedStatusRequest{
			Method:        r.Method,
			URLPath:       r.URL.Path,
			FS:            body.FS,
			Paths:         body.Paths,
			Authorization: r.Header.Get("Authorization"),
			AuthUser:      user,
			AuthPass:      pass,
			HasAuth:       hasAuth,
		}
		requests <- request
		respond(w, r, request)
	})
	return handler, requests
}

func startStatusHTTPServer(t *testing.T, respond statusResponder) (*httptest.Server, <-chan capturedStatusRequest) {
	t.Helper()
	handler, requests := newStatusTestHandler(respond)
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return server, requests
}

func startStatusUnixServer(t *testing.T, socketPath string, respond statusResponder) (string, <-chan capturedStatusRequest) {
	t.Helper()
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen on Unix socket %q: %v", socketPath, err)
	}
	handler, requests := newStatusTestHandler(respond)
	server := &http.Server{Handler: handler}
	go func() {
		_ = server.Serve(listener)
	}()
	t.Cleanup(func() {
		_ = server.Close()
		_ = listener.Close()
	})
	return "unix://" + socketPath, requests
}

func runVFSStatusAt(t *testing.T, address string, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	commandArgs := []string{"vfs", "status", "--rc-addr", address}
	return runVFSStatus(t, append(commandArgs, args...)...)
}

func writeJSONResponse(w http.ResponseWriter, statusCode int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	_ = json.NewEncoder(w).Encode(body)
}

func writeRawResponse(w http.ResponseWriter, statusCode int, contentType, body string) {
	if contentType != "" {
		w.Header().Set("Content-Type", contentType)
	}
	w.WriteHeader(statusCode)
	_, _ = io.WriteString(w, body)
}

func writeStatusEnvelope(w http.ResponseWriter, status map[string]any) {
	writeJSONResponse(w, http.StatusOK, map[string]any{
		"statuses": []any{status},
	})
}

func statusResult(path, local, upload string) map[string]any {
	bytes := map[string]any{
		"unbroken_run": int64(2),
		"total_held":   int64(4),
		"total_size":   int64(10),
	}
	switch local {
	case "fully_held":
		bytes["unbroken_run"] = int64(10)
		bytes["total_held"] = int64(10)
	case "not_held":
		bytes["unbroken_run"] = int64(0)
		bytes["total_held"] = int64(0)
	case "underivable":
		bytes["unbroken_run"] = "not_determinable"
		bytes["total_held"] = "not_determinable"
	case "not_regular_file":
		bytes["unbroken_run"] = "not_measurable"
		bytes["total_held"] = "not_measurable"
		bytes["total_size"] = "not_measurable"
	}
	return map[string]any{
		"path":   path,
		"local":  local,
		"upload": upload,
		"bytes":  bytes,
	}
}

func receiveStatusRequest(t *testing.T, requests <-chan capturedStatusRequest) capturedStatusRequest {
	t.Helper()
	select {
	case request := <-requests:
		return request
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for the status request")
		return capturedStatusRequest{}
	}
}

func decodeSingleJSONObject(t *testing.T, output string) map[string]any {
	t.Helper()
	decoder := json.NewDecoder(strings.NewReader(output))
	var object map[string]any
	if err := decoder.Decode(&object); err != nil {
		t.Fatalf("stdout %q is not a single JSON object: %v", output, err)
	}
	if object == nil {
		t.Fatalf("stdout %q contains a JSON value other than an object", output)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		t.Fatalf("stdout %q contains more than one JSON value", output)
	}
	return object
}

func assertJSONOutputEquals(t *testing.T, output string, want map[string]any) {
	t.Helper()
	got := decodeSingleJSONObject(t, output)
	gotJSON, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal decoded output: %v", err)
	}
	wantJSON, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal expected output: %v", err)
	}
	if string(gotJSON) != string(wantJSON) {
		t.Errorf("stdout JSON = %s, want %s", gotJSON, wantJSON)
	}
}

func assertErrorJSON(t *testing.T, output, path, wantCode string) {
	t.Helper()
	object := decodeSingleJSONObject(t, output)
	if got, _ := object["path"].(string); got != path {
		t.Errorf("JSON path = %q, want %q", got, path)
	}
	errorObject, ok := object["error"].(map[string]any)
	if !ok {
		t.Fatalf("JSON error = %v, want an error object", object["error"])
	}
	if wantCode != "" {
		if got, _ := errorObject["code"].(string); got != wantCode {
			t.Errorf("JSON error code = %q, want %q", got, wantCode)
		}
	}
	if message, ok := errorObject["message"].(string); !ok || message == "" {
		t.Errorf("JSON error message = %v, want a non-empty string", errorObject["message"])
	}
}

// TestUsageError checks that an invocation with no path argument or with
// more than one path argument is a usage error: exit status 2, the
// project's standard usage text on standard error, and nothing on standard
// output for error handling.
func TestUsageError(t *testing.T) {
	for _, args := range [][]string{
		{},                                 // no path argument
		{"/one/file.txt", "/two/file.txt"}, // more than one path argument
		{"/one/file.txt", "/two/file.txt", "/three/file.txt"},
	} {
		code, stdout, stderr := runVFSStatus(t, append([]string{"vfs", "status"}, args...)...)
		if code != 2 {
			t.Errorf("vfs status %v: exit code = %d, want 2 (usage error)", args, code)
		}
		if stdout != "" {
			t.Errorf("vfs status %v: stdout = %q, want nothing", args, stdout)
		}
		if !strings.Contains(stderr, "Usage:") || !strings.Contains(stderr, "status <path>") {
			t.Errorf("vfs status %v: stderr = %q, want the standard usage text", args, stderr)
		}
	}
}

// TestEmptyPathIsUsageError checks that an empty path argument is a usage
// error like a missing one: exit status 2, usage text on standard error,
// nothing on standard output for exit status codes.
func TestEmptyPathIsUsageError(t *testing.T) {
	code, stdout, stderr := runVFSStatus(t, "vfs", "status", "")
	if code != 2 {
		t.Errorf("vfs status \"\": exit code = %d, want 2 (usage error)", code)
	}
	if stdout != "" {
		t.Errorf("vfs status \"\": stdout = %q, want nothing", stdout)
	}
	if !strings.Contains(stderr, "Usage:") {
		t.Errorf("vfs status \"\": stderr = %q, want the standard usage text", stderr)
	}
}

// TestSinglePathNoMount checks a valid single path argument with no mount
// running: the command answers with a single JSON object on standard output
// and exit status 1 (query failed), and the object carries the contract
// shape of the output format: either a state object
// (path, local, upload, bytes) or an error object (path, error with code
// and message). The message is not part of the contract.
func TestSinglePathNoMount(t *testing.T) {
	path := filepath.Join(t.TempDir(), "file.txt")
	code, stdout, _ := runVFSStatus(t, "vfs", "status", path)
	if code != 1 {
		t.Errorf("vfs status %s: exit code = %d, want 1 (query failed)", path, code)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &out); err != nil {
		t.Fatalf("vfs status %s: stdout = %q, want a single JSON object: %v", path, stdout, err)
	}
	gotPath, _ := out["path"].(string)
	if gotPath != path {
		t.Errorf("vfs status %s: JSON \"path\" = %q, want the requested path", path, gotPath)
	}
	if _, ok := out["local"]; ok {
		for _, field := range []string{"upload", "bytes"} {
			if _, ok := out[field]; !ok {
				t.Errorf("vfs status %s: state object is missing %q", path, field)
			}
		}
		return
	}
	errObj, ok := out["error"].(map[string]any)
	if !ok {
		t.Fatalf("vfs status %s: JSON has neither a state object nor an \"error\" object: %v", path, out)
	}
	for _, field := range []string{"code", "message"} {
		if _, ok := errObj[field].(string); !ok {
			t.Errorf("vfs status %s: error object is missing string %q", path, field)
		}
	}
}

// TestHelp checks that --help prints the command's help text on standard
// output and exits 0 for help.
func TestHelp(t *testing.T) {
	code, stdout, _ := runVFSStatus(t, "vfs", "status", "--help")
	if code != 0 {
		t.Errorf("vfs status --help: exit code = %d, want 0", code)
	}
	for _, want := range []string{
		"Report the cache and transfer state of a file on a VFS mount.",
		"Usage:",
		"status <path>",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("vfs status --help: stdout does not contain %q", want)
		}
	}
	if strings.Contains(stdout, "--no-output") {
		t.Error("vfs status --help advertises --no-output, which is not part of the command contract")
	}
}

// TestExitCodeAndOutputContract checks the contract over a range of argv
// shapes: every exit status is one of the command's declared codes
// (0, 1, 2, 11 or 12), and whenever
// the exit status is not the usage error 2, standard output is a single
// JSON object.
func TestExitCodeAndOutputContract(t *testing.T) {
	paths := []string{"", "/nonexistent/mount/file.txt", "relative/file.txt"}
	var shapes [][]string
	for _, a := range paths {
		for _, b := range paths {
			shapes = append(shapes,
				[]string{a, b},
				[]string{"--fs=mydrive", a, b},
			)
		}
		shapes = append(shapes, []string{a}, []string{"--fs=mydrive", a})
	}
	shapes = append(shapes,
		[]string{},
		[]string{"--fs=mydrive"},
		// An unknown flag is rejected by the command machinery itself.
		[]string{"--no-such-flag"},
		[]string{"--no-such-flag", "/nonexistent/mount/file.txt"},
	)
	for _, args := range shapes {
		code, stdout, _ := runVFSStatus(t, append([]string{"vfs", "status"}, args...)...)
		switch code {
		case 0, 1, 2, 11, 12:
		default:
			t.Errorf("vfs status %v: exit code = %d, want one of 0, 1, 2, 11, 12", args, code)
			continue
		}
		if code == 2 {
			continue // a usage error prints usage text to standard error only
		}
		var out map[string]any
		if err := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &out); err != nil {
			t.Errorf("vfs status %v: exit code = %d, but stdout = %q is not a single JSON object: %v", args, code, stdout, err)
		}
	}
}

func expectedExitCodeForContract(local, upload string) int {
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

func TestExitCodeForStateCacheOffExamples(t *testing.T) {
	tests := []struct {
		name   string
		local  string
		upload string
		want   int
	}{
		{name: "cache-off remote-only file", local: "not_held", upload: "not_tracked", want: 12},
		{name: "cache-off partly-held file", local: "partly_held", upload: "not_tracked", want: 12},
		{name: "cache-off download failure", local: "download_failed", upload: "not_tracked", want: 12},
		{name: "cache-off underivable file", local: "underivable", upload: "not_tracked", want: 12},
		{name: "cache-off non-regular entry carve-out", local: "not_regular_file", upload: "not_tracked", want: 11},
		{name: "fully-held and uploaded", local: "fully_held", upload: "uploaded", want: 0},
		{name: "fully-held and not applicable", local: "fully_held", upload: "not_applicable", want: 0},
		{name: "fully-held but upload outstanding", local: "fully_held", upload: "awaiting_upload", want: 12},
		{name: "local state takes precedence on cache-keeping mount", local: "download_failed", upload: "upload_failed", want: 11},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := exitCodeForState(test.local, test.upload); got != test.want {
				t.Errorf("exitCodeForState(%q, %q) = %d, want %d", test.local, test.upload, got, test.want)
			}
		})
	}
}

func TestExitCodeForStateCacheModeProperty(t *testing.T) {
	localStates := []string{
		"fully_held",
		"partly_held",
		"not_held",
		"underivable",
		"download_failed",
		"not_regular_file",
	}
	cacheKeepingUploadStates := []string{
		"uploaded",
		"awaiting_upload",
		"uploading",
		"upload_failed",
		"not_applicable",
	}
	property := func(localIndex, uploadIndex uint8, cacheOff bool) bool {
		local := localStates[int(localIndex)%len(localStates)]
		upload := cacheKeepingUploadStates[int(uploadIndex)%len(cacheKeepingUploadStates)]
		if cacheOff {
			upload = "not_tracked"
		}
		got := exitCodeForState(local, upload)
		want := expectedExitCodeForContract(local, upload)
		allowed := got == 0 || got == 11 || got == 12
		return allowed && got == want
	}
	config := &quick.Config{
		MaxCount: 512,
		Rand:     rand.New(rand.NewSource(0x5eed)),
	}
	if err := quick.Check(property, config); err != nil {
		t.Fatalf("state/cache-mode exit mapping violates the CLI contract: %v", err)
	}
}

// TestExitCodeForStateExhaustiveContract checks the full local×upload matrix:
// exit 0 only for fully_held with uploaded/not_applicable, 11 for
// non-regular files (even cache-off), and 12 for every other cache-off
// content state.
func TestExitCodeForStateExhaustiveContract(t *testing.T) {
	localStates := []string{
		"fully_held",
		"partly_held",
		"not_held",
		"underivable",
		"download_failed",
		"not_regular_file",
	}
	uploadStates := []string{
		"uploaded",
		"awaiting_upload",
		"uploading",
		"upload_failed",
		"not_applicable",
		"not_tracked",
	}
	for _, local := range localStates {
		for _, upload := range uploadStates {
			got := exitCodeForState(local, upload)
			want := expectedExitCodeForContract(local, upload)
			if got != want {
				t.Errorf("exitCodeForState(%q, %q) = %d, want %d", local, upload, got, want)
			}
			if want == 0 && !(local == "fully_held" && (upload == "uploaded" || upload == "not_applicable")) {
				t.Errorf("exit 0 for (%q, %q): success must be only fully_held with uploaded/not_applicable", local, upload)
			}
			switch got {
			case 0, 11, 12:
			default:
				t.Errorf("exitCodeForState(%q, %q) = %d, want one of 0, 11, 12", local, upload, got)
			}
		}
	}
}

func TestNoOutputFlagIsRemoved(t *testing.T) {
	path := "/mnt/test/file.txt"
	server, requests := startStatusHTTPServer(t, func(w http.ResponseWriter, _ *http.Request, request capturedStatusRequest) {
		writeStatusEnvelope(w, statusResult(request.Paths[0], "fully_held", "uploaded"))
	})

	code, stdout, stderr := runVFSStatusAt(t, server.URL, "--no-output", path)
	if code != 2 {
		t.Errorf("vfs status --no-output: exit code = %d, want 2 for an unknown flag", code)
	}
	if stdout != "" {
		t.Errorf("vfs status --no-output: stdout = %q, want nothing for a usage error", stdout)
	}
	if !strings.Contains(stderr, "--no-output") {
		t.Errorf("vfs status --no-output: stderr = %q, want the rejected flag to be named", stderr)
	}
	select {
	case request := <-requests:
		t.Errorf("vfs status --no-output unexpectedly sent an RC request: %+v", request)
	default:
	}
}

func TestPerPathFailureOutputAndExitCode(t *testing.T) {
	path := "/mnt/test/missing.txt"
	want := map[string]any{
		"path": path,
		"error": map[string]any{
			"code":    "not_found",
			"message": "File does not exist in mount",
		},
	}
	server, _ := startStatusHTTPServer(t, func(w http.ResponseWriter, _ *http.Request, _ capturedStatusRequest) {
		writeStatusEnvelope(w, want)
	})

	code, stdout, _ := runVFSStatusAt(t, server.URL, path)
	if code != 1 {
		t.Errorf("vfs status for a per-path error: exit code = %d, want 1", code)
	}
	assertJSONOutputEquals(t, stdout, want)
}

func TestWholeRequestFailuresAreJSON(t *testing.T) {
	tests := []struct {
		name        string
		statusCode  int
		contentType string
		body        string
		wantCode    string
	}{
		{
			name:        "endpoint typed error",
			statusCode:  http.StatusBadRequest,
			contentType: "application/json",
			body:        `{"error":{"code":"no_active_mount","message":"no active mount"}}`,
			wantCode:    "no_active_mount",
		},
		{
			name:        "rc server code-less bad request",
			statusCode:  http.StatusBadRequest,
			contentType: "application/json",
			body:        `{"status":400,"error":"could not decode request"}`,
			wantCode:    "param_invalid",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := "/mnt/test/file.txt"
			server, requests := startStatusHTTPServer(t, func(w http.ResponseWriter, _ *http.Request, _ capturedStatusRequest) {
				writeRawResponse(w, test.statusCode, test.contentType, test.body)
			})
			code, stdout, _ := runVFSStatusAt(t, server.URL, path)
			if code != 1 {
				t.Errorf("whole-request %s: exit code = %d, want 1", test.name, code)
			}
			assertErrorJSON(t, stdout, path, test.wantCode)
			request := receiveStatusRequest(t, requests)
			if request.URLPath != "/vfs/file-status" {
				t.Errorf("RC request path = %q, want /vfs/file-status", request.URLPath)
			}
		})
	}
}

func TestOtherRCFailuresStillProduceJSON(t *testing.T) {
	tests := []struct {
		name        string
		statusCode  int
		contentType string
		body        string
	}{
		{name: "authentication rejection", statusCode: http.StatusUnauthorized, contentType: "text/plain", body: "Unauthorized"},
		{name: "forbidden", statusCode: http.StatusForbidden, contentType: "text/plain", body: "Forbidden"},
		{name: "server error", statusCode: http.StatusInternalServerError, contentType: "text/plain", body: "internal error"},
		{name: "invalid JSON response", statusCode: http.StatusOK, contentType: "application/json", body: "not-json"},
		{name: "missing statuses", statusCode: http.StatusOK, contentType: "application/json", body: `{}`},
		{name: "empty statuses", statusCode: http.StatusOK, contentType: "application/json", body: `{"statuses":[]}`},
		{name: "non-object status", statusCode: http.StatusOK, contentType: "application/json", body: `{"statuses":[null]}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := "/mnt/test/file.txt"
			server, _ := startStatusHTTPServer(t, func(w http.ResponseWriter, _ *http.Request, _ capturedStatusRequest) {
				writeRawResponse(w, test.statusCode, test.contentType, test.body)
			})
			code, stdout, _ := runVFSStatusAt(t, server.URL, path)
			if code != 1 {
				t.Errorf("%s: exit code = %d, want 1", test.name, code)
			}
			assertErrorJSON(t, stdout, path, "")
		})
	}
}

func TestConnectionFailureMapsToMountUnreachableJSON(t *testing.T) {
	path := "/mnt/test/file.txt"
	code, stdout, _ := runVFSStatusAt(t, "http://127.0.0.1:0", path)
	if code != 1 {
		t.Errorf("connection failure: exit code = %d, want 1", code)
	}
	assertErrorJSON(t, stdout, path, "mount_unreachable")
}

func TestStatusOutputAndExitMappingAgainstControlledRC(t *testing.T) {
	tests := []struct {
		name     string
		local    string
		upload   string
		wantExit int
	}{
		{name: "fully held and uploaded", local: "fully_held", upload: "uploaded", wantExit: 0},
		{name: "fully held and not applicable", local: "fully_held", upload: "not_applicable", wantExit: 0},
		{name: "upload outstanding", local: "fully_held", upload: "awaiting_upload", wantExit: 12},
		{name: "cache-keeping mount not fully held", local: "partly_held", upload: "uploaded", wantExit: 11},
		{name: "cache-off content state", local: "not_held", upload: "not_tracked", wantExit: 12},
		{name: "cache-off download failure", local: "download_failed", upload: "not_tracked", wantExit: 12},
		{name: "cache-off non-regular carve-out", local: "not_regular_file", upload: "not_tracked", wantExit: 11},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "file.txt")
			want := statusResult(path, test.local, test.upload)
			server, requests := startStatusHTTPServer(t, func(w http.ResponseWriter, _ *http.Request, request capturedStatusRequest) {
				writeStatusEnvelope(w, statusResult(request.Paths[0], test.local, test.upload))
			})

			code, stdout, _ := runVFSStatusAt(t, server.URL, path)
			if code != test.wantExit {
				t.Errorf("state (%s, %s): exit code = %d, want %d", test.local, test.upload, code, test.wantExit)
			}
			assertJSONOutputEquals(t, stdout, want)
			request := receiveStatusRequest(t, requests)
			if request.Method != http.MethodPost || request.URLPath != "/vfs/file-status" {
				t.Errorf("RC request = %s %s, want POST /vfs/file-status", request.Method, request.URLPath)
			}
			if len(request.Paths) != 1 || request.Paths[0] != path {
				t.Errorf("RC paths = %v, want [%q]", request.Paths, path)
			}
		})
	}
}

func TestFSSelectionIsSentToRC(t *testing.T) {
	path := "/mnt/shared/file.txt"
	server, requests := startStatusHTTPServer(t, func(w http.ResponseWriter, _ *http.Request, request capturedStatusRequest) {
		if request.FS == "drive[1]" {
			writeStatusEnvelope(w, statusResult(request.Paths[0], "fully_held", "uploaded"))
			return
		}
		writeStatusEnvelope(w, statusResult(request.Paths[0], "not_held", "not_applicable"))
	})

	code, stdout, _ := runVFSStatusAt(t, server.URL, "--fs=drive[1]", path)
	if code != 0 {
		t.Errorf("vfs status --fs drive[1]: exit code = %d, want 0", code)
	}
	assertJSONOutputEquals(t, stdout, statusResult(path, "fully_held", "uploaded"))
	request := receiveStatusRequest(t, requests)
	if request.FS != "drive[1]" {
		t.Errorf("RC fs = %q, want drive[1]", request.FS)
	}
	if len(request.Paths) != 1 || request.Paths[0] != path {
		t.Errorf("RC paths = %v, want [%q]", request.Paths, path)
	}
}

// TestFSOmittedSendsEmptyFS checks the negative half of --fs selection:
// without --fs the RC body carries no mount identifier and the
// endpoint answer is still echoed byte-for-byte with exit 0.
func TestFSOmittedSendsEmptyFS(t *testing.T) {
	path := "/mnt/shared/file.txt"
	server, requests := startStatusHTTPServer(t, func(w http.ResponseWriter, _ *http.Request, request capturedStatusRequest) {
		writeStatusEnvelope(w, statusResult(request.Paths[0], "fully_held", "uploaded"))
	})

	code, stdout, _ := runVFSStatusAt(t, server.URL, path)
	if code != 0 {
		t.Errorf("vfs status without --fs: exit code = %d, want 0", code)
	}
	assertJSONOutputEquals(t, stdout, statusResult(path, "fully_held", "uploaded"))
	request := receiveStatusRequest(t, requests)
	if request.FS != "" {
		t.Errorf("RC fs = %q, want empty when --fs is omitted", request.FS)
	}
	if len(request.Paths) != 1 || request.Paths[0] != path {
		t.Errorf("RC paths = %v, want [%q]", request.Paths, path)
	}
}

func TestRCAuthenticationAndAuthenticationErrorOutput(t *testing.T) {
	path := "/mnt/private/file.txt"
	server, requests := startStatusHTTPServer(t, func(w http.ResponseWriter, _ *http.Request, request capturedStatusRequest) {
		if !request.HasAuth || request.AuthUser != "rc-user" || request.AuthPass != "rc-pass" {
			writeRawResponse(w, http.StatusUnauthorized, "text/plain", "Unauthorized")
			return
		}
		writeStatusEnvelope(w, statusResult(request.Paths[0], "fully_held", "uploaded"))
	})

	code, stdout, _ := runVFSStatusAt(t, server.URL, "--rc-user=rc-user", "--rc-pass=rc-pass", path)
	if code != 0 {
		t.Errorf("authenticated vfs status: exit code = %d, want 0", code)
	}
	assertJSONOutputEquals(t, stdout, statusResult(path, "fully_held", "uploaded"))
	goodRequest := receiveStatusRequest(t, requests)
	if !goodRequest.HasAuth || goodRequest.AuthUser != "rc-user" || goodRequest.AuthPass != "rc-pass" {
		t.Errorf("RC basic auth = (%q, %q, present=%t), want configured credentials", goodRequest.AuthUser, goodRequest.AuthPass, goodRequest.HasAuth)
	}

	code, stdout, _ = runVFSStatusAt(t, server.URL, "--rc-user=rc-user", "--rc-pass=wrong", path)
	if code != 1 {
		t.Errorf("rejected RC credentials: exit code = %d, want 1", code)
	}
	assertErrorJSON(t, stdout, path, "")
	badRequest := receiveStatusRequest(t, requests)
	if !badRequest.HasAuth || badRequest.AuthPass != "wrong" {
		t.Errorf("rejected RC auth = (%q, present=%t), want the supplied wrong password", badRequest.AuthPass, badRequest.HasAuth)
	}
}

func TestUnixRCAddress(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix-domain sockets are not available on Windows")
	}
	path := filepath.Join(t.TempDir(), "file.txt")
	address, requests := startStatusUnixServer(t, filepath.Join(t.TempDir(), "rclone.sock"), func(w http.ResponseWriter, _ *http.Request, request capturedStatusRequest) {
		writeStatusEnvelope(w, statusResult(request.Paths[0], "fully_held", "uploaded"))
	})

	code, stdout, _ := runVFSStatusAt(t, address, path)
	if code != 0 {
		t.Fatalf("vfs status over %q: exit code = %d, stdout = %q", address, code, stdout)
	}
	assertJSONOutputEquals(t, stdout, statusResult(path, "fully_held", "uploaded"))
	request := receiveStatusRequest(t, requests)
	if request.Method != http.MethodPost || request.URLPath != "/vfs/file-status" {
		t.Errorf("Unix RC request = %s %s, want POST /vfs/file-status", request.Method, request.URLPath)
	}
}

func TestResponseBodyDisconnectMapsToMountUnreachable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "response writer cannot hijack", http.StatusInternalServerError)
			return
		}
		connection, buffer, err := hijacker.Hijack()
		if err != nil {
			return
		}
		_, _ = io.WriteString(buffer, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 128\r\nConnection: close\r\n\r\n{\"statuses\":[")
		_ = buffer.Flush()
		_ = connection.Close()
	}))
	t.Cleanup(server.Close)

	path := "/mnt/test/file.txt"
	code, stdout, _ := runVFSStatusAt(t, server.URL, path)
	if code != 1 {
		t.Errorf("mid-response disconnect: exit code = %d, want 1", code)
	}
	assertErrorJSON(t, stdout, path, "mount_unreachable")
}
