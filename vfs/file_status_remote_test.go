package vfs

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"sync"
	"testing"
	"testing/quick"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/rc"
	"github.com/rclone/rclone/fs/rc/rcserver"
	"github.com/rclone/rclone/fstest"
	"github.com/rclone/rclone/vfs/vfscommon"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fileStatusRemoteCallCounts struct {
	newObject     int
	list          int
	listR         int
	listP         int
	dirCacheFlush int
}

type fileStatusCountingFs struct {
	fs.Fs
	features *fs.Features

	mu    sync.Mutex
	calls fileStatusRemoteCallCounts
}

func newFileStatusCountingFs(inner fs.Fs) *fileStatusCountingFs {
	counted := &fileStatusCountingFs{Fs: inner}
	if innerFeatures := inner.Features(); innerFeatures != nil {
		features := *innerFeatures
		if listR := features.ListR; listR != nil {
			features.ListR = func(ctx context.Context, dir string, callback fs.ListRCallback) error {
				counted.record(func(calls *fileStatusRemoteCallCounts) { calls.listR++ })
				return listR(ctx, dir, callback)
			}
		}
		if listP := features.ListP; listP != nil {
			features.ListP = func(ctx context.Context, dir string, callback fs.ListRCallback) error {
				counted.record(func(calls *fileStatusRemoteCallCounts) { calls.listP++ })
				return listP(ctx, dir, callback)
			}
		}
		if dirCacheFlush := features.DirCacheFlush; dirCacheFlush != nil {
			features.DirCacheFlush = func() {
				counted.record(func(calls *fileStatusRemoteCallCounts) { calls.dirCacheFlush++ })
				dirCacheFlush()
			}
		}
		counted.features = &features
	}
	return counted
}

func registerFileStatusTestMount(t *testing.T, testVFS *VFS, mountPoint string) {
	t.Helper()
	RegisterMountPointForVFS(testVFS, mountPoint)
	t.Cleanup(func() { UnregisterMountPointForVFS(testVFS) })
}

func (f *fileStatusCountingFs) Features() *fs.Features {
	return f.features
}

func (f *fileStatusCountingFs) record(update func(*fileStatusRemoteCallCounts)) {
	f.mu.Lock()
	update(&f.calls)
	f.mu.Unlock()
}

func (f *fileStatusCountingFs) NewObject(ctx context.Context, remote string) (fs.Object, error) {
	f.record(func(calls *fileStatusRemoteCallCounts) { calls.newObject++ })
	return f.Fs.NewObject(ctx, remote)
}

func (f *fileStatusCountingFs) List(ctx context.Context, dir string) (fs.DirEntries, error) {
	f.record(func(calls *fileStatusRemoteCallCounts) { calls.list++ })
	return f.Fs.List(ctx, dir)
}

func (f *fileStatusCountingFs) resetCalls() {
	f.mu.Lock()
	f.calls = fileStatusRemoteCallCounts{}
	f.mu.Unlock()
}

func (f *fileStatusCountingFs) callCounts() fileStatusRemoteCallCounts {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func newFileStatusCountingVFS(t *testing.T) (*fstest.Run, *VFS, *fileStatusCountingFs, string) {
	t.Helper()
	if *fstest.RemoteName != "" {
		t.Skip("counting-filesystem status tests use the local test remote")
	}

	run := fstest.NewRun(t)
	counted := newFileStatusCountingFs(run.Fremote)
	opt := vfscommon.Opt
	opt.DirCacheTime = fs.Duration(time.Hour)
	testVFS := New(context.Background(), counted, &opt)
	t.Cleanup(func() { cleanupVFS(t, testVFS) })

	mountPoint := filepath.Join(t.TempDir(), "mount")
	registerFileStatusTestMount(t, testVFS, mountPoint)
	return run, testVFS, counted, mountPoint
}

func decodeFileStatusEnvelope(out rc.Params) (map[string]any, error) {
	raw, err := json.Marshal(out)
	if err != nil {
		return nil, err
	}
	var envelope map[string]any
	err = json.Unmarshal(raw, &envelope)
	return envelope, err
}

func decodeFileStatusSingleResult(out rc.Params) (map[string]any, error) {
	envelope, err := decodeFileStatusEnvelope(out)
	if err != nil {
		return nil, err
	}
	statuses, ok := envelope["statuses"].([]any)
	if !ok || len(statuses) != 1 {
		return nil, fmt.Errorf("expected one status result, got %v", envelope["statuses"])
	}
	result, ok := statuses[0].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("expected status object, got %T", statuses[0])
	}
	return result, nil
}

func fileStatusPerPathErrorCode(result map[string]any) (string, bool) {
	errorObject, ok := result["error"].(map[string]any)
	if !ok {
		return "", false
	}
	code, ok := errorObject["code"].(string)
	return code, ok
}

func requireFileStatusSingleResult(t *testing.T, out rc.Params) map[string]any {
	t.Helper()
	result, err := decodeFileStatusSingleResult(out)
	require.NoError(t, err)
	return result
}

