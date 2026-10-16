// Test suite for the states the vfs/file-status endpoint
// reports and the exit-status map of FR-027
// (specs/001-vfs-file-status/).
//
// These tests are endpoint-level: they drive the registered
// vfs/file-status remote-control call over an in-process VFS
// built on a local remote, the way rc_test.go drives the
// other vfs/ endpoints, and assert the reported states, the
// byte quantities and the exit status FR-027 maps each
// reported pair to (contracts/api.md "Response Format" and
// "State Value Definitions"; data-model.md "ByteInfo").
//
// The in-process VFS a test creates is mounted at the remote's
// host root, so the request names the mount by its "fs"
// identifier and the endpoint answers for the paths under the
// remote's root, which is the mount's host interior. The
// mount-point registry that identifier-less resolution goes
// through (FR-009/FR-014, RegisterMountPointForVFS) reports the
// registered point for a VFS created this way, and null for a
// VFS created without one.
//
// They exercise the implemented state derivation (vfs/file_status.go
// fileStatusOf, T020) and the exit map of FR-027. The exit map
// itself and the byte invariants over generated quantities pass;
// the fixture tests that run the invariant checker against the
// quantities the endpoint returns expose the Phase 8 gaps
// (zero-byte, unknown size, range gaps, provenance, metadata) as
// failures for the implementation waves to close.
package vfs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/quick"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/rc"
	"github.com/rclone/rclone/fstest"
	"github.com/rclone/rclone/fstest/mockobject"
	"github.com/rclone/rclone/vfs/vfscache/writeback"
	"github.com/rclone/rclone/vfs/vfscommon"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The local-cache fact's values, the upload fact's values
// and the two byte-quantity markers, spelled exactly as the
// endpoint serialises them (contracts/api.md "State Value
// Definitions" and "Byte Info Fields"). The markers live in
// a namespace of their own, disjoint from the values of
// either fact (FR-002).
const (
	statusFullyHeld      = "fully_held"
	statusPartlyHeld     = "partly_held"
	statusNotHeld        = "not_held"
	statusUnderivable    = "underivable"
	statusDownloadFailed = "download_failed"
	statusNotRegularFile = "not_regular_file"

	statusUploaded       = "uploaded"
	statusAwaitingUpload = "awaiting_upload"
	statusUploading      = "uploading"
	statusUploadFailed   = "upload_failed"
	statusNotApplicable  = "not_applicable"
	statusNotTracked     = "not_tracked"

	markerNotDeterminable = "not_determinable"
	markerNotMeasurable   = "not_measurable"
)

// The exit statuses FR-027 publishes: success, the query
// failure, the usage error, not fully held locally and
// upload not complete.
const (
	exitSuccess           = 0
	exitQueryFailed       = 1
	exitUsageError        = 2
	exitNotFullyHeld      = 11
	exitUploadNotComplete = 12
)

// localStates and uploadStates are the values of either
// fact, in the order the state set publishes them.
var (
	localStates = []string{
		statusFullyHeld, statusPartlyHeld, statusNotHeld,
		statusUnderivable, statusDownloadFailed, statusNotRegularFile,
	}
	uploadStates = []string{
		statusUploaded, statusAwaitingUpload, statusUploading,
		statusUploadFailed, statusNotApplicable, statusNotTracked,
	}
)

// fileStatusStatePartlyHeldRun is the number of bytes the
// partly-held fixture reads through a mount that downloads
// in chunks of the same size, so the mount holds a part of
// the file and no more.
const fileStatusStatePartlyHeldRun = 5

// fileStatusUnknownSizeFs reports a known content stream with an unknown
// size for one remote path.
type fileStatusUnknownSizeFs struct {
	fs.Fs
	remote  string
	content []byte
}

func (f fileStatusUnknownSizeFs) List(ctx context.Context, dir string) (fs.DirEntries, error) {
	entries, err := f.Fs.List(ctx, dir)
	if err != nil {
		return nil, err
	}
	for i, entry := range entries {
		if object, ok := entry.(fs.Object); ok && object.Remote() == f.remote {
			entries[i] = f.mockObject()
		}
	}
	return entries, nil
}

func (f fileStatusUnknownSizeFs) NewObject(ctx context.Context, remote string) (fs.Object, error) {
	object, err := f.Fs.NewObject(ctx, remote)
	if err != nil {
		return nil, err
	}
	if remote == f.remote {
		return f.mockObject(), nil
	}
	return object, nil
}

func (f fileStatusUnknownSizeFs) mockObject() fs.Object {
	object := mockobject.New(f.remote).WithContent(f.content, mockobject.SeekModeRange)
	object.SetFs(f.Fs)
	object.SetUnknownSize(true)
	return object
}

// newFileStatusStateRun builds an in-process VFS over a
// local remote with the given options and returns it with
// the registered vfs/file-status call, the way rc_test.go
// drives the other vfs/ endpoints.
func newFileStatusStateRun(t *testing.T, opt *vfscommon.Options) (*fstest.Run, *VFS, *rc.Call) {
	if *fstest.RemoteName != "" {
		t.Skip("Skipping test on non local remote")
	}
	r := fstest.NewRun(t)
	vfs := New(context.Background(), r.Fremote, opt)
	// The endpoint checks fs-identifier queries for containment
	// against the VFS mount point (T075), rejecting a VFS with
	// none, so the harness mounts the test remote at its host
	// root, the way newFileStatusStateRunAtRemoteRoot does.
	vfs.MountPoint = r.Fremote.Root()
	t.Cleanup(func() {
		cleanupVFS(t, vfs)
	})
	call := rc.Calls.Get("vfs/file-status")
	require.NotNil(t, call)
	return r, vfs, call
}

// newFileStatusStateRunAtRemoteRoot uses the test remote's host root as the
// VFS mount point so endpoint paths map directly to its listed entries.
func newFileStatusStateRunAtRemoteRoot(t *testing.T, opt *vfscommon.Options) (*fstest.Run, *VFS, *rc.Call) {
	r, vfs, call := newFileStatusStateRun(t, opt)
	vfs.MountPoint = r.Fremote.Root()
	return r, vfs, call
}

func newFileStatusStateVFSAtRemoteRoot(t *testing.T, r *fstest.Run, remote fs.Fs, opt *vfscommon.Options) (*VFS, *rc.Call) {
	vfs := New(context.Background(), remote, opt)
	vfs.MountPoint = r.Fremote.Root()
	t.Cleanup(func() {
		cleanupVFS(t, vfs)
	})
	call := rc.Calls.Get("vfs/file-status")
	require.NotNil(t, call)
	return vfs, call
}

// fileStatusStateOpt returns the default VFS options with
// the cache mode and the writeback delay overridden, the
// way the read/write tests build their options. DirCacheTime
// is set to 0 to disable directory caching for tests.
func fileStatusStateOpt(cacheMode vfscommon.CacheMode, writeBack fs.Duration) *vfscommon.Options {
	opt := vfscommon.Opt
	opt.CacheMode = cacheMode
	opt.WriteBack = writeBack
	opt.DirCacheTime = 0
	return &opt
}

// fileStatusStatePath returns the host path of a file within
// the mount: for an in-process VFS over a local remote, the
// file's path under the remote's root, which is the mount's
// host interior.
func fileStatusStatePath(r *fstest.Run, name string) string {
	return filepath.Join(r.Fremote.Root(), filepath.FromSlash(name))
}

// fileStatusStateCall calls the endpoint with the given
// mount identifier and paths and returns the response
// document decoded through JSON, so the tests read the
// shape the contract fixes rather than the Go types the
// handler happens to build it from.
func fileStatusStateCall(t *testing.T, call *rc.Call, mount string, paths []string) map[string]any {
	t.Helper()
	out, err := call.Fn(context.Background(), rc.Params{
		"fs":    mount,
		"paths": paths,
	})
	require.NoError(t, err)
	b, err := json.Marshal(out)
	require.NoError(t, err)
	var doc map[string]any
	require.NoError(t, json.Unmarshal(b, &doc))
	return doc
}

// fileStatusStateStatuses returns the per-path results of a
// response document, which the contract wraps in a single
// "statuses" key the way "vfs/list" wraps its "vfses".
func fileStatusStateStatuses(t *testing.T, doc map[string]any) []map[string]any {
	t.Helper()
	raw, ok := doc["statuses"].([]any)
	require.True(t, ok, "the response carries no statuses array: %v", doc)
	statuses := make([]map[string]any, len(raw))
	for i, s := range raw {
		status, ok := s.(map[string]any)
		require.True(t, ok, "statuses[%d] is not an object: %v", i, s)
		statuses[i] = status
	}
	return statuses
}

// fileStatusStateResult returns the result reporting the
// given path: the contract fixes that a caller matches
// answers by path and never by position, and that the order
// results come back in carries no promise (FR-006).
func fileStatusStateResult(t *testing.T, statuses []map[string]any, path string) map[string]any {
	t.Helper()
	for _, status := range statuses {
		if status["path"] == path {
			return status
		}
	}
	t.Fatalf("no result for path %q in %v", path, statuses)
	return nil
}

// requireFileStatusStatePair asserts that a result reports
// both facts of FR-022 — the local-cache fact and the
// upload fact as two independent values, always present,
// never collapsed into one combined status — and returns
// them.
func requireFileStatusStatePair(t *testing.T, result map[string]any) (local, upload string) {
	t.Helper()
	local, ok := result["local"].(string)
	require.True(t, ok, "the result carries no local fact: %v", result)
	upload, ok = result["upload"].(string)
	require.True(t, ok, "the result carries no upload fact: %v", result)
	return local, upload
}

// fileStatusByteQuantity is one byte quantity of a result: a
// measurement, Number set and Marker empty, or a marker,
// Marker set and Number zero — never both, and never neither
// (data-model.md "ByteInfo").
type fileStatusByteQuantity struct {
	Number   int64
	IsNumber bool
	Marker   string
}

// parseFileStatusByteQuantity parses one byte quantity of a
// result, which the contract fixes as a JSON number or one
// of the two marker strings.
func parseFileStatusByteQuantity(t *testing.T, v any) fileStatusByteQuantity {
	t.Helper()
	switch q := v.(type) {
	case float64:
		return fileStatusByteQuantity{Number: int64(q), IsNumber: true}
	case string:
		return fileStatusByteQuantity{Marker: q}
	default:
		t.Fatalf("byte quantity %v is neither a number nor a marker string", v)
		return fileStatusByteQuantity{}
	}
}

// fileStatusByteInfo returns the three byte quantities of a
// result: the unbroken run of held bytes from offset zero,
// the total bytes held anywhere in the file, and the file's
// total size (FR-002, FR-002a).
func fileStatusByteInfo(t *testing.T, result map[string]any) (unbrokenRun, totalHeld, totalSize fileStatusByteQuantity) {
	t.Helper()
	bytes, ok := result["bytes"].(map[string]any)
	require.True(t, ok, "the result carries no bytes object: %v", result)
	return parseFileStatusByteQuantity(t, bytes["unbroken_run"]),
		parseFileStatusByteQuantity(t, bytes["total_held"]),
		parseFileStatusByteQuantity(t, bytes["total_size"])
}

// requireFileStatusByteNumber asserts that a byte quantity
// is the given measurement, 0 included — a measurement the
// mount can state is that number, never a marker standing
// in for it (FR-002).
func requireFileStatusByteNumber(t *testing.T, q fileStatusByteQuantity, want int64) {
	t.Helper()
	require.True(t, q.IsNumber, "the quantity is the marker %q, want the number %d", q.Marker, want)
	assert.Equal(t, want, q.Number)
}

// requireFileStatusByteMarker asserts that a byte quantity
// is the given marker, which no number ever stands in for.
func requireFileStatusByteMarker(t *testing.T, q fileStatusByteQuantity, marker string) {
	t.Helper()
	require.False(t, q.IsNumber, "the quantity is the number %d, want the marker %q", q.Number, marker)
	assert.Equal(t, marker, q.Marker)
}

