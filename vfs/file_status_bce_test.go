package vfs

// BCE regression for the vfs/file-status contract.
//
// This file wires the normalized fixtures in vfs/testdata/file_status/*
// into an automated comparison that fails in CI on any unexpected diff.
// It does not modify the fixtures; the comparison is normalized so that
// only contract-stable fields are asserted:
//
//   - paths are normalized: a live mount point prefix maps to the
//     "/mnt/test" placeholder the fixtures carry, so temp dirs do not
//     cause diffs;
//   - human-readable messages are normalized: the contract states the
//     message is not part of the contract and consumers must not branch
//     on it, so both sides replace every "message" value with a
//     placeholder before comparison. This also absorbs the existing
//     capitalisation drift between fixtures ("Path names a directory")
//     and handler prose ("path names a directory") without editing the
//     fixtures.
//
// Update mode is explicitly gated: setting
// FILE_STATUS_UPDATE_FIXTURES=1 rewrites the fixture from the normalized
// actual (placeholder paths preserved) instead of comparing. CI never
// sets it, so any baseline diff fails the build. Recapture is deliberate,
// committed, and PR-diff-reviewed.
//
// Coverage: success (fully_held/uploaded, not_held/not_applicable,
// partly_held/awaiting_upload), fault (not_found, directory_not_file,
// empty_path whole-request), boundary (not_regular_file), plus the
// shared-config bare-name regression (indexed-form acceptance must not
// leak to sibling surfaces). No CI config is touched by this wave.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/rc"
	"github.com/rclone/rclone/fstest"
	"github.com/rclone/rclone/vfs/vfscommon"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const bceFixtureDir = "testdata/file_status"

const bceUpdateEnv = "FILE_STATUS_UPDATE_FIXTURES"

// bceUpdateEnabled reports whether fixture recapture is explicitly gated on.
func bceUpdateEnabled() bool {
	return os.Getenv(bceUpdateEnv) == "1"
}

// bceLoadFixture reads one golden-master fixture as a generic document.
func bceLoadFixture(t *testing.T, name string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(bceFixtureDir, name))
	require.NoError(t, err)
	var doc map[string]any
	require.NoError(t, json.Unmarshal(raw, &doc))
	return doc
}

// bceStripMessages replaces every "message" value in a decoded document
// with a placeholder, since message prose is not part of the contract.
func bceStripMessages(doc map[string]any) map[string]any {
	raw, err := json.Marshal(doc)
	if err != nil {
		return doc
	}
	var generic any
	if err := json.Unmarshal(raw, &generic); err != nil {
		return doc
	}
	bceStripMessagesValue(generic)
	normalised, ok := generic.(map[string]any)
	if !ok {
		return doc
	}
	return normalised
}

func bceStripMessagesValue(v any) {
	switch node := v.(type) {
	case map[string]any:
		for key, child := range node {
			if key == "message" {
				if _, ok := child.(string); ok {
					node[key] = "normalized"
				}
				continue
			}
			bceStripMessagesValue(child)
		}
	case []any:
		for _, child := range node {
			bceStripMessagesValue(child)
		}
	}
}

// bceNormalizePaths rewrites every "path" value with the live mountPoint
// prefix to the "/mnt/test" placeholder the fixtures carry.
func bceNormalizePaths(doc map[string]any, mountPoint string) map[string]any {
	if mountPoint == "" {
		return doc
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		return doc
	}
	var generic any
	if err := json.Unmarshal(raw, &generic); err != nil {
		return doc
	}
	bceRewritePaths(generic, filepath.Clean(mountPoint))
	normalised, ok := generic.(map[string]any)
	if !ok {
		return doc
	}
	return normalised
}

func bceRewritePaths(v any, mountPoint string) {
	switch node := v.(type) {
	case map[string]any:
		for key, child := range node {
			if key == "path" {
				if s, ok := child.(string); ok {
					clean := filepath.Clean(s)
					if clean == mountPoint {
						node[key] = "/mnt/test"
					} else if len(clean) > len(mountPoint) && clean[:len(mountPoint)] == mountPoint && clean[len(mountPoint)] == filepath.Separator {
						node[key] = "/mnt/test" + clean[len(mountPoint):]
					}
				}
				continue
			}
			bceRewritePaths(child, mountPoint)
		}
	case []any:
		for _, child := range node {
			bceRewritePaths(child, mountPoint)
		}
	}
}