func startFileStatusHTTPServer(t *testing.T) string {
	t.Helper()
	opt := rc.Opt
	opt.Enabled = true
	opt.HTTP.ListenAddr = []string{"localhost:0"}
	server, err := rcserver.Start(context.Background(), &opt)
	require.NoError(t, err)
	require.NotNil(t, server)
	t.Cleanup(func() {
		require.NoError(t, server.Shutdown())
		server.Wait()
	})
	return server.URLs()[0]
}

func fileStatusEnvelopeHasShape(decoded map[string]any, statusCode int, wantCode string, wantPaths []string) bool {
	if statusCode == http.StatusBadRequest {
		if len(decoded) != 1 {
			return false
		}
		errorObject, ok := decoded["error"].(map[string]any)
		if !ok || len(errorObject) != 2 {
			return false
		}
		code, codeOK := errorObject["code"].(string)
		message, messageOK := errorObject["message"].(string)
		return codeOK && code == wantCode && messageOK && message != ""
	}
	if statusCode != http.StatusOK || len(decoded) != 1 {
		return false
	}
	statuses, ok := decoded["statuses"].([]any)
	if !ok || len(statuses) != len(wantPaths) {
		return false
	}
	for i, rawResult := range statuses {
		result, ok := rawResult.(map[string]any)
		if !ok {
			return false
		}
		path, ok := result["path"].(string)
		if !ok || path != wantPaths[i] {
			return false
		}
		if rawError, hasError := result["error"]; hasError {
			errorObject, ok := rawError.(map[string]any)
			if !ok || len(result) != 2 || len(errorObject) != 2 {
				return false
			}
			if _, ok := errorObject["code"].(string); !ok {
				return false
			}
			if _, ok := errorObject["message"].(string); !ok {
				return false
			}
			continue
		}
		if _, ok := result["local"].(string); !ok {
			return false
		}
		if _, ok := result["upload"].(string); !ok {
			return false
		}
		if _, ok := result["bytes"].(map[string]any); !ok {
			return false
		}
	}
	return true
}

// TestFileStatusDoesNotQueryRemote checks that local VFS listings answer
// status reads without looking up objects or refreshing remote listings.
func TestFileStatusDoesNotQueryRemote(t *testing.T) {
	run, testVFS, counted, mountPoint := newFileStatusCountingVFS(t)
	ctx := context.Background()
	run.WriteObject(ctx, "listed.txt", "listed contents", time.Now())
	require.NoError(t, run.Fremote.Mkdir(ctx, "nested"))
	run.WriteObject(ctx, "nested/present.txt", "nested contents", time.Now())

	root, err := testVFS.Root()
	require.NoError(t, err)
	_, err = root.ReadDirAll()
	require.NoError(t, err)
	nestedNode, err := root.Stat("nested")
	require.NoError(t, err)
	require.True(t, nestedNode.IsDir())

	call := rc.Calls.Get("vfs/file-status")
	require.NotNil(t, call)
	fsName := fs.ConfigString(counted)
	tests := []struct {
		name      string
		path      string
		wantCode  string
		wantLocal string
	}{
		{
			name:      "listed file uses its local listing",
			path:      filepath.Join(mountPoint, "listed.txt"),
			wantLocal: string(FileLocalNotHeld),
		},
		{
			name:     "absent entry in a current listing is not found",
			path:     filepath.Join(mountPoint, "missing.txt"),
			wantCode: "not_found",
		},
		{
			name:     "unread directory listing is unaccountable",
			path:     filepath.Join(mountPoint, "nested", "missing.txt"),
			wantCode: "unaccountable_path",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			counted.resetCalls()
			out, err := call.Fn(ctx, rc.Params{
				"fs":    fsName,
				"paths": []string{test.path},
			})
			require.NoError(t, err)
			result := requireFileStatusSingleResult(t, out)
			assert.Equal(t, test.path, result["path"])
			if test.wantCode != "" {
				code, ok := fileStatusPerPathErrorCode(result)
				require.True(t, ok, "expected per-path error result, got %v", result)
				assert.Equal(t, test.wantCode, code)
			} else {
				assert.Equal(t, test.wantLocal, result["local"])
				assert.NotContains(t, result, "error")
			}
			assert.Equal(t, fileStatusRemoteCallCounts{}, counted.callCounts(), "status read made a remote call")
		})
	}
}

// TestFileStatusCurrentListingPropertyNoRemoteLookup checks the no-lookup
// invariant for arbitrary missing entries in one current local listing.
func TestFileStatusCurrentListingPropertyNoRemoteLookup(t *testing.T) {
	_, testVFS, counted, mountPoint := newFileStatusCountingVFS(t)
	root, err := testVFS.Root()
	require.NoError(t, err)
	_, err = root.ReadDirAll()
	require.NoError(t, err)
	call := rc.Calls.Get("vfs/file-status")
	require.NotNil(t, call)
	fsName := fs.ConfigString(counted)

	property := func(n uint64) bool {
		path := filepath.Join(mountPoint, fmt.Sprintf("missing-%016x", n))
		counted.resetCalls()
		out, err := call.Fn(context.Background(), rc.Params{
			"fs":    fsName,
			"paths": []string{path},
		})
		if err != nil {
			return false
		}
		result, err := decodeFileStatusSingleResult(out)
		if err != nil || result["path"] != path {
			return false
		}
		code, ok := fileStatusPerPathErrorCode(result)
		return ok && code == "not_found" && counted.callCounts() == (fileStatusRemoteCallCounts{})
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 24}); err != nil {
		t.Error(err)
	}
}