// fileStatusExitCode is the oracle of FR-027 and SC-021:
// the exit status the command maps a reported (local,
// upload) pair to, on a mount that keeps a cache
// (mountKeepsCache) or one that keeps none.
//
// Success only where the file is fully held locally and its
// upload fact is uploaded or not applicable — a file the
// mount never wrote is not waiting for anything, its
// content is already on the remote. Otherwise the
// not-fully-held-locally status takes precedence, because
// the local fact is the primitive the feature reports and
// the upload fact stays in the reported output for a caller
// that needs it. The rule has exactly one exception, which
// keeps the upload-not-complete status reachable: on a
// mount that keeps no cache at all no file is ever fully
// held, so every content value there takes the
// upload-not-complete status — 11 would say nothing a
// caller cannot already tell from the mount's
// configuration, while 12 says outstanding work is
// outstanding. A non-regular file keeps the
// not-fully-held-locally status on every mount, its local
// fact being no content value (FR-032).
func fileStatusExitCode(local, upload string, mountKeepsCache bool) int {
	switch local {
	case statusFullyHeld:
		switch upload {
		case statusUploaded, statusNotApplicable:
			return exitSuccess
		default:
			return exitUploadNotComplete
		}
	case statusNotRegularFile:
		return exitNotFullyHeld
	default:
		if !mountKeepsCache {
			return exitUploadNotComplete
		}
		return exitNotFullyHeld
	}
}

// wantFileStatusStateExit derives the same exit status from
// the requirement text by a second route, as a cross-check
// of the oracle: success where the file is fully held and
// its upload is settled; otherwise the non-regular-file
// value, which asserts nothing about held content, keeps
// the not-fully-held-locally status; otherwise a content
// value on a mount that keeps no cache takes the
// upload-not-complete status; otherwise a fully held file
// whose upload is not settled takes the upload-not-complete
// status; and every other pair takes the not-fully-held-
// locally status, the local fact taking precedence.
func wantFileStatusStateExit(local, upload string, mountKeepsCache bool) int {
	if local == statusFullyHeld {
		if upload == statusUploaded || upload == statusNotApplicable {
			return exitSuccess
		}
		// fully_held with any outstanding upload state
		// (awaiting_upload, uploading, upload_failed) takes
		// upload-not-complete (FR-027, SC-021).
		return exitUploadNotComplete
	}
	if local == statusNotRegularFile {
		return exitNotFullyHeld
	}
	if !mountKeepsCache {
		return exitUploadNotComplete
	}
	return exitNotFullyHeld
}

// fileStatusExitCodeForCode returns the exit status FR-027
// maps a failure to answer to, by the code naming it: the
// usage error where the caller fixes the failure by
// correcting the invocation — an absent or empty paths
// array — and the query failure where the caller fixes it
// by changing something about the world. A per-path error
// result — a directory, an absent entry, a path outside the
// mount — is a query failure, and no number the published
// list already carries (3 for a directory, 4 for a missing
// file) is reused for one.
func fileStatusExitCodeForCode(code string) int {
	if code == "empty_path" {
		return exitUsageError
	}
	return exitQueryFailed
}

// checkFileStatusByteInvariants reports whether the three
// byte quantities of a result reporting the local fact
// local satisfy the invariants data-model.md fixes for that
// state (data-model.md, "ByteInfo" validation rules):
//
//   - fully_held: unbroken_run == total_size == total_held,
//     unless total_size is not_determinable, when
//     unbroken_run == total_held
//   - not_held: unbroken_run == 0, total_held == 0,
//     total_size the file's size or not_determinable
//   - partly_held: 0 <= unbroken_run <= total_held, total_held > 0,
//     and, when total_size is known, unbroken_run < total_size and
//     total_held <= total_size
//   - download_failed: 0 <= unbroken_run <= total_held <=
//     total_size, or 0 <= unbroken_run <= total_held with
//     total_size not_determinable where the size is unknown
//     (FR-028, FR-002)
//   - underivable: both held quantities carry
//     not_determinable
//   - not_regular_file: all three carry not_measurable
func checkFileStatusByteInvariants(local string, unbrokenRun, totalHeld, totalSize fileStatusByteQuantity) error {
	quantities := map[string]fileStatusByteQuantity{
		"unbroken_run": unbrokenRun,
		"total_held":   totalHeld,
		"total_size":   totalSize,
	}
	for name, q := range quantities {
		if q.IsNumber && q.Marker != "" {
			return fmt.Errorf("%s is both a number and a marker", name)
		}
		if !q.IsNumber && q.Marker != markerNotDeterminable && q.Marker != markerNotMeasurable {
			return fmt.Errorf("%s carries the unknown marker %q", name, q.Marker)
		}
	}
	switch local {
	case statusFullyHeld:
		if !unbrokenRun.IsNumber || !totalHeld.IsNumber {
			return fmt.Errorf("fully_held reports a held quantity as a marker")
		}
		if totalSize.Marker == markerNotDeterminable {
			if unbrokenRun.Number != totalHeld.Number {
				return fmt.Errorf("fully_held with a not_determinable total_size reports unbroken_run %d != total_held %d", unbrokenRun.Number, totalHeld.Number)
			}
			return nil
		}
		if !totalSize.IsNumber {
			return fmt.Errorf("fully_held reports total_size as the marker %q", totalSize.Marker)
		}
		if unbrokenRun.Number != totalSize.Number || totalHeld.Number != totalSize.Number {
			return fmt.Errorf("fully_held reports unbroken_run %d, total_held %d, total_size %d", unbrokenRun.Number, totalHeld.Number, totalSize.Number)
		}
	case statusNotHeld:
		if !unbrokenRun.IsNumber || unbrokenRun.Number != 0 {
			return fmt.Errorf("not_held reports unbroken_run %d, marker %q", unbrokenRun.Number, unbrokenRun.Marker)
		}
		if !totalHeld.IsNumber || totalHeld.Number != 0 {
			return fmt.Errorf("not_held reports total_held %d, marker %q", totalHeld.Number, totalHeld.Marker)
		}
		if totalSize.Marker != "" && totalSize.Marker != markerNotDeterminable {
			return fmt.Errorf("not_held reports total_size as the marker %q", totalSize.Marker)
		}
	case statusPartlyHeld:
		if !unbrokenRun.IsNumber || !totalHeld.IsNumber {
			return fmt.Errorf("partly_held reports a held quantity as a marker")
		}
		if unbrokenRun.Number < 0 {
			return fmt.Errorf("partly_held reports unbroken_run %d, want >= 0", unbrokenRun.Number)
		}
		if totalHeld.Number <= 0 {
			return fmt.Errorf("partly_held reports total_held %d, want > 0", totalHeld.Number)
		}
		if totalHeld.Number < unbrokenRun.Number {
			return fmt.Errorf("partly_held reports total_held %d < unbroken_run %d", totalHeld.Number, unbrokenRun.Number)
		}
		if totalSize.Marker == markerNotDeterminable {
			// The total-size bounds cannot be checked without a size.
			return nil
		}
		if !totalSize.IsNumber {
			return fmt.Errorf("partly_held reports total_size as the marker %q", totalSize.Marker)
		}
		if unbrokenRun.Number >= totalSize.Number {
			return fmt.Errorf("partly_held reports unbroken_run %d >= total_size %d", unbrokenRun.Number, totalSize.Number)
		}
		if totalHeld.Number > totalSize.Number {
			return fmt.Errorf("partly_held reports total_held %d > total_size %d", totalHeld.Number, totalSize.Number)
		}
	case statusDownloadFailed:
		if !unbrokenRun.IsNumber || !totalHeld.IsNumber {
			return fmt.Errorf("download_failed reports a held quantity as a marker")
		}
		if unbrokenRun.Number < 0 || totalHeld.Number < unbrokenRun.Number {
			return fmt.Errorf("download_failed reports unbroken_run %d, total_held %d, want 0 <= unbroken_run <= total_held", unbrokenRun.Number, totalHeld.Number)
		}
		if totalSize.Marker == markerNotDeterminable {
			// A failed download of a file whose total size is unknown
			// keeps the held quantities against a not_determinable
			// total_size; the size bound cannot be checked without a
			// size (FR-028, FR-002).
			return nil
		}
		if !totalSize.IsNumber {
			return fmt.Errorf("download_failed reports total_size as the marker %q", totalSize.Marker)
		}
		if totalSize.Number < totalHeld.Number {
			return fmt.Errorf("download_failed reports total_held %d > total_size %d", totalHeld.Number, totalSize.Number)
		}
	case statusUnderivable:
		if unbrokenRun.Marker != markerNotDeterminable || totalHeld.Marker != markerNotDeterminable {
			return fmt.Errorf("underivable reports the held quantities as %q and %q, want both %q", unbrokenRun.Marker, totalHeld.Marker, markerNotDeterminable)
		}
		if totalSize.Marker == markerNotMeasurable {
			return fmt.Errorf("underivable reports total_size as %q", markerNotMeasurable)
		}
	case statusNotRegularFile:
		if unbrokenRun.Marker != markerNotMeasurable || totalHeld.Marker != markerNotMeasurable || totalSize.Marker != markerNotMeasurable {
			return fmt.Errorf("not_regular_file reports the quantities as %q, %q and %q, want all three %q", unbrokenRun.Marker, totalHeld.Marker, totalSize.Marker, markerNotMeasurable)
		}
	default:
		return fmt.Errorf("unknown local fact %q", local)
	}
	return nil
}

// listFileStatusStateRoot lists the mount's root through the
// VFS, so the mount holds the directory entries a state read
// decides existence from (FR-033): a file-manager overlay
// has just listed the directory it draws, which is the case
// the not_found value is reported from.
func listFileStatusStateRoot(t *testing.T, vfs *VFS) {
	t.Helper()
	root, err := vfs.Root()
	require.NoError(t, err)
	_, err = root.ReadDirAll()
	require.NoError(t, err)
}

// writeFileStatusStateThrough writes a file through the mount
// and closes it, leaving the content in the cache and the
// writeback item queued for the mount's writeback delay.
// Refreshes the VFS directory cache so the endpoint can find the file.
func writeFileStatusStateThrough(t *testing.T, vfs *VFS, name, content string) {
	t.Helper()
	h, err := vfs.OpenFile(name, os.O_WRONLY|os.O_CREATE, 0777)
	require.NoError(t, err)
	n, err := h.Write([]byte(content))
	require.NoError(t, err)
	assert.Equal(t, len(content), n)
	require.NoError(t, h.Close())
	listFileStatusStateRoot(t, vfs) // refresh directory cache
}

// readFileStatusStateThrough reads a file through the mount,
// caching the content read.
func readFileStatusStateThrough(t *testing.T, vfs *VFS, name, content string) {
	t.Helper()
	h, err := vfs.OpenFile(name, os.O_RDONLY, 0777)
	require.NoError(t, err)
	buf := make([]byte, len(content))
	_, err = io.ReadFull(h, buf)
	require.NoError(t, err)
	assert.Equal(t, content, string(buf))
	require.NoError(t, h.Close())
}

// arrangeFileStatusStateRemoteFile writes a file straight to
// the remote, so the mount holds no cache entry for it, and
// returns its host path. Refreshes the VFS directory cache so
// the endpoint can find the file.
func arrangeFileStatusStateRemoteFile(t *testing.T, r *fstest.Run, vfs *VFS, name, content string) string {
	t.Helper()
	r.WriteObject(context.Background(), name, content, t1)
	listFileStatusStateRoot(t, vfs) // refresh directory cache
	return fileStatusStatePath(r, name)
}

// arrangeFileStatusStateCachedFile writes a file to the
// remote and reads it through the mount, caching its
// content, and returns its host path.
func arrangeFileStatusStateCachedFile(t *testing.T, r *fstest.Run, vfs *VFS, name, content string) string {
	t.Helper()
	r.WriteObject(context.Background(), name, content, t1)
	readFileStatusStateThrough(t, vfs, name, content)
	listFileStatusStateRoot(t, vfs) // refresh directory cache
	return fileStatusStatePath(r, name)
}

// arrangeFileStatusStatePartlyCachedFile writes a file to
// the remote and reads only its first bytes through a mount
// that downloads in chunks of the same size, so the mount
// holds an unbroken run of the file and no more, and
// returns its host path.
func arrangeFileStatusStatePartlyCachedFile(t *testing.T, r *fstest.Run, vfs *VFS, name, content string) string {
	t.Helper()
	r.WriteObject(context.Background(), name, content, t1)
	h, err := vfs.OpenFile(name, os.O_RDONLY, 0777)
	require.NoError(t, err)
	buf := make([]byte, fileStatusStatePartlyHeldRun)
	_, err = io.ReadFull(h, buf)
	require.NoError(t, err)
	require.NoError(t, h.Close())
	return fileStatusStatePath(r, name)
}

type fileStatusStateCacheRange struct {
	offset  int64
	content []byte
}

