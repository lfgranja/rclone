package vfs

// Portable VFS-level acceptance for vfs/file-status.
//
// This suite runs against the default local backend AND against any
// configured remote via `go test ./vfs/ -remote TestXxx:`. It must NOT
// skip merely because fstest.RemoteName is set: it builds its VFS with
// fstest.NewRun(t) directly, which honors -remote, instead of the
// local-only helpers that skip on non-local remotes.
//
// Each test registers its own mount point, establishes a current listing
// through the VFS (the overlay case), performs a write-through
// with a long writeback delay so the upload stays queued, then asserts a
// one-path response plus the byte invariants.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/rc"
	"github.com/rclone/rclone/fstest"
	"github.com/rclone/rclone/vfs/vfscommon"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// conformanceOpt returns caching options with a long writeback delay so a
// file written through the mount stays awaiting_upload while queried.
func conformanceOpt() *vfscommon.Options {
	opt := vfscommon.Opt
	opt.CacheMode = vfscommon.CacheModeFull
	opt.WriteBack = fs.Duration(time.Hour)
	opt.DirCacheTime = 0
	return &opt
}

// conformanceMount creates a VFS over the test remote (whatever -remote
// selects), registers a temp mount point, and returns both with cleanup.
func conformanceMount(t *testing.T, r *fstest.Run, opt *vfscommon.Options) (*VFS, string) {
	t.Helper()
	testVFS := New(context.Background(), r.Fremote, opt)
	t.Cleanup(func() { cleanupVFS(t, testVFS) })
	mountPoint := filepath.Join(t.TempDir(), "mount")
	RegisterMountPointForVFS(testVFS, mountPoint)
	t.Cleanup(func() { UnregisterMountPointForVFS(testVFS) })
	return testVFS, mountPoint
}

// conformanceListRoot lists the mount root through the VFS so the parent
// listing a status read decides existence from is current.
func conformanceListRoot(t *testing.T, testVFS *VFS) {
	t.Helper()
	root, err := testVFS.Root()
	require.NoError(t, err)
	_, err = root.ReadDirAll()
	require.NoError(t, err)
}

// conformanceWriteThrough writes a file through the mount and refreshes the
// directory cache so the endpoint can find it.
func conformanceWriteThrough(t *testing.T, testVFS *VFS, name, content string) {
	t.Helper()
	h, err := testVFS.OpenFile(name, os.O_WRONLY|os.O_CREATE, 0777)
	require.NoError(t, err)
	n, err := h.Write([]byte(content))
	require.NoError(t, err)
	require.Equal(t, len(content), n)
	require.NoError(t, h.Close())
	conformanceListRoot(t, testVFS)
}

// conformanceQueryOne calls the endpoint for a single path and returns the
// one result object.
func conformanceQueryOne(t *testing.T, r *fstest.Run, path string) map[string]any {
	t.Helper()
	call := rc.Calls.Get("vfs/file-status")
	require.NotNil(t, call)
	out, err := call.Fn(context.Background(), rc.Params{
		"fs":    fs.ConfigString(r.Fremote),
		"paths": []string{path},
	})
	require.NoError(t, err)
	raw, err := json.Marshal(out)
	require.NoError(t, err)
	var doc map[string]any
	require.NoError(t, json.Unmarshal(raw, &doc))
	statuses, ok := doc["statuses"].([]any)
	require.True(t, ok, "response carries no statuses array: %v", doc)
	require.Len(t, statuses, 1, "one-path request must yield one result")
	result, ok := statuses[0].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, path, result["path"])
	return result
}

// TestFileStatusRemoteConformanceWriteThrough is the portable acceptance:
// a file written through the mount with a long writeback delay reports
// fully_held + awaiting_upload with exact byte counts satisfying the
// byte invariants.
func TestFileStatusRemoteConformanceWriteThrough(t *testing.T) {
	r := fstest.NewRun(t)
	t.Logf("conformance remote: %q", r.Fremote.String())
	testVFS, mountPoint := conformanceMount(t, r, conformanceOpt())
	conformanceListRoot(t, testVFS)

	content := strings.Repeat("c", 1024)
	const name = "conformance-write.txt"
	conformanceWriteThrough(t, testVFS, name, content)
	t.Cleanup(func() { drainFileStatusStateWriteback(t, testVFS) })

	path := filepath.Join(mountPoint, name)
	result := conformanceQueryOne(t, r, path)

	local, ok := result["local"].(string)
	require.True(t, ok, "result carries no local fact: %v", result)
	upload, ok := result["upload"].(string)
	require.True(t, ok, "result carries no upload fact: %v", result)
	assert.Equal(t, "fully_held", local)
	assert.Equal(t, "awaiting_upload", upload)

	bytes, ok := result["bytes"].(map[string]any)
	require.True(t, ok, "result carries no bytes object: %v", result)
	assert.Equal(t, float64(len(content)), bytes["unbroken_run"])
	assert.Equal(t, float64(len(content)), bytes["total_held"])
	assert.Equal(t, float64(len(content)), bytes["total_size"])

	unbrokenRun := fileStatusByteQuantity{Number: int64(len(content)), IsNumber: true}
	totalHeld := fileStatusByteQuantity{Number: int64(len(content)), IsNumber: true}
	totalSize := fileStatusByteQuantity{Number: int64(len(content)), IsNumber: true}
	assert.NoError(t, checkFileStatusByteInvariants(local, unbrokenRun, totalHeld, totalSize))
	assert.Equal(t, exitUploadNotComplete, fileStatusExitCode(local, upload, true))
}

// TestFileStatusRemoteConformanceRemoteOnly covers the complementary
// acceptance: a file on the remote the mount never wrote reports not_held
// with true-zero held bytes and the known size, from a current listing.
func TestFileStatusRemoteConformanceRemoteOnly(t *testing.T) {
	r := fstest.NewRun(t)
	t.Logf("conformance remote: %q", r.Fremote.String())
	testVFS, mountPoint := conformanceMount(t, r, conformanceOpt())
	conformanceListRoot(t, testVFS)

	content := strings.Repeat("r", 512)
	const name = "conformance-remote.txt"
	r.WriteObject(context.Background(), name, content, time.Now())
	conformanceListRoot(t, testVFS)

	path := filepath.Join(mountPoint, name)
	result := conformanceQueryOne(t, r, path)

	local, ok := result["local"].(string)
	require.True(t, ok, "result carries no local fact: %v", result)
	upload, ok := result["upload"].(string)
	require.True(t, ok, "result carries no upload fact: %v", result)
	assert.Equal(t, "not_held", local)
	assert.Equal(t, "not_applicable", upload)

	bytes, ok := result["bytes"].(map[string]any)
	require.True(t, ok, "result carries no bytes object: %v", result)
	assert.Equal(t, float64(0), bytes["unbroken_run"])
	assert.Equal(t, float64(0), bytes["total_held"])
	assert.Equal(t, float64(len(content)), bytes["total_size"])

	unbrokenRun := fileStatusByteQuantity{Number: 0, IsNumber: true}
	totalHeld := fileStatusByteQuantity{Number: 0, IsNumber: true}
	totalSize := fileStatusByteQuantity{Number: int64(len(content)), IsNumber: true}
	assert.NoError(t, checkFileStatusByteInvariants(local, unbrokenRun, totalHeld, totalSize))
	assert.Equal(t, exitNotFullyHeld, fileStatusExitCode(local, upload, true))
}
