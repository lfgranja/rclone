package vfs

// Harnesses and acceptance tests for the concurrency and
// latency criteria of the vfs/file-status endpoint
// (specs/001-vfs-file-status/spec.md):
//
//   - FR-016's two-oracle concurrency test: deterministic on
//     a quiescent mount, invariant while a transfer and a
//     reset of the file being asked about are in flight,
//     both under the project's race-detector CI target.
//   - SC-010's latency measurement: warm-up calibrated by a
//     preliminary steady-state experiment and recorded
//     (never fewer than 5 reads discarded, never more than
//     the 20-read budget), reads issued as independent
//     concurrently scheduled requests released from a
//     common barrier with no read waiting on another's
//     completion, the pass rule the per-run maximum over 100
//     reads with every run passing, and the reset-in-flight
//     case measured over the same 100 reads spanning one
//     engaged reset window, the reset initiated at the
//     window start with its overlap proven per run.
//
// This file also hosts the load drivers shared with the
// SC-027 resource harness in file_status_resource_test.go:
// the paced ≥10 MiB transfer load with first-write
// readiness and per-chunk overlap proof, and the engaged
// cache-reset drivers (single reset with a recorded window,
// and the repopulating reset loop) that prove the file
// being asked about was really under reset while reads
// ran. A reset that reports anything but ResetComplete
// never held the file under reset, so the harness treats
// it as a setup conflict and fails instead of measuring
// against an idle file.
//
// The two no-load assertions run in short mode; the load
// matrices skip themselves in short mode and are compiled
// either way.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/rc"
	"github.com/rclone/rclone/fstest"
	"github.com/rclone/rclone/vfs/vfscache"
	"github.com/rclone/rclone/vfs/vfscommon"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// SC-010's constants: the ceiling is Nielsen's 0.1 s rule,
// one run measures 100 reads after a calibrated warm-up
// (never fewer than 5, never more than the 20-read
// budget), and the pass rule is the per-run maximum with
// every run passing.
const (
	// fileStatusLatencyCeiling is the time one single-path
	// state read must complete within, irrespective of the
	// mount's contents, of any transfer in progress
	// elsewhere in the mount, and of a reset of the file
	// being asked about.
	fileStatusLatencyCeiling = 100 * time.Millisecond

	// fileStatusReadsPerRun is the number of reads one
	// timing run measures; the pass rule is the maximum
	// over these reads, never a mean or a percentile.
	fileStatusReadsPerRun = 100

	// fileStatusWarmupDiscard is the warm-up budget one
	// timing run may discard: the calibrated warm-up
	// never exceeds it.
	fileStatusWarmupDiscard = 20

	// fileStatusWarmupFloor is the smallest warm-up
	// discard the harness ever performs.
	fileStatusWarmupFloor = 5

	// fileStatusWarmupBudget is the number of reads the
	// preliminary steady-state experiment issues to
	// determine c₁.
	fileStatusWarmupBudget = 40

	// fileStatusWarmupSteady is how many consecutive
	// reads must lie inside the steady band before the
	// experiment declares the steady state reached.
	fileStatusWarmupSteady = 5

	// fileStatusDefaultRuns is how many timing runs a
	// measurement performs; the per-run max rule applies to
	// every run, so every one of them must pass.
	fileStatusDefaultRuns = 3

	// fileStatusMinConcurrentReads is the smallest number
	// of reads that must overlap in flight for a timing
	// run to count as concurrent. SC-010 requires at
	// least 10 concurrent reads while the transfer and
	// one reset are in flight; the harness releases all
	// 100 from one barrier and proves the overlap from
	// the recorded start/end instants.
	fileStatusMinConcurrentReads = 10
)

// FR-016's oracle call set: N endpoint calls issued in
// parallel, over a set of distinct paths each named more
// than once so the same-path-requested-repeatedly clause
// is covered too.
const (
	fileStatusOracleCalls = 16
	fileStatusOraclePaths = 8
)

// SC-010's declared load: a concurrent transfer of at
// least 10 MiB through the mount, paced so it stays in
// flight across a measurement window, and a drain delay
// for stopping a load driver.
const (
	fileStatusTransferSize   = 10 << 20  // the transfer load's payload
	fileStatusTransferChunk  = 256 << 10 // transfer chunk between paces
	fileStatusTransferPace   = 1 * time.Millisecond
	fileStatusLoadDrainDelay = 5 * time.Second
)

// fileStatusResetTargetSize is the size of the file the
// single-reset latency window asks about. The engaged
// reset path performs real local disk I/O whose duration
// grows with nothing in particular, so the size only
// needs to be large enough that the reset window is wide
// enough to span: 8 MiB was measured at ~1 ms per
// engaged reset, spanning ~90 of the 100 reads.
const fileStatusResetTargetSize = 8 << 20

// fileStatusResetLoopSize is the size of the file the
// reset-loop driver (oracle and CPU-under-load) churns.
// Smaller than the single-reset target because the loop
// repopulates the entry between resets and iterates far
// more often.
const fileStatusResetLoopSize = 2 << 20

