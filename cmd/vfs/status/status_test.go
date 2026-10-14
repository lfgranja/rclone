package status

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
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

// TestUsageError checks that an invocation with no path argument or with
// more than one path argument is a usage error: exit status 2, the
// project's standard usage text on standard error, and nothing on standard
// output (spec commands/cli.md, "Error Handling").
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
// nothing on standard output (spec commands/cli.md, "Exit Status Codes").
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
// shape of spec commands/cli.md, "Output Format": either a state object
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
// output and exits 0 (spec commands/cli.md, "Help").
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
}

// TestExitCodeAndOutputContract checks the contract over a range of argv
// shapes: every exit status is one of the command's declared codes (spec
// commands/cli.md, "Exit Status Codes": 0, 1, 2, 11 or 12), and whenever
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