// seedFileStatusStateCacheRanges installs specific held bytes in a cache item
// so range-gap and unknown-size fixtures do not depend on download buffering.
func seedFileStatusStateCacheRanges(t *testing.T, vfs *VFS, name string, object fs.Object, cachedRanges ...fileStatusStateCacheRange) {
	t.Helper()
	item := vfs.cache.Item(name)
	require.NoError(t, item.Open(object))
	t.Cleanup(func() {
		if err := item.Close(nil); err != nil {
			t.Errorf("closing cache item %q: %v", name, err)
		}
	})
	for _, cachedRange := range cachedRanges {
		n, skipped, err := item.WriteAtNoOverwrite(cachedRange.content, cachedRange.offset)
		require.NoError(t, err)
		require.Equal(t, len(cachedRange.content), n)
		require.Zero(t, skipped)
	}
}

// fileStatusStateIndexedMount returns the indexed fs identifier
// vfs/list reports for the nth VFS over the shared test remote
// (FR-010): fstest.NewRun shares one remote name across runs, so a
// test with two or more VFSes alive addresses each by its index in
// creation order, the way TestFileStatusBCESharedConfigBareName
// addresses its shared-config mounts. Tests hold no parallelism,
// so the indices match creation order exactly.
func fileStatusStateIndexedMount(r *fstest.Run, index int) string {
	return fmt.Sprintf("%s[%d]", fs.ConfigString(r.Fremote), index)
}

// holdFileStatusStateResetInFlight arranges a cached file and starts
// a cache reset that blocks inside the replacement object's Size
// while holding the item lock — the deterministic mid-reset
// arrangement of TestFileStatusObservationResetDoesNotWait
// (fileStatusResetBarrierObject) — and returns the file's host path.
// While the reset is held, the endpoint reports the file as
// underivable without waiting for the reset (FR-004, FR-026). The
// reset is released by cleanup, so the test body only reads.
func holdFileStatusStateResetInFlight(t *testing.T, r *fstest.Run, vfs *VFS, name, content string) string {
	t.Helper()
	arrangeFileStatusStateCachedFile(t, r, vfs, name, content)
	path := fileStatusStatePath(r, name)

	remoteObject, err := r.Fremote.NewObject(context.Background(), name)
	require.NoError(t, err)
	item := vfs.cache.Item(name)
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseReset := func() { releaseOnce.Do(func() { close(release) }) }
	barrierObject := &fileStatusResetBarrierObject{
		Object:  remoteObject,
		entered: make(chan struct{}),
		release: release,
	}
	require.NoError(t, item.Open(barrierObject))
	t.Cleanup(func() {
		assert.NoError(t, item.Close(nil))
	})

	barrierObject.armed = true
	resetDone := make(chan struct{})
	var resetErr error
	go func() {
		_, _, resetErr = item.Reset()
		close(resetDone)
	}()
	t.Cleanup(func() {
		releaseReset()
		select {
		case <-resetDone:
		case <-time.After(5 * time.Second):
			t.Errorf("cache reset did not finish after releasing its barrier")
		}
		assert.NoError(t, resetErr)
	})

	select {
	case <-barrierObject.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("reset did not reach the locked object-fingerprint barrier")
	}
	return path
}

// drainFileStatusStateWriteback makes every item queued for
// writeback eligible immediately, so the mount's writeback
// loop uploads it on its next check and the test's cleanup
// does not wait out the fixture's writeback delay.
func drainFileStatusStateWriteback(t *testing.T, vfs *VFS) {
	t.Helper()
	if vfs.cache == nil {
		return
	}
	queue, ok := vfs.cache.Queue()["queue"].([]writeback.QueueInfo)
	require.True(t, ok, "the writeback queue is not a queue: %v", vfs.cache.Queue()["queue"])
	for _, item := range queue {
		require.NoError(t, vfs.cache.QueueSetExpiry(item.ID, time.Now(), 0))
	}
}

// TestFileStatusStateExitMapTable pins the exit-status map
// of FR-027 over every combination of the six local-cache
// fact values and the six upload fact values — the 36 pairs
// FR-022 requires the endpoint to report independently — for
// a mount that keeps a cache and one that keeps none. The
// map is a function of the reported facts alone, so it is
// total over all 36 pairs whatever the mount can produce,
// and a caller branching on the status is never left
// guessing which of two true conditions the status refers
// to, the upload fact staying in the reported output.
func TestFileStatusStateExitMapTable(t *testing.T) {
	for _, mountKeepsCache := range []bool{true, false} {
		for _, local := range localStates {
			for _, upload := range uploadStates {
				name := fmt.Sprintf("local=%s/upload=%s/cache=%v", local, upload, mountKeepsCache)
				t.Run(name, func(t *testing.T) {
					assert.Equal(t,
						wantFileStatusStateExit(local, upload, mountKeepsCache),
						fileStatusExitCode(local, upload, mountKeepsCache))
				})
			}
		}
	}

	// The boundary cases FR-027 and SC-021 document,
	// checked against the map by hand.
	for _, tc := range []struct {
		local, upload string
		keepsCache    bool
		want          int
	}{
		// A fully held file whose upload fact is uploaded or
		// not applicable exits successfully: a file the mount
		// never wrote is not waiting for anything, its
		// content is already on the remote.
		{statusFullyHeld, statusUploaded, true, exitSuccess},
		{statusFullyHeld, statusNotApplicable, true, exitSuccess},
		// A fully held file whose upload fact is outstanding
		// work does not.
		{statusFullyHeld, statusAwaitingUpload, true, exitUploadNotComplete},
		{statusFullyHeld, statusUploading, true, exitUploadNotComplete},
		{statusFullyHeld, statusUploadFailed, true, exitUploadNotComplete},
		// The not-fully-held-locally status takes precedence
		// over the upload-not-complete status, and the upload
		// fact stays in the reported output.
		{statusPartlyHeld, statusAwaitingUpload, true, exitNotFullyHeld},
		{statusPartlyHeld, statusUploading, true, exitNotFullyHeld},
		{statusNotHeld, statusAwaitingUpload, true, exitNotFullyHeld},
		{statusUnderivable, statusUploading, true, exitNotFullyHeld},
		// A failed download is a value of the local fact, so
		// the file is by definition not fully held and takes
		// the not-fully-held-locally status, never the
		// upload-not-complete one.
		{statusDownloadFailed, statusAwaitingUpload, true, exitNotFullyHeld},
		{statusDownloadFailed, statusUploadFailed, true, exitNotFullyHeld},
		// On a mount that keeps no cache every content value
		// takes the upload-not-complete status and no such
		// file exits successfully (SC-021).
		{statusNotHeld, statusNotTracked, false, exitUploadNotComplete},
		{statusPartlyHeld, statusNotTracked, false, exitUploadNotComplete},
		{statusUnderivable, statusNotTracked, false, exitUploadNotComplete},
		{statusDownloadFailed, statusNotTracked, false, exitUploadNotComplete},
		// A non-regular file keeps the not-fully-held-locally
		// status even on a mount that keeps no cache, the
		// cache-off exception covering files whose local fact
		// is a content value and this value reporting none
		// (FR-032, SC-021).
		{statusNotRegularFile, statusNotTracked, false, exitNotFullyHeld},
		{statusNotRegularFile, statusNotApplicable, true, exitNotFullyHeld},
		{statusNotRegularFile, statusUploaded, true, exitNotFullyHeld},
	} {
		assert.Equal(t, tc.want, fileStatusExitCode(tc.local, tc.upload, tc.keepsCache),
			"local=%s upload=%s keepsCache=%v", tc.local, tc.upload, tc.keepsCache)
	}

	// The exit status of a failure to answer, by the code
	// that names it.
	for _, tc := range []struct {
		code string
		want int
	}{
		{"empty_path", exitUsageError},
		{"no_active_mount", exitQueryFailed},
		{"ambiguous_mount", exitQueryFailed},
		{"outside_any_mount", exitQueryFailed},
		{"directory_not_file", exitQueryFailed},
		{"not_found", exitQueryFailed},
		{"unaccountable_path", exitQueryFailed},
		{"param_invalid", exitQueryFailed},
	} {
		assert.Equal(t, tc.want, fileStatusExitCodeForCode(tc.code), "code=%s", tc.code)
	}
}

// TestFileStatusStateNotHeldNotApplicable checks a file on
// the remote that the mount never wrote: the mount holds no
// cache entry for it, so the local fact is not_held with
// the held byte quantities a true zero — a measurement of a
// mount holding none of the file, not a missing value — and
// the total size the file's size, which the mount knows
// from the entry it holds (FR-002, FR-004). The upload fact
// is not_applicable, the mount never having tracked an
// upload for it (FR-003). The pair maps to the
// not-fully-held-locally exit status (FR-027).
func TestFileStatusStateNotHeldNotApplicable(t *testing.T) {
	r, vfs, call := newFileStatusStateRun(t, fileStatusStateOpt(vfscommon.CacheModeFull, writeBackDelay))
	listFileStatusStateRoot(t, vfs)
	content := "0123456789"
	path := arrangeFileStatusStateRemoteFile(t, r, vfs, "never-written.txt", content)

	doc := fileStatusStateCall(t, call, fs.ConfigString(r.Fremote), []string{path})
	result := fileStatusStateResult(t, fileStatusStateStatuses(t, doc), path)

	local, upload := requireFileStatusStatePair(t, result)
	assert.Equal(t, statusNotHeld, local)
	assert.Equal(t, statusNotApplicable, upload)

	unbrokenRun, totalHeld, totalSize := fileStatusByteInfo(t, result)
	requireFileStatusByteNumber(t, unbrokenRun, 0)
	requireFileStatusByteNumber(t, totalHeld, 0)
	requireFileStatusByteNumber(t, totalSize, int64(len(content)))

	assert.Equal(t, exitNotFullyHeld, fileStatusExitCode(local, upload, true))
	assert.NoError(t, checkFileStatusByteInvariants(local, unbrokenRun, totalHeld, totalSize))
}

// TestFileStatusStateCacheOffContentValue checks the
// prescribed cache-off verification of SC-021: a regular
// file on a mount that keeps no cache reports the local
// fact of a mount holding no copy of it and the upload fact
// not_tracked, and takes the upload-not-complete status
// specifically — on such a mount no file is ever fully
// held, so the fact that bears on durability there is the
// upload one, and no file on it exits successfully.
func TestFileStatusStateCacheOffContentValue(t *testing.T) {
	r, vfs, call := newFileStatusStateRun(t, fileStatusStateOpt(vfscommon.CacheModeOff, writeBackDelay))
	listFileStatusStateRoot(t, vfs)
	content := "written straight to the remote"
	path := arrangeFileStatusStateRemoteFile(t, r, vfs, "remote-only.txt", content)

	doc := fileStatusStateCall(t, call, fs.ConfigString(r.Fremote), []string{path})
	result := fileStatusStateResult(t, fileStatusStateStatuses(t, doc), path)

	local, upload := requireFileStatusStatePair(t, result)
	assert.Equal(t, statusNotHeld, local)
	assert.Equal(t, statusNotTracked, upload)

	unbrokenRun, totalHeld, totalSize := fileStatusByteInfo(t, result)
	requireFileStatusByteNumber(t, unbrokenRun, 0)
	requireFileStatusByteNumber(t, totalHeld, 0)
	requireFileStatusByteNumber(t, totalSize, int64(len(content)))

	assert.Equal(t, exitUploadNotComplete, fileStatusExitCode(local, upload, false))
	assert.NoError(t, checkFileStatusByteInvariants(local, unbrokenRun, totalHeld, totalSize))
}