// fileStatusNewRun is the rc_test.go harness with caching
// enabled: the concurrency and resource criteria are
// measured against a mount with a live cache, since a
// state read is a read of the cache's state.
func fileStatusNewRun(t *testing.T, method string) (r *fstest.Run, vfs *VFS, call *rc.Call) {
	if *fstest.RemoteName != "" {
		t.Skip("Skipping test on non local remote")
	}
	opt := vfscommon.Opt
	opt.CacheMode = vfscommon.CacheModeMinimal
	r, vfs = newTestVFSOpt(t, &opt)
	call = rc.Calls.Get(method)
	require.NotNil(t, call)
	return r, vfs, call
}

// fileStatusNewRunFull is fileStatusNewRun with full
// caching and no handle-caching grace period. Full mode
// populates cache entries on plain reads (minimal only
// caches read/write opens), and disabling the grace
// period keeps a closed file out of the grace-skipped
// reset path, so the reset drivers below can engage the
// file deterministically.
func fileStatusNewRunFull(t *testing.T, method string) (r *fstest.Run, vfs *VFS, call *rc.Call) {
	if *fstest.RemoteName != "" {
		t.Skip("Skipping test on non local remote")
	}
	opt := vfscommon.Opt
	opt.CacheMode = vfscommon.CacheModeFull
	opt.HandleCaching = 0
	r, vfs = newTestVFSOpt(t, &opt)
	call = rc.Calls.Get(method)
	require.NotNil(t, call)
	return r, vfs, call
}

// fileStatusWaitClean waits until the cache entry for the
// mount-relative name is no longer dirty (its upload
// completed) so the measurement that follows runs on a
// quiet mount. A file that never becomes clean is a
// setup conflict, not a slow test.
func fileStatusWaitClean(t *testing.T, vfs *VFS, mountName string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		item := vfs.cache.Item(mountName)
		require.NotNil(t, item, "cache entry for %q vanished while waiting for clean", mountName)
		if !item.IsDirty() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("cache entry for %q still dirty after %v (upload never drained)", mountName, timeout)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// fileStatusHoldOpen opens the mount-relative name for
// reading and pins the cache entry open: one byte is
// read through the handle because the mount opens the
// cache item lazily on first I/O, and without an open
// the reset below takes the instant RemovedNotInUse path
// instead of the engaged one. The caller closes the
// returned handle when the window is over.
func fileStatusHoldOpen(t *testing.T, vfs *VFS, mountName string) Handle {
	t.Helper()
	fh, err := vfs.Open(mountName)
	require.NoError(t, err, "opening %q to hold", mountName)
	_, err = fh.Read(make([]byte, 1))
	require.NoError(t, err, "first read through held handle for %q (pins the cache item open)", mountName)
	return fh
}

// fileStatusCalibrateWarmup runs the preliminary
// steady-state experiment SC-010 requires: budget reads
// are issued, the steady band is derived from the median
// of the last 20, and c₁ is the first index from which
// fileStatusWarmupSteady consecutive reads lie inside it.
// It returns the discard count the timing runs must use:
// c₁ clamped to [fileStatusWarmupFloor,
// fileStatusWarmupDiscard]. A mount that never reaches
// the steady state within the budget fails instead of
// silently measuring cold reads.
func fileStatusCalibrateWarmup(t *testing.T, read func() error) int {
	t.Helper()
	durations := make([]time.Duration, 0, fileStatusWarmupBudget)
	for i := 0; i < fileStatusWarmupBudget; i++ {
		start := time.Now()
		require.NoError(t, read(), "calibration read %d", i)
		durations = append(durations, time.Since(start))
	}
	tail := append([]time.Duration(nil), durations[len(durations)-20:]...)
	for i, j := 0, len(tail)-1; i < j; i, j = i+1, j-1 {
		tail[i], tail[j] = tail[j], tail[i]
	}
	// Median of the tail by insertion sort; 20 elements only.
	for i := 1; i < len(tail); i++ {
		for j := i; j > 0 && tail[j] < tail[j-1]; j-- {
			tail[j], tail[j-1] = tail[j-1], tail[j]
		}
	}
	median := tail[len(tail)/2]
	band := median + 5*time.Millisecond
	if slack := 2 * median; slack > band-median {
		band = median + slack
	}
	c1 := -1
outer:
	for i := 0; i+fileStatusWarmupSteady <= len(durations); i++ {
		for j := i; j < i+fileStatusWarmupSteady; j++ {
			if durations[j] > band {
				continue outer
			}
		}
		c1 = i
		break
	}
	t.Logf("warm-up calibration: %d reads, tail median %v, steady band ≤%v, c₁=%d",
		len(durations), median, band, c1)
	require.NotEqual(t, -1, c1,
		"SC-010: no steady state within the %d-read calibration budget (tail median %v)", fileStatusWarmupBudget, median)
	require.LessOrEqual(t, c1, fileStatusWarmupDiscard,
		"SC-010: steady state needs c₁=%d reads, more than the %d-read discard budget", c1, fileStatusWarmupDiscard)
	if c1 < fileStatusWarmupFloor {
		c1 = fileStatusWarmupFloor
	}
	t.Logf("warm-up discard per run: %d (floor %d, budget %d)", c1, fileStatusWarmupFloor, fileStatusWarmupDiscard)
	return c1
}

// fileStatusBarrierReadsErr issues n reads as independent
// concurrently scheduled requests: every goroutine parks
// on one barrier and the release starts them at once, so
// no read waits on another's completion and each read is
// timed from its intended start — the release instant —
// so a read delayed behind another's completion is
// measured as delayed. sampleReset, when non-nil, is
// called by each read just before its endpoint call and
// reports whether the file was observed under reset at
// that instant. It returns the run's maximum read time,
// every read's start/end instants, the peak number of
// reads in flight together, how many reads observed the
// reset, and the first per-read error, if any.
func fileStatusBarrierReadsErr(n int, read func() error, sampleReset func() bool) (max time.Duration, starts, ends []time.Time, maxInflight int64, overlapped int, readErr error) {
	starts = make([]time.Time, n)
	ends = make([]time.Time, n)
	errs := make([]error, n)
	sawReset := make([]bool, n)
	var inFlight atomic.Int64
	release := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			<-release
			start := time.Now()
			starts[i] = start
			current := inFlight.Add(1)
			for {
				peak := maxInflight
				if current <= peak || atomic.CompareAndSwapInt64(&maxInflight, peak, current) {
					break
				}
			}
			if sampleReset != nil && sampleReset() {
				sawReset[i] = true
			}
			errs[i] = read()
			ends[i] = time.Now()
			inFlight.Add(-1)
		}(i)
	}
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			return 0, nil, nil, 0, 0, fmt.Errorf("measured read %d: %w", i, err)
		}
	}
	for i := 0; i < n; i++ {
		if d := ends[i].Sub(starts[i]); d > max {
			max = d
		}
		if sawReset[i] {
			overlapped++
		}
	}
	return max, starts, ends, maxInflight, overlapped, nil
}