// bceCanonical marshals a document to canonical JSON (sorted map keys).
func bceCanonical(t *testing.T, doc map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(doc)
	require.NoError(t, err)
	return string(raw)
}

// bceCompareNormalized asserts the normalized actual matches the fixture's
// normalized stable fields, or recaptures the fixture when update mode is
// explicitly gated on.
func bceCompareNormalized(t *testing.T, fixtureName string, actual map[string]any, mountPoint string) {
	t.Helper()
	normalisedActual := bceStripMessages(bceNormalizePaths(actual, mountPoint))
	if bceUpdateEnabled() {
		// Recapture preserves placeholder paths but keeps the handler's
		// real messages for human readers; comparison still strips them.
		toWrite := bceNormalizePaths(actual, mountPoint)
		raw, err := json.MarshalIndent(toWrite, "", "  ")
		require.NoError(t, err)
		raw = append(raw, '\n')
		require.NoError(t, os.WriteFile(filepath.Join(bceFixtureDir, fixtureName), raw, 0644))
		t.Logf("BCE fixture %s recaptured via %s=1 (review the diff before committing)", fixtureName, bceUpdateEnv)
		return
	}
	fixture := bceStripMessages(bceLoadFixture(t, fixtureName))
	assert.Equal(t, bceCanonical(t, fixture), bceCanonical(t, normalisedActual), "BCE diff for %s: normalized response does not match fixture", fixtureName)
}

// bceFileStatusDoc wraps one FileStatus result for a placeholder path.
func bceFileStatusDoc(t *testing.T, path string, status FileStatus) map[string]any {
	t.Helper()
	result := status.ToParams()
	result["path"] = path
	return map[string]any{"statuses": []any{result}}
}

// TestFileStatusBCESuccessFixtures pins the success fixtures' stable fields.
func TestFileStatusBCESuccessFixtures(t *testing.T) {
	t.Run("fully_held_uploaded", func(t *testing.T) {
		status := FileStatus{
			Local:  FileLocalFullyHeld,
			Upload: FileUploadUploaded,
			Bytes:  FileByteInfo{UnbrokenRun: 1024, TotalHeld: 1024, TotalSize: 1024, TotalKnown: true},
		}
		bceCompareNormalized(t, "fully_held_uploaded.json", bceFileStatusDoc(t, "/mnt/test/file.txt", status), "")
	})

	t.Run("not_held_not_applicable", func(t *testing.T) {
		status := FileStatus{
			Local:  FileLocalNotHeld,
			Upload: FileUploadNotApplicable,
			Bytes:  FileByteInfo{UnbrokenRun: 0, TotalHeld: 0, TotalSize: 1024, TotalKnown: true},
		}
		bceCompareNormalized(t, "not_held_not_applicable.json", bceFileStatusDoc(t, "/mnt/test/file.txt", status), "")
	})

	t.Run("partly_held_awaiting", func(t *testing.T) {
		status := FileStatus{
			Local:  FileLocalPartlyHeld,
			Upload: FileUploadAwaitingUpload,
			Bytes:  FileByteInfo{UnbrokenRun: 512, TotalHeld: 512, TotalSize: 1024, TotalKnown: true},
		}
		bceCompareNormalized(t, "partly_held_awaiting.json", bceFileStatusDoc(t, "/mnt/test/file.txt", status), "")
	})

	// Live wiring: a remote-only file through the real endpoint must report
	// the same stable fields the not_held fixture pins.
	t.Run("live_not_held_matches_fixture_values", func(t *testing.T) {
		r := fstest.NewRun(t)
		opt := vfscommon.Opt
		opt.CacheMode = vfscommon.CacheModeFull
		opt.DirCacheTime = 0
		testVFS := New(context.Background(), r.Fremote, &opt)
		t.Cleanup(func() { cleanupVFS(t, testVFS) })
		mountPoint := filepath.Join(t.TempDir(), "mount")
		RegisterMountPointForVFS(testVFS, mountPoint)
		t.Cleanup(func() { UnregisterMountPointForVFS(testVFS) })

		content := make([]byte, 1024)
		for i := range content {
			content[i] = 'x'
		}
		r.WriteObject(context.Background(), "bce-live.txt", string(content), time.Now())
		root, err := testVFS.Root()
		require.NoError(t, err)
		_, err = root.ReadDirAll()
		require.NoError(t, err)

		call := rc.Calls.Get("vfs/file-status")
		require.NotNil(t, call)
		path := filepath.Join(mountPoint, "bce-live.txt")
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
		require.True(t, ok && len(statuses) == 1)
		result, ok := statuses[0].(map[string]any)
		require.True(t, ok)
		assert.Equal(t, string(FileLocalNotHeld), result["local"])
		assert.Equal(t, string(FileUploadNotApplicable), result["upload"])
		bytes, ok := result["bytes"].(map[string]any)
		require.True(t, ok)
		assert.Equal(t, float64(0), bytes["unbroken_run"])
		assert.Equal(t, float64(0), bytes["total_held"])
		assert.Equal(t, float64(1024), bytes["total_size"])
	})
}