// TestFileStatusStateNotTrackedMountGuard checks that
// not_tracked is a mount-level guard and never a per-file
// observation (FR-003, FR-011): every file a mount that
// keeps no cache reports — one the mount never wrote and
// one written straight through it to the remote — carries
// not_tracked, and no file a cache-keeping mount reports
// ever does.
func TestFileStatusStateNotTrackedMountGuard(t *testing.T) {
	r, vfs, call := newFileStatusStateRun(t, fileStatusStateOpt(vfscommon.CacheModeOff, writeBackDelay))
	listFileStatusStateRoot(t, vfs)
	// A cache-keeping VFS over the shared test remote joins the
	// same active name below, so both mounts are addressed by
	// their indexed names from the start.
	mount := fileStatusStateIndexedMount(r, 0)

	remoteOnly := arrangeFileStatusStateRemoteFile(t, r, vfs, "remote-only.txt", "on the remote already")
	writeFileStatusStateThrough(t, vfs, "written-through.txt", "written through a mount that keeps no cache")
	writtenThrough := fileStatusStatePath(r, "written-through.txt")

	doc := fileStatusStateCall(t, call, mount, []string{remoteOnly, writtenThrough})
	statuses := fileStatusStateStatuses(t, doc)
	require.Len(t, statuses, 2)
	for _, path := range []string{remoteOnly, writtenThrough} {
		result := fileStatusStateResult(t, statuses, path)
		local, upload := requireFileStatusStatePair(t, result)
		assert.Equal(t, statusNotHeld, local, "path=%s", path)
		assert.Equal(t, statusNotTracked, upload, "path=%s", path)
		assert.Equal(t, exitUploadNotComplete, fileStatusExitCode(local, upload, false), "path=%s", path)
	}

	// A mount that keeps a cache reports the upload fact per
	// file and never not_tracked, whatever the file.
	cacheKeeping := fileStatusStateOpt(vfscommon.CacheModeFull, writeBackDelay)
	r2, vfs2, call2 := newFileStatusStateRun(t, cacheKeeping)
	listFileStatusStateRoot(t, vfs2)
	mount2 := fileStatusStateIndexedMount(r2, 1)
	neverWritten := arrangeFileStatusStateRemoteFile(t, r2, vfs2, "never-written.txt", "content")
	fullyCached := arrangeFileStatusStateCachedFile(t, r2, vfs2, "fully-cached.txt", "content")

	doc2 := fileStatusStateCall(t, call2, mount2, []string{neverWritten, fullyCached})
	statuses2 := fileStatusStateStatuses(t, doc2)
	require.Len(t, statuses2, 2)
	for _, path := range []string{neverWritten, fullyCached} {
		result := fileStatusStateResult(t, statuses2, path)
		_, upload := requireFileStatusStatePair(t, result)
		assert.NotEqual(t, statusNotTracked, upload, "path=%s", path)
	}
}

// TestFileStatusStateFullyHeldUploaded checks a file written
// through a cache-keeping mount whose writeback has
// completed: the content is held in the cache in full and
// the upload is completed, so the pair maps to the success
// exit status (FR-027).
func TestFileStatusStateFullyHeldUploaded(t *testing.T) {
	r, vfs, call := newFileStatusStateRun(t, fileStatusStateOpt(vfscommon.CacheModeFull, writeBackDelay))
	content := "written through the mount and uploaded"
	writeFileStatusStateThrough(t, vfs, "written-and-uploaded.txt", content)
	vfs.WaitForWriters(waitForWritersDelay)

	path := fileStatusStatePath(r, "written-and-uploaded.txt")
	doc := fileStatusStateCall(t, call, fs.ConfigString(r.Fremote), []string{path})
	result := fileStatusStateResult(t, fileStatusStateStatuses(t, doc), path)

	local, upload := requireFileStatusStatePair(t, result)
	assert.Equal(t, statusFullyHeld, local)
	assert.Equal(t, statusUploaded, upload)

	unbrokenRun, totalHeld, totalSize := fileStatusByteInfo(t, result)
	requireFileStatusByteNumber(t, unbrokenRun, int64(len(content)))
	requireFileStatusByteNumber(t, totalHeld, int64(len(content)))
	requireFileStatusByteNumber(t, totalSize, int64(len(content)))

	assert.Equal(t, exitSuccess, fileStatusExitCode(local, upload, true))
	assert.NoError(t, checkFileStatusByteInvariants(local, unbrokenRun, totalHeld, totalSize))
}

// TestFileStatusStateFullyHeldAwaitingUpload checks a file
// written through a cache-keeping mount whose writeback is
// still pending: the written content is held in the cache in
// full, so the local fact is fully_held, and the upload fact
// is awaiting_upload, the writeback item being queued but
// not started (FR-003). The pair maps to the
// upload-not-complete exit status, the upload fact being
// outstanding work a caller asking "is my data there yet"
// must not read as a drained mount (FR-027, SC-007).
func TestFileStatusStateFullyHeldAwaitingUpload(t *testing.T) {
	// A writeback delay long enough that the item is still
	// queued when the endpoint is asked about the file; the
	// test's cleanup makes it eligible immediately instead of
	// waiting the delay out.
	r, vfs, call := newFileStatusStateRun(t, fileStatusStateOpt(vfscommon.CacheModeFull, fs.Duration(time.Hour)))
	content := "written through the mount, upload still queued"
	writeFileStatusStateThrough(t, vfs, "pending-upload.txt", content)

	path := fileStatusStatePath(r, "pending-upload.txt")
	doc := fileStatusStateCall(t, call, fs.ConfigString(r.Fremote), []string{path})
	result := fileStatusStateResult(t, fileStatusStateStatuses(t, doc), path)

	local, upload := requireFileStatusStatePair(t, result)
	assert.Equal(t, statusFullyHeld, local)
	assert.Equal(t, statusAwaitingUpload, upload)

	unbrokenRun, totalHeld, totalSize := fileStatusByteInfo(t, result)
	requireFileStatusByteNumber(t, unbrokenRun, int64(len(content)))
	requireFileStatusByteNumber(t, totalHeld, int64(len(content)))
	requireFileStatusByteNumber(t, totalSize, int64(len(content)))

	assert.Equal(t, exitUploadNotComplete, fileStatusExitCode(local, upload, true))
	assert.NoError(t, checkFileStatusByteInvariants(local, unbrokenRun, totalHeld, totalSize))

	drainFileStatusStateWriteback(t, vfs)
}

// TestFileStatusStateFullyHeldNotApplicable checks a file
// the mount cached by reading it from the remote: the
// content is held in full and the mount never wrote the
// file, so the upload fact is not_applicable and the pair
// maps to the success exit status — reading a fully cached
// file changes nothing about its content being on the
// remote (FR-003, FR-027).
func TestFileStatusStateFullyHeldNotApplicable(t *testing.T) {
	r, vfs, call := newFileStatusStateRun(t, fileStatusStateOpt(vfscommon.CacheModeFull, writeBackDelay))
	content := "read through the mount and cached"
	path := arrangeFileStatusStateCachedFile(t, r, vfs, "read-and-cached.txt", content)

	doc := fileStatusStateCall(t, call, fs.ConfigString(r.Fremote), []string{path})
	result := fileStatusStateResult(t, fileStatusStateStatuses(t, doc), path)

	local, upload := requireFileStatusStatePair(t, result)
	assert.Equal(t, statusFullyHeld, local)
	assert.Equal(t, statusNotApplicable, upload)

	unbrokenRun, totalHeld, totalSize := fileStatusByteInfo(t, result)
	requireFileStatusByteNumber(t, unbrokenRun, int64(len(content)))
	requireFileStatusByteNumber(t, totalHeld, int64(len(content)))
	requireFileStatusByteNumber(t, totalSize, int64(len(content)))

	assert.Equal(t, exitSuccess, fileStatusExitCode(local, upload, true))
	assert.NoError(t, checkFileStatusByteInvariants(local, unbrokenRun, totalHeld, totalSize))
}

// TestFileStatusStateUploadProvenance contrasts a file merely read from the
// remote with one written through the mount and successfully uploaded.
func TestFileStatusStateUploadProvenance(t *testing.T) {
	r, vfs, call := newFileStatusStateRunAtRemoteRoot(t, fileStatusStateOpt(vfscommon.CacheModeFull, writeBackDelay))
	readContent := "read from the remote"
	readPath := arrangeFileStatusStateCachedFile(t, r, vfs, "read-only.txt", readContent)

	writtenContent := "written through the mount"
	writeFileStatusStateThrough(t, vfs, "written.txt", writtenContent)
	vfs.WaitForWriters(waitForWritersDelay)
	writtenPath := fileStatusStatePath(r, "written.txt")

	doc := fileStatusStateCall(t, call, fs.ConfigString(r.Fremote), []string{readPath, writtenPath})
	statuses := fileStatusStateStatuses(t, doc)
	for _, tc := range []struct {
		path        string
		contentSize int64
		wantUpload  string
	}{
		{path: readPath, contentSize: int64(len(readContent)), wantUpload: statusNotApplicable},
		{path: writtenPath, contentSize: int64(len(writtenContent)), wantUpload: statusUploaded},
	} {
		result := fileStatusStateResult(t, statuses, tc.path)
		local, upload := requireFileStatusStatePair(t, result)
		assert.Equal(t, statusFullyHeld, local, "path=%s", tc.path)
		assert.Equal(t, tc.wantUpload, upload, "path=%s", tc.path)
		unbrokenRun, totalHeld, totalSize := fileStatusByteInfo(t, result)
		requireFileStatusByteNumber(t, unbrokenRun, tc.contentSize)
		requireFileStatusByteNumber(t, totalHeld, tc.contentSize)
		requireFileStatusByteNumber(t, totalSize, tc.contentSize)
		assert.Equal(t, exitSuccess, fileStatusExitCode(local, upload, true), "path=%s", tc.path)
		assert.NoError(t, checkFileStatusByteInvariants(local, unbrokenRun, totalHeld, totalSize))
	}
}

// TestFileStatusStatePartlyHeld checks a file the mount
// holds in part: an unbroken run of bytes from offset zero
// and no more, so the local fact is partly_held with both
// held quantities the bytes that did arrive and the total
// size the file's size, and the pair maps to the
// not-fully-held-locally exit status (FR-002, FR-002a,
// FR-027). The two quantities are distinct — the unbroken
// run and the total bytes held anywhere in the file — so a
// caller can tell a file readable offline from its start
// from one whose held bytes are scattered with gaps.
func TestFileStatusStatePartlyHeld(t *testing.T) {
	// Specific held bytes are seeded into the cache item, the way
	// TestFileStatusStatePartlyHeldRangeGaps arranges its ranges,
	// so the fixture does not depend on download buffering, which
	// fetches the whole file and reports it fully held.
	opt := fileStatusStateOpt(vfscommon.CacheModeFull, writeBackDelay)
	opt.HandleCaching = 0
	r, vfs, call := newFileStatusStateRunAtRemoteRoot(t, opt)
	content := "0123456789abcdefghijklmnopqrstuvwxyz"
	const name = "partly-cached.txt"
	path := arrangeFileStatusStateRemoteFile(t, r, vfs, name, content)
	object, err := r.Fremote.NewObject(context.Background(), name)
	require.NoError(t, err)
	seedFileStatusStateCacheRanges(t, vfs, name, object,
		fileStatusStateCacheRange{offset: 0, content: []byte(content[:fileStatusStatePartlyHeldRun])},
		fileStatusStateCacheRange{offset: 2 * fileStatusStatePartlyHeldRun, content: []byte(content[2*fileStatusStatePartlyHeldRun : 3*fileStatusStatePartlyHeldRun])},
	)

	doc := fileStatusStateCall(t, call, fs.ConfigString(r.Fremote), []string{path})
	result := fileStatusStateResult(t, fileStatusStateStatuses(t, doc), path)

	local, upload := requireFileStatusStatePair(t, result)
	assert.Equal(t, statusPartlyHeld, local)
	assert.Equal(t, statusNotApplicable, upload)

	unbrokenRun, totalHeld, totalSize := fileStatusByteInfo(t, result)
	requireFileStatusByteNumber(t, unbrokenRun, fileStatusStatePartlyHeldRun)
	requireFileStatusByteNumber(t, totalHeld, 2*fileStatusStatePartlyHeldRun)
	requireFileStatusByteNumber(t, totalSize, int64(len(content)))

	assert.Equal(t, exitNotFullyHeld, fileStatusExitCode(local, upload, true))
	assert.NoError(t, checkFileStatusByteInvariants(local, unbrokenRun, totalHeld, totalSize))
}

