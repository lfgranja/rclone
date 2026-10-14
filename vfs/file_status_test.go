package vfs

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"testing/quick"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/rc"
	"github.com/rclone/rclone/fs/rc/rcserver"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The tests in this file pin the request shape (T010) and the
// whole-request delivery (T011) of the vfs/file-status endpoint of
// contracts/api.md. They assert the contract of FR-006 against the
// implemented handler (vfs/rc.go rcFileStatus) and the typed-error
// delivery (fs/rc FileStatusError rendered by the rc server's
// writeFileStatusError).

// fileStatusChildEnv marks the re-executed child process of
// TestFileStatusNoActiveMount.
const fileStatusChildEnv = "RCLONE_FILE_STATUS_CHILD"

// fileStatusJSONBody marshals in as the JSON body of an rc call.
func fileStatusJSONBody(t *testing.T, in rc.Params) string {
	t.Helper()
	raw, err := json.Marshal(in)
	require.NoError(t, err)
	return string(raw)
}

// fileStatusHTTPPost POSTs body to the vfs/file-status endpoint of the rc
// server at testURL, with the given Content-Type and optional basic
// auth credentials, and returns the response, the raw body and the body
// decoded as a JSON object when it is one.
func fileStatusHTTPPost(t *testing.T, testURL, body, contentType, user, pass string) (*http.Response, string, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, testURL+"vfs/file-status", strings.NewReader(body))
	require.NoError(t, err)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if user != "" || pass != "" {
		req.SetBasicAuth(user, pass)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = resp.Body.Close()
	})
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	var decoded map[string]any
	if strings.Contains(resp.Header.Get("Content-Type"), "application/json") {
		require.NoError(t, json.Unmarshal(raw, &decoded), "body: %s", raw)
	}
	return resp, string(raw), decoded
}

// requireWholeRequestRejection asserts that err is the typed whole-request
// rejection of the endpoint carrying the given error code and the HTTP
// status 400, and that its Render shape is the error object of the
// contract: the code and the message, with no path field.
func requireWholeRequestRejection(t *testing.T, err error, code string) {
	t.Helper()
	var fileStatusErr *rc.FileStatusError
	require.ErrorAs(t, err, &fileStatusErr)
	assert.Equal(t, code, fileStatusErr.Code)
	assert.Equal(t, http.StatusBadRequest, fileStatusErr.Status)
	rendered := fileStatusErr.Render()
	assert.Equal(t, rc.Params{
		"error": rc.Params{
			"code":    code,
			"message": fileStatusErr.Message,
		},
	}, rendered)
	assert.NotContains(t, rendered, "path")
	errorParams, ok := rendered["error"].(rc.Params)
	require.True(t, ok)
	assert.NotContains(t, errorParams, "path")
}

// TestFileStatusEmptyPaths pins the request-shape level of FR-006: an
// absent or empty paths array is the whole-request empty_path failure,
// decided before the identifier is looked at, so a request naming no
// path and no identifier is empty_path too and never ambiguous_mount.
func TestFileStatusEmptyPaths(t *testing.T) {
	r, _, call := rcNewRun(t, "vfs/file-status")
	fsName := fs.ConfigString(r.Fremote)

	tests := []struct {
		name string
		in   rc.Params
	}{
		{"absent paths", rc.Params{"fs": fsName}},
		{"empty paths", rc.Params{"fs": fsName, "paths": []string{}}},
		{"no paths and no identifier", rc.Params{}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			out, err := call.Fn(context.Background(), test.in)
			require.Error(t, err)
			requireWholeRequestRejection(t, err, "empty_path")
			assert.Nil(t, out)
		})
	}
}

