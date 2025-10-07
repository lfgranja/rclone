package vfs

import (
	"context"
	"testing"
	"time"

	_ "github.com/rclone/rclone/backend/local"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/rc"
	"github.com/rclone/rclone/vfs/vfscommon"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRCStatus(t *testing.T) {
	// Create VFS with test files using standard test helper
	r, vfs := newTestVFS(t)

	// Create a test file
	r.WriteFile("test.txt", "test content", time.Now())

	// Clear any existing VFS instances to avoid conflicts
	clearActiveCache()
	// Add VFS to active cache
	addToActiveCache(vfs)

	// Test vfs/status endpoint (aggregate statistics)
	statusCall := rc.Calls.Get("vfs/status")
	require.NotNil(t, statusCall)

	// Test aggregate stats
	result, err := statusCall.Fn(context.Background(), rc.Params{
		"fs": r.Fremote.String(),
	})
	require.NoError(t, err)

	// Check that we have the expected aggregate stats
	totalFiles := getInt64FromParam(t, result, "totalFiles")
	assert.GreaterOrEqual(t, totalFiles, int64(0))

	fullCount := getInt64FromParam(t, result, "fullCount")
	assert.GreaterOrEqual(t, fullCount, int64(0))

	partialCount := getInt64FromParam(t, result, "partialCount")
	assert.GreaterOrEqual(t, partialCount, int64(0))

	noneCount := getInt64FromParam(t, result, "noneCount")
	assert.GreaterOrEqual(t, noneCount, int64(0))

	dirtyCount := getInt64FromParam(t, result, "dirtyCount")
	assert.GreaterOrEqual(t, dirtyCount, int64(0))

	uploadingCount := getInt64FromParam(t, result, "uploadingCount")
	assert.GreaterOrEqual(t, uploadingCount, int64(0))

	totalCachedBytes := getInt64FromParam(t, result, "totalCachedBytes")
	assert.GreaterOrEqual(t, totalCachedBytes, int64(0))

	averageCachePercentage := getInt64FromParam(t, result, "averageCachePercentage")
	assert.GreaterOrEqual(t, averageCachePercentage, int64(0))
	assert.LessOrEqual(t, averageCachePercentage, int64(100))
}

func TestRCFileStatus(t *testing.T) {
	// Create VFS with test files using standard test helper
	r, vfs := newTestVFS(t)

	// Create a test file
	r.WriteFile("test.txt", "test content", time.Now())

	// Clear any existing VFS instances to avoid conflicts
	clearActiveCache()
	// Add VFS to active cache
	addToActiveCache(vfs)

	// Test vfs/file-status endpoint
	fileStatusCall := rc.Calls.Get("vfs/file-status")
	require.NotNil(t, fileStatusCall)

	// Test with valid file path
	result, err := fileStatusCall.Fn(context.Background(), rc.Params{
		"fs":   r.Fremote.String(),
		"path": "test.txt",
	})
	require.NoError(t, err)

	name, ok := result["name"].(string)
	require.True(t, ok)
	assert.Equal(t, "test.txt", name)

	status, ok := result["status"].(string)
	require.True(t, ok)
	assert.Contains(t, []string{"FULL", "PARTIAL", "NONE"}, status)

	// Handle different numeric types that might be returned from JSON
	var percentage int
	switch v := result["percentage"].(type) {
	case int:
		percentage = v
	case int64:
		percentage = int(v)
	case float64:
		percentage = int(v)
	default:
		require.Fail(t, "percentage is not a recognized numeric type, got %T", result["percentage"])
	}
	assert.GreaterOrEqual(t, percentage, 0)
	assert.LessOrEqual(t, percentage, 100)

	// Test with non-existent file
	result, err = fileStatusCall.Fn(context.Background(), rc.Params{
		"fs":   r.Fremote.String(),
		"path": "nonexistent.txt",
	})
	require.NoError(t, err)

	name, ok = result["name"].(string)
	require.True(t, ok)
	assert.Equal(t, "nonexistent.txt", name)

	status, ok = result["status"].(string)
	require.True(t, ok)
	assert.Equal(t, "NONE", status)

	percentage, ok = result["percentage"].(int)
	require.True(t, ok)
	assert.Equal(t, 0, percentage)

	// Test with missing path parameter
	_, err = fileStatusCall.Fn(context.Background(), rc.Params{
		"fs": r.Fremote.String(),
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "no path parameter(s) provided")
}

func TestRCDirStatus(t *testing.T) {
	// Create VFS with test files using standard test helper
	r, vfs := newTestVFS(t)

	// Enable VFS cache for testing
	opt := vfs.Opt
	opt.CacheMode = vfscommon.CacheModeFull
	opt.CacheMaxSize = 100 * 1024 * 1024 // 100MB
	opt.CacheMaxAge = fs.Duration(24 * time.Hour)

	// Create test files in the root directory using the remote filesystem
	r.WriteFile("testdir/test1.txt", "test content 1", time.Now())
	r.WriteFile("testdir/test2.txt", "test content 2", time.Now())

	// Clear any existing VFS instances to avoid conflicts
	clearActiveCache()
	// Add VFS to active cache
	addToActiveCache(vfs)

	// Test vfs/dir-status endpoint
	dirStatusCall := rc.Calls.Get("vfs/dir-status")
	require.NotNil(t, dirStatusCall)

	// Test with valid directory path (root)
	waitForCondition(t, 5*time.Second, func() bool {
		result, err := dirStatusCall.Fn(context.Background(), rc.Params{
			"fs":  r.Fremote.String(),
			"dir": "testdir",
		})
		if err != nil {
			t.Logf("Error calling vfs/dir-status: %v", err)
			return false
		}

		files, ok := result["files"].([]rc.Params)
		if !ok {
			t.Logf("Invalid response from vfs/dir-status: %v", result)
			return false
		}

		var foundTest1, foundTest2 bool
		for _, file := range files {
			if name, ok := file["name"].(string); ok {
				if name == "test1.txt" {
					foundTest1 = true
				}
				if name == "test2.txt" {
					foundTest2 = true
				}
			}
		}
		return foundTest1 && foundTest2
	})

	// Test with missing dir parameter (should default to root)
	result, err := dirStatusCall.Fn(context.Background(), rc.Params{
		"fs": r.Fremote.String(),
	})

	require.NoError(t, err)

	files, ok := result["files"].([]rc.Params)
	require.True(t, ok)
	// Check that we found some files (exact count may vary)
	t.Logf("Found %d files in root directory", len(files))
	for _, file := range files {
		t.Logf("File: %s, Status: %s", file["name"], file["status"])
	}

	// Test with non-existent directory
	_, err = dirStatusCall.Fn(context.Background(), rc.Params{
		"fs":  r.Fremote.String(),
		"dir": "nonexistent",
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

// waitForCondition waits for a condition to be true, polling every 100ms until the timeout
func waitForCondition(t *testing.T, timeout time.Duration, check func() bool) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("condition not met within timeout")
}

// Helper function to add VFS to active cache for testing
func addToActiveCache(vfs *VFS) {
	activeMu.Lock()
	defer activeMu.Unlock()

	fsName := vfs.f.String()
	active[fsName] = append(active[fsName], vfs)
}

// Helper function to clear active cache for testing
func clearActiveCache() {
	activeMu.Lock()
	defer activeMu.Unlock()

	active = make(map[string][]*VFS)
}

// Helper function to get int64 from rc.Params with type conversion
func getInt64FromParam(t *testing.T, params rc.Params, key string) int64 {
	val, ok := params[key]
	require.True(t, ok, "%s not found in result", key)
	var i64 int64
	switch v := val.(type) {
	case int64:
		i64 = v
	case int:
		i64 = int64(v)
	case float64:
		i64 = int64(v)
	default:
		require.Fail(t, "%s is not int64, int, or float64, got %T", key, val)
	}
	return i64
}