// fileStatusAssertLatency asserts the SC-010 pass rule
// over per-run maxima: every run's maximum read time is
// within the 100 ms ceiling.
func fileStatusAssertLatency(t *testing.T, maxima []time.Duration) {
	t.Helper()
	for run, max := range maxima {
		assert.LessOrEqual(t, max, fileStatusLatencyCeiling,
			"SC-010: run %d maximum read time exceeds the %s ceiling", run, fileStatusLatencyCeiling)
	}
}

// fileStatusTransferProof is the readiness and overlap
// record of one transfer load driver.
type fileStatusTransferProof struct {
	ready    chan struct{}
	once     sync.Once
	bytes    atomic.Int64
	chunks   atomic.Int64
	mu       sync.Mutex
	chunkEnd []time.Time // end instants of recent chunk writes
}

// fileStatusNoteChunk records one completed chunk write
// of n bytes ending now.
func (p *fileStatusTransferProof) noteChunk(n int, end time.Time) {
	p.once.Do(func() { close(p.ready) })
	p.bytes.Add(int64(n))
	p.chunks.Add(1)
	p.mu.Lock()
	p.chunkEnd = append(p.chunkEnd, end)
	if len(p.chunkEnd) > 16 {
		p.chunkEnd = p.chunkEnd[len(p.chunkEnd)-16:]
	}
	p.mu.Unlock()
}

// fileStatusChunkDuring reports whether any recorded
// chunk write ended inside [start, end].
func (p *fileStatusTransferProof) chunkDuring(start, end time.Time) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, ts := range p.chunkEnd {
		if !ts.Before(start) && !ts.After(end) {
			return true
		}
	}
	return false
}

// fileStatusWaitChunkAfter waits for a chunk write ending
// after t0, proving the transfer was alive past that
// instant. It reports whether one was observed before
// the timeout: a transfer that produces no chunk for
// that long is stalled, and the window measured against
// it proves nothing.
func (p *fileStatusTransferProof) waitForChunkAfter(t0 time.Time, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		p.mu.Lock()
		for _, ts := range p.chunkEnd {
			if ts.After(t0) {
				p.mu.Unlock()
				return true
			}
		}
		p.mu.Unlock()
		time.Sleep(time.Millisecond)
	}
	return false
}

