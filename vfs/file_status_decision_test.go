// Test suite for the vfs/file-status endpoint decision order
// (spec FR-006, contracts/api.md "Decision order").
//
// These tests drive the endpoint through the in-process rc
// harness, the way rc_test.go drives its siblings: the call is
// fetched from rc.Calls and its Fn invoked directly.
//
// The endpoint's handler is not implemented yet, so every
// request is answered with the whole-request empty_path
// rejection the stub answers with. The tests asserting that
// rejection pass; every test asserting another outcome of the
// decision order fails, which is the state this file is
// written to produce. The per-path outcomes of decision-order
// level 3 and the overlapping-mount-point refusal are not
// constructible at all until the mount-point registry the
// handler resolves against lands with it (FR-009/FR-014,
// T022), and are pinned as skips for that implementation.
package vfs

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/quick"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/rc"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// requireRejection asserts that the endpoint answered err with
// the whole-request rejection code, delivered as a typed
// *rc.FileStatusError carrying the HTTP status the endpoint
// answers with (contracts/api.md "Whole-Request Error
// Response").
//
// The handler stub still answers with a plain error whose
// message opens with the code, in the "code: message" shape
// FileStatusError.Error reports, so until the handler returns
// the typed error the code is read from that prefix instead.
func requireRejection(t *testing.T, err error, code string) {
	t.Helper()
	require.Error(t, err)
	var fse *rc.FileStatusError
	if errors.As(err, &fse) {
		require.Equal(t, code, fse.Code)
		require.Equal(t, http.StatusBadRequest, fse.Status)
		return
	}
	got, _, _ := strings.Cut(err.Error(), ": ")
	require.Equal(t, code, got)
}

// fileStatusRejectionCode returns the code of a whole-request
// rejection, reading it from a typed *rc.FileStatusError where
// the endpoint answers one and from the "code: message" prefix
// of the plain error the handler stub answers otherwise.
func fileStatusRejectionCode(err error) string {
	var fse *rc.FileStatusError
	if errors.As(err, &fse) {
		return fse.Code
	}
	code, _, _ := strings.Cut(err.Error(), ": ")
	return code
}

// copyFileStatusParams returns a copy of a file-status request,
// so concurrent calls never share the parameter map, which the
// endpoint may consume parameters from, as the sibling handlers'
// getVFS does.
func copyFileStatusParams(in rc.Params) rc.Params {
	out := make(rc.Params, len(in))
	for k, v := range in {
		out[k] = v
	}
	if paths, ok := in["paths"].([]string); ok {
		pathsCopy := make([]string, len(paths))
		copy(pathsCopy, paths)
		out["paths"] = pathsCopy
	}
	return out
}

// fileStatusCallOutcome calls the endpoint with a private copy
// of the request and normalizes the outcome to a comparable
// string: the rejection code for a whole-request rejection, or
// the canonical JSON of the response otherwise. Comparing
// outcomes compares the code vector.
func fileStatusCallOutcome(call *rc.Call, in rc.Params) string {
	out, err := call.Fn(context.Background(), copyFileStatusParams(in))
	if err != nil {
		return "rejection:" + fileStatusRejectionCode(err)
	}
	b, marshalErr := json.Marshal(out)
	if marshalErr != nil {
		return "unmarshalable"
	}
	return "statuses:" + string(b)
}

// withEmptyActiveMap empties the VFS active cache for the
// duration of fn, restoring it afterwards, so a request can be
// observed reaching a server with no mount active at all.
func withEmptyActiveMap(t *testing.T, fn func()) {
	t.Helper()
	activeMu.Lock()
	saved := active
	active = map[string][]*VFS{}
	activeMu.Unlock()
	defer func() {
		activeMu.Lock()
		active = saved
		activeMu.Unlock()
	}()
	fn()
}

// fileStatusRequest is a generated request for the determinism
// property: an "fs" identifier and a "paths" array of generated
// path strings.
type fileStatusRequest struct {
	Fs    string
	Paths []string
}

