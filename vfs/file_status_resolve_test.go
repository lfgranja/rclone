package vfs

import (
	"context"
	"encoding/json"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"testing/quick"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/rc"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestFileStatusResolveCleanIdempotent checks that vfs/file-status
// normalizes paths idempotently: passing an already normalized path
// back to the endpoint returns the same path unchanged.
func TestFileStatusResolveCleanIdempotent(t *testing.T) {
	r, _, call := rcNewRun(t, "vfs/file-status")
	require.NoError(t, r.Fremote.Mkdir(context.Background(), ""))

	root := string(filepath.Separator)
	if runtime.GOOS == "windows" {
		root = `C:\`
	}

	cleanIsIdempotent := func(tail string) bool {
		if strings.ContainsRune(tail, 0) {
			return true
		}
		if runtime.GOOS == "windows" {
			tail = strings.ReplaceAll(tail, ":", "")
		}
		inPath := root + tail
		out1, err := call.Fn(context.Background(), rc.Params{"paths": []any{inPath}})
		if err != nil {
			return false
		}
		statuses1 := decodeStatuses(t, out1)
		if len(statuses1) != 1 {
			return false
		}
		got1, ok := statuses1[0]["path"].(string)
		if !ok {
			return false
		}
		out2, err := call.Fn(context.Background(), rc.Params{"paths": []any{got1}})
		if err != nil {
			return false
		}
		statuses2 := decodeStatuses(t, out2)
		if len(statuses2) != 1 {
			return false
		}
		got2, _ := statuses2[0]["path"].(string)
		return got1 == filepath.Clean(inPath) && got2 == got1
	}
	if err := quick.Check(cleanIsIdempotent, nil); err != nil {
		t.Error(err)
	}
}

// TestFileStatusResolveCleanCollapse checks that vfs/file-status
// collapses dot segments and duplicate separators under host path rules.
func TestFileStatusResolveCleanCollapse(t *testing.T) {
	r, _, call := rcNewRun(t, "vfs/file-status")
	require.NoError(t, r.Fremote.Mkdir(context.Background(), ""))

	var cases []struct {
		in   string
		want string
	}
	if runtime.GOOS == "windows" {
		cases = []struct {
			in   string
			want string
		}{
			{`C:\mnt\x\..\y`, `C:\mnt\y`},
			{`C:\mnt\\y`, `C:\mnt\y`},
			{`C:\mnt\.\y`, `C:\mnt\y`},
			{`C:\mnt\a\.\b\..\c`, `C:\mnt\a\c`},
			{`C:\mnt\.\`, `C:\mnt`},
			{`C:\mnt\..\y`, `C:\y`},
			{`\\server\share\..\y`, `\\server\y`},
		}
	} else {
		cases = []struct {
			in   string
			want string
		}{
			{"/mnt/x/../y", "/mnt/y"},
			{"/mnt//y", "/mnt/y"},
			{"/mnt/./y", "/mnt/y"},
			{"/mnt/a/./b/../c", "/mnt/a/c"},
			{"/mnt//x", "/mnt/x"},
		}
	}
	for _, tc := range cases {
		out, err := call.Fn(context.Background(), rc.Params{"paths": []any{tc.in}})
		require.NoError(t, err)
		statuses := decodeStatuses(t, out)
		require.Len(t, statuses, 1)
		assert.Equal(t, tc.want, statuses[0]["path"])
	}
}

// TestFileStatusResolveRelativeDetection checks that vfs/file-status
// rejects empty and relative paths with empty_path while accepting
// host-absolute paths.
func TestFileStatusResolveRelativeDetection(t *testing.T) {
	r, _, call := rcNewRun(t, "vfs/file-status")
	require.NoError(t, r.Fremote.Mkdir(context.Background(), ""))

	sep := string(filepath.Separator)
	for _, rel := range []string{"", "mnt/y", "mnt" + sep + "y"} {
		out, err := call.Fn(context.Background(), rc.Params{"paths": []any{rel}})
		require.Error(t, err)
		assert.Nil(t, out)
		var fse *rc.FileStatusError
		require.ErrorAs(t, err, &fse)
		assert.Equal(t, "empty_path", fse.Code)
		assert.Equal(t, 400, fse.Status)
	}

	absCases := []string{sep + "mnt" + sep + "y"}
	if runtime.GOOS == "windows" {
		absCases = []string{`C:\mnt\y`, `\\server\share\y`}
	}
	for _, abs := range absCases {
		out, err := call.Fn(context.Background(), rc.Params{"paths": []any{abs}})
		require.NoError(t, err)
		statuses := decodeStatuses(t, out)
		require.Len(t, statuses, 1)
		assert.Equal(t, abs, statuses[0]["path"])
	}
}

// TestFileStatusResolveNonAbsoluteEntry checks that any non-absolute
// entry in "paths" rejects the entire request with empty_path (HTTP 400).
func TestFileStatusResolveNonAbsoluteEntry(t *testing.T) {
	r, _, call := rcNewRun(t, "vfs/file-status")
	require.NoError(t, r.Fremote.Mkdir(context.Background(), ""))
	sep := string(filepath.Separator)
	absFile := sep + "mnt" + sep + "file"
	if runtime.GOOS == "windows" {
		absFile = `C:\mnt\file`
	}

	for _, in := range []rc.Params{
		// No identifier: the shape is decided before the
		// identifier is looked at.
		{"paths": []any{"mnt/relative/file"}},
		// An active mount named: the shape still wins.
		{"fs": fs.ConfigString(r.Fremote), "paths": []any{"mnt/relative/file"}},
		// One non-absolute entry among absolute ones refuses
		// the whole request.
		{"fs": fs.ConfigString(r.Fremote), "paths": []any{absFile, "mnt/relative/file"}},
		// An empty entry names no file and is the same error.
		{"paths": []any{""}},
	} {
		out, err := call.Fn(context.Background(), in)
		require.Error(t, err)
		assert.Nil(t, out)
		var fse *rc.FileStatusError
		require.ErrorAs(t, err, &fse)
		assert.Equal(t, "empty_path", fse.Code)
		assert.Equal(t, 400, fse.Status)
	}
}

// TestFileStatusResolveMountPointReport checks that vfs/list preserves
// "vfses" and includes a "mountPoints" map keyed by VFS name, with nil
// for unmounted VFS instances.
func TestFileStatusResolveMountPointReport(t *testing.T) {
	r, _, call := rcNewRun(t, "vfs/list")
	require.NoError(t, r.Fremote.Mkdir(context.Background(), ""))

	out, err := call.Fn(context.Background(), nil)
	require.NoError(t, err)

	vfses, ok := out["vfses"].([]string)
	require.True(t, ok, "the report must keep its vfses array")
	assert.Contains(t, vfses, fs.ConfigString(r.Fremote))

	mountPoints, ok := out["mountPoints"]
	if !ok {
		t.Skip("vfs/list mountPoints field is added with mount-point registration")
	}
	points, ok := mountPointsObject(mountPoints)
	require.True(t, ok, "mountPoints must be an object keyed by mount name")

	// Every name the array returns appears in mountPoints, and a
	// VFS created without a mount point reports null there.
	require.Equal(t, len(vfses), len(points))
	for _, name := range vfses {
		point, ok := points[name]
		require.True(t, ok, "mountPoints must carry an entry for %q", name)
		assert.Nil(t, point, "a VFS created without a mount point reports a null mount point")
	}
}

// TestFileStatusResolveOutsideAnyMount checks that an absolute path
// outside every registered mount point produces a per-path
// "outside_any_mount" error inside a 200 response.
func TestFileStatusResolveOutsideAnyMount(t *testing.T) {
	r, _, call := rcNewRun(t, "vfs/file-status")
	require.NoError(t, r.Fremote.Mkdir(context.Background(), ""))

	// An absolute path on the mount host that lies inside no
	// registered mount point: no VFS in this process was created
	// with one. The path is already clean, so the result echoes
	// it unchanged whichever of the two forms the endpoint
	// repeats.
	outside := filepath.Join(string(filepath.Separator), "no", "such", "mount", "point", "file")
	if runtime.GOOS == "windows" {
		outside = `C:\no\such\mount\point\file`
	}

	for _, in := range []rc.Params{
		// No identifier: the single path is resolved against
		// the registered mount points and lies inside none.
		{"paths": []any{outside}},
		// An identifier naming the active mount: the path is
		// then outside that mount's mount point.
		{"fs": fs.ConfigString(r.Fremote), "paths": []any{outside}},
	} {
		out, err := call.Fn(context.Background(), in)
		require.NoError(t, err)

		statuses := decodeStatuses(t, out)
		require.Len(t, statuses, 1)
		result := statuses[0]
		assert.Equal(t, outside, result["path"])
		perPathError, ok := result["error"].(map[string]any)
		require.True(t, ok, "the result must carry the per-path error object")
		assert.Equal(t, "outside_any_mount", perPathError["code"])
	}
}

// mountPointsObject adapts the "mountPoints" value of the report
// of the mounts currently running, which the contract fixes as an
// object keyed by mount name, to a plain map whichever of the two
// map types the report carries it as.
func mountPointsObject(mountPoints any) (map[string]any, bool) {
	switch points := mountPoints.(type) {
	case rc.Params:
		return points, true
	case map[string]any:
		return points, true
	}
	return nil, false
}

// decodeStatuses decodes the "statuses" array of a file-status
// response through JSON, so the test reads the response shape the
// contract fixes rather than the Go types an implementation
// happens to carry it in.
func decodeStatuses(t *testing.T, out rc.Params) []map[string]any {
	t.Helper()
	b, err := json.Marshal(out)
	require.NoError(t, err)
	var response struct {
		Statuses []map[string]any `json:"statuses"`
	}
	require.NoError(t, json.Unmarshal(b, &response))
	return response.Statuses
}