// fileStatusTransferLoad keeps a transfer of at least
// fileStatusTransferSize bytes through the mount in
// flight until stop is closed: the payload is written
// through the mount in paced chunks, so a transfer is
// concurrently in progress across a measurement window
// rather than completing in a burst. Every completed
// chunk is recorded on proof, whose readiness gate
// (first chunk written) the caller waits on before
// measuring, and whose chunk instants prove after the
// window that writes landed inside it.
func fileStatusTransferLoad(vfs *VFS, name string, payload []byte, stop <-chan struct{}, proof *fileStatusTransferProof) error {
	for {
		select {
		case <-stop:
			return nil
		default:
		}
		fh, err := vfs.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
		if err != nil {
			return err
		}
		failed := false
		for off := 0; off < len(payload); off += fileStatusTransferChunk {
			select {
			case <-stop:
				_ = fh.Close()
				return nil
			default:
			}
			end := off + fileStatusTransferChunk
			if end > len(payload) {
				end = len(payload)
			}
			if _, err := fh.Write(payload[off:end]); err != nil {
				_ = fh.Close()
				return err
			}
			proof.noteChunk(end-off, time.Now())
			// Pace the transfer so it stays in flight
			// across a measurement window.
			time.Sleep(fileStatusTransferPace)
			_ = failed
		}
		if err := fh.Close(); err != nil {
			return err
		}
	}
}

// fileStatusStartTransfer starts the transfer load in the
// background and waits for its readiness gate — the first
// chunk written — before returning, so no measurement
// window starts against a transfer that never got going.
// Cleanup stops the driver and requires it to exit.
func fileStatusStartTransfer(t *testing.T, vfs *VFS, name string, payload []byte) (*fileStatusTransferProof, chan struct{}) {
	t.Helper()
	require.GreaterOrEqual(t, len(payload), fileStatusTransferSize,
		"SC-010: the transfer load must move at least 10 MiB, payload is %d bytes", len(payload))
	proof := &fileStatusTransferProof{ready: make(chan struct{})}
	stop := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- fileStatusTransferLoad(vfs, name, payload, stop, proof)
	}()
	select {
	case <-proof.ready:
	case <-time.After(60 * time.Second):
		t.Fatal("transfer load never wrote its first chunk (readiness gate): no transfer was in flight")
	}
	t.Logf("transfer load ready: ≥10 MiB payload (%d bytes) in flight on %q", len(payload), name)
	t.Cleanup(func() {
		close(stop)
		select {
		case err := <-done:
			assert.NoError(t, err)
		case <-time.After(fileStatusLoadDrainDelay):
			t.Error("transfer load did not stop")
		}
	})
	return proof, stop
}

// fileStatusResetOutcome is the recorded window of one
// cache reset: when it ran and whether it really held
// the file under reset.
type fileStatusResetOutcome struct {
	start   time.Time
	end     time.Time
	result  vfscache.ResetResult
	missing bool
}

// fileStatusEngaged reports whether the reset took the
// engaged path — the only one that holds the file under
// reset. Any skipped or removed outcome means the file
// was not under reset, whatever the caller intended.
func (o fileStatusResetOutcome) engaged() bool {
	return !o.missing && o.result == vfscache.ResetComplete
}

// fileStatusDoReset initiates one reset of the named
// file's cache item — the same reset primitive the
// cache cleaner uses — and records its window and
// outcome. The caller proves engagement from the
// returned outcome; a skipped reset is reported, never
// mistaken for a reset window.
func fileStatusDoReset(vfs *VFS, mountName string) fileStatusResetOutcome {
	item := vfs.cache.Item(mountName)
	if item == nil {
		return fileStatusResetOutcome{missing: true}
	}
	outcome := fileStatusResetOutcome{start: time.Now()}
	outcome.result, _, _ = item.Reset()
	outcome.end = time.Now()
	return outcome
}

// fileStatusResetLoop is a reset load driver that keeps
// the named file under intermittent reset until stopped:
// each iteration repopulates the entry through the mount
// (a reset empties it, and an empty entry resets to
// SkippedEmpty) and then resets it, counting attempts
// and engaged resets. At least one engaged reset is the
// proof the driver really held the file under reset.
type fileStatusResetLoop struct {
	stop     chan struct{}
	wg       sync.WaitGroup
	attempts atomic.Int64
	engaged  atomic.Int64
	errors   atomic.Int64
}

// fileStatusStartResetLoop starts the reset loop driver.
// The file must be held open by the caller (see
// fileStatusHoldOpen) so resets engage instead of taking
// the instant not-in-use path.
func fileStatusStartResetLoop(vfs *VFS, mountName string) *fileStatusResetLoop {
	loop := &fileStatusResetLoop{stop: make(chan struct{})}
	loop.wg.Add(1)
	go func() {
		defer loop.wg.Done()
		for {
			select {
			case <-loop.stop:
				return
			default:
			}
			if _, err := vfs.ReadFile(mountName); err != nil {
				loop.errors.Add(1)
				continue
			}
			outcome := fileStatusDoReset(vfs, mountName)
			loop.attempts.Add(1)
			if outcome.engaged() {
				loop.engaged.Add(1)
			}
		}
	}()
	return loop
}

// fileStatusStop stops the driver and returns the
// attempt/engaged/error counts.
func (loop *fileStatusResetLoop) fileStatusStop() (attempts, engaged, errors int64) {
	close(loop.stop)
	loop.wg.Wait()
	return loop.attempts.Load(), loop.engaged.Load(), loop.errors.Load()
}