// TestFileStatusDecisionRequestShape covers decision-order
// level 1 (FR-006): the request shape is settled before any
// mount is consulted. An absent or empty "paths" array, or any
// entry that is not an absolute path, is the whole-request
// empty_path rejection; an entry that is not a string is the
// whole-request param_invalid rejection, decided before the
// absoluteness check.
func TestFileStatusDecisionRequestShape(t *testing.T) {
	call := rc.Calls.Get("vfs/file-status")
	require.NotNil(t, call)

	t.Run("absent paths", func(t *testing.T) {
		_, err := call.Fn(context.Background(), rc.Params{})
		requireRejection(t, err, "empty_path")
	})

	t.Run("empty paths array", func(t *testing.T) {
		_, err := call.Fn(context.Background(), rc.Params{"paths": []string{}})
		requireRejection(t, err, "empty_path")
	})

	t.Run("non-absolute entry", func(t *testing.T) {
		_, err := call.Fn(context.Background(), rc.Params{"paths": []string{"relative/file"}})
		requireRejection(t, err, "empty_path")
	})

	t.Run("absent paths with no identifier is empty_path not ambiguous_mount", func(t *testing.T) {
		_, err := call.Fn(context.Background(), rc.Params{})
		requireRejection(t, err, "empty_path")
	})

	t.Run("non-absolute entry with unmatched identifier", func(t *testing.T) {
		// The request shape is settled before the identifier is
		// looked at, so the non-absolute entry takes empty_path
		// even though the identifier matches no running mount.
		in := rc.Params{
			"fs":    "no-such-mount",
			"paths": []string{"relative/file"},
		}
		_, err := call.Fn(context.Background(), in)
		requireRejection(t, err, "empty_path")
	})

	t.Run("non-string entry", func(t *testing.T) {
		// An entry that is not a JSON string is an invalid
		// parameter value, decided before the absoluteness check,
		// so it takes param_invalid rather than empty_path.
		in := rc.Params{"paths": []any{42}}
		_, err := call.Fn(context.Background(), in)
		requireRejection(t, err, "param_invalid")
	})

	t.Run("paths is not an array", func(t *testing.T) {
		in := rc.Params{"paths": "/not/an/array"}
		_, err := call.Fn(context.Background(), in)
		requireRejection(t, err, "param_invalid")
	})
}

// TestFileStatusDecisionNoActiveMount covers the identifier
// resolution of decision-order level 2 (FR-006, FR-009,
// FR-010): an identifier matching no running mount is
// no_active_mount, whatever the paths turn out to be, and so
// is a request reaching a server with no mount active at all.
func TestFileStatusDecisionNoActiveMount(t *testing.T) {
	call := rc.Calls.Get("vfs/file-status")
	require.NotNil(t, call)

	t.Run("unmatched identifier", func(t *testing.T) {
		// The identifier is resolved before any path is examined,
		// so it takes no_active_mount even though the path is also
		// outside every mount.
		in := rc.Params{
			"fs":    "no-such-mount",
			"paths": []string{"/no/such/path"},
		}
		_, err := call.Fn(context.Background(), in)
		requireRejection(t, err, "no_active_mount")
	})

	t.Run("unmatched indexed identifier", func(t *testing.T) {
		// An indexed name naming no running mount is the
		// no-active-mount failure like any other unmatched
		// identifier (FR-010).
		in := rc.Params{
			"fs":    "mydrive[1]",
			"paths": []string{"/no/such/path"},
		}
		_, err := call.Fn(context.Background(), in)
		requireRejection(t, err, "no_active_mount")
	})

	t.Run("empty active map", func(t *testing.T) {
		withEmptyActiveMap(t, func() {
			in := rc.Params{
				"fs":    "no-such-mount",
				"paths": []string{"/no/such/path"},
			}
			_, err := call.Fn(context.Background(), in)
			requireRejection(t, err, "no_active_mount")
		})
	})
}

