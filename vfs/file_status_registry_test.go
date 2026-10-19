package vfs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"testing/quick"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/rc"
	"github.com/rclone/rclone/fstest"
	"github.com/rclone/rclone/vfs/vfscommon"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newFileStatusRegistryVFS(t *testing.T, f fs.Fs, opt *vfscommon.Options) (*VFS, func()) {
	t.Helper()
	vfs := New(context.Background(), f, opt)
	shutDown := false
	t.Cleanup(func() {
		vfs.WaitForWriters(waitForWritersDelay)
		require.NoError(t, vfs.CleanUp())
		if !shutDown {
			vfs.Shutdown()
		}
	})
	return vfs, func() {
		if !shutDown {
			vfs.Shutdown()
			shutDown = true
		}
	}
}

func fileStatusRegistrySnapshot(t *testing.T, call *rc.Call) ([]string, map[string]any) {
	t.Helper()
	out, err := call.Fn(context.Background(), nil)
	require.NoError(t, err)
	names, ok := out["vfses"].([]string)
	require.True(t, ok, "vfs/list must return its vfses array")
	points, ok := mountPointsObject(out["mountPoints"])
	require.True(t, ok, "vfs/list must return its mountPoints object")
	return names, points
}

func fileStatusIdentifierForMountPoint(t *testing.T, names []string, points map[string]any, mountPoint string) string {
	t.Helper()
	for name, value := range points {
		if value == mountPoint {
			require.Contains(t, names, name, "mount-point keys must be names emitted by vfs/list")
			return name
		}
	}
	require.FailNow(t, "vfs/list did not report mount point %q", mountPoint)
	return ""
}

func fileStatusRegistryResult(t *testing.T, call *rc.Call, mountPoint, identifier string) map[string]any {
	t.Helper()
	result, err := fileStatusRegistryQuery(t, call, mountPoint, identifier)
	require.NoError(t, err)
	return result
}

func fileStatusRegistryQuery(t *testing.T, call *rc.Call, mountPoint, identifier string) (map[string]any, error) {
	t.Helper()
	in := rc.Params{"paths": []string{filepath.Join(mountPoint, "file.txt")}}
	if identifier != "" {
		in["fs"] = identifier
	}
	out, err := call.Fn(context.Background(), in)
	if err != nil {
		return nil, err
	}
	statuses := decodeStatuses(t, out)
	require.Len(t, statuses, 1)
	return statuses[0], nil
}

func TestFileStatusRegistryIndexedNamesAndTeardown(t *testing.T) {
	r := fstest.NewRun(t)
	r.WriteObject(context.Background(), "file.txt", "content", time.Now())

	vfs1, shutdown1 := newFileStatusRegistryVFS(t, r.Fremote, nil)
	opt2 := vfs1.Opt
	opt2.NoModTime = !opt2.NoModTime
	vfs2, shutdown2 := newFileStatusRegistryVFS(t, r.Fremote, &opt2)
	require.NotSame(t, vfs1, vfs2, "different VFS options must keep shared-config mounts distinct")

	configName := fs.ConfigString(r.Fremote)
	mountPoint1 := filepath.Join(t.TempDir(), "mount-one")
	mountPoint2 := filepath.Join(t.TempDir(), "mount-two")
	RegisterMountPointForVFS(vfs1, mountPoint1)
	RegisterMountPointForVFS(vfs2, mountPoint2)
	cleanupNames := map[string]struct{}{}
	for name, point := range GetAllMountPoints() {
		if point == mountPoint1 || point == mountPoint2 {
			cleanupNames[name] = struct{}{}
		}
	}
	t.Cleanup(func() {
		for name := range cleanupNames {
			UnregisterMountPoint(name)
		}
		UnregisterMountPointForVFS(vfs1)
		UnregisterMountPointForVFS(vfs2)
	})

	listCall := rc.Calls.Get("vfs/list")
	statusCall := rc.Calls.Get("vfs/file-status")
	require.NotNil(t, listCall)
	require.NotNil(t, statusCall)

	names, points := fileStatusRegistrySnapshot(t, listCall)
	id1 := fileStatusIdentifierForMountPoint(t, names, points, mountPoint1)
	id2 := fileStatusIdentifierForMountPoint(t, names, points, mountPoint2)
	assert.NotEqual(t, id1, id2)
	assert.NotContains(t, fileStatusRegistryResult(t, statusCall, mountPoint1, ""), "error")
	assert.NotContains(t, fileStatusRegistryResult(t, statusCall, mountPoint2, ""), "error")
	result, err := fileStatusRegistryQuery(t, statusCall, mountPoint2, id2)
	assert.NoError(t, err, "an identifier emitted by vfs/list must resolve the indexed VFS")
	if err == nil {
		assert.NotContains(t, result, "error")
	}

	// Removing index zero changes the surviving VFS's reported name. Its
	// registered path must move with it instead of remaining under a stale suffix.
	UnregisterMountPointForVFS(vfs1)
	shutdown1()

	names, points = fileStatusRegistrySnapshot(t, listCall)
	assert.Contains(t, names, configName)
	assert.NotContains(t, names, configName+"[1]", "a sole remaining VFS is reported without a stale index")
	assert.Equal(t, mountPoint2, points[configName], "the surviving mount point must be reindexed with its VFS")
	assert.NotContains(t, fileStatusRegistryResult(t, statusCall, mountPoint2, ""), "error")
	survivorID := ""
	for name, point := range points {
		if point == mountPoint2 {
			survivorID = name
			break
		}
	}
	if assert.NotEmpty(t, survivorID, "vfs/list must report the surviving mount point") {
		result, err = fileStatusRegistryQuery(t, statusCall, mountPoint2, survivorID)
		assert.NoError(t, err)
		if err == nil {
			assert.NotContains(t, result, "error")
		}
	}

	UnregisterMountPointForVFS(vfs2)
	shutdown2()
}