// fileStatusSampleBeingReset polls IsBeingReset until
// stop is closed, counting observations of the file
// under reset. It is secondary evidence beside the
// reset's own outcome: the outcome proves a reset ran,
// the sampler proves reads could observe it.
func fileStatusSampleBeingReset(vfs *VFS, mountName string, interval time.Duration, stop <-chan struct{}) *atomic.Int64 {
	var count atomic.Int64
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				if item := vfs.cache.Item(mountName); item != nil && item.IsBeingReset() {
					count.Add(1)
				}
			}
		}
	}()
	return &count
}

// fileStatusEndpointCall issues one vfs/file-status
// call in-process — the endpoint handler invocation
// pattern the SC-010 measurement point is defined
// against — and returns the per-path code vector of
// its response, aligned with the requested paths: a
// result that reports an error contributes its error
// code, and a result that reports a state contributes
// its local fact. Results are matched by the path
// each result echoes, since the contract promises no
// ordering. A whole-request rejection is returned as
// the error it is: the oracle compares per-path
// answers, and a request naming valid paths must be
// answered per path.
func fileStatusEndpointCall(call *rc.Call, in rc.Params, paths []string) ([]string, error) {
	out, err := call.Fn(context.Background(), in)
	if err != nil {
		return nil, err
	}
	return fileStatusCodeVector(out, paths)
}

// fileStatusCodeVector extracts the per-path code vector
// from a successful response. The wire contract is JSON,
// so the response is normalized through it, keeping the
// oracle independent of the handler's Go types.
func fileStatusCodeVector(out rc.Params, paths []string) ([]string, error) {
	blob, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("marshaling response: %w", err)
	}
	var response struct {
		Statuses []struct {
			Path  string `json:"path"`
			Local string `json:"local"`
			Error *struct {
				Code string `json:"code"`
			} `json:"error"`
		} `json:"statuses"`
	}
	if err := json.Unmarshal(blob, &response); err != nil {
		return nil, fmt.Errorf("decoding response: %w", err)
	}
	// A request naming each path once is answered
	// exactly once per path (FR-016's once-per-path
	// rule).
	if len(response.Statuses) != len(paths) {
		return nil, fmt.Errorf("response answers %d results for %d requested paths",
			len(response.Statuses), len(paths))
	}
	codes := make(map[string]string, len(response.Statuses))
	for _, status := range response.Statuses {
		if status.Error != nil {
			codes[status.Path] = status.Error.Code
		} else {
			codes[status.Path] = status.Local
		}
	}
	if len(codes) != len(response.Statuses) {
		return nil, fmt.Errorf("response answers %d results for %d distinct paths",
			len(response.Statuses), len(codes))
	}
	vector := make([]string, len(paths))
	for i, path := range paths {
		code, ok := codes[path]
		if !ok {
			return nil, fmt.Errorf("response answers no result for path %q", path)
		}
		vector[i] = code
	}
	return vector, nil
}

// fileStatusAssertPerPathResults asserts that a response
// is a well-formed per-path result set: one result per
// requested path, each echoing the path it answers and
// carrying either both reported facts or a per-path
// error — never a whole-request rejection, and never a
// state result missing a fact.
func fileStatusAssertPerPathResults(t *testing.T, out rc.Params, paths []string) {
	t.Helper()
	blob, err := json.Marshal(out)
	require.NoError(t, err)
	var response struct {
		Statuses []struct {
			Path   string `json:"path"`
			Local  string `json:"local"`
			Upload string `json:"upload"`
			Error  *struct {
				Code string `json:"code"`
			} `json:"error"`
		} `json:"statuses"`
	}
	require.NoError(t, json.Unmarshal(blob, &response))
	byPath := make(map[string]int, len(response.Statuses))
	for _, status := range response.Statuses {
		byPath[status.Path]++
	}
	assert.Len(t, response.Statuses, len(paths),
		"response does not answer each requested path exactly once")
	for _, path := range paths {
		assert.Equal(t, 1, byPath[path], "path %q is not answered exactly once", path)
	}
	for _, status := range response.Statuses {
		if status.Error != nil {
			continue
		}
		assert.NotEmpty(t, status.Path, "state result echoes no path")
		assert.NotEmpty(t, status.Local, "result %q reports no local fact", status.Path)
		assert.NotEmpty(t, status.Upload, "result %q reports no upload fact", status.Path)
	}
}