// TestFileStatusStateZeroByteFiles distinguishes an empty file whose cache
// entry was read from one that remains remote-only. Both have a known size of
// zero, but the cached file is fully held.
func TestFileStatusStateZeroByteFiles(t *testing.T) {
	r, vfs, call := newFileStatusStateRunAtRemoteRoot(t, fileStatusStateOpt(vfscommon.CacheModeFull, writeBackDelay))
	remoteName := "empty-remote.txt"
	cachedName := "empty-cached.txt"
	r.WriteObject(context.Background(), remoteName, "", t1)
	r.WriteObject(context.Background(), cachedName, "", t1)
	listFileStatusStateRoot(t, vfs)

	h, err := vfs.OpenFile(cachedName, os.O_RDONLY, 0777)
	require.NoError(t, err)
	n, err := h.Read([]byte{0})
	assert.Zero(t, n)
	require.ErrorIs(t, err, io.EOF)
	require.NoError(t, h.Close())
	listFileStatusStateRoot(t, vfs)

	remotePath := fileStatusStatePath(r, remoteName)
	cachedPath := fileStatusStatePath(r, cachedName)
	doc := fileStatusStateCall(t, call, fs.ConfigString(r.Fremote), []string{remotePath, cachedPath})
	statuses := fileStatusStateStatuses(t, doc)
	for _, tc := range []struct {
		path      string
		wantLocal string
	}{
		{path: remotePath, wantLocal: statusNotHeld},
		{path: cachedPath, wantLocal: statusFullyHeld},
	} {
		result := fileStatusStateResult(t, statuses, tc.path)
		local, _ := requireFileStatusStatePair(t, result)
		assert.Equal(t, tc.wantLocal, local, "path=%s", tc.path)

		unbrokenRun, totalHeld, totalSize := fileStatusByteInfo(t, result)
		requireFileStatusByteNumber(t, unbrokenRun, 0)
		requireFileStatusByteNumber(t, totalHeld, 0)
		requireFileStatusByteNumber(t, totalSize, 0)
		assert.NoError(t, checkFileStatusByteInvariants(local, unbrokenRun, totalHeld, totalSize))
	}
}

// TestFileStatusStateUnknownTotalSizeUsesCachedPrefix reports the readable
// prefix even when the remote does not report a total file size.
func TestFileStatusStateUnknownTotalSizeUsesCachedPrefix(t *testing.T) {
	if *fstest.RemoteName != "" {
		t.Skip("Skipping test on non local remote")
	}
	r := fstest.NewRun(t)
	const remoteName = "unknown-size.txt"
	content := "0123456789abcdefghijklmnopqrstuvwxyz"
	remote := fileStatusUnknownSizeFs{Fs: r.Fremote, remote: remoteName, content: []byte(content)}
	opt := fileStatusStateOpt(vfscommon.CacheModeFull, writeBackDelay)
	vfs, call := newFileStatusStateVFSAtRemoteRoot(t, r, remote, opt)

	path := arrangeFileStatusStateRemoteFile(t, r, vfs, remoteName, content)
	object, err := remote.NewObject(context.Background(), remoteName)
	require.NoError(t, err)
	seedFileStatusStateCacheRanges(t, vfs, remoteName, object, fileStatusStateCacheRange{
		offset:  0,
		content: []byte(content[:fileStatusStatePartlyHeldRun]),
	})
	doc := fileStatusStateCall(t, call, fs.ConfigString(remote), []string{path})
	result := fileStatusStateResult(t, fileStatusStateStatuses(t, doc), path)

	local, _ := requireFileStatusStatePair(t, result)
	assert.Equal(t, statusPartlyHeld, local)
	unbrokenRun, totalHeld, totalSize := fileStatusByteInfo(t, result)
	requireFileStatusByteNumber(t, unbrokenRun, fileStatusStatePartlyHeldRun)
	requireFileStatusByteNumber(t, totalHeld, fileStatusStatePartlyHeldRun)
	requireFileStatusByteMarker(t, totalSize, markerNotDeterminable)
	assert.NoError(t, checkFileStatusByteInvariants(local, unbrokenRun, totalHeld, totalSize))
}

// TestFileStatusStatePartlyHeldRangeGaps distinguishes the prefix available
// from the total bytes held when a later cached range follows a gap.
func TestFileStatusStatePartlyHeldRangeGaps(t *testing.T) {
	const chunkSize = 8
	opt := fileStatusStateOpt(vfscommon.CacheModeFull, writeBackDelay)
	opt.HandleCaching = 0
	r, vfs, call := newFileStatusStateRunAtRemoteRoot(t, opt)
	content := strings.Repeat("01234567", 8)
	path := arrangeFileStatusStateRemoteFile(t, r, vfs, "range-gaps.txt", content)
	object, err := r.Fremote.NewObject(context.Background(), "range-gaps.txt")
	require.NoError(t, err)
	seedFileStatusStateCacheRanges(t, vfs, "range-gaps.txt", object,
		fileStatusStateCacheRange{offset: 0, content: []byte(content[:chunkSize])},
		fileStatusStateCacheRange{offset: 2 * chunkSize, content: []byte(content[2*chunkSize : 3*chunkSize])},
	)

	doc := fileStatusStateCall(t, call, fs.ConfigString(r.Fremote), []string{path})
	result := fileStatusStateResult(t, fileStatusStateStatuses(t, doc), path)
	local, _ := requireFileStatusStatePair(t, result)
	assert.Equal(t, statusPartlyHeld, local)
	unbrokenRun, totalHeld, totalSize := fileStatusByteInfo(t, result)
	requireFileStatusByteNumber(t, unbrokenRun, chunkSize)
	requireFileStatusByteNumber(t, totalHeld, 2*chunkSize)
	requireFileStatusByteNumber(t, totalSize, int64(len(content)))
	assert.Greater(t, totalHeld.Number, unbrokenRun.Number)
	assert.NoError(t, checkFileStatusByteInvariants(local, unbrokenRun, totalHeld, totalSize))
}

// TestFileStatusStatePartlyHeldSuffixOnlyRange allows a file to be partly held
// when the cache contains bytes only after offset zero.
func TestFileStatusStatePartlyHeldSuffixOnlyRange(t *testing.T) {
	const chunkSize = 8
	opt := fileStatusStateOpt(vfscommon.CacheModeFull, writeBackDelay)
	opt.HandleCaching = 0
	r, vfs, call := newFileStatusStateRunAtRemoteRoot(t, opt)
	content := strings.Repeat("01234567", 8)
	path := arrangeFileStatusStateRemoteFile(t, r, vfs, "suffix-only.txt", content)
	object, err := r.Fremote.NewObject(context.Background(), "suffix-only.txt")
	require.NoError(t, err)
	seedFileStatusStateCacheRanges(t, vfs, "suffix-only.txt", object,
		fileStatusStateCacheRange{offset: 2 * chunkSize, content: []byte(content[2*chunkSize : 3*chunkSize])},
	)

	doc := fileStatusStateCall(t, call, fs.ConfigString(r.Fremote), []string{path})
	result := fileStatusStateResult(t, fileStatusStateStatuses(t, doc), path)
	local, _ := requireFileStatusStatePair(t, result)
	assert.Equal(t, statusPartlyHeld, local)
	unbrokenRun, totalHeld, totalSize := fileStatusByteInfo(t, result)
	requireFileStatusByteNumber(t, unbrokenRun, 0)
	requireFileStatusByteNumber(t, totalHeld, chunkSize)
	requireFileStatusByteNumber(t, totalSize, int64(len(content)))
	assert.NoError(t, checkFileStatusByteInvariants(local, unbrokenRun, totalHeld, totalSize))
}

// TestFileStatusStateByteInvariantsAcrossRangePatterns checks the endpoint's
// byte quantities against a generated set of discontiguous cache ranges.
func TestFileStatusStateByteInvariantsAcrossRangePatterns(t *testing.T) {
	const (
		chunkSize  = 8
		chunkCount = 8
		fileSize   = chunkSize * chunkCount
	)
	opt := fileStatusStateOpt(vfscommon.CacheModeFull, writeBackDelay)
	opt.HandleCaching = 0
	r, vfs, call := newFileStatusStateRunAtRemoteRoot(t, opt)
	content := strings.Repeat("x", fileSize)
	mount := fs.ConfigString(r.Fremote)
	iteration := 0
	var mismatch string
	property := func(mask uint8) bool {
		iteration++
		name := fmt.Sprintf("range-property-%d.txt", iteration)
		path := arrangeFileStatusStateRemoteFile(t, r, vfs, name, content)
		object, err := r.Fremote.NewObject(context.Background(), name)
		require.NoError(t, err)
		cachedRanges := make([]fileStatusStateCacheRange, 0, chunkCount)
		for chunk := range chunkCount {
			if mask&(uint8(1)<<uint(chunk)) == 0 {
				continue
			}
			start := chunk * chunkSize
			cachedRanges = append(cachedRanges, fileStatusStateCacheRange{
				offset:  int64(start),
				content: []byte(content[start : start+chunkSize]),
			})
		}
		seedFileStatusStateCacheRanges(t, vfs, name, object, cachedRanges...)

		doc := fileStatusStateCall(t, call, mount, []string{path})
		result := fileStatusStateResult(t, fileStatusStateStatuses(t, doc), path)
		local, _ := requireFileStatusStatePair(t, result)
		unbrokenRun, totalHeld, totalSize := fileStatusByteInfo(t, result)

		var wantUnbrokenRun, wantTotalHeld int64
		for chunk := 0; chunk < chunkCount; chunk++ {
			if mask&(uint8(1)<<uint(chunk)) == 0 {
				continue
			}
			wantTotalHeld += chunkSize
			if wantUnbrokenRun == int64(chunk*chunkSize) {
				wantUnbrokenRun += chunkSize
			}
		}
		wantLocal := statusPartlyHeld
		if mask == 0 {
			wantLocal = statusNotHeld
		} else if mask == ^uint8(0) {
			wantLocal = statusFullyHeld
		}
		if local != wantLocal || !unbrokenRun.IsNumber || unbrokenRun.Number != wantUnbrokenRun ||
			!totalHeld.IsNumber || totalHeld.Number != wantTotalHeld ||
			!totalSize.IsNumber || totalSize.Number != fileSize {
			mismatch = fmt.Sprintf("mask=%08b: got local=%s bytes=%+v/%+v/%+v; want local=%s unbroken=%d total-held=%d total-size=%d",
				mask, local, unbrokenRun, totalHeld, totalSize, wantLocal, wantUnbrokenRun, wantTotalHeld, fileSize)
			return false
		}
		if err := checkFileStatusByteInvariants(local, unbrokenRun, totalHeld, totalSize); err != nil {
			mismatch = fmt.Sprintf("mask=%08b: %v", mask, err)
			return false
		}
		return true
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 16}); err != nil {
		t.Fatalf("byte invariant property failed: %s: %v", mismatch, err)
	}
}

// TestFileStatusStateNotRegularFile checks a path naming a
// symlink on a mount started to show links: such a path has
// no size and no held byte ranges for any content value to
// describe, so the local fact is the distinct value
// not_regular_file and all three byte quantities carry the
// not_measurable marker, the fields still being present so
// the object shape is the one every other reported state
// uses (FR-032). The upload fact is reported on its own
// terms — the mount never wrote the file, so it is
// not_applicable — and the pair maps to the
// not-fully-held-locally exit status, not to a query
// failure and not to a status of its own (FR-027, FR-032).
func TestFileStatusStateNotRegularFile(t *testing.T) {
	opt := vfscommon.Opt
	opt.CacheMode = vfscommon.CacheModeFull
	opt.Links = true
	r, vfs, call := newFileStatusStateRun(t, &opt)
	// A link file on the remote, written before the mount lists
	// its parent so the listing the state read decides existence
	// from holds the entry (T069 listing-only rule): the mount
	// started to show links presents it as the symlink its
	// suffix-less name names, carrying the path the link points at.
	r.WriteObject(context.Background(), "link.rclonelink", "/target/of/link", t1)
	listFileStatusStateRoot(t, vfs)
	path := fileStatusStatePath(r, "link")

	doc := fileStatusStateCall(t, call, fs.ConfigString(r.Fremote), []string{path})
	result := fileStatusStateResult(t, fileStatusStateStatuses(t, doc), path)

	local, upload := requireFileStatusStatePair(t, result)
	assert.Equal(t, statusNotRegularFile, local)
	assert.Equal(t, statusNotApplicable, upload)

	unbrokenRun, totalHeld, totalSize := fileStatusByteInfo(t, result)
	requireFileStatusByteMarker(t, unbrokenRun, markerNotMeasurable)
	requireFileStatusByteMarker(t, totalHeld, markerNotMeasurable)
	requireFileStatusByteMarker(t, totalSize, markerNotMeasurable)

	assert.Equal(t, exitNotFullyHeld, fileStatusExitCode(local, upload, true))
	assert.NoError(t, checkFileStatusByteInvariants(local, unbrokenRun, totalHeld, totalSize))
}