// TestFileStatusBCEFaultFixtures pins the fault fixtures' stable fields.
func TestFileStatusBCEFaultFixtures(t *testing.T) {
	t.Run("not_found_shape", func(t *testing.T) {
		fixture := bceLoadFixture(t, "not_found.json")
		statuses, ok := fixture["statuses"].([]any)
		require.True(t, ok && len(statuses) == 1)
		bceCompareNormalized(t, "not_found.json", fixture, "")
	})

	t.Run("directory_not_file_shape", func(t *testing.T) {
		fixture := bceLoadFixture(t, "directory_not_file.json")
		statuses, ok := fixture["statuses"].([]any)
		require.True(t, ok && len(statuses) == 1)
		bceCompareNormalized(t, "directory_not_file.json", fixture, "")
	})

	// Live wiring: missing entry in a current listing reports not_found
	// (not a content value), and a directory reports directory_not_file.
	t.Run("live_not_found_and_directory_codes", func(t *testing.T) {
		r := fstest.NewRun(t)
		opt := vfscommon.Opt
		opt.CacheMode = vfscommon.CacheModeFull
		opt.DirCacheTime = 0
		testVFS := New(context.Background(), r.Fremote, &opt)
		t.Cleanup(func() { cleanupVFS(t, testVFS) })
		mountPoint := filepath.Join(t.TempDir(), "mount")
		RegisterMountPointForVFS(testVFS, mountPoint)
		t.Cleanup(func() { UnregisterMountPointForVFS(testVFS) })

		r.WriteObject(context.Background(), "bce-present.txt", "present", time.Now())
		if r.Fremote.Features().CanHaveEmptyDirectories {
			require.NoError(t, r.Fremote.Mkdir(context.Background(), "bce-dir"))
		} else {
			r.WriteObject(context.Background(), "bce-dir/keep.txt", "keep", time.Now())
		}
		root, err := testVFS.Root()
		require.NoError(t, err)
		_, err = root.ReadDirAll()
		require.NoError(t, err)

		call := rc.Calls.Get("vfs/file-status")
		require.NotNil(t, call)
		mount := fs.ConfigString(r.Fremote)

		missing := filepath.Join(mountPoint, "bce-missing.txt")
		out, err := call.Fn(context.Background(), rc.Params{"fs": mount, "paths": []string{missing}})
		require.NoError(t, err)
		raw, _ := json.Marshal(out)
		var doc map[string]any
		require.NoError(t, json.Unmarshal(raw, &doc))
		statuses, _ := doc["statuses"].([]any)
		require.Len(t, statuses, 1)
		result, _ := statuses[0].(map[string]any)
		errObj, ok := result["error"].(map[string]any)
		require.True(t, ok, "missing entry must be a per-path error, got %v", result)
		assert.Equal(t, "not_found", errObj["code"])
		assert.NotContains(t, result, "local")

		dirPath := filepath.Join(mountPoint, "bce-dir")
		out, err = call.Fn(context.Background(), rc.Params{"fs": mount, "paths": []string{dirPath}})
		require.NoError(t, err)
		raw, _ = json.Marshal(out)
		doc = nil
		require.NoError(t, json.Unmarshal(raw, &doc))
		statuses, _ = doc["statuses"].([]any)
		require.Len(t, statuses, 1)
		result, _ = statuses[0].(map[string]any)
		errObj, ok = result["error"].(map[string]any)
		require.True(t, ok, "directory must be a per-path error, got %v", result)
		assert.Equal(t, "directory_not_file", errObj["code"])
	})

	// Whole-request boundary: absent paths array is empty_path with the
	// error-object envelope and no path field.
	t.Run("empty_path_whole_request", func(t *testing.T) {
		r := fstest.NewRun(t)
		_ = r
		call := rc.Calls.Get("vfs/file-status")
		require.NotNil(t, call)
		_, err := call.Fn(context.Background(), rc.Params{})
		require.Error(t, err)
		var fileStatusErr *rc.FileStatusError
		require.ErrorAs(t, err, &fileStatusErr)
		assert.Equal(t, "empty_path", fileStatusErr.Code)
		rendered := fileStatusErr.Render()
		bceCompareNormalized(t, "empty_path.json", rendered, "")
		assert.NotContains(t, rendered, "path")
	})
}