// TestFileStatusPerfSingleReadLatency asserts SC-010's
// basic case: a single-path state read of a cached file
// completes within 100 milliseconds under no load. The
// warm-up is calibrated by the preliminary steady-state
// experiment and recorded, the reads of each run are
// released from one barrier as independent concurrently
// scheduled requests, and the pass rule is applied per
// run with every run passing.
func TestFileStatusPerfSingleReadLatency(t *testing.T) {
	r, vfs, call := fileStatusNewRun(t, "vfs/file-status")
	const mountName = "/single-read.txt"
	payload := []byte("single-read state payload")
	require.NoError(t, vfs.WriteFile(mountName, payload, 0o644))
	fileStatusWaitClean(t, vfs, mountName, 60*time.Second)
	path := filepath.Join(r.Fremote.Root(), "single-read.txt")
	in := rc.Params{
		"fs":    fs.ConfigString(r.Fremote),
		"paths": []string{path},
	}
	read := func() error {
		_, err := call.Fn(context.Background(), in)
		return err
	}
	warmup := fileStatusCalibrateWarmup(t, read)
	var maxima []time.Duration
	for run := 0; run < fileStatusDefaultRuns; run++ {
		for i := 0; i < warmup; i++ {
			require.NoError(t, read(), "run %d warm-up read %d", run, i)
		}
		max, _, _, maxInflight, _, err := fileStatusBarrierReadsErr(fileStatusReadsPerRun, read, nil)
		require.NoError(t, err, "run %d", run)
		t.Logf("run %d: max=%v peak-in-flight=%d (need ≥%d)", run, max, maxInflight, fileStatusMinConcurrentReads)
		assert.GreaterOrEqual(t, maxInflight, int64(fileStatusMinConcurrentReads),
			"SC-010: run %d peaked at %d reads in flight, fewer than the %d concurrent reads the criterion requires",
			run, maxInflight, fileStatusMinConcurrentReads)
		maxima = append(maxima, max)
	}
	fileStatusAssertLatency(t, maxima)
	fileStatusLogDistribution(t, "single-read maxima", maxima, fileStatusLatencyCeiling)
}

// TestFileStatusPerfConcurrentOracle asserts FR-016's
// first oracle: on a quiescent mount the endpoint is
// deterministic, so the same N requests issued in
// parallel answer with the same per-path code vector as
// when issued serially — every concurrent response
// equals a serial ordering of the requests, with no
// cross-path dependency. The request set names each
// path more than once, so the same-path-requested-
// repeatedly clause of FR-016 is covered too.
func TestFileStatusPerfConcurrentOracle(t *testing.T) {
	r, _, call := rcNewRun(t, "vfs/file-status")
	distinct := make([]string, fileStatusOraclePaths)
	for i := range distinct {
		distinct[i] = filepath.Join(r.Fremote.Root(), fmt.Sprintf("oracle-%02d.txt", i))
	}
	requests := make([]rc.Params, fileStatusOracleCalls)
	for i := range requests {
		requests[i] = rc.Params{
			"fs":    fs.ConfigString(r.Fremote),
			"paths": []string{distinct[i%len(distinct)]},
		}
	}
	// Serial oracle: the requests issued one at a time.
	serial := make([][]string, len(requests))
	for i, in := range requests {
		var err error
		serial[i], err = fileStatusEndpointCall(call, in, []string{distinct[i%len(distinct)]})
		require.NoError(t, err, "serial request %d", i)
	}
	// Concurrent oracle: the same requests issued in
	// parallel, each an independently scheduled request.
	concurrent := make([][]string, len(requests))
	errs := make([]error, len(requests))
	var wg sync.WaitGroup
	wg.Add(len(requests))
	for i, in := range requests {
		go func(i int, in rc.Params) {
			defer wg.Done()
			concurrent[i], errs[i] = fileStatusEndpointCall(call, in, []string{distinct[i%len(distinct)]})
		}(i, in)
	}
	wg.Wait()
	for i, err := range errs {
		require.NoError(t, err, "concurrent request %d", i)
	}
	for i := range serial {
		assert.Equal(t, serial[i], concurrent[i],
			"concurrent request %d answers differently from the same request issued serially", i)
	}
}

