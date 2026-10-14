package vfs

import (
	"context"
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

// TestFileStatusObservationResetDoesNotWait checks that an in-flight reset is
// reported as underivable while Reset holds the item lock, without waiting for
// the reset to finish (FR-004/026, SC-010).
func TestFileStatusObservationResetDoesNotWait(t *testing.T) {
	opt := fileStatusStateOpt(vfscommon.CacheModeFull, fs.Duration(time.Hour))
	r, vfs, _ := newFileStatusStateRun(t, opt)
	const name = "reset-in-flight"
	arrangeFileStatusStateCachedFile(t, r, vfs, name, "reset contents")

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
	require.Positive(t, item.GetInfo().Rs.Size())
	require.False(t, item.IsDirty())

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

	statusDone := make(chan FileStatus, 1)
	go func() {
		statusDone <- fileStatusOf(vfs, name, false, false)
	}()

	timer := time.NewTimer(fileStatusObservationPromptLimit)
	defer timer.Stop()
	var statusResult FileStatus
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
	assert.Equal(t, FileLocalUnderivable, statusResult.Local)
	statusParams := statusResult.ToParams()
	bytes, ok := statusParams["bytes"].(rc.Params)
	require.True(t, ok, "the status has no byte quantities: %v", statusParams)
	assert.Equal(t, markerNotDeterminable, bytes["unbroken_run"])
	assert.Equal(t, markerNotDeterminable, bytes["total_held"])
}

// TestFileStatusObservationCacheStatsInvariant checks cold and repeated
// status reads against the published cache stats (FR-005/015, US1/AC4).
func TestFileStatusObservationCacheStatsInvariant(t *testing.T) {
	opt := fileStatusStateOpt(vfscommon.CacheModeFull, fs.Duration(time.Hour))
	r, vfs, _ := newFileStatusStateRun(t, opt)
	const name = "cold-status-stats"
	arrangeFileStatusStateRemoteFile(t, r, vfs, name, "status contents")

	var violation string
	err := quick.Check(func(seed uint8) bool {
		vfs.cache.Remove(name)
		before := vfs.cache.Stats()
		reads := 2 + int(seed%5)
		unchanged := true
		for read := 0; read < reads; read++ {
			_ = fileStatusOf(vfs, name, false, false)
			after := vfs.cache.Stats()
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
	r, vfs, _ := newFileStatusStateRun(t, opt)
	const (
		name = "coherent-status"
		size = int64(2048)
	)
	r.WriteObject(context.Background(), name, strings.Repeat("x", int(size)), time.Now())
	listFileStatusStateRoot(t, vfs)
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

	stateA := fileStatusOf(vfs, name, false, false)
	require.Equal(t, FileLocalPartlyHeld, stateA.Local)
	require.Equal(t, FileUploadUploadFailed, stateA.Upload)
	require.NoError(t, item.Truncate(size-1))
	stateB := fileStatusOf(vfs, name, false, false)
	require.NotEqual(t, stateA, stateB)
	require.Equal(t, FileLocalPartlyHeld, stateB.Local)
	require.Equal(t, FileUploadUploadFailed, stateB.Upload)
	written, err = item.WriteAt([]byte("z"), size-1)
	require.NoError(t, err)
	require.Equal(t, 1, written)
	require.Equal(t, stateA, fileStatusOf(vfs, name, false, false))

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
			observed := fileStatusOf(vfs, name, false, false)
			if updateErr := <-update.done; updateErr != nil {
				violation = fmt.Sprintf("update %d/%d failed: %v", observation+1, observations, updateErr)
				return false
			}
			currentSize = targetSize
			if observed != stateA && observed != stateB {
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
