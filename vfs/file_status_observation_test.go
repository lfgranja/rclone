package vfs

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"testing/quick"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/rc"
	"github.com/rclone/rclone/vfs/vfscommon"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const fileStatusObservationPromptLimit = 100 * time.Millisecond

type fileStatusResetBarrierObject struct {
	fs.Object
	armed   bool
	entered chan struct{}
	release <-chan struct{}
	once    sync.Once
}

func (o *fileStatusResetBarrierObject) Size() int64 {
	if o.armed {
		o.once.Do(func() {
			close(o.entered)
			<-o.release
		})
	}
	return o.Object.Size()
}

type fileStatusObservationCallResult struct {
	out rc.Params
	err error
}

func fileStatusObservationDocument(out rc.Params) (map[string]any, error) {
	body, err := json.Marshal(out)
	if err != nil {
		return nil, err
	}
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, err
	}
	return doc, nil
}

func fileStatusObservationPathResult(out rc.Params, path string) (map[string]any, error) {
	doc, err := fileStatusObservationDocument(out)
	if err != nil {
		return nil, err
	}
	statuses, ok := doc["statuses"].([]any)
	if !ok {
		return nil, fmt.Errorf("response has no statuses array: %v", doc)
	}
	for i, raw := range statuses {
		status, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("statuses[%d] is not an object: %v", i, raw)
		}
		if status["path"] == path {
			return status, nil
		}
	}
	return nil, fmt.Errorf("response has no result for path %q: %v", path, statuses)
}

func fileStatusObservationPublicState(t *testing.T, call *rc.Call, fsName, path string) map[string]any {
	t.Helper()
	doc := fileStatusStateCall(t, call, fsName, []string{path})
	return fileStatusStateResult(t, fileStatusStateStatuses(t, doc), path)
}

func fileStatusObservationPublishedCacheStats(call *rc.Call, fsName string) (map[string]any, error) {
	out, err := call.Fn(context.Background(), rc.Params{"fs": fsName})
	if err != nil {
		return nil, err
	}
	doc, err := fileStatusObservationDocument(out)
	if err != nil {
		return nil, err
	}
	diskCache, ok := doc["diskCache"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("vfs/stats response has no diskCache object: %v", doc)
	}
	return diskCache, nil
}

// TestFileStatusObservationResetDoesNotWait checks that an in-flight reset is
// reported as underivable while Reset holds the item lock, without waiting for
// the reset to finish (FR-004/026, SC-010).
func TestFileStatusObservationResetDoesNotWait(t *testing.T) {
	opt := fileStatusStateOpt(vfscommon.CacheModeFull, fs.Duration(time.Hour))
	r, vfs, call := newFileStatusStateRun(t, opt)
	vfs.MountPoint = r.Fremote.Root()
	const name = "reset-in-flight"
	arrangeFileStatusStateCachedFile(t, r, vfs, name, "reset contents")
	path := fileStatusStatePath(r, name)
	fsName := fs.ConfigString(r.Fremote)
	initial := fileStatusObservationPublicState(t, call, fsName, path)
	local, _ := requireFileStatusStatePair(t, initial)
	require.Equal(t, statusFullyHeld, local)

	remoteObject, err := r.Fremote.NewObject(context.Background(), name)
	require.NoError(t, err)
	item := vfs.cache.Item(name)
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseReset := func() {
		releaseOnce.Do(func() { close(release) })
	}
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
	defer func() {
		releaseReset()
		select {
		case <-resetDone:
		case <-time.After(5 * time.Second):
			t.Errorf("cache reset did not finish after releasing its barrier")
		}
	}()

	select {
	case <-barrierObject.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("reset did not reach the locked object-fingerprint barrier")
	}

	statusDone := make(chan fileStatusObservationCallResult, 1)
	go func() {
		out, err := call.Fn(context.Background(), rc.Params{
			"fs":    fsName,
			"paths": []string{path},
		})
		statusDone <- fileStatusObservationCallResult{out: out, err: err}
	}()

	timer := time.NewTimer(fileStatusObservationPromptLimit)
	defer timer.Stop()
	var statusResult fileStatusObservationCallResult
	returnedPromptly := true
	select {
	case statusResult = <-statusDone:
	case <-timer.C:
		returnedPromptly = false
		t.Errorf("status read waited for the in-flight reset beyond %v", fileStatusObservationPromptLimit)
		releaseReset()
		select {
		case statusResult = <-statusDone:
		case <-time.After(5 * time.Second):
			t.Fatal("status read did not finish after releasing the reset barrier")
		}
	}
	releaseReset()
	select {
	case <-resetDone:
	case <-time.After(5 * time.Second):
		t.Fatal("cache reset did not finish after its barrier was released")
	}
	assert.NoError(t, resetErr)
	assert.True(t, returnedPromptly)
	require.NoError(t, statusResult.err)
	result, err := fileStatusObservationPathResult(statusResult.out, path)
	require.NoError(t, err)
	reportedLocal, ok := result["local"].(string)
	require.True(t, ok, "the result carries no local fact: %v", result)
	assert.Equal(t, statusUnderivable, reportedLocal)
	unbrokenRun, totalHeld, _ := fileStatusByteInfo(t, result)
	requireFileStatusByteMarker(t, unbrokenRun, markerNotDeterminable)
	requireFileStatusByteMarker(t, totalHeld, markerNotDeterminable)
}