// TestFileStatusNonAbsoluteEntry pins that an entry which is not an
// absolute path on the mount host takes the same whole-request
// empty_path failure as an empty array, decided once for the request
// rather than per path, and never resolved against the caller's working
// directory.
func TestFileStatusNonAbsoluteEntry(t *testing.T) {
	r, _, call := rcNewRun(t, "vfs/file-status")
	fsName := fs.ConfigString(r.Fremote)

	tests := []struct {
		name  string
		paths []string
	}{
		{"relative entry", []string{"file.txt"}},
		{"dot relative entry", []string{"./file.txt"}},
		{"parent relative entry", []string{"../file.txt"}},
		{"absolute and relative entries", []string{"/file.txt", "dir/file.txt"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			in := rc.Params{"fs": fsName, "paths": test.paths}
			out, err := call.Fn(context.Background(), in)
			require.Error(t, err)
			requireWholeRequestRejection(t, err, "empty_path")
			assert.Nil(t, out)
		})
	}
}

// TestFileStatusNonStringEntry pins that an entry which is not a JSON
// string is an invalid parameter value: the whole-request param_invalid
// failure, decided before the absoluteness check, so a request carrying
// both a non-string entry and a relative entry is param_invalid.
func TestFileStatusNonStringEntry(t *testing.T) {
	r, _, call := rcNewRun(t, "vfs/file-status")
	fsName := fs.ConfigString(r.Fremote)

	tests := []struct {
		name  string
		paths []any
	}{
		{"number entry", []any{42}},
		{"boolean entry", []any{true}},
		{"null entry", []any{nil}},
		{"string and number entries", []any{"/file.txt", 42}},
		{"relative string and number entries", []any{"file.txt", 42}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			in := rc.Params{"fs": fsName, "paths": test.paths}
			out, err := call.Fn(context.Background(), in)
			require.Error(t, err)
			requireWholeRequestRejection(t, err, "param_invalid")
			assert.Nil(t, out)
		})
	}
}

// TestFileStatusUnknownParameterIgnored pins that the endpoint is
// read-only in the way of its read-only siblings: an unknown parameter
// is not a failure at all and the request is answered per path.
func TestFileStatusUnknownParameterIgnored(t *testing.T) {
	r, _, call := rcNewRun(t, "vfs/file-status")
	fsName := fs.ConfigString(r.Fremote)

	in := rc.Params{
		"fs":      fsName,
		"paths":   []string{"/no/such/mount/point/file.txt"},
		"unknown": "ignored",
	}
	out, err := call.Fn(context.Background(), in)
	require.NoError(t, err)
	require.NotNil(t, out)
	// Decode the response the way the rc server delivers it, so the
	// assertion holds for any Go type the handler builds it from.
	raw, err := json.Marshal(out)
	require.NoError(t, err)
	var delivered map[string]any
	require.NoError(t, json.Unmarshal(raw, &delivered))
	statuses, ok := delivered["statuses"].([]any)
	require.True(t, ok, "statuses is not an array: %v", out["statuses"])
	require.Len(t, statuses, 1)
}

// TestFileStatusFsIdentifierResolution pins the identifier-resolution
// level of FR-006: an identifier matching no running mount is
// no_active_mount whatever the paths turn out to be, a request carrying
// no identifier and naming more than one path is ambiguous_mount, and a
// request carrying no identifier and naming exactly one path resolves
// that path against the registered mount points, a path inside none of
// them being the per-path outside_any_mount result.
func TestFileStatusFsIdentifierResolution(t *testing.T) {
	_, _, call := rcNewRun(t, "vfs/file-status")

	t.Run("unmatched identifier", func(t *testing.T) {
		in := rc.Params{"fs": "no-such-mount", "paths": []string{"/file.txt"}}
		out, err := call.Fn(context.Background(), in)
		require.Error(t, err)
		requireWholeRequestRejection(t, err, "no_active_mount")
		assert.Nil(t, out)
	})

	t.Run("no identifier several paths", func(t *testing.T) {
		in := rc.Params{"paths": []string{"/one/file.txt", "/two/file.txt"}}
		out, err := call.Fn(context.Background(), in)
		require.Error(t, err)
		requireWholeRequestRejection(t, err, "ambiguous_mount")
		assert.Nil(t, out)
	})

	t.Run("no identifier single path", func(t *testing.T) {
		path := "/no/such/mount/point/file.txt"
		in := rc.Params{"paths": []string{path}}
		out, err := call.Fn(context.Background(), in)
		require.NoError(t, err)
		raw, err := json.Marshal(out)
		require.NoError(t, err)
		var delivered map[string]any
		require.NoError(t, json.Unmarshal(raw, &delivered))
		statuses, ok := delivered["statuses"].([]any)
		require.True(t, ok, "statuses is not an array: %v", out["statuses"])
		require.Len(t, statuses, 1)
		status, ok := statuses[0].(map[string]any)
		require.True(t, ok, "status is not an object: %v", statuses[0])
		assert.Equal(t, path, status["path"])
		errResult, ok := status["error"].(map[string]any)
		require.True(t, ok, "status is not an error result: %v", status)
		assert.Equal(t, "outside_any_mount", errResult["code"])
	})
}