// TestFileStatusStateCacheOffNotRegularFile is the carve-out
// SC-021 prescribes: a non-regular file on a cache-off
// mount started to show links. The cache-off exception of
// FR-027 covers files whose local fact is a content value,
// and this value reports none, so the file keeps the
// not-fully-held-locally status even on a mount that keeps
// no cache, where every content value takes the
// upload-not-complete status instead. Whether an entry is a
// link is a property of the mount being started to show
// links, independent of the cache mode, so the value is
// reachable here at all.
func TestFileStatusStateCacheOffNotRegularFile(t *testing.T) {
	opt := vfscommon.Opt
	opt.CacheMode = vfscommon.CacheModeOff
	opt.Links = true
	r, vfs, call := newFileStatusStateRun(t, &opt)
	// Written before the mount lists its parent so the listing
	// the state read decides existence from holds the entry
	// (T069 listing-only rule), the same order the
	// TestFileStatusStateNotRegularFile fixture uses.
	r.WriteObject(context.Background(), "link.rclonelink", "/target/of/link", t1)
	listFileStatusStateRoot(t, vfs)
	path := fileStatusStatePath(r, "link")

	doc := fileStatusStateCall(t, call, fs.ConfigString(r.Fremote), []string{path})
	result := fileStatusStateResult(t, fileStatusStateStatuses(t, doc), path)

	local, upload := requireFileStatusStatePair(t, result)
	assert.Equal(t, statusNotRegularFile, local)
	assert.Equal(t, statusNotTracked, upload)

	unbrokenRun, totalHeld, totalSize := fileStatusByteInfo(t, result)
	requireFileStatusByteMarker(t, unbrokenRun, markerNotMeasurable)
	requireFileStatusByteMarker(t, totalHeld, markerNotMeasurable)
	requireFileStatusByteMarker(t, totalSize, markerNotMeasurable)

	// The carve-out: 11, not the 12 every content value on
	// this mount takes.
	assert.Equal(t, exitNotFullyHeld, fileStatusExitCode(local, upload, false))
	assert.NoError(t, checkFileStatusByteInvariants(local, unbrokenRun, totalHeld, totalSize))
}

// TestFileStatusStateMetadataEntry reports a synthesized metadata file from
// its readable in-memory content, even though it has no remote cache item.
func TestFileStatusStateMetadataEntry(t *testing.T) {
	opt := fileStatusStateOpt(vfscommon.CacheModeFull, writeBackDelay)
	opt.MetadataExtension = ".meta"
	r, vfs, call := newFileStatusStateRunAtRemoteRoot(t, opt)
	const sourceName = "metadata-source.txt"
	r.WriteObject(context.Background(), sourceName, "source content", t1)
	listFileStatusStateRoot(t, vfs)

	metadataName := sourceName + opt.MetadataExtension
	metadataPath := fileStatusStatePath(r, metadataName)
	doc := fileStatusStateCall(t, call, fs.ConfigString(r.Fremote), []string{metadataPath})
	result := fileStatusStateResult(t, fileStatusStateStatuses(t, doc), metadataPath)

	metadataNode, err := vfs.Stat(metadataName)
	require.NoError(t, err)
	require.True(t, metadataNode.IsFile())
	assert.True(t, metadataNode.Mode().IsRegular())
	metadataSize := metadataNode.Size()
	require.GreaterOrEqual(t, metadataSize, int64(0))

	local, upload := requireFileStatusStatePair(t, result)
	assert.Equal(t, statusFullyHeld, local)
	assert.Equal(t, statusNotApplicable, upload)
	unbrokenRun, totalHeld, totalSize := fileStatusByteInfo(t, result)
	requireFileStatusByteNumber(t, unbrokenRun, metadataSize)
	requireFileStatusByteNumber(t, totalHeld, metadataSize)
	requireFileStatusByteNumber(t, totalSize, metadataSize)
	assert.NoError(t, checkFileStatusByteInvariants(local, unbrokenRun, totalHeld, totalSize))
}

// TestFileStatusStateLinksDisabled reports a link-suffixed remote entry as an
// ordinary file when the VFS was not started with link translation enabled.
func TestFileStatusStateLinksDisabled(t *testing.T) {
	opt := fileStatusStateOpt(vfscommon.CacheModeFull, writeBackDelay)
	opt.Links = false
	r, vfs, call := newFileStatusStateRunAtRemoteRoot(t, opt)
	linkName := "shortcut" + fs.LinkSuffix
	linkContent := "target/that/is/not-followed"
	path := arrangeFileStatusStateCachedFile(t, r, vfs, linkName, linkContent)

	node, err := vfs.Stat(linkName)
	require.NoError(t, err)
	assert.True(t, node.Mode().IsRegular())

	doc := fileStatusStateCall(t, call, fs.ConfigString(r.Fremote), []string{path})
	result := fileStatusStateResult(t, fileStatusStateStatuses(t, doc), path)
	local, _ := requireFileStatusStatePair(t, result)
	assert.Equal(t, statusFullyHeld, local)
	unbrokenRun, totalHeld, totalSize := fileStatusByteInfo(t, result)
	requireFileStatusByteNumber(t, unbrokenRun, int64(len(linkContent)))
	requireFileStatusByteNumber(t, totalHeld, int64(len(linkContent)))
	requireFileStatusByteNumber(t, totalSize, int64(len(linkContent)))
	assert.NoError(t, checkFileStatusByteInvariants(local, unbrokenRun, totalHeld, totalSize))
}

// TestFileStatusStateCallerPermissionsDoNotChangeStatus verifies that a VFS
// entry without read permission bits still receives its ordinary mount-state
// answer; caller permissions are not part of that answer.
func TestFileStatusStateCallerPermissionsDoNotChangeStatus(t *testing.T) {
	opt := fileStatusStateOpt(vfscommon.CacheModeFull, writeBackDelay)
	opt.FilePerms = 0
	r, vfs, call := newFileStatusStateRunAtRemoteRoot(t, opt)
	content := "mount-visible metadata"
	path := arrangeFileStatusStateCachedFile(t, r, vfs, "no-read-bits.txt", content)

	node, err := vfs.Stat("no-read-bits.txt")
	require.NoError(t, err)
	assert.Zero(t, node.Mode().Perm()&0444)

	doc := fileStatusStateCall(t, call, fs.ConfigString(r.Fremote), []string{path})
	result := fileStatusStateResult(t, fileStatusStateStatuses(t, doc), path)
	local, _ := requireFileStatusStatePair(t, result)
	assert.Equal(t, statusFullyHeld, local)
	unbrokenRun, totalHeld, totalSize := fileStatusByteInfo(t, result)
	requireFileStatusByteNumber(t, unbrokenRun, int64(len(content)))
	requireFileStatusByteNumber(t, totalHeld, int64(len(content)))
	requireFileStatusByteNumber(t, totalSize, int64(len(content)))
	assert.NoError(t, checkFileStatusByteInvariants(local, unbrokenRun, totalHeld, totalSize))
}

// TestFileStatusStateCachedFileDuringHandleGracePeriod reports a closed file
// while the VFS still retains its cache handle for the configured grace period.
func TestFileStatusStateCachedFileDuringHandleGracePeriod(t *testing.T) {
	opt := fileStatusStateOpt(vfscommon.CacheModeFull, writeBackDelay)
	opt.HandleCaching = fs.Duration(10 * time.Second)
	r, vfs, call := newFileStatusStateRunAtRemoteRoot(t, opt)
	content := "cached through a handle that has just closed"
	path := arrangeFileStatusStateCachedFile(t, r, vfs, "grace-period.txt", content)

	doc := fileStatusStateCall(t, call, fs.ConfigString(r.Fremote), []string{path})
	result := fileStatusStateResult(t, fileStatusStateStatuses(t, doc), path)
	local, _ := requireFileStatusStatePair(t, result)
	assert.Equal(t, statusFullyHeld, local)
	unbrokenRun, totalHeld, totalSize := fileStatusByteInfo(t, result)
	requireFileStatusByteNumber(t, unbrokenRun, int64(len(content)))
	requireFileStatusByteNumber(t, totalHeld, int64(len(content)))
	requireFileStatusByteNumber(t, totalSize, int64(len(content)))
	assert.NoError(t, checkFileStatusByteInvariants(local, unbrokenRun, totalHeld, totalSize))
}

// TestFileStatusStateDirectory checks a path naming a
// directory: a directory is a failure to answer, distinct
// from every value of either fact, carrying the
// directory_not_file code as a per-path result inside the
// 200 response alongside the results that succeeded (FR-006,
// FR-012). It is a query failure, so the command exits with
// the query failure status 1 — not the 3 the published list
// already carries for a directory, which would give that
// number two meanings (FR-027).
func TestFileStatusStateDirectory(t *testing.T) {
	r, vfs, call := newFileStatusStateRun(t, fileStatusStateOpt(vfscommon.CacheModeFull, writeBackDelay))
	// The remote holds a directory once any file in it is
	// written, and the mount holds the entry once its parent
	// is listed through the mount.
	r.WriteObject(context.Background(), "subdir/file.txt", "content", t1)
	listFileStatusStateRoot(t, vfs)
	path := fileStatusStatePath(r, "subdir")

	doc := fileStatusStateCall(t, call, fs.ConfigString(r.Fremote), []string{path})
	result := fileStatusStateResult(t, fileStatusStateStatuses(t, doc), path)

	assert.Equal(t, path, result["path"])
	perPathError, ok := result["error"].(map[string]any)
	require.True(t, ok, "the result is not an error result: %v", result)
	assert.Equal(t, "directory_not_file", perPathError["code"])
	assert.Contains(t, perPathError, "message")
	for _, field := range []string{"local", "upload", "bytes"} {
		assert.NotContains(t, result, field, "an error result carries no state")
	}
	assert.Equal(t, exitQueryFailed, fileStatusExitCodeForCode("directory_not_file"))
}

// TestFileStatusStateNotFound checks a path naming no entry
// the mount can see: the mount holds the parent's listing,
// so the entry's absence is a positive statement the mount
// can make, and the path carries the not_found code as a
// per-path result inside the 200 response — never the
// not_held value, which would assert the entry exists and
// has no local copy, the misleading negative FR-008 exists
// to prevent (FR-006, FR-033). It is a query failure, so
// the command exits with the query failure status 1 — not
// the 4 the published list already carries for a missing
// file (FR-027).
func TestFileStatusStateNotFound(t *testing.T) {
	r, vfs, call := newFileStatusStateRun(t, fileStatusStateOpt(vfscommon.CacheModeFull, writeBackDelay))
	listFileStatusStateRoot(t, vfs)
	path := fileStatusStatePath(r, "no-such-file.txt")

	doc := fileStatusStateCall(t, call, fs.ConfigString(r.Fremote), []string{path})
	result := fileStatusStateResult(t, fileStatusStateStatuses(t, doc), path)

	assert.Equal(t, path, result["path"])
	perPathError, ok := result["error"].(map[string]any)
	require.True(t, ok, "the result is not an error result: %v", result)
	assert.Equal(t, "not_found", perPathError["code"])
	assert.Contains(t, perPathError, "message")
	for _, field := range []string{"local", "upload", "bytes"} {
		assert.NotContains(t, result, field, "an error result carries no state")
	}
	assert.Equal(t, exitQueryFailed, fileStatusExitCodeForCode("not_found"))
}

// TestFileStatusStateMultiPath checks that one call naming
// several paths is answered with one result per requested
// path, an invalid path carrying its own error result
// alongside the results that succeeded, so one bad path
// does not cost the caller every other answer (FR-006). The
// results are matched by path, never by position.
func TestFileStatusStateMultiPath(t *testing.T) {
	opt := vfscommon.Opt
	opt.CacheMode = vfscommon.CacheModeFull
	opt.Links = true
	r, vfs, call := newFileStatusStateRun(t, &opt)
	r.WriteObject(context.Background(), "subdir/file.txt", "content", t1)
	r.WriteObject(context.Background(), "link.rclonelink", "/target/of/link", t1)
	content := "the one good file in the call"
	cached := arrangeFileStatusStateCachedFile(t, r, vfs, "good-file.txt", content)
	listFileStatusStateRoot(t, vfs)

	directory := fileStatusStatePath(r, "subdir")
	link := fileStatusStatePath(r, "link")
	absent := fileStatusStatePath(r, "no-such-file.txt")

	doc := fileStatusStateCall(t, call, fs.ConfigString(r.Fremote), []string{cached, directory, link, absent})
	statuses := fileStatusStateStatuses(t, doc)
	require.Len(t, statuses, 4)

	result := fileStatusStateResult(t, statuses, cached)
	local, upload := requireFileStatusStatePair(t, result)
	assert.Equal(t, statusFullyHeld, local)
	assert.Equal(t, statusNotApplicable, upload)

	result = fileStatusStateResult(t, statuses, directory)
	perPathError, ok := result["error"].(map[string]any)
	require.True(t, ok, "the result is not an error result: %v", result)
	assert.Equal(t, "directory_not_file", perPathError["code"])

	result = fileStatusStateResult(t, statuses, link)
	local, upload = requireFileStatusStatePair(t, result)
	assert.Equal(t, statusNotRegularFile, local)
	assert.Equal(t, statusNotApplicable, upload)

	result = fileStatusStateResult(t, statuses, absent)
	perPathError, ok = result["error"].(map[string]any)
	require.True(t, ok, "result is not an error result: %v", result)
	assert.Equal(t, "not_found", perPathError["code"])
}