// TestFileStatusObservationCacheStatsInvariant checks cold and repeated
// status reads against the published cache stats (FR-005/015, US1/AC4).
func TestFileStatusObservationCacheStatsInvariant(t *testing.T) {
	opt := fileStatusStateOpt(vfscommon.CacheModeFull, fs.Duration(time.Hour))
	r, vfs, statusCall := newFileStatusStateRun(t, opt)
	vfs.MountPoint = r.Fremote.Root()
	const name = "cold-status-stats"
	path := arrangeFileStatusStateRemoteFile(t, r, vfs, name, "status contents")
	fsName := fs.ConfigString(r.Fremote)
	statsCall := rc.Calls.Get("vfs/stats")
	require.NotNil(t, statsCall)

	var violation string
	err := quick.Check(func(seed uint8) bool {
		vfs.cache.Remove(name)
		before, statsErr := fileStatusObservationPublishedCacheStats(statsCall, fsName)
		if statsErr != nil {
			violation = fmt.Sprintf("could not read initial published cache stats: %v", statsErr)
			return false
		}
		reads := 2 + int(seed%5)
		unchanged := true
		for read := 0; read < reads; read++ {
			out, callErr := statusCall.Fn(context.Background(), rc.Params{
				"fs":    fsName,
				"paths": []string{path},
			})
			if callErr != nil {
				violation = fmt.Sprintf("status read %d/%d failed: %v", read+1, reads, callErr)
				return false
			}
			result, resultErr := fileStatusObservationPathResult(out, path)
			if resultErr != nil {
				violation = fmt.Sprintf("status read %d/%d returned no per-path result: %v", read+1, reads, resultErr)
				return false
			}
			if _, ok := result["local"].(string); !ok {
				violation = fmt.Sprintf("status read %d/%d returned no state: %v", read+1, reads, result)
				return false
			}
			after, statsErr := fileStatusObservationPublishedCacheStats(statsCall, fsName)
			if statsErr != nil {
				violation = fmt.Sprintf("could not read cache stats after status read %d/%d: %v", read+1, reads, statsErr)
				return false
			}
			if !reflect.DeepEqual(before, after) {
				unchanged = false
				if violation == "" {
					violation = fmt.Sprintf("status read %d/%d changed cache stats: before=%v after=%v", read+1, reads, before, after)
				}
			}
		}
		return unchanged
	}, &quick.Config{MaxCount: 16})
	require.NoError(t, err, "cold/repeated status reads changed published cache stats: %s", violation)
}

type fileStatusObservationMutation struct {
	size    int64
	started chan struct{}
	done    chan error
}