// TestFileStatusNoActiveMount pins that a request reaching a server
// with no mount active at all is the whole-request no_active_mount
// failure, whatever its paths. The active VFS cache is process-wide
// state that earlier tests in this package populate, so the request is
// made in a re-executed child process in which no test has created a
// VFS yet.
func TestFileStatusNoActiveMount(t *testing.T) {
	if os.Getenv(fileStatusChildEnv) == "1" {
		// Child process: no test has created a VFS in this process, so
		// the active cache is empty.
		vfs, count := activeCacheEntries()
		if count != 0 || vfs != nil {
			t.Fatalf("precondition failed: %d VFSes active in child process", count)
		}
		call := rc.Calls.Get("vfs/file-status")
		if call == nil {
			t.Fatal("vfs/file-status is not registered")
		}
		in := rc.Params{"paths": []string{"/file.txt"}}
		out, err := call.Fn(context.Background(), in)
		if err == nil {
			t.Fatalf("expected the whole-request no_active_mount failure, got a response: %v", out)
		}
		var fileStatusErr *rc.FileStatusError
		if !errors.As(err, &fileStatusErr) {
			t.Fatalf("expected a *rc.FileStatusError, got %T: %v", err, err)
		}
		if fileStatusErr.Code != "no_active_mount" {
			t.Fatalf("expected code no_active_mount, got %q", fileStatusErr.Code)
		}
		if fileStatusErr.Status != http.StatusBadRequest {
			t.Fatalf("expected status %d, got %d", http.StatusBadRequest, fileStatusErr.Status)
		}
		if out != nil {
			t.Fatalf("expected no response, got %v", out)
		}
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestFileStatusNoActiveMount$")
	cmd.Env = append(os.Environ(), fileStatusChildEnv+"=1")
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, "child process output: %s", output)
}

// TestFileStatusClassifiesArbitraryPaths is the property test of the
// FR-006 decision order over the request shape: an arbitrary paths
// array classifies into exactly one of the two outcomes the contract
// allows — the whole-request 400 failure decided at the shape level, or
// the 200 response carrying one result per path — and the classifier's
// expectation is encoded through the endpoint's responses.
func TestFileStatusClassifiesArbitraryPaths(t *testing.T) {
	r, _, call := rcNewRun(t, "vfs/file-status")
	fsName := fs.ConfigString(r.Fremote)

	// classify is the reference classifier of the request-shape level:
	// an absent or empty array, or any entry that is not an absolute
	// path on the host, is the whole-request empty_path failure;
	// anything else is answered with one result per path.
	classify := func(paths []string) (code string, wholeRequest bool) {
		if len(paths) == 0 {
			return "empty_path", true
		}
		for _, path := range paths {
			if !filepath.IsAbs(path) {
				return "empty_path", true
			}
		}
		return "", false
	}

	// request asks the endpoint for the generated paths and reports
	// whether the response matches the classifier.
	request := func(paths []string) bool {
		in := rc.Params{"fs": fsName, "paths": paths}
		out, err := call.Fn(context.Background(), in)
		code, wholeRequest := classify(paths)
		if wholeRequest {
			var fileStatusErr *rc.FileStatusError
			if !errors.As(err, &fileStatusErr) {
				return false
			}
			return fileStatusErr.Code == code &&
				fileStatusErr.Status == http.StatusBadRequest &&
				out == nil
		}
		if err != nil || out == nil {
			return false
		}
		raw, err := json.Marshal(out)
		if err != nil {
			return false
		}
		var delivered map[string]any
		if err := json.Unmarshal(raw, &delivered); err != nil {
			return false
		}
		statuses, ok := delivered["statuses"].([]any)
		return ok && len(statuses) == len(paths)
	}

	if err := quick.Check(request, nil); err != nil {
		t.Error(err)
	}

	// The same classification holds with every entry absolute, which
	// exercises the per-path branch of the classifier.
	absolute := func(paths []string) bool {
		absolutePaths := make([]string, len(paths))
		for i, path := range paths {
			absolutePaths[i] = "/" + path
		}
		return request(absolutePaths)
	}
	if err := quick.Check(absolute, nil); err != nil {
		t.Error(err)
	}
}