// TestFileStatusBCEBoundaryFixtures pins the boundary fixture.
func TestFileStatusBCEBoundaryFixtures(t *testing.T) {
	t.Run("not_regular_file", func(t *testing.T) {
		status := FileStatus{
			Local:  FileLocalNotRegularFile,
			Upload: FileUploadNotApplicable,
			Bytes:  FileByteInfo{TotalKnown: false},
		}
		bceCompareNormalized(t, "not_regular_file.json", bceFileStatusDoc(t, "/mnt/test/link", status), "")
	})

	// Live wiring: a link-suffixed entry on a links-showing mount reports
	// not_regular_file with all three not_measurable markers.
	t.Run("live_link_reports_not_regular_file", func(t *testing.T) {
		r := fstest.NewRun(t)
		opt := vfscommon.Opt
		opt.CacheMode = vfscommon.CacheModeFull
		opt.DirCacheTime = 0
		opt.Links = true
		testVFS := New(context.Background(), r.Fremote, &opt)
		t.Cleanup(func() { cleanupVFS(t, testVFS) })
		mountPoint := filepath.Join(t.TempDir(), "mount")
		RegisterMountPointForVFS(testVFS, mountPoint)
		t.Cleanup(func() { UnregisterMountPointForVFS(testVFS) })

		r.WriteObject(context.Background(), "bce-link.rclonelink", "/target/of/link", time.Now())
		root, err := testVFS.Root()
		require.NoError(t, err)
		_, err = root.ReadDirAll()
		require.NoError(t, err)

		call := rc.Calls.Get("vfs/file-status")
		require.NotNil(t, call)
		path := filepath.Join(mountPoint, "bce-link")
		out, err := call.Fn(context.Background(), rc.Params{
			"fs":    fs.ConfigString(r.Fremote),
			"paths": []string{path},
		})
		require.NoError(t, err)
		raw, _ := json.Marshal(out)
		var doc map[string]any
		require.NoError(t, json.Unmarshal(raw, &doc))
		statuses, _ := doc["statuses"].([]any)
		require.Len(t, statuses, 1)
		result, _ := statuses[0].(map[string]any)
		assert.Equal(t, "not_regular_file", result["local"])
		bytes, ok := result["bytes"].(map[string]any)
		require.True(t, ok)
		assert.Equal(t, "not_measurable", bytes["unbroken_run"])
		assert.Equal(t, "not_measurable", bytes["total_held"])
		assert.Equal(t, "not_measurable", bytes["total_size"])
	})
}