// TestFileStatusObservationSnapshotUnderInterleaving checks that every
// concurrent status result equals a complete quiescent per-file observation,
// not a mixture of the two records surrounding an update (FR-016/022).
func TestFileStatusObservationSnapshotUnderInterleaving(t *testing.T) {
	previousProcs := runtime.GOMAXPROCS(2)
	defer runtime.GOMAXPROCS(previousProcs)

	opt := fileStatusStateOpt(vfscommon.CacheModeFull, fs.Duration(0))
	r, vfs, statusCall := newFileStatusStateRun(t, opt)
	vfs.MountPoint = r.Fremote.Root()
	const (
		name = "coherent-status"
		size = int64(2048)
	)
	path := arrangeFileStatusStateRemoteFile(t, r, vfs, name, strings.Repeat("x", int(size)))
	fsName := fs.ConfigString(r.Fremote)
	remoteObject, err := r.Fremote.NewObject(context.Background(), name)
	require.NoError(t, err)
	item := vfs.cache.Item(name)
	require.NoError(t, item.Open(remoteObject))
	t.Cleanup(func() {
		assert.NoError(t, item.Close(nil))
	})
	for offset := int64(0); offset < size; offset += 2 {
		written, _, writeErr := item.WriteAtNoOverwrite([]byte{byte(offset)}, offset)
		require.NoError(t, writeErr)
		require.Equal(t, 1, written)
	}
	written, _, err := item.WriteAtNoOverwrite([]byte("z"), size-1)
	require.NoError(t, err)
	require.Equal(t, 1, written)
	item.Dirty()

	stateA := fileStatusObservationPublicState(t, statusCall, fsName, path)
	local, upload := requireFileStatusStatePair(t, stateA)
	require.Equal(t, statusPartlyHeld, local)
	require.Equal(t, statusUploadFailed, upload)
	require.NoError(t, item.Truncate(size-1))
	stateB := fileStatusObservationPublicState(t, statusCall, fsName, path)
	require.NotEqual(t, stateA, stateB)
	local, upload = requireFileStatusStatePair(t, stateB)
	require.Equal(t, statusPartlyHeld, local)
	require.Equal(t, statusUploadFailed, upload)
	written, err = item.WriteAt([]byte("z"), size-1)
	require.NoError(t, err)
	require.Equal(t, 1, written)
	require.Equal(t, stateA, fileStatusObservationPublicState(t, statusCall, fsName, path))

	updates := make(chan fileStatusObservationMutation)
	writerReady := make(chan struct{})
	writerDone := make(chan struct{})
	go func() {
		close(writerReady)
		defer close(writerDone)
		for update := range updates {
			close(update.started)
			runtime.Gosched()
			if update.size == size {
				written, writeErr := item.WriteAt([]byte("z"), size-1)
				if writeErr == nil && written != 1 {
					writeErr = fmt.Errorf("restoring final cached byte wrote %d bytes", written)
				}
				update.done <- writeErr
			} else {
				update.done <- item.Truncate(update.size)
			}
		}
	}()
	<-writerReady
	defer func() {
		close(updates)
		select {
		case <-writerDone:
		case <-time.After(5 * time.Second):
			t.Errorf("snapshot interleaving writer did not finish")
		}
	}()

	currentSize := size
	var violation string
	err = quick.Check(func(seed uint8) bool {
		if currentSize != size {
			written, restoreErr := item.WriteAt([]byte("z"), size-1)
			if restoreErr != nil {
				violation = fmt.Sprintf("could not restore initial snapshot: %v", restoreErr)
				return false
			}
			if written != 1 {
				violation = fmt.Sprintf("restoring initial snapshot wrote %d bytes", written)
				return false
			}
			currentSize = size
		}

		valid := true
		observations := 128 + int(seed%128)
		for observation := 0; observation < observations; observation++ {
			targetSize := size - 1
			if observation%2 != 0 {
				targetSize = size
			}
			update := fileStatusObservationMutation{
				size:    targetSize,
				started: make(chan struct{}),
				done:    make(chan error, 1),
			}
			updates <- update
			<-update.started
			observed := fileStatusObservationPublicState(t, statusCall, fsName, path)
			if updateErr := <-update.done; updateErr != nil {
				violation = fmt.Sprintf("update %d/%d failed: %v", observation+1, observations, updateErr)
				return false
			}
			currentSize = targetSize
			if !reflect.DeepEqual(observed, stateA) && !reflect.DeepEqual(observed, stateB) {
				valid = false
				if violation == "" {
					violation = fmt.Sprintf("observation %d/%d was not either complete record: got=%+v stateA=%+v stateB=%+v", observation+1, observations, observed, stateA, stateB)
				}
			}
		}
		return valid
	}, &quick.Config{MaxCount: 8})
	require.NoError(t, err, "status read observed a torn per-file record: %s", violation)
}