// TestFileStatusHTTPDelivery pins the delivery of the whole-request
// rejections over the control interface: a rejection the endpoint itself
// originates — empty_path, no_active_mount, ambiguous_mount or an
// invalid parameter value — is answered by the rc server's error writer
// as status 400 with the error object of the contract, the code and the
// message with no path field, while a body the rc server cannot decode
// is answered by the framework before the handler runs, as the
// code-less 400 a consumer treats as param_invalid.
func TestFileStatusHTTPDelivery(t *testing.T) {
	r, _, _ := rcNewRun(t, "vfs/file-status")
	fsName := fs.ConfigString(r.Fremote)

	opt := rc.Opt
	opt.Enabled = true
	opt.HTTP.ListenAddr = []string{"localhost:0"}
	rcServer, err := rcserver.Start(context.Background(), &opt)
	require.NoError(t, err)
	require.NotNil(t, rcServer)
	defer func() {
		require.NoError(t, rcServer.Shutdown())
		rcServer.Wait()
	}()
	testURL := rcServer.URLs()[0]

	// requireRejection asserts that a whole-request rejection the
	// endpoint itself originates is delivered as status 400 with the
	// error object of the contract and no path field.
	requireRejection := func(t *testing.T, in rc.Params, code string) {
		t.Helper()
		body := fileStatusJSONBody(t, in)
		resp, raw, decoded := fileStatusHTTPPost(t, testURL, body, "application/json", "", "")
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode, "body: %s", raw)
		assert.Contains(t, resp.Header.Get("Content-Type"), "application/json")
		require.NotNil(t, decoded, "body: %s", raw)
		assert.Len(t, decoded, 1)
		assert.NotContains(t, decoded, "path")
		assert.NotContains(t, decoded, "status")
		assert.NotContains(t, decoded, "input")
		errorParams, ok := decoded["error"].(map[string]any)
		require.True(t, ok, "error is not an object: %s", raw)
		assert.Equal(t, code, errorParams["code"])
		assert.Contains(t, errorParams, "message")
		assert.NotContains(t, errorParams, "path")
	}

	t.Run("empty paths", func(t *testing.T) {
		requireRejection(t, rc.Params{"fs": fsName, "paths": []string{}}, "empty_path")
	})

	t.Run("non-absolute entry", func(t *testing.T) {
		requireRejection(t, rc.Params{"fs": fsName, "paths": []string{"file.txt"}}, "empty_path")
	})

	t.Run("non-string entry", func(t *testing.T) {
		requireRejection(t, rc.Params{"fs": fsName, "paths": []any{"/file.txt", 42}}, "param_invalid")
	})

	t.Run("unmatched identifier", func(t *testing.T) {
		requireRejection(t, rc.Params{"fs": "no-such-mount", "paths": []string{"/file.txt"}}, "no_active_mount")
	})

	t.Run("no identifier several paths", func(t *testing.T) {
		requireRejection(t, rc.Params{"paths": []string{"/one/file.txt", "/two/file.txt"}}, "ambiguous_mount")
	})

	t.Run("valid request answered per path", func(t *testing.T) {
		body := fileStatusJSONBody(t, rc.Params{"fs": fsName, "paths": []string{"/no/such/mount/point/file.txt"}})
		resp, raw, decoded := fileStatusHTTPPost(t, testURL, body, "application/json", "", "")
		assert.Equal(t, http.StatusOK, resp.StatusCode, "body: %s", raw)
		require.NotNil(t, decoded, "body: %s", raw)
		statuses, ok := decoded["statuses"].([]any)
		require.True(t, ok, "statuses is not an array: %s", raw)
		require.Len(t, statuses, 1)
	})

	// A body the rc server cannot decode is rejected by the framework
	// before the handler runs, with the rc server's own error envelope
	// whose error is a plain string and so carries no error.code; a
	// consumer treats that code-less 400 as param_invalid. The
	// envelope's shape is pinned by fs/rc/rcserver/rcserver_test.go:597-613.
	t.Run("undecodable body", func(t *testing.T) {
		resp, raw, decoded := fileStatusHTTPPost(t, testURL, `{ param1":"string", "param3":true }`, "application/json", "", "")
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode, "body: %s", raw)
		require.NotNil(t, decoded, "body: %s", raw)
		assert.Equal(t, float64(http.StatusBadRequest), decoded["status"])
		assert.Equal(t, "vfs/file-status", decoded["path"])
		// The error is a plain string, not an object carrying a code,
		// which is what makes a consumer treat the 400 as param_invalid.
		errorText, ok := decoded["error"].(string)
		require.True(t, ok, "error is not a plain string: %s", raw)
		assert.NotEmpty(t, errorText)
		assert.Contains(t, decoded, "input")
	})
}