// TestFileStatusPerfLatencyUnderLoad asserts SC-010
// under its declared load: a concurrent transfer of at
// least 10 MiB through the mount elsewhere in the mount,
// and one engaged reset of the file being asked about
// initiated at the start of each measurement window, the
// same 100 reads spanning the reset window. Each run
// proves its load: transfer chunks landed inside the
// window, the reset engaged (ResetComplete) with the
// recorded window overlapped by at least 10 reads, and
// at least 10 reads in flight together. A change which
// starts waiting on the file's reset or transfer
// condition fails the ceiling rather than degrading
// quietly. Peak CPU utilization per run is reported
// alongside the latency (SC-027).
func TestFileStatusPerfLatencyUnderLoad(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping the SC-010 under-load matrix in short mode")
	}
	r, vfs, call := fileStatusNewRunFull(t, "vfs/file-status")
	// The file the reads ask about, written through the
	// mount so the cache holds an entry the reset can
	// act on. The transfer load runs elsewhere in the
	// mount, on its own file.
	const target = "/under-load.txt"
	hostTarget := filepath.Join(r.Fremote.Root(), "under-load.txt")
	payload := make([]byte, fileStatusTransferSize)
	for i := range payload {
		payload[i] = byte(i)
	}
	targetPayload := make([]byte, fileStatusResetTargetSize)
	for i := range targetPayload {
		targetPayload[i] = byte(i)
	}
	require.NoError(t, vfs.WriteFile(target, targetPayload, 0o644))
	fileStatusWaitClean(t, vfs, target, 120*time.Second)
	// Hold the file open so the reset takes the engaged
	// path; without an open it is removed instantly and
	// no reset window exists to span.
	held := fileStatusHoldOpen(t, vfs, target)
	defer func() {
		assert.NoError(t, held.Close())
	}()
	const transferPath = "/transfer-load.bin"

	in := rc.Params{
		"fs":    fs.ConfigString(r.Fremote),
		"paths": []string{hostTarget},
	}
	read := func() error {
		_, err := call.Fn(context.Background(), in)
		return err
	}
	// Calibrated before any load starts, recorded per run below.
	warmup := fileStatusCalibrateWarmup(t, read)

	// Transfer load: a concurrent transfer of at least
	// 10 MiB through the mount, readiness-gated so no
	// window starts before it is in flight.
	proof, _ := fileStatusStartTransfer(t, vfs, transferPath, payload)

	var maxima []time.Duration
	for run := 0; run < fileStatusDefaultRuns; run++ {
		// Repopulate: the previous run's reset emptied
		// the entry, and an empty entry resets to
		// SkippedEmpty instead of engaging.
		_, err := vfs.ReadFile(target)
		require.NoError(t, err, "run %d repopulating %q", run, target)
		for i := 0; i < warmup; i++ {
			require.NoError(t, read(), "run %d warm-up read %d", run, i)
		}
		// One engaged reset and the 100 reads share a
		// single barrier: every goroutine parks first
		// (50 ms settle inside the helper), then the
		// reset is initiated at the window start and
		// the reads span its window.
		release := make(chan struct{})
		var outcome fileStatusResetOutcome
		var resetDone sync.WaitGroup
		resetDone.Add(1)
		go func() {
			defer resetDone.Done()
			<-release
			outcome = fileStatusDoReset(vfs, target)
		}()
		starts := make([]time.Time, fileStatusReadsPerRun)
		ends := make([]time.Time, fileStatusReadsPerRun)
		errs := make([]error, fileStatusReadsPerRun)
		sawReset := make([]bool, fileStatusReadsPerRun)
		var inFlight, maxInflight atomic.Int64
		var wg sync.WaitGroup
		wg.Add(fileStatusReadsPerRun)
		for i := 0; i < fileStatusReadsPerRun; i++ {
			go func(i int) {
				defer wg.Done()
				<-release
				starts[i] = time.Now()
				current := inFlight.Add(1)
				for {
					peak := maxInflight.Load()
					if current <= peak || maxInflight.CompareAndSwap(peak, current) {
						break
					}
				}
				if item := vfs.cache.Item(target); item != nil && item.IsBeingReset() {
					sawReset[i] = true
				}
				errs[i] = read()
				ends[i] = time.Now()
				inFlight.Add(-1)
			}(i)
		}
		time.Sleep(50 * time.Millisecond)
		windowStart := time.Now()
		cpuStart, cpuErr := fileStatusProcessCPUNS()
		require.NoError(t, cpuErr, "run %d reading the process CPU clock", run)
		close(release)
		wg.Wait()
		resetDone.Wait()
		for i, err := range errs {
			require.NoError(t, err, "run %d measured read %d", run, i)
		}
		windowEnd := time.Now()
		var max time.Duration
		overlapped := 0
		for i := 0; i < fileStatusReadsPerRun; i++ {
			if d := ends[i].Sub(starts[i]); d > max {
				max = d
			}
			if sawReset[i] {
				overlapped++
			}
		}
		peakInflight := maxInflight.Load()
		cpuEnd, err := fileStatusProcessCPUNS()
		require.NoError(t, err, "run %d reading the process CPU clock", run)
		wall := windowEnd.Sub(windowStart)
		var utilPct float64
		if wall > 0 {
			utilPct = float64(cpuEnd-cpuStart) / float64(wall) * 100
		}
		// Overlap proof: the reset engaged, and reads
		// spanning its window.
		require.True(t, outcome.engaged(),
			"run %d: reset did not engage (result=%v): no reset window existed to span", run, outcome.result)
		spanOverlap := 0
		for i := 0; i < fileStatusReadsPerRun; i++ {
			if !starts[i].After(outcome.end) && !ends[i].Before(outcome.start) {
				spanOverlap++
			}
		}
		t.Logf("run %d: max=%v reset=[%v window %v] span-overlap=%d/100 sampled-under-reset=%d peak-in-flight=%d cpu=%v wall=%v util=%.1f%%",
			run, max, outcome.result, outcome.end.Sub(outcome.start), spanOverlap, overlapped, peakInflight,
			time.Duration(cpuEnd-cpuStart), wall, utilPct)
		assert.GreaterOrEqual(t, spanOverlap, fileStatusMinConcurrentReads,
			"run %d: only %d reads spanned the reset window, fewer than the %d the criterion requires",
			run, spanOverlap, fileStatusMinConcurrentReads)
		assert.GreaterOrEqual(t, peakInflight, int64(fileStatusMinConcurrentReads),
			"run %d: peak %d reads in flight, fewer than the %d concurrent reads the criterion requires",
			run, peakInflight, fileStatusMinConcurrentReads)
		// Transfer overlap proof: a chunk landed
		// inside the window, else wait briefly for the
		// next one; a stalled transfer proves nothing.
		if !proof.chunkDuring(windowStart, windowEnd) {
			require.True(t, proof.waitForChunkAfter(windowStart, 10*time.Second),
				"run %d: no transfer chunk inside the window and none followed: the transfer was not in flight", run)
			t.Logf("run %d: transfer chunk arrived just after the window (still overlapping its span)", run)
		}
		assert.LessOrEqual(t, utilPct, float64(100*runtime.NumCPU()),
			"run %d: CPU utilization %.1f%% exceeds %d×100%%: the process CPU clock is misbehaving", run, utilPct, runtime.NumCPU())
		maxima = append(maxima, max)
	}
	fileStatusAssertLatency(t, maxima)
	fileStatusLogDistribution(t, "under-load maxima", maxima, fileStatusLatencyCeiling)
}