func TestFileStatusRegistryDeduplicatedMountKeepsSingleReportedPoint(t *testing.T) {
	r := fstest.NewRun(t)
	r.WriteObject(context.Background(), "file.txt", "content", time.Now())

	vfs1, shutdown1 := newFileStatusRegistryVFS(t, r.Fremote, nil)
	vfs2, shutdown2 := newFileStatusRegistryVFS(t, r.Fremote, &vfs1.Opt)
	require.Same(t, vfs1, vfs2, "option-identical mounts share the active VFS")

	configName := fs.ConfigString(r.Fremote)
	mountPoint1 := filepath.Join(t.TempDir(), "first-mount")
	mountPoint2 := filepath.Join(t.TempDir(), "second-mount")
	RegisterMountPointForVFS(vfs1, mountPoint1)
	listCall := rc.Calls.Get("vfs/list")
	statusCall := rc.Calls.Get("vfs/file-status")
	require.NotNil(t, listCall)
	require.NotNil(t, statusCall)
	t.Cleanup(func() {
		for name, point := range GetAllMountPoints() {
			if point == mountPoint1 || point == mountPoint2 {
				UnregisterMountPoint(name)
			}
		}
		UnregisterMountPointForVFS(vfs1)
	})

	namesBefore, points := fileStatusRegistrySnapshot(t, listCall)
	require.Equal(t, []string{configName}, namesBefore)
	require.Len(t, points, 1)
	require.Equal(t, mountPoint1, points[configName])

	RegisterMountPointForVFS(vfs2, mountPoint2)
	names, points := fileStatusRegistrySnapshot(t, listCall)
	assert.Equal(t, namesBefore, names, "a deduplicated VFS must not add a vfs/list name")
	require.Len(t, points, 1, "mountPoints remains one value per reported VFS name")
	reportedPoint, ok := points[configName].(string)
	require.True(t, ok, "a mounted VFS reports one mount-point string")
	assert.Contains(t, []string{mountPoint1, mountPoint2}, reportedPoint)
	assert.NotContains(t, fileStatusRegistryResult(t, statusCall, mountPoint1, ""), "error",
		"the private registry must retain the first live path")
	assert.NotContains(t, fileStatusRegistryResult(t, statusCall, mountPoint2, ""), "error",
		"the private registry must resolve the second live path to the shared VFS")

	// The second mount releases one reference to the shared VFS. The sole
	// reported point must be the first mount that is still live.
	UnregisterMountPointForVFS(vfs2)
	shutdown2()
	names, points = fileStatusRegistrySnapshot(t, listCall)
	assert.Equal(t, namesBefore, names)
	require.Len(t, points, 1)
	assert.Equal(t, mountPoint1, points[configName], "unregistering one shared reference must not leave a stale point")
	assert.NotContains(t, fileStatusRegistryResult(t, statusCall, mountPoint1, ""), "error")
	secondPath := fileStatusRegistryResult(t, statusCall, mountPoint2, "")
	perPathError, ok := secondPath["error"].(map[string]any)
	require.True(t, ok, "unmounting the second reference must remove its path")
	assert.Equal(t, "outside_any_mount", perPathError["code"])

	UnregisterMountPointForVFS(vfs1)
	shutdown1()
	names, points = fileStatusRegistrySnapshot(t, listCall)
	assert.NotContains(t, names, configName)
	assert.NotContains(t, points, configName)
}