// TestFileStatusAuth pins the authentication behaviour of the endpoint:
// it is registered NoAuth like its read-only siblings, so where the
// control server requires authentication a caller without credentials is
// answered by the middleware as plain text 401, and the rc server's
// authorisation check never answers 403 for this endpoint — an
// authenticated caller always reaches the handler.
func TestFileStatusAuth(t *testing.T) {
	opt := rc.Opt
	opt.Enabled = true
	opt.HTTP.ListenAddr = []string{"localhost:0"}
	opt.Auth.BasicUser = "testuser"
	opt.Auth.BasicPass = "testpass"
	rcServer, err := rcserver.Start(context.Background(), &opt)
	require.NoError(t, err)
	require.NotNil(t, rcServer)
	defer func() {
		require.NoError(t, rcServer.Shutdown())
		rcServer.Wait()
	}()
	testURL := rcServer.URLs()[0]

	body := fileStatusJSONBody(t, rc.Params{"paths": []string{}})

	t.Run("no credentials answered 401 plain text", func(t *testing.T) {
		resp, raw, _ := fileStatusHTTPPost(t, testURL, body, "application/json", "", "")
		assert.Equal(t, http.StatusUnauthorized, resp.StatusCode, "body: %s", raw)
		assert.NotEqual(t, http.StatusForbidden, resp.StatusCode)
		assert.Contains(t, resp.Header.Get("Content-Type"), "text/plain")
	})

	t.Run("with credentials the handler answers, never 403", func(t *testing.T) {
		resp, raw, _ := fileStatusHTTPPost(t, testURL, body, "application/json", opt.Auth.BasicUser, opt.Auth.BasicPass)
		// The request is past the middleware and the endpoint answers
		// the whole-request empty_path rejection, which is decided
		// before any mount is consulted; the rc server's
		// authorisation check, which would answer 403, never fires
		// for a NoAuth call.
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode, "body: %s", raw)
		assert.NotEqual(t, http.StatusForbidden, resp.StatusCode)
		assert.NotEqual(t, http.StatusUnauthorized, resp.StatusCode)
	})
}