// TestFileStatusStateUnknownParameterIgnored checks that an
// unknown parameter is not a failure at all: the endpoint is
// read-only in the way of its read-only siblings, which
// ignore unknown parameters, so the request is answered per
// path (FR-006).
func TestFileStatusStateUnknownParameterIgnored(t *testing.T) {
	r, vfs, call := newFileStatusStateRun(t, fileStatusStateOpt(vfscommon.CacheModeFull, writeBackDelay))
	listFileStatusStateRoot(t, vfs)
	content := "content"
	path := arrangeFileStatusStateRemoteFile(t, r, vfs, "never-written.txt", content)

	out, err := call.Fn(context.Background(), rc.Params{
		"fs":      fs.ConfigString(r.Fremote),
		"paths":   []string{path},
		"unknown": "ignored",
	})
	require.NoError(t, err)
	b, err := json.Marshal(out)
	require.NoError(t, err)
	var doc map[string]any
	require.NoError(t, json.Unmarshal(b, &doc))
	statuses := fileStatusStateStatuses(t, doc)
	require.Len(t, statuses, 1)
	assert.Equal(t, path, statuses[0]["path"])
}

// TestFileStatusStateSingleObservationInstant checks FR-022:
// the local-cache fact and the upload fact are two
// independent values, both present for every reported file,
// never collapsed into a single combined status. Both facts
// are captured at a single point in the request — the mount
// observes the file's cached state once and derives both
// facts from that observation — so a reported pair always
// describes one moment; the shape that single observation
// produces is both facts present, whatever the state, which
// is what these fixtures assert over a file the mount never
// wrote, a file it holds in full and a file it holds in
// part.
func TestFileStatusStateSingleObservationInstant(t *testing.T) {
	r, vfs, call := newFileStatusStateRun(t, fileStatusStateOpt(vfscommon.CacheModeFull, writeBackDelay))
	listFileStatusStateRoot(t, vfs)
	// A second VFS over the shared test remote joins the same
	// active name below, so both mounts are addressed by their
	// indexed names.
	mount := fileStatusStateIndexedMount(r, 0)
	neverWritten := arrangeFileStatusStateRemoteFile(t, r, vfs, "never-written.txt", "content")
	fullyCached := arrangeFileStatusStateCachedFile(t, r, vfs, "fully-cached.txt", "content")

	// A mount that downloads in small chunks, so a file read
	// part way is held in part.
	opt := vfscommon.Opt
	opt.CacheMode = vfscommon.CacheModeFull
	opt.ChunkSize = fileStatusStatePartlyHeldRun
	r2, vfs2, call2 := newFileStatusStateRun(t, &opt)
	partlyCached := arrangeFileStatusStatePartlyCachedFile(t, r2, vfs2, "partly-cached.txt", "0123456789abcdefghij")

	for _, query := range []struct {
		call  *rc.Call
		mount string
		paths []string
	}{
		{call, mount, []string{neverWritten, fullyCached}},
		{call2, fileStatusStateIndexedMount(r2, 1), []string{partlyCached}},
	} {
		doc := fileStatusStateCall(t, query.call, query.mount, query.paths)
		statuses := fileStatusStateStatuses(t, doc)
		for _, path := range query.paths {
			result := fileStatusStateResult(t, statuses, path)
			local, upload := requireFileStatusStatePair(t, result)
			assert.Contains(t, localStates, local, "path=%s", path)
			assert.Contains(t, uploadStates, upload, "path=%s", path)
			// The markers never appear as a fact's value, and
			// a fact's value never appears as a marker: the
			// two namespaces are disjoint (FR-002).
			assert.NotContains(t, localStates, markerNotDeterminable)
			assert.NotContains(t, uploadStates, markerNotMeasurable)
		}
	}
}

// TestFileStatusStateUnreachableFixtures pins the contract
// for the states whose fixtures need fault-injection hooks:
// download_failed, underivable, uploading and upload_failed. Each
// case names the hook its fixture arranges, and asserts the state,
// the byte quantities and the exit status FR-027 maps the pair to.
//
// The derivation (vfs/file_status.go fileStatusOf, T020) is
// implemented; the fault-injection hooks live in
// file_status_failure_test.go and file_status_observation_test.go.
// The cases below arrange those hooks — the error-injecting
// remote, the mid-reset barrier and the release-gated upload
// steps — and assert the state, the byte quantities and the
// exit status FR-027 maps each pair to.
func TestFileStatusStateUnreachableFixtures(t *testing.T) {
	content := "0123456789"

	t.Run("download_failed", func(t *testing.T) {
		// A remote whose read of the file fails after caching
		// part of it — the error-injecting remote of
		// TestFileStatusPartialDownloadFailureClearsAfterSuccessfulRetry.
		// The contract: the local fact takes the failure value, not partly_held, and
		// the held quantities still describe the bytes that
		// did arrive, so a caller can tell a mount that lost
		// data from one still fetching (FR-028). A failed
		// download is a value of the local fact, so the file
		// is not fully held and takes the not-fully-held-
		// locally status, never the upload-not-complete one
		// (FR-027).
		const name = "failed-download.txt"
		const partial = 7
		r, fixture := newFileStatusFailureFixture(t, fileStatusStateOpt(vfscommon.CacheModeFull, fileStatusFailureWriteBackDelay))
		r.WriteObject(context.Background(), name, content, t1)
		listFileStatusStateRoot(t, fixture.vfs)
		path := fileStatusStatePath(r, name)
		faultErr := errors.New("injected partial download failure")
		fixture.control.failDownloadPlan(name,
			fileStatusDownloadFault{after: partial, err: faultErr},
			fileStatusDownloadFault{err: faultErr},
		)

		_, _ = fixture.vfs.ReadFile(name)
		select {
		case remote := <-fixture.control.downloadErrors:
			assert.Equal(t, name, remote)
		case <-time.After(fileStatusFailureWaitTimeout):
			t.Fatal("the injected download error was not reached")
		}

		result := fileStatusFailureResult(t, r, fixture, path)

		local, upload := requireFileStatusStatePair(t, result)
		assert.Equal(t, statusDownloadFailed, local)
		assert.Equal(t, statusNotApplicable, upload)

		unbrokenRun, totalHeld, totalSize := fileStatusByteInfo(t, result)
		requireFileStatusByteNumber(t, unbrokenRun, partial)
		requireFileStatusByteNumber(t, totalHeld, partial)
		requireFileStatusByteNumber(t, totalSize, int64(len(content)))

		assert.Equal(t, exitNotFullyHeld, fileStatusExitCode(local, upload, true))
		assert.NoError(t, checkFileStatusByteInvariants(local, unbrokenRun, totalHeld, totalSize))
	})

	t.Run("underivable", func(t *testing.T) {
		// The file's cache record mid-rewrite by a concurrent
		// reset when the read lands — the reset hook holding
		// the record open of
		// TestFileStatusObservationResetDoesNotWait. The contract: the mount
		// holds a cache entry it cannot derive the held bytes
		// from, the one condition that produces underivable
		// (FR-004), so both held quantities carry the
		// not_determinable marker while the total size stays
		// a number or its own marker (FR-002). The read does
		// not wait for the reset (FR-026), and the pair maps
		// to the not-fully-held-locally status (FR-027).
		r, vfs, call := newFileStatusStateRun(t, fileStatusStateOpt(vfscommon.CacheModeFull, writeBackDelay))
		path := holdFileStatusStateResetInFlight(t, r, vfs, "mid-reset.txt", content)

		doc := fileStatusStateCall(t, call, fs.ConfigString(r.Fremote), []string{path})
		result := fileStatusStateResult(t, fileStatusStateStatuses(t, doc), path)

		local, upload := requireFileStatusStatePair(t, result)
		assert.Equal(t, statusUnderivable, local)
		assert.Contains(t, uploadStates, upload)

		unbrokenRun, totalHeld, totalSize := fileStatusByteInfo(t, result)
		requireFileStatusByteMarker(t, unbrokenRun, markerNotDeterminable)
		requireFileStatusByteMarker(t, totalHeld, markerNotDeterminable)

		assert.Equal(t, exitNotFullyHeld, fileStatusExitCode(local, upload, true))
		assert.NoError(t, checkFileStatusByteInvariants(local, unbrokenRun, totalHeld, totalSize))
	})

	t.Run("uploading", func(t *testing.T) {
		// The file's upload in flight when the read lands — a
		// slow remote keeping the writeback upload open, the
		// release-gated upload step of TestFileStatusUploadInFlight.
		// The contract: the upload fact is uploading, the
		// writeback item being transferred, while the local
		// fact reports the written content held in full. The
		// pair maps to the upload-not-complete status, the
		// upload fact being outstanding work (FR-027).
		releaseUpload := make(chan struct{})
		var releaseOnce sync.Once
		unblockUpload := func() { releaseOnce.Do(func() { close(releaseUpload) }) }
		defer unblockUpload()

		r, fixture := newFileStatusFailureFixture(t, fileStatusStateOpt(vfscommon.CacheModeFull, fileStatusFailureWriteBackDelay))
		fixture.control.setUploadSteps(fileStatusUploadStep{release: releaseUpload})
		writeFileStatusStateThrough(t, fixture.vfs, "in-flight.txt", content)
		path := fileStatusStatePath(r, "in-flight.txt")
		drainFileStatusStateWriteback(t, fixture.vfs)
		attempt := receiveFileStatusUploadAttempt(t, fixture.control)
		assert.Contains(t, attempt.remote, "in-flight.txt")

		result := fileStatusFailureResult(t, r, fixture, path)

		local, upload := requireFileStatusStatePair(t, result)
		assert.Equal(t, statusFullyHeld, local)
		assert.Equal(t, statusUploading, upload)

		unbrokenRun, totalHeld, totalSize := fileStatusByteInfo(t, result)
		requireFileStatusByteNumber(t, unbrokenRun, int64(len(content)))
		requireFileStatusByteNumber(t, totalHeld, int64(len(content)))
		requireFileStatusByteNumber(t, totalSize, int64(len(content)))

		assert.Equal(t, exitUploadNotComplete, fileStatusExitCode(local, upload, true))
		assert.NoError(t, checkFileStatusByteInvariants(local, unbrokenRun, totalHeld, totalSize))

		unblockUpload()
		waitForFileStatusUploadState(t, r, fixture, path, "uploaded")
	})

	t.Run("upload_failed", func(t *testing.T) {
		// The file's upload failed on this mount instance — a
		// remote whose writes fail, the trapped upload error of
		// TestFileStatusUploadFailureRetriesAndClearsAfterSuccess.
		// The contract: the upload fact is upload_failed, a failure reported
		// for the life of the mount that observed it and not
		// carried across a restart, while the local fact
		// reports the written content held in full. A failed
		// upload never produces the query-failed status,
		// because the query was answered and the answer is
		// that the file is outstanding work (FR-027, FR-028).
		r, fixture := newFileStatusFailureFixture(t, fileStatusStateOpt(vfscommon.CacheModeFull, fileStatusFailureWriteBackDelay))
		fixture.control.setUploadSteps(fileStatusUploadStep{err: errors.New("injected upload failure")})
		writeFileStatusStateThrough(t, fixture.vfs, "failed-upload.txt", content)
		path := fileStatusStatePath(r, "failed-upload.txt")

		drainFileStatusStateWriteback(t, fixture.vfs)
		attempt := receiveFileStatusUploadAttempt(t, fixture.control)
		assert.Contains(t, attempt.remote, "failed-upload.txt")
		result := waitForFileStatusUploadSettled(t, r, fixture, path)

		local, upload := requireFileStatusStatePair(t, result)
		assert.Equal(t, statusFullyHeld, local)
		assert.Equal(t, statusUploadFailed, upload)

		unbrokenRun, totalHeld, totalSize := fileStatusByteInfo(t, result)
		requireFileStatusByteNumber(t, unbrokenRun, int64(len(content)))
		requireFileStatusByteNumber(t, totalHeld, int64(len(content)))
		requireFileStatusByteNumber(t, totalSize, int64(len(content)))

		assert.Equal(t, exitUploadNotComplete, fileStatusExitCode(local, upload, true))
		assert.NoError(t, checkFileStatusByteInvariants(local, unbrokenRun, totalHeld, totalSize))
	})
}