func TestFileStatusRegistryNullMountPointDoesNotBecomeHostPath(t *testing.T) {
	r, vfs, listCall := rcNewRun(t, "vfs/list")
	statusCall := rc.Calls.Get("vfs/file-status")
	require.NotNil(t, statusCall)
	configName := fs.ConfigString(r.Fremote)

	names, points := fileStatusRegistrySnapshot(t, listCall)
	require.Contains(t, names, configName)
	require.Contains(t, points, configName)
	require.Nil(t, points[configName])
	assert.Empty(t, vfs.MountPoint)

	hostPath := filepath.Join(t.TempDir(), "remote-file")
	result := fileStatusRegistryResult(t, statusCall, filepath.Dir(hostPath), configName)
	perPathError, ok := result["error"].(map[string]any)
	require.True(t, ok, "a VFS without a host mount point cannot answer a host path")
	assert.Equal(t, "outside_any_mount", perPathError["code"])
	assert.Empty(t, vfs.MountPoint, "status lookup must not synthesize a host mount point")

	_, points = fileStatusRegistrySnapshot(t, listCall)
	require.Contains(t, points, configName)
	assert.Nil(t, points[configName], "vfs/list must keep the mount point null")
}

func TestFileStatusRegistryCompletenessProperty(t *testing.T) {
	r, vfs, listCall := rcNewRun(t, "vfs/list")
	statusCall := rc.Calls.Get("vfs/file-status")
	require.NotNil(t, statusCall)
	r.WriteObject(context.Background(), "file.txt", "content", time.Now())
	rootDir, err := vfs.Root()
	require.NoError(t, err)
	entries, err := rootDir.ReadDirAll()
	require.NoError(t, err)
	listedFile := false
	for _, entry := range entries {
		if entry.Name() == "file.txt" {
			listedFile = true
			break
		}
	}
	require.True(t, listedFile, "the test file must be present in the VFS listing")
	t.Cleanup(func() { UnregisterMountPointForVFS(vfs) })
	root := t.TempDir()

	complete := func(input string) bool {
		digest := sha256.Sum256([]byte(input))
		mountPoint := filepath.Join(root, "mount-"+hex.EncodeToString(digest[:]))
		if err := os.MkdirAll(mountPoint, 0700); err != nil {
			return false
		}
		RegisterMountPointForVFS(vfs, mountPoint)

		names, points := fileStatusRegistrySnapshot(t, listCall)
		if len(names) != len(points) {
			return false
		}
		for _, name := range names {
			if _, ok := points[name]; !ok {
				return false
			}
		}

		identifier := ""
		for name, point := range points {
			if point == mountPoint {
				identifier = name
				break
			}
		}
		if identifier == "" {
			return false
		}

		path := filepath.Join(mountPoint, "file.txt")
		out, err := statusCall.Fn(context.Background(), rc.Params{"paths": []string{path}})
		if err != nil {
			return false
		}
		statuses := decodeStatuses(t, out)
		if len(statuses) != 1 {
			return false
		}
		result := statuses[0]
		if result["path"] != path {
			return false
		}
		_, hasPathError := result["error"]
		return !hasPathError
	}

	if err := quick.Check(complete, &quick.Config{MaxCount: 40}); err != nil {
		t.Error(err)
	}
}