// TestFileStatusMalformedFsValuesHTTPContract checks malformed fs values
// are delivered as the endpoint's typed param_invalid response.
func TestFileStatusMalformedFsValuesHTTPContract(t *testing.T) {
	rcNewRun(t, "vfs/file-status")
	testURL := startFileStatusHTTPServer(t)
	path := filepath.Join(t.TempDir(), "file.txt")
	tests := []struct {
		name  string
		value any
	}{
		{name: "null", value: nil},
		{name: "number", value: 42},
		{name: "boolean", value: true},
		{name: "array", value: []any{"remote"}},
		{name: "object", value: map[string]any{"name": "remote"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			body := fileStatusJSONBody(t, rc.Params{
				"fs":    test.value,
				"paths": []string{path},
			})
			resp, raw, decoded := fileStatusHTTPPost(t, testURL, body, "application/json", "", "")
			require.Equal(t, http.StatusBadRequest, resp.StatusCode, "body: %s", raw)
			assert.Contains(t, resp.Header.Get("Content-Type"), "application/json")
			require.NotNil(t, decoded, "body: %s", raw)
			require.Len(t, decoded, 1)
			errorObject, ok := decoded["error"].(map[string]any)
			require.True(t, ok, "error is not an object: %s", raw)
			require.Len(t, errorObject, 2)
			assert.Equal(t, "param_invalid", errorObject["code"])
			message, ok := errorObject["message"].(string)
			assert.True(t, ok)
			assert.NotEmpty(t, message)
			assert.NotContains(t, decoded, "path")
		})
	}
}

// TestFileStatusJSONEnvelopeProperty checks the HTTP response envelope
// across generated successful and whole-request-failure inputs.
func TestFileStatusJSONEnvelopeProperty(t *testing.T) {
	run, testVFS, _ := rcNewRun(t, "vfs/file-status")
	base := t.TempDir()
	registerFileStatusTestMount(t, testVFS, filepath.Join(base, "mount"))
	fsName := fs.ConfigString(run.Fremote)
	testURL := startFileStatusHTTPServer(t)

	property := func(n uint64) bool {
		var (
			in         rc.Params
			wantStatus int
			wantCode   string
			wantPaths  []string
		)
		switch n % 3 {
		case 0:
			path := filepath.Join(base, "outside", fmt.Sprintf("probe-%016x", n))
			in = rc.Params{"fs": fsName, "paths": []string{path}}
			wantStatus = http.StatusOK
			wantPaths = []string{path}
		case 1:
			in = rc.Params{"fs": fsName, "paths": []string{fmt.Sprintf("relative-%016x", n)}}
			wantStatus = http.StatusBadRequest
			wantCode = "empty_path"
		default:
			in = rc.Params{"fs": fsName, "paths": []any{n}}
			wantStatus = http.StatusBadRequest
			wantCode = "param_invalid"
		}
		body := fileStatusJSONBody(t, in)
		resp, _, decoded := fileStatusHTTPPost(t, testURL, body, "application/json", "", "")
		return resp.StatusCode == wantStatus && decoded != nil &&
			fileStatusEnvelopeHasShape(decoded, wantStatus, wantCode, wantPaths)
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 30}); err != nil {
		t.Error(err)
	}
}

// TestFileStatusUnknownParameterProperty checks arbitrary unknown parameter
// names and JSON values do not change the endpoint response.
func TestFileStatusUnknownParameterProperty(t *testing.T) {
	run, testVFS, call := rcNewRun(t, "vfs/file-status")
	base := t.TempDir()
	registerFileStatusTestMount(t, testVFS, filepath.Join(base, "mount"))
	fsName := fs.ConfigString(run.Fremote)
	path := filepath.Join(base, "outside", "probe.txt")
	request := func() rc.Params {
		return rc.Params{"fs": fsName, "paths": []string{path}}
	}

	baseline, err := call.Fn(context.Background(), request())
	require.NoError(t, err)
	baselineJSON, err := json.Marshal(baseline)
	require.NoError(t, err)

	property := func(n uint64) bool {
		in := request()
		var value any
		switch n % 5 {
		case 0:
			value = nil
		case 1:
			value = n
		case 2:
			value = fmt.Sprintf("ignored-%d", n)
		case 3:
			value = []any{n, "ignored", nil}
		default:
			value = map[string]any{"ignored": n}
		}
		in[fmt.Sprintf("unknown-%016x", n)] = value
		out, err := call.Fn(context.Background(), in)
		if err != nil {
			return false
		}
		gotJSON, err := json.Marshal(out)
		return err == nil && bytes.Equal(baselineJSON, gotJSON)
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 30}); err != nil {
		t.Error(err)
	}
}