// TestFileStatusDecisionOutsideAnyMount covers the path
// resolution of decision-order level 3 (FR-006, FR-008,
// FR-009): a single path naming no mount is resolved against
// the registered mount points, and a path inside none of them
// is outside_any_mount. That outcome is reachable without a
// mount: a request carrying no identifier never takes
// no_active_mount, because it names no mount for none to be
// running. A parent component that is a symlink whose target
// does not exist names nothing reachable, so such a path is
// inside no mount and takes outside_any_mount too.
func TestFileStatusDecisionOutsideAnyMount(t *testing.T) {
	call := rc.Calls.Get("vfs/file-status")
	require.NotNil(t, call)

	t.Run("single path inside no registered mount point", func(t *testing.T) {
		in := rc.Params{"paths": []string{filepath.Join(t.TempDir(), "no-such-file.txt")}}
		_, err := call.Fn(context.Background(), in)
		requireRejection(t, err, "outside_any_mount")
	})

	t.Run("single path with no mount active", func(t *testing.T) {
		withEmptyActiveMap(t, func() {
			in := rc.Params{"paths": []string{"/no/such/mount/point/file.txt"}}
			_, err := call.Fn(context.Background(), in)
			requireRejection(t, err, "outside_any_mount")
		})
	})

	t.Run("dangling symlink in parent", func(t *testing.T) {
		dir := t.TempDir()
		dangling := filepath.Join(dir, "dangling-link")
		require.NoError(t, os.Symlink(filepath.Join(dir, "no-such-target"), dangling))
		in := rc.Params{"paths": []string{filepath.Join(dangling, "file.txt")}}
		_, err := call.Fn(context.Background(), in)
		requireRejection(t, err, "outside_any_mount")
	})

	t.Run("path outside a mount without a mount point", func(t *testing.T) {
		// A VFS created without a mount point, as the in-process
		// harness creates, carries no mount point, and a mount
		// point it does not carry is not reachable (FR-009/FR-014),
		// so every path is outside the mount the call resolved to.
		r, _ := newTestVFS(t)
		in := rc.Params{
			"fs":    fs.ConfigString(r.Fremote),
			"paths": []string{"/no/such/mount/point/file.txt"},
		}
		_, err := call.Fn(context.Background(), in)
		requireRejection(t, err, "outside_any_mount")
	})
}

// TestFileStatusDecisionAmbiguousMount covers the ambiguity of
// decision-order level 2 (FR-006, FR-009): a request naming at
// least one path but more than one, and no mount, is
// ambiguous_mount, whether or not any mount is active.
func TestFileStatusDecisionAmbiguousMount(t *testing.T) {
	call := rc.Calls.Get("vfs/file-status")
	require.NotNil(t, call)

	t.Run("multiple paths with no identifier", func(t *testing.T) {
		in := rc.Params{"paths": []string{"/no/such/path/one", "/no/such/path/two"}}
		_, err := call.Fn(context.Background(), in)
		requireRejection(t, err, "ambiguous_mount")
	})

	t.Run("multiple paths with no identifier and no mount active", func(t *testing.T) {
		withEmptyActiveMap(t, func() {
			in := rc.Params{"paths": []string{"/no/such/path/one", "/no/such/path/two"}}
			_, err := call.Fn(context.Background(), in)
			requireRejection(t, err, "ambiguous_mount")
		})
	})

	t.Run("path inside overlapping mount points", func(t *testing.T) {
		// A path lying inside more than one registered mount point
		// is refused as ambiguous_mount, never resolved to either
		// mount, with no longest-prefix or first-registered
		// tiebreak (FR-009). Constructing it needs two mounts with
		// overlapping registered mount points, which the
		// mount-point registry the handler resolves against
		// provides (FR-009/FR-014, T022).
		t.Skip("requires two mounts with overlapping registered mount points (FR-009, FR-014, T022)")
	})
}

// TestFileStatusDecisionPerPathClassification covers the
// per-path outcomes of decision-order level 3 (FR-006,
// FR-032): not_found, directory_not_file, the non-regular-file
// local fact with not_measurable byte quantities, and
// unaccountable_path.
//
// Each is reached only through a mount created with a mount
// point (FR-009/FR-014): the in-process harness creates VFSes
// without one, and no existing symbol registers a mount point,
// so these outcomes are not constructible until the
// mount-point registry the handler resolves against lands with
// it (T022). Each skip is pinned here for that implementation
// to replace with the real test.
func TestFileStatusDecisionPerPathClassification(t *testing.T) {
	t.Run("not_found", func(t *testing.T) {
		t.Skip("requires a mount with a registered mount point holding entries (FR-009, FR-014, T022)")
	})

	t.Run("directory_not_file", func(t *testing.T) {
		t.Skip("requires a mount with a registered mount point holding a directory (FR-009, FR-014, T022)")
	})

	t.Run("not_regular_file with not_measurable bytes", func(t *testing.T) {
		// A path whose final component is not a regular file
		// reports the non-regular-file value of the local fact
		// with not_measurable for all three byte quantities
		// (FR-032, contracts/api.md "Byte Info Fields").
		t.Skip("requires a mount started to show links with a registered mount point (FR-009, FR-014, FR-032, T022)")
	})

	t.Run("unaccountable_path", func(t *testing.T) {
		t.Skip("requires a mount with a registered mount point holding no listing of the parent (FR-009, FR-014, T022)")
	})
}

