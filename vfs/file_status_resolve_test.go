package vfs

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"testing/quick"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/rc"
	"github.com/rclone/rclone/vfs/vfscommon"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestFileStatusResolveCleanIdempotent checks the idempotence the
// FR-009 normalization relies on: cleaning an already clean path
// is a fixpoint, so a request entry normalized once can be
// compared against a registered mount point without being cleaned
// a second time.
func TestFileStatusResolveCleanIdempotent(t *testing.T) {
	cleanIsIdempotent := func(p string) bool {
		return filepath.Clean(filepath.Clean(p)) == filepath.Clean(p)
	}
	if err := quick.Check(cleanIsIdempotent, nil); err != nil {
		t.Error(err)
	}
}

// TestFileStatusResolveCleanCollapse checks that lexical
// normalization resolves the dot segments and the duplicate
// separators FR-009 names, under the separator, drive-letter and
// UNC rules of the host the mount runs on.
func TestFileStatusResolveCleanCollapse(t *testing.T) {
	sep := string(filepath.Separator)
	for _, tc := range []struct {
		in   string
		want string
	}{
		{"/mnt/x/../y", sep + "mnt" + sep + "y"},
		{"/mnt//y", sep + "mnt" + sep + "y"},
		{"/mnt/./y", sep + "mnt" + sep + "y"},
		{"/mnt/a/./b/../c", sep + "mnt" + sep + "a" + sep + "c"},
		{sep + "mnt" + sep + sep + "x", sep + "mnt" + sep + "x"},
		{"mnt/x/../y", "mnt" + sep + "y"},
	} {
		assert.Equal(t, tc.want, filepath.Clean(tc.in))
	}

	// A drive letter and a UNC share are roots of their own on
	// Windows, and normalization resolves against them like any
	// other component.
	if runtime.GOOS == "windows" {
		assert.Equal(t, `C:\mnt`, filepath.Clean(`C:\mnt\.\`))
		assert.Equal(t, `C:\y`, filepath.Clean(`C:\mnt\..\y`))
		assert.Equal(t, `\\server\y`, filepath.Clean(`\\server\share\..\y`))
	}
}

// TestFileStatusResolveRelativeDetection checks the
// absolute/relative distinction FR-009 draws: a relative entry is
// refused as empty_path rather than resolved against the caller's
// working directory, so the endpoint has to tell the two apart
// with the host's own IsAbs.
func TestFileStatusResolveRelativeDetection(t *testing.T) {
	sep := string(filepath.Separator)
	assert.False(t, filepath.IsAbs(""))
	assert.False(t, filepath.IsAbs("mnt/y"))
	assert.False(t, filepath.IsAbs("mnt"+sep+"y"))
	if runtime.GOOS == "windows" {
		assert.True(t, filepath.IsAbs(`C:\mnt\y`))
		assert.True(t, filepath.IsAbs(`\\server\share\y`))
	} else {
		assert.True(t, filepath.IsAbs(sep+"mnt"+sep+"y"))
	}
}

// TestFileStatusResolveNonAbsoluteEntry checks that an entry
// which is not an absolute path refuses the whole request with
// the empty_path rejection (FR-013): the rejection is decided on
// the request's shape, before any mount is consulted and before
// any identifier is looked at (FR-006 decision order 1), so a
// request carrying no "fs" at all is empty_path and never
// no_active_mount, and one bad entry costs the caller every
// other answer rather than just its own.
func TestFileStatusResolveNonAbsoluteEntry(t *testing.T) {
	r, _, call := rcNewRun(t, "vfs/file-status")
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

// TestFileStatusResolveMountPointReport checks the one change
// FR-014 grants this work: the report of the mounts currently
// running keeps its "vfses" array unchanged and gains a
// "mountPoints" object of its own, keyed by the names the array
// returns so a caller pairs a mount point with a mount by name
// and never by position, carrying null for a VFS created without
// a mount point — which every VFS a test process creates is.
func TestFileStatusResolveMountPointReport(t *testing.T) {
	r, _, call := rcNewRun(t, "vfs/list")

	out, err := call.Fn(context.Background(), nil)
	require.NoError(t, err)

	vfses, ok := out["vfses"].([]string)
	require.True(t, ok, "the report must keep its vfses array")
	assert.Contains(t, vfses, fs.ConfigString(r.Fremote))

	mountPoints, ok := out["mountPoints"]
	require.True(t, ok, "the report must carry a mountPoints key")
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

// TestFileStatusResolveOutsideAnyMount checks that a single
// absolute path lying inside no registered mount point is the
// outside-any-mount failure (FR-008): it is a per-path result
// inside the 200 response, not a whole-request failure, because
// the request's shape is well-formed and its identifier, where it
// carries one, names an active mount — a path inside no
// registered mount point resolves to no mount rather than to one
// nothing knows about, and never to no_active_mount.
func TestFileStatusResolveOutsideAnyMount(t *testing.T) {
	r, _, call := rcNewRun(t, "vfs/file-status")

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

func TestFileStatusResolveParentSymlinkUsesTargetMount(t *testing.T) {
	r, vfs, call := rcNewRun(t, "vfs/file-status")
	r.WriteObject(context.Background(), "file.txt", "content", time.Now())

	mountPoint := t.TempDir()
	linkParent := filepath.Join(t.TempDir(), "mount-alias")
	if err := os.Symlink(mountPoint, linkParent); err != nil {
		t.Skipf("cannot create directory symlink: %v", err)
	}
	RegisterMountPointForVFS(vfs, mountPoint)
	t.Cleanup(func() { UnregisterMountPointForVFS(vfs) })

	result := fileStatusResolvePath(t, call, filepath.Join(linkParent, "file.txt"))
	assert.NotContains(t, result, "error", "parent links are resolved to the mount serving their target")
}

func TestFileStatusResolveDanglingParentSymlinkIsOutsideAnyMount(t *testing.T) {
	_, vfs, call := rcNewRun(t, "vfs/file-status")
	mountPoint := t.TempDir()
	linkPath := filepath.Join(mountPoint, "dangling")
	danglingTarget := filepath.Join(t.TempDir(), "target-does-not-exist")
	if err := os.Symlink(danglingTarget, linkPath); err != nil {
		t.Skipf("cannot create directory symlink: %v", err)
	}
	RegisterMountPointForVFS(vfs, mountPoint)
	t.Cleanup(func() { UnregisterMountPointForVFS(vfs) })

	result := fileStatusResolvePath(t, call, filepath.Join(linkPath, "file.txt"))
	perPathError, ok := result["error"].(map[string]any)
	require.True(t, ok, "a dangling parent link names no reachable mount path")
	assert.Equal(t, "outside_any_mount", perPathError["code"])
}

func TestFileStatusResolveFinalSymlinkIsNotFollowed(t *testing.T) {
	opt := vfscommon.Opt
	opt.Links = true
	r, vfs, call := newFileStatusStateRun(t, &opt)

	r.WriteObject(context.Background(), "target.txt", "target contents", time.Now())
	r.WriteObject(context.Background(), "link.rclonelink", "target.txt", time.Now())
	mountPoint := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(mountPoint, "target.txt"), []byte("host target"), 0600))
	if err := os.Symlink("target.txt", filepath.Join(mountPoint, "link")); err != nil {
		t.Skipf("cannot create file symlink: %v", err)
	}
	RegisterMountPointForVFS(vfs, mountPoint)
	t.Cleanup(func() { UnregisterMountPointForVFS(vfs) })

	result := fileStatusResolvePath(t, call, filepath.Join(mountPoint, "link"))
	assert.Equal(t, "not_regular_file", result["local"], "the final component is the VFS symlink itself")
}

func TestFileStatusResolveOverlappingMountsAreAmbiguous(t *testing.T) {
	r, vfs1, call := rcNewRun(t, "vfs/file-status")
	opt2 := vfs1.Opt
	opt2.NoModTime = !opt2.NoModTime
	vfs2 := New(context.Background(), r.Fremote, &opt2)
	t.Cleanup(func() { cleanupVFS(t, vfs2) })

	mountPoint1 := t.TempDir()
	mountPoint2 := filepath.Join(mountPoint1, "nested")
	require.NoError(t, os.Mkdir(mountPoint2, 0700))
	RegisterMountPointForVFS(vfs1, mountPoint1)
	RegisterMountPointForVFS(vfs2, mountPoint2)
	t.Cleanup(func() {
		UnregisterMountPointForVFS(vfs2)
		UnregisterMountPointForVFS(vfs1)
	})

	result := fileStatusResolvePath(t, call, filepath.Join(mountPoint2, "file.txt"))
	perPathError, ok := result["error"].(map[string]any)
	require.True(t, ok, "overlapping registrations do not select either mount")
	assert.Equal(t, "ambiguous_mount", perPathError["code"])
}

func TestFileStatusResolveComponentBoundary(t *testing.T) {
	_, vfs, call := rcNewRun(t, "vfs/file-status")
	parent := t.TempDir()
	mountPoint := filepath.Join(parent, "mount")
	sibling := filepath.Join(parent, "mount-extra")
	require.NoError(t, os.Mkdir(mountPoint, 0700))
	require.NoError(t, os.Mkdir(sibling, 0700))
	RegisterMountPointForVFS(vfs, mountPoint)
	t.Cleanup(func() { UnregisterMountPointForVFS(vfs) })

	result := fileStatusResolvePath(t, call, filepath.Join(sibling, "file.txt"))
	perPathError, ok := result["error"].(map[string]any)
	require.True(t, ok, "a sibling component sharing the mount-point prefix is outside")
	assert.Equal(t, "outside_any_mount", perPathError["code"])
}

func TestFileStatusResolveWindowsCaseInsensitive(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows path matching is platform-specific")
	}
	r, vfs, call := rcNewRun(t, "vfs/file-status")
	r.WriteObject(context.Background(), "file.txt", "content", time.Now())
	mountPoint := t.TempDir()
	RegisterMountPointForVFS(vfs, mountPoint)
	t.Cleanup(func() { UnregisterMountPointForVFS(vfs) })

	requestPath := filepath.Join(togglePathCase(mountPoint), "file.txt")
	result := fileStatusResolvePath(t, call, requestPath)
	assert.NotContains(t, result, "error", "Windows mount matching is case-insensitive")
}

func togglePathCase(path string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z':
			return r - 'a' + 'A'
		case r >= 'A' && r <= 'Z':
			return r - 'A' + 'a'
		default:
			return r
		}
	}, path)
}

func fileStatusResolvePath(t *testing.T, call *rc.Call, path string) map[string]any {
	t.Helper()
	out, err := call.Fn(context.Background(), rc.Params{"paths": []string{path}})
	require.NoError(t, err)
	statuses := decodeStatuses(t, out)
	require.Len(t, statuses, 1)
	return statuses[0]
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