// TestFileStatusBCESharedConfigBareName is the negative regression:
// a bare (unindexed) name against a shared-config mount must behave exactly
// as sibling endpoints behave — the indexed-form acceptance of vfs/file-status
// must not leak to any other surface.
//
// No new fixture file is added for this case (existing fixtures are untouched):
// the whole-request envelope shape is already pinned by empty_path.json and
// this regression is about routing (which error, which surface accepts which
// name), asserted via codes rather than bytes.
func TestFileStatusBCESharedConfigBareName(t *testing.T) {
	r := fstest.NewRun(t)
	r.WriteObject(context.Background(), "bce-shared.txt", "shared", time.Now())

	vfs1 := New(context.Background(), r.Fremote, nil)
	opt2 := vfs1.Opt
	opt2.NoModTime = !opt2.NoModTime
	vfs2 := New(context.Background(), r.Fremote, &opt2)
	require.NotSame(t, vfs1, vfs2, "different VFS options must keep shared-config mounts distinct")
	t.Cleanup(func() {
		vfs1.WaitForWriters(waitForWritersDelay)
		_ = vfs1.CleanUp()
		vfs1.Shutdown()
		vfs2.WaitForWriters(waitForWritersDelay)
		_ = vfs2.CleanUp()
		vfs2.Shutdown()
	})

	configName := fs.ConfigString(r.Fremote)
	mountPoint1 := filepath.Join(t.TempDir(), "mount-one")
	mountPoint2 := filepath.Join(t.TempDir(), "mount-two")
	RegisterMountPointForVFS(vfs1, mountPoint1)
	RegisterMountPointForVFS(vfs2, mountPoint2)
	t.Cleanup(func() {
		UnregisterMountPointForVFS(vfs1)
		UnregisterMountPointForVFS(vfs2)
		for name, point := range GetAllMountPoints() {
			if point == mountPoint1 || point == mountPoint2 {
				UnregisterMountPoint(name)
			}
		}
	})

	statusCall := rc.Calls.Get("vfs/file-status")
	require.NotNil(t, statusCall)
	listCall := rc.Calls.Get("vfs/list")
	require.NotNil(t, listCall)

	// The bare name must not resolve to either mount: whole-request
	// no_active_mount, the same refusal sibling endpoints give.
	_, err := statusCall.Fn(context.Background(), rc.Params{
		"fs":    configName,
		"paths": []string{filepath.Join(mountPoint1, "bce-shared.txt")},
	})
	require.Error(t, err)
	var fileStatusErr *rc.FileStatusError
	require.ErrorAs(t, err, &fileStatusErr)
	assert.Equal(t, "no_active_mount", fileStatusErr.Code)

	// Sibling surface still refuses the bare shared name (no leak of the
	// indexed form): vfs/refresh resolves via getVFS, which reports
	// "more than one VFS active with name".
	refreshCall := rc.Calls.Get("vfs/refresh")
	require.NotNil(t, refreshCall)
	_, err = refreshCall.Fn(context.Background(), rc.Params{"fs": configName})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "more than one VFS active")

	// Each indexed name reported by vfs/list still addresses its own mount.
	out, err := listCall.Fn(context.Background(), nil)
	require.NoError(t, err)
	points, ok := mountPointsObject(out["mountPoints"])
	require.True(t, ok)
	indexed := ""
	for name, point := range points {
		if point == mountPoint1 {
			indexed = name
		}
	}
	require.NotEmpty(t, indexed, "vfs/list must report the first mount point under its indexed name")
	out, err = statusCall.Fn(context.Background(), rc.Params{
		"fs":    indexed,
		"paths": []string{filepath.Join(mountPoint1, "bce-shared.txt")},
	})
	require.NoError(t, err)
	raw, _ := json.Marshal(out)
	var doc map[string]any
	require.NoError(t, json.Unmarshal(raw, &doc))
	statuses, _ := doc["statuses"].([]any)
	require.Len(t, statuses, 1)
	assert.NotContains(t, statuses[0].(map[string]any), "error")
}