// TestFileStatusPerfConcurrentOracleUnderLoad asserts
// FR-016's second oracle: while a transfer and resets of
// the file being asked about are in flight, every
// concurrent response is a well-formed per-path result —
// the read never waits on the file's transfer or reset
// condition, and no cross-path dependency leaks into the
// answer. The drivers prove they were really in flight:
// transfer chunks landed during the run and at least one
// reset engaged.
func TestFileStatusPerfConcurrentOracleUnderLoad(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping the under-load concurrency oracle in short mode")
	}
	r, vfs, call := fileStatusNewRunFull(t, "vfs/file-status")
	// The file the concurrent requests ask about — the
	// file the reset targets — and the transfer load's
	// own file, elsewhere in the mount.
	const target = "/oracle-under-load.txt"
	hostTarget := filepath.Join(r.Fremote.Root(), "oracle-under-load.txt")
	payload := make([]byte, fileStatusTransferSize)
	for i := range payload {
		payload[i] = byte(i)
	}
	loopPayload := make([]byte, fileStatusResetLoopSize)
	for i := range loopPayload {
		loopPayload[i] = byte(i)
	}
	require.NoError(t, vfs.WriteFile(target, loopPayload, 0o644))
	fileStatusWaitClean(t, vfs, target, 120*time.Second)
	held := fileStatusHoldOpen(t, vfs, target)
	defer func() {
		assert.NoError(t, held.Close())
	}()
	const transferPath = "/oracle-transfer-load.bin"

	// Transfer load, in flight for the oracle.
	proof, _ := fileStatusStartTransfer(t, vfs, transferPath, payload)

	// Reset loop, churning the file being asked about
	// for the whole oracle, with a sampler watching for
	// the file under reset.
	loop := fileStatusStartResetLoop(vfs, target)
	sampleStop := make(chan struct{})
	sampled := fileStatusSampleBeingReset(vfs, target, 100*time.Microsecond, sampleStop)

	// N parallel requests naming the file under reset.
	windowStart := time.Now()
	in := rc.Params{
		"fs":    fs.ConfigString(r.Fremote),
		"paths": []string{hostTarget},
	}
	results := make([]rc.Params, fileStatusOracleCalls)
	errs := make([]error, fileStatusOracleCalls)
	var wg sync.WaitGroup
	wg.Add(fileStatusOracleCalls)
	for i := 0; i < fileStatusOracleCalls; i++ {
		go func(i int, in rc.Params) {
			defer wg.Done()
			results[i], errs[i] = call.Fn(context.Background(), in)
		}(i, in)
	}
	wg.Wait()
	windowEnd := time.Now()
	close(sampleStop)
	attempts, engaged, loopErrs := loop.fileStatusStop()
	for i, err := range errs {
		require.NoError(t, err, "concurrent request %d", i)
	}
	for _, out := range results {
		fileStatusAssertPerPathResults(t, out, []string{hostTarget})
	}
	// Load proofs: the transfer wrote inside the window
	// and at least one reset engaged while reads ran.
	t.Logf("oracle under load: %d reads, reset attempts=%d engaged=%d repopulate-errors=%d sampled-under-reset=%d",
		fileStatusOracleCalls, attempts, engaged, loopErrs, sampled.Load())
	require.GreaterOrEqual(t, engaged, int64(1),
		"no reset engaged during the oracle (%d attempts): the reads ran against an idle file", attempts)
	assert.GreaterOrEqual(t, sampled.Load(), int64(1),
		"the file was never observed under reset while the %d reads ran", fileStatusOracleCalls)
	if !proof.chunkDuring(windowStart, windowEnd) {
		require.True(t, proof.waitForChunkAfter(windowStart, 10*time.Second),
			"no transfer chunk inside the oracle window and none followed: the transfer was not in flight")
	}
}