// TestFileStatusStateUnderivableNonBlocking checks FR-026: a
// read of a file whose cached contents are being concurrently
// reset returns without waiting for the reset, reporting the
// local fact as underivable with both held quantities
// not_determinable rather than blocking on the file's reset
// condition — the ceiling of SC-010 holds while a reset is
// in progress, so a file-manager overlay asking about every
// entry it draws is never held up by one.
//
// The deterministic mid-reset arrangement lives in
// holdFileStatusStateResetInFlight, the reset hook of
// TestFileStatusObservationResetDoesNotWait; this case pins the
// non-blocking contract while the reset is held open.
func TestFileStatusStateUnderivableNonBlocking(t *testing.T) {
	r, vfs, call := newFileStatusStateRun(t, fileStatusStateOpt(vfscommon.CacheModeFull, writeBackDelay))
	content := "0123456789"
	path := holdFileStatusStateResetInFlight(t, r, vfs, "mid-reset.txt", content)

	start := time.Now()
	doc := fileStatusStateCall(t, call, fs.ConfigString(r.Fremote), []string{path})
	// The read never waits on the file's reset or transfer
	// condition: it returns within the ceiling's order of
	// magnitude, however busy the mount is (SC-010 bounds
	// one read at 100ms; the bound here is a sanity check
	// that the read returns at all, not the ceiling itself).
	assert.Less(t, time.Since(start), time.Second)

	result := fileStatusStateResult(t, fileStatusStateStatuses(t, doc), path)
	local, upload := requireFileStatusStatePair(t, result)
	assert.Equal(t, statusUnderivable, local)
	assert.Contains(t, uploadStates, upload)

	unbrokenRun, totalHeld, _ := fileStatusByteInfo(t, result)
	requireFileStatusByteMarker(t, unbrokenRun, markerNotDeterminable)
	requireFileStatusByteMarker(t, totalHeld, markerNotDeterminable)

	assert.Equal(t, exitNotFullyHeld, fileStatusExitCode(local, upload, true))
}

// TestFileStatusStateByteInvariantsProperty checks the
// byte-quantity invariant checker of data-model.md over
// generated quantities: the invariants admit exactly the
// chains 0 <= unbroken_run <= total_held <= total_size, with
// the head of the chain strict and non-empty where the state
// is partly_held, and the marker carve-outs hold per state —
// so the checker's verdict flips with each inequality and
// each marker, and no fact value ever appears as a marker.
func TestFileStatusStateByteInvariantsProperty(t *testing.T) {
	quantity := func(n int64) fileStatusByteQuantity {
		return fileStatusByteQuantity{Number: n, IsNumber: true}
	}
	determinable := fileStatusByteQuantity{Marker: markerNotDeterminable}
	measurable := fileStatusByteQuantity{Marker: markerNotMeasurable}

	// Over generated byte counts the checker accepts the
	// download_failed chain always, and accepts partly_held
	// when bytes are held and its prefix is shorter than the
	// known size. The prefix may be zero when only later ranges
	// are held.
	chain := func(unbrokenRun, totalHeld, totalSize int64) bool {
		if unbrokenRun < 0 || totalHeld < 0 || totalSize < 0 ||
			unbrokenRun > totalHeld || totalHeld > totalSize {
			return true // a generation outside the chain says nothing
		}
		if err := checkFileStatusByteInvariants(statusDownloadFailed, quantity(unbrokenRun), quantity(totalHeld), quantity(totalSize)); err != nil {
			return false
		}
		err := checkFileStatusByteInvariants(statusPartlyHeld, quantity(unbrokenRun), quantity(totalHeld), quantity(totalSize))
		if totalHeld > 0 && unbrokenRun < totalSize {
			return err == nil
		}
		return err != nil
	}
	require.NoError(t, quick.Check(chain, nil))

	// fully_held admits the degenerate chain whose three
	// quantities are equal, and the same chain with the total
	// size not_determinable when the two held quantities are
	// equal — the carve-out — but nothing else.
	fullyHeld := func(unbrokenRun, totalHeld int64, sizeDeterminable bool) bool {
		if unbrokenRun < 0 || totalHeld < 0 {
			return true
		}
		totalSize := quantity(totalHeld)
		if !sizeDeterminable {
			totalSize = determinable
		}
		err := checkFileStatusByteInvariants(statusFullyHeld, quantity(unbrokenRun), quantity(totalHeld), totalSize)
		if unbrokenRun == totalHeld {
			return err == nil
		}
		return err != nil
	}
	require.NoError(t, quick.Check(fullyHeld, nil))

	// not_held admits a zero run and zero total held with any
	// total size, number or not_determinable, and nothing
	// else.
	notHeld := func(totalHeld int64, sizeDeterminable bool) bool {
		if totalHeld < 0 {
			return true
		}
		totalSize := quantity(totalHeld)
		if !sizeDeterminable {
			totalSize = determinable
		}
		err := checkFileStatusByteInvariants(statusNotHeld, quantity(0), quantity(totalHeld), totalSize)
		return (totalHeld == 0) == (err == nil)
	}
	require.NoError(t, quick.Check(notHeld, nil))

	// The marker carve-outs: underivable carries
	// not_determinable on both held quantities and rejects a
	// number there; not_regular_file carries not_measurable on
	// all three and rejects anything else; and no state
	// accepts a value of either fact as a marker, the two
	// namespaces being disjoint (FR-002).
	for _, tc := range []struct {
		local       string
		unbrokenRun fileStatusByteQuantity
		totalHeld   fileStatusByteQuantity
		totalSize   fileStatusByteQuantity
		wantErr     bool
	}{
		{statusUnderivable, determinable, determinable, quantity(1024), false},
		{statusUnderivable, determinable, determinable, determinable, false},
		{statusUnderivable, quantity(0), determinable, quantity(1024), true},
		{statusUnderivable, determinable, quantity(0), quantity(1024), true},
		{statusUnderivable, determinable, determinable, measurable, true},
		{statusNotRegularFile, measurable, measurable, measurable, false},
		{statusNotRegularFile, measurable, measurable, quantity(0), true},
		{statusNotRegularFile, measurable, determinable, measurable, true},
		{statusFullyHeld, quantity(1024), quantity(1024), quantity(1024), false},
		{statusFullyHeld, quantity(1024), quantity(1024), determinable, false},
		{statusFullyHeld, quantity(1024), quantity(1024), measurable, true},
		{statusPartlyHeld, quantity(512), quantity(512), quantity(1024), false},
		{statusPartlyHeld, quantity(512), quantity(512), determinable, false},
		{statusPartlyHeld, quantity(512), quantity(768), quantity(1024), false},
		{statusPartlyHeld, quantity(0), quantity(512), quantity(1024), false},
		{statusPartlyHeld, quantity(0), quantity(512), determinable, false},
		{statusPartlyHeld, quantity(0), quantity(0), quantity(1024), true},
		{statusPartlyHeld, quantity(512), quantity(1024), quantity(1024), false},
		{statusPartlyHeld, quantity(512), quantity(256), quantity(1024), true},
		{statusPartlyHeld, quantity(512), quantity(1024), quantity(512), true},
		{statusDownloadFailed, quantity(0), quantity(0), quantity(1024), false},
		{statusDownloadFailed, quantity(512), quantity(512), quantity(1024), false},
		{statusDownloadFailed, quantity(512), quantity(256), quantity(1024), true},
		{statusDownloadFailed, quantity(512), quantity(512), determinable, false},
		// A value of either fact is not a marker of any
		// quantity, and a marker is not a value of either
		// fact.
		{statusFullyHeld, fileStatusByteQuantity{Marker: statusFullyHeld}, quantity(0), quantity(0), true},
		{statusNotRegularFile, measurable, measurable, fileStatusByteQuantity{Marker: statusNotTracked}, true},
		{statusNotHeld, quantity(0), quantity(0), fileStatusByteQuantity{Marker: markerNotMeasurable}, true},
	} {
		err := checkFileStatusByteInvariants(tc.local, tc.unbrokenRun, tc.totalHeld, tc.totalSize)
		if tc.wantErr {
			assert.Error(t, err, "local=%s", tc.local)
		} else {
			assert.NoError(t, err, "local=%s", tc.local)
		}
	}
}

// TestFileStatusStateByteInvariantsFromEndpoint runs the
// byte-quantity invariant checker of data-model.md against
// the quantities the endpoint returns, over every state a
// fixture can arrange: a file the mount never wrote, a file
// it holds in full, a file it holds in part, a file it
// cached by reading and a symlink on a mount started to show
// links. The endpoint returns real quantities (vfs/file_status.go
// fileStatusOf); failures here expose Phase 8 byte-quantity gaps
// for the implementation waves to close.
func TestFileStatusStateByteInvariantsFromEndpoint(t *testing.T) {
	// A file the mount never wrote, on a mount that keeps a
	// cache: not_held with a true zero held and the file's
	// size.
	r, vfs, call := newFileStatusStateRun(t, fileStatusStateOpt(vfscommon.CacheModeFull, writeBackDelay))
	listFileStatusStateRoot(t, vfs)
	// Two more VFSes over the shared test remote join the same
	// active name below, so each mount is addressed by its
	// indexed name.
	mount := fileStatusStateIndexedMount(r, 0)
	neverWritten := arrangeFileStatusStateRemoteFile(t, r, vfs, "never-written.txt", "0123456789")
	fullyCached := arrangeFileStatusStateCachedFile(t, r, vfs, "fully-cached.txt", "0123456789")

	doc := fileStatusStateCall(t, call, mount, []string{neverWritten, fullyCached})
	statuses := fileStatusStateStatuses(t, doc)
	for _, path := range []string{neverWritten, fullyCached} {
		result := fileStatusStateResult(t, statuses, path)
		local, _ := requireFileStatusStatePair(t, result)
		unbrokenRun, totalHeld, totalSize := fileStatusByteInfo(t, result)
		assert.NoError(t, checkFileStatusByteInvariants(local, unbrokenRun, totalHeld, totalSize), "path=%s", path)
	}

	// A file the mount holds in part, on a mount that
	// downloads in small chunks.
	opt := vfscommon.Opt
	opt.CacheMode = vfscommon.CacheModeFull
	opt.ChunkSize = fileStatusStatePartlyHeldRun
	r2, vfs2, call2 := newFileStatusStateRun(t, &opt)
	partlyCached := arrangeFileStatusStatePartlyCachedFile(t, r2, vfs2, "partly-cached.txt", "0123456789abcdefghijklmnopqrstuvwxyz")

	doc2 := fileStatusStateCall(t, call2, fileStatusStateIndexedMount(r2, 1), []string{partlyCached})
	statuses2 := fileStatusStateStatuses(t, doc2)
	result := fileStatusStateResult(t, statuses2, partlyCached)
	local, _ := requireFileStatusStatePair(t, result)
	unbrokenRun, totalHeld, totalSize := fileStatusByteInfo(t, result)
	assert.NoError(t, checkFileStatusByteInvariants(local, unbrokenRun, totalHeld, totalSize))

	// A symlink on a mount started to show links: a
	// non-regular file, whose quantities are all the
	// not_measurable marker.
	opt2 := vfscommon.Opt
	opt2.CacheMode = vfscommon.CacheModeFull
	opt2.Links = true
	r3, vfs3, call3 := newFileStatusStateRun(t, &opt2)
	// Written before the mount lists its parent so the listing
	// the state read decides existence from holds the entry
	// (T069 listing-only rule).
	r3.WriteObject(context.Background(), "link.rclonelink", "/target/of/link", t1)
	listFileStatusStateRoot(t, vfs3)
	link := fileStatusStatePath(r3, "link")

	doc3 := fileStatusStateCall(t, call3, fileStatusStateIndexedMount(r3, 2), []string{link})
	statuses3 := fileStatusStateStatuses(t, doc3)
	result = fileStatusStateResult(t, statuses3, link)
	local, _ = requireFileStatusStatePair(t, result)
	unbrokenRun, totalHeld, totalSize = fileStatusByteInfo(t, result)
	assert.NoError(t, checkFileStatusByteInvariants(local, unbrokenRun, totalHeld, totalSize))

	_ = vfs
	_ = vfs2
}