// TestFileStatusDecisionMountUnreachableNotEndpointCode pins
// that mount_unreachable is never an endpoint outcome: it is
// produced by the command when no server is reached at all,
// since no request arrives to be answered (FR-006).
func TestFileStatusDecisionMountUnreachableNotEndpointCode(t *testing.T) {
	call := rc.Calls.Get("vfs/file-status")
	require.NotNil(t, call)

	requests := []rc.Params{
		{},
		{"paths": []string{}},
		{"paths": []string{"relative/file"}},
		{"paths": []string{"/no/such/path"}},
		{"paths": []string{"/no/such/path/one", "/no/such/path/two"}},
		{"fs": "no-such-mount", "paths": []string{"/no/such/path"}},
		{"paths": []any{42}},
	}
	for _, in := range requests {
		_, err := call.Fn(context.Background(), copyFileStatusParams(in))
		if err == nil {
			continue
		}
		require.NotEqual(t, "mount_unreachable", fileStatusRejectionCode(err), "mount_unreachable is a command-layer outcome, not an endpoint rejection")
	}
}

// TestFileStatusDecisionDeterminism checks the determinism
// invariant of the decision order (FR-006): for a fixed
// request, repeated calls yield the identical code vector,
// whatever the request shape.
func TestFileStatusDecisionDeterminism(t *testing.T) {
	call := rc.Calls.Get("vfs/file-status")
	require.NotNil(t, call)

	check := func(req fileStatusRequest) bool {
		in := rc.Params{
			"fs":    req.Fs,
			"paths": req.Paths,
		}
		first := fileStatusCallOutcome(call, in)
		for i := 0; i < 5; i++ {
			if fileStatusCallOutcome(call, in) != first {
				return false
			}
		}
		return true
	}
	require.NoError(t, quick.Check(check, &quick.Config{MaxCount: 50}))
}

// TestFileStatusDecisionDeterminismConcurrent checks the same
// determinism invariant under concurrent interleaves: calls for
// one fixed request, interleaved across goroutines, yield the
// identical code vector (FR-016, contracts/api.md "Concurrent
// Requests").
func TestFileStatusDecisionDeterminismConcurrent(t *testing.T) {
	call := rc.Calls.Get("vfs/file-status")
	require.NotNil(t, call)

	dir := t.TempDir()
	dangling := filepath.Join(dir, "dangling-link")
	require.NoError(t, os.Symlink(filepath.Join(dir, "no-such-target"), dangling))

	// One fixed request per decision-order level, so the
	// interleaves cover every outcome the endpoint can answer
	// today and every rejection it originates.
	requests := []rc.Params{
		{},
		{"paths": []string{}},
		{"paths": []string{"relative/file"}},
		{"paths": []string{"relative/file"}, "fs": "no-such-mount"},
		{"paths": []string{filepath.Join(dir, "no-such-file.txt")}},
		{"paths": []string{filepath.Join(dangling, "file.txt")}},
		{"paths": []string{"/no/such/path/one", "/no/such/path/two"}},
		{"fs": "no-such-mount", "paths": []string{"/no/such/path/one", "/no/such/path/two"}},
		{"fs": "mydrive[1]", "paths": []string{"/no/such/path"}},
		{"paths": []any{42}},
	}

	const goroutines = 8
	const callsPerGoroutine = 16
	for _, request := range requests {
		results := make(chan string, goroutines*callsPerGoroutine)
		var wg sync.WaitGroup
		for g := 0; g < goroutines; g++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; i < callsPerGoroutine; i++ {
					results <- fileStatusCallOutcome(call, request)
				}
			}()
		}
		wg.Wait()
		close(results)
		var first string
		for got := range results {
			if first == "" {
				first = got
			}
			assert.Equal(t, first, got, "concurrent interleaves of one request must yield identical outcomes")
		}
	}
}
