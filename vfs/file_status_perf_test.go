package vfs

// Harnesses and acceptance tests for the concurrency and
// resource-usage criteria of the vfs/file-status endpoint
// (specs/001-vfs-file-status/spec.md):
//
//   - FR-016's two-oracle concurrency test: deterministic on
//     a quiescent mount, invariant while a transfer and a
//     reset of the file being asked about are in flight,
//     both under the project's race-detector CI target.
//   - SC-010's latency measurement: the first 20 reads of a
//     run discarded as warm-up (never fewer than 5), reads
//     issued as independent concurrently scheduled requests
//     with no read waiting on another's completion, the pass
//     rule the per-run maximum over 100 reads with every run
//     passing, and the reset-in-flight case measured over
//     the same 100 reads spanning the reset window, the
//     reset initiated at the window start.
//   - The Resource Usage measurements: per-query ΔM from
//     field 2 of /proc/self/statm over at least 100 warm
//     repetitions, peak VmHWM bounded by C₀ + k·D
//     baseline-subtracted, per-query CPU time (user+sys)
//     growing linearly with the response size, and the
//     WSS/MRC curve under the declared load matrix of mount
//     sizes × query rates.
//
// The two no-load assertions run in short mode and fail
// against the current stub, which rejects every request;
// the load matrices skip themselves in short mode and are
// compiled either way.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/rc"
	"github.com/rclone/rclone/fstest"
	"github.com/rclone/rclone/vfs/vfscommon"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// SC-010's constants: the ceiling is Nielsen's 0.1 s rule,
// one run measures 100 reads after discarding the first 20
// (never fewer than 5), and the pass rule is the per-run
// maximum with every run passing.
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

	// fileStatusWarmupDiscard is the number of leading
	// reads one timing run discards as warm-up.
	fileStatusWarmupDiscard = 20

	// fileStatusWarmupFloor is the smallest warm-up
	// discard the harness ever performs.
	fileStatusWarmupFloor = 5

	// fileStatusDefaultRuns is how many timing runs a
	// measurement performs; the per-run max rule applies to
	// every run, so every one of them must pass.
	fileStatusDefaultRuns = 3
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
	fileStatusTransferSize   = 10 << 20           // the transfer load's payload
	fileStatusTransferChunk  = 1 << 20            // transfer chunk between paces
	fileStatusTransferPace   = 5 * time.Millisecond
	fileStatusLoadDrainDelay = 5 * time.Second
)

// Resource Usage constants: ΔM is averaged over at least
// 100 repetitions of a warm query; the VmHWM curve's
// per-distinct-file slope, the CPU linearity slack and
// the WSS/MRC curve's RSS flatness tolerance are the
// constants those curves are asserted against.
const (
	fileStatusDeltaMReps       = 100                   // warm repetitions of the measured query
	fileStatusDeltaMPopulation = 100                   // mount file count ΔM is measured against
	fileStatusDeltaMPages      = 4                     // per-query ΔM bound, in pages
	fileStatusVmHWMCurve       = 64                    // D values the VmHWM curve varies over
	fileStatusVmHWMSlopePages  = 128                   // k, the per-distinct-file slope, in pages
	fileStatusCPUStep          = 8                     // response-size step, in result objects
	fileStatusCPUWarmup        = 20                    // warm-up repetitions before measuring CPU
	fileStatusCPUReps          = 5000                  // measured repetitions per response size
	fileStatusCPUSlack         = 100 * time.Microsecond // fixed per-query CPU slack
	fileStatusClockTicks       = 100                   // sysconf(_SC_CLK_TCK), the /proc/self/stat unit
	fileStatusWSSUniverse      = 1000                  // paths the warm query set is drawn from
	fileStatusWSSQuerySet      = 100                   // the fixed, pre-generated warm query set
	fileStatusWSSQueries       = 100                   // queries per matrix point
	fileStatusRSSFlatPages     = 128                   // RSS flatness tolerance, in pages
)

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

// fileStatusTimingRun performs one SC-010 timing run
// against a single read function: the first warmup reads
// are discarded as warm-up (never fewer than
// fileStatusWarmupFloor), windowStart is invoked at the
// start of the measurement window, and readsPerRun reads
// are then issued as independent concurrently scheduled
// requests — no read waits on another's completion, and
// each read is timed from its intended start, so a read
// delayed behind another's completion is measured as
// delayed. The run's maximum read time is returned.
func fileStatusTimingRun(warmup int, read func() error, windowStart func(stop <-chan struct{})) (time.Duration, error) {
	if warmup < fileStatusWarmupFloor {
		warmup = fileStatusWarmupFloor
	}
	for i := 0; i < warmup; i++ {
		if err := read(); err != nil {
			return 0, fmt.Errorf("warm-up read %d: %w", i, err)
		}
	}
	var windowStop chan struct{}
	if windowStart != nil {
		windowStop = make(chan struct{})
		windowStart(windowStop)
		defer close(windowStop)
	}
	durations := make([]time.Duration, fileStatusReadsPerRun)
	errs := make([]error, fileStatusReadsPerRun)
	var wg sync.WaitGroup
	wg.Add(fileStatusReadsPerRun)
	for i := 0; i < fileStatusReadsPerRun; i++ {
		// Each read is timed from its intended start —
		// the moment the request is issued — not from
		// when its goroutine happens to run, so a read
		// queued behind another's completion counts the
		// queueing against the ceiling.
		start := time.Now()
		go func(i int, start time.Time) {
			defer wg.Done()
			errs[i] = read()
			durations[i] = time.Since(start)
		}(i, start)
	}
	wg.Wait()
	var max time.Duration
	for i, err := range errs {
		if err != nil {
			return 0, fmt.Errorf("measured read %d: %w", i, err)
		}
		if durations[i] > max {
			max = durations[i]
		}
	}
	return max, nil
}

// fileStatusTimingRuns performs runs timing runs and
// returns the per-run maxima, in run order. The SC-010
// pass rule applies to every run individually: a
// measurement passes only when every run's maximum is
// within the ceiling, so the rule is never satisfied by
// the minimum of these maxima.
func fileStatusTimingRuns(read func() error, windowStart func(stop <-chan struct{}), runs int) ([]time.Duration, error) {
	maxima := make([]time.Duration, 0, runs)
	for i := 0; i < runs; i++ {
		max, err := fileStatusTimingRun(fileStatusWarmupDiscard, read, windowStart)
		if err != nil {
			return nil, fmt.Errorf("run %d: %w", i, err)
		}
		maxima = append(maxima, max)
	}
	return maxima, nil
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

// fileStatusStatmResidentPages returns this process's
// resident set size in pages, read from field 2 of
// /proc/self/statm — the ΔM measurement of the Resource
// Usage requirements. It fails on a host without the
// Linux procfs, which the resource matrices treat as a
// skip.
func fileStatusStatmResidentPages() (int64, error) {
	blob, err := os.ReadFile("/proc/self/statm")
	if err != nil {
		return 0, fmt.Errorf("reading /proc/self/statm: %w", err)
	}
	fields := strings.Fields(string(blob))
	if len(fields) < 2 {
		return 0, fmt.Errorf("short /proc/self/statm: %q", blob)
	}
	// Field 1 is the total program size and field 2 the
	// resident set, both in pages.
	return strconv.ParseInt(fields[1], 10, 64)
}

// fileStatusDeltaM measures the per-query ΔM — the
// change in the resident set around one query — averaged
// over at least fileStatusDeltaMReps repetitions of a
// warm query. warmup is issued once first, so the cold
// read's index-entry allocation — a one-time setup cost
// per distinct file — is paid before the measurement
// rather than counted in the steady-state ΔM.
func fileStatusDeltaM(warmup func() error, query func() error, reps int) (float64, error) {
	if err := warmup(); err != nil {
		return 0, fmt.Errorf("warm-up query: %w", err)
	}
	if reps < fileStatusDeltaMReps {
		reps = fileStatusDeltaMReps
	}
	var total float64
	for i := 0; i < reps; i++ {
		before, err := fileStatusStatmResidentPages()
		if err != nil {
			return 0, err
		}
		if err := query(); err != nil {
			return 0, fmt.Errorf("measured query %d: %w", i, err)
		}
		after, err := fileStatusStatmResidentPages()
		if err != nil {
			return 0, err
		}
		total += float64(after - before)
	}
	return total / float64(reps), nil
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

// fileStatusTransferLoad keeps a transfer of at least
// fileStatusTransferSize bytes through the mount in
// flight until stop is closed: the payload is written
// through the mount in paced chunks, so a transfer is
// concurrently in progress across a measurement window
// rather than completing in a burst.
func fileStatusTransferLoad(vfs *VFS, name string, payload []byte, stop <-chan struct{}) error {
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
		for off := 0; off < len(payload); off += fileStatusTransferChunk {
			if _, err := fh.Write(payload[off:min(off+fileStatusTransferChunk, len(payload))]); err != nil {
				_ = fh.Close()
				return err
			}
			// Pace the transfer so it stays in flight
			// across a measurement window.
			time.Sleep(fileStatusTransferPace)
		}
		if err := fh.Close(); err != nil {
			return err
		}
	}
}

// fileStatusResetLoad initiates a reset of the named
// file's cache item — the same reset primitive the
// cache cleaner uses — at the window's start, and the
// goroutine lives until stop is closed, so the window's
// reads span the reset window. Whether the reset acts on
// the item is the cleaner's own decision (its skip
// rules apply), as it is for the cache cleaner itself.
func fileStatusResetLoad(vfs *VFS, name string, stop <-chan struct{}) {
	go func() {
		item := vfs.cache.Item(name)
		if item == nil {
			return
		}
		_, _, _ = item.Reset()
		<-stop
	}()
}

// fileStatusVmHWM returns the process's peak resident
// set size in pages, read from the VmHWM line of
// /proc/self/status. VmHWM never decreases on its own,
// so per-point HWM isolation resets it between points.
func fileStatusVmHWM() (int64, error) {
	blob, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0, fmt.Errorf("reading /proc/self/status: %w", err)
	}
	for _, line := range strings.Split(string(blob), "\n") {
		if !strings.HasPrefix(line, "VmHWM:") {
			continue
		}
		fields := strings.Fields(line)
		// "VmHWM:" then the peak, in kB.
		if len(fields) < 2 {
			return 0, fmt.Errorf("malformed VmHWM line %q", line)
		}
		kb, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("parsing VmHWM line %q: %w", line, err)
		}
		return kb * 1024 / int64(os.Getpagesize()), nil
	}
	return 0, errors.New("no VmHWM line in /proc/self/status")
}

// fileStatusResetHWM resets the never-decreasing
// high-water mark (per-point HWM isolation), so each
// curve point measures its own peak rather than the
// process's running peak. It is best-effort: where the
// procfs does not permit the reset, the curve degrades
// to measuring the running peak, which still bounds the
// growth per point.
func fileStatusResetHWM() {
	_ = os.WriteFile("/proc/self/clear_refs", []byte("5"), 0o644)
}

// fileStatusProcessCPUTime returns the process's CPU
// time (user plus system) since process start. The
// Resource Usage requirements measure per-query CPU with
// clock_gettime(CLOCK_PROCESS_CPUTIME_ID) deltas around
// the handler, with getrusage(RUSAGE_SELF) as the
// portable fallback; this harness realizes the fallback
// by reading the utime and stime fields (14 and 15) of
// /proc/self/stat — the same counters getrusage reports —
// which keeps the harness compilable on every platform
// the build tests, since neither entry point is in the
// portable standard library. Off Linux no per-process CPU
// clock is reachable at all, so the CPU matrix skips
// itself.
func fileStatusProcessCPUTime() (time.Duration, error) {
	blob, err := os.ReadFile("/proc/self/stat")
	if err != nil {
		return 0, fmt.Errorf("reading /proc/self/stat: %w", err)
	}
	// Field 2 (comm) is parenthesised and may contain
	// spaces, so the field count is anchored on its
	// closing parenthesis: the fields after it start at
	// field 3.
	stat := string(blob)
	i := strings.LastIndexByte(stat, ')')
	if i < 0 || i+1 >= len(stat) {
		return 0, fmt.Errorf("malformed /proc/self/stat: %q", blob)
	}
	fields := strings.Fields(stat[i+1:])
	// utime is field 14 and stime field 15; field 3 is
	// the first field after comm, so they sit at offsets
	// 11 and 12 here.
	if len(fields) < 13 {
		return 0, fmt.Errorf("short /proc/self/stat: %q", blob)
	}
	utime, err := strconv.ParseInt(fields[11], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parsing utime: %w", err)
	}
	stime, err := strconv.ParseInt(fields[12], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parsing stime: %w", err)
	}
	return time.Duration(utime+stime) * time.Second / fileStatusClockTicks, nil
}

// TestFileStatusPerfSingleReadLatency asserts SC-010's
// basic case: a single-path state read completes within
// 100 milliseconds under no load, the reads issued as
// independent concurrently scheduled requests and the
// pass rule applied per run, every run passing.
func TestFileStatusPerfSingleReadLatency(t *testing.T) {
	r, _, call := rcNewRun(t, "vfs/file-status")
	path := filepath.Join(r.Fremote.Root(), "single-read.txt")
	in := rc.Params{
		"fs":    fs.ConfigString(r.Fremote),
		"paths": []string{path},
	}
	maxima, err := fileStatusTimingRuns(func() error {
		_, err := call.Fn(context.Background(), in)
		return err
	}, nil, fileStatusDefaultRuns)
	require.NoError(t, err)
	fileStatusAssertLatency(t, maxima)
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
// and a reset of the file being asked about initiated at
// the start of each measurement window, the same 100
// reads spanning the reset window. A change which starts
// waiting on the file's reset or transfer condition
// fails the ceiling rather than degrading quietly.
func TestFileStatusPerfLatencyUnderLoad(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping the SC-010 under-load matrix in short mode")
	}
	r, vfs, call := fileStatusNewRun(t, "vfs/file-status")
	// The file the reads ask about, written through the
	// mount so the cache holds an entry the reset can
	// act on. The transfer load runs elsewhere in the
	// mount, on its own file.
	target := "/under-load.txt"
	hostTarget := filepath.Join(r.Fremote.Root(), "under-load.txt")
	payload := make([]byte, fileStatusTransferSize)
	for i := range payload {
		payload[i] = byte(i)
	}
	require.NoError(t, vfs.WriteFile(target, payload, 0o644))
	transferPath := "/transfer-load.bin"

	// Transfer load: a concurrent transfer of at least
	// 10 MiB through the mount, in flight for the
	// measurement.
	stopTransfer := make(chan struct{})
	transferErr := make(chan error, 1)
	go func() {
		transferErr <- fileStatusTransferLoad(vfs, transferPath, payload, stopTransfer)
	}()
	t.Cleanup(func() {
		close(stopTransfer)
		select {
		case err := <-transferErr:
			assert.NoError(t, err)
		case <-time.After(fileStatusLoadDrainDelay):
			t.Error("transfer load did not stop")
		}
	})

	in := rc.Params{
		"fs":    fs.ConfigString(r.Fremote),
		"paths": []string{hostTarget},
	}
	read := func() error {
		_, err := call.Fn(context.Background(), in)
		return err
	}
	// The reset of the file being asked about is
	// initiated at the start of each measurement window
	// and lives across the window's reads.
	windowStart := func(stop <-chan struct{}) {
		fileStatusResetLoad(vfs, target, stop)
	}
	maxima, err := fileStatusTimingRuns(read, windowStart, fileStatusDefaultRuns)
	require.NoError(t, err)
	fileStatusAssertLatency(t, maxima)
}

// TestFileStatusPerfConcurrentOracleUnderLoad asserts
// FR-016's second oracle: while a transfer and a reset
// of the file being asked about are in flight, every
// concurrent response is a well-formed per-path result —
// the read never waits on the file's transfer or reset
// condition, and no cross-path dependency leaks into the
// answer.
func TestFileStatusPerfConcurrentOracleUnderLoad(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping the under-load concurrency oracle in short mode")
	}
	r, vfs, call := fileStatusNewRun(t, "vfs/file-status")
	// The file the concurrent requests ask about — the
	// file the reset targets — and the transfer load's
	// own file, elsewhere in the mount.
	target := "/oracle-under-load.txt"
	hostTarget := filepath.Join(r.Fremote.Root(), "oracle-under-load.txt")
	payload := make([]byte, fileStatusTransferSize)
	for i := range payload {
		payload[i] = byte(i)
	}
	require.NoError(t, vfs.WriteFile(target, payload, 0o644))
	transferPath := "/oracle-transfer-load.bin"

	// Transfer load, in flight for the oracle.
	stopTransfer := make(chan struct{})
	transferErr := make(chan error, 1)
	go func() {
		transferErr <- fileStatusTransferLoad(vfs, transferPath, payload, stopTransfer)
	}()
	t.Cleanup(func() {
		close(stopTransfer)
		select {
		case err := <-transferErr:
			assert.NoError(t, err)
		case <-time.After(fileStatusLoadDrainDelay):
			t.Error("transfer load did not stop")
		}
	})

	// Reset of the file being asked about, in flight for
	// the oracle: initiated while the concurrent requests
	// run and held open across them.
	resetStop := make(chan struct{})
	fileStatusResetLoad(vfs, target, resetStop)

	// N parallel requests naming the file under reset.
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
	close(resetStop)
	for i, err := range errs {
		require.NoError(t, err, "concurrent request %d", i)
	}
	for _, out := range results {
		fileStatusAssertPerPathResults(t, out, []string{hostTarget})
	}
}

// TestFileStatusPerfDeltaM asserts the Resource Usage
// ΔM bound: per-query ΔM — the change in /proc/self/statm
// resident pages (field 2) around a warm query, measured
// after a warm-up query per distinct file and averaged
// over at least 100 repetitions — is proportional to the
// response size, not to the mount's file count. The
// mount is populated with files first, so a ΔM that
// scaled with the mount's file count would move the
// resident set by far more than the per-result-object
// bound.
func TestFileStatusPerfDeltaM(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping the ΔM matrix in short mode")
	}
	if runtime.GOOS != "linux" {
		t.Skip("ΔM is measured from /proc/self/statm")
	}
	r, vfs, call := rcNewRun(t, "vfs/file-status")
	// Populate the mount with files, so the measurement
	// is taken against a mount whose file count the
	// result does not scale with.
	for i := 0; i < fileStatusDeltaMPopulation; i++ {
		name := fmt.Sprintf("deltam-%04d.txt", i)
		require.NoError(t, vfs.WriteFile("/"+name, []byte(name), 0o644))
	}
	require.NoError(t, vfs.WriteFile("/deltam-query.txt", []byte("deltam"), 0o644))
	in := rc.Params{
		"fs":    fs.ConfigString(r.Fremote),
		"paths": []string{filepath.Join(r.Fremote.Root(), "deltam-query.txt")},
	}
	query := func() error {
		_, err := call.Fn(context.Background(), in)
		return err
	}
	// The warm-up query pays the cold read's index-entry
	// allocation for the distinct file before the
	// measurement, and the average is taken over at
	// least 100 repetitions of the warm query.
	avg, err := fileStatusDeltaM(query, query, fileStatusDeltaMReps)
	require.NoError(t, err)
	assert.LessOrEqual(t, avg, float64(fileStatusDeltaMPages),
		"per-query ΔM of %v pages exceeds the %d-page per-result-object bound", avg, fileStatusDeltaMPages)
}

// TestFileStatusPerfVmHWM asserts the Resource Usage RSS
// bound: peak RSS (VmHWM) is bounded by C₀ + k·D, where
// D is the number of distinct files queried within the
// cache-cleaner retention window — the
// baseline-subtracted ΔVmHWM(D) = VmHWM(D) − VmHWM(0)
// grows linearly with D, C₀ the D = 0 baseline and k the
// slope the curve measures. The high-water mark is reset
// between points, so each point measures its own peak.
func TestFileStatusPerfVmHWM(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping the VmHWM matrix in short mode")
	}
	if runtime.GOOS != "linux" {
		t.Skip("VmHWM is read from /proc/self/status")
	}
	r, vfs, call := rcNewRun(t, "vfs/file-status")
	// The distinct files the curve varies D over,
	// written through the mount so the cache holds an
	// entry for each.
	distinct := make([]string, fileStatusVmHWMCurve)
	for i := range distinct {
		name := fmt.Sprintf("vmhwm-%04d.txt", i)
		distinct[i] = filepath.Join(r.Fremote.Root(), name)
		require.NoError(t, vfs.WriteFile("/"+name, []byte(name), 0o644))
	}
	// The D = 0 baseline: the peak with no distinct
	// file queried.
	fileStatusResetHWM()
	baseline, err := fileStatusVmHWM()
	require.NoError(t, err)
	prevDelta := int64(0)
	for d := 1; d <= fileStatusVmHWMCurve; d++ {
		// Per-point HWM isolation: each point measures
		// its own peak.
		fileStatusResetHWM()
		// Query d distinct files — the d-th cold, its
		// index-entry allocation the one-time setup cost
		// the C₀ + k·D bound allows.
		paths := make([]string, d)
		copy(paths, distinct[:d])
		in := rc.Params{
			"fs":    fs.ConfigString(r.Fremote),
			"paths": paths,
		}
		_, err := call.Fn(context.Background(), in)
		require.NoError(t, err, "querying %d distinct files", d)
		hwm, err := fileStatusVmHWM()
		require.NoError(t, err)
		delta := hwm - baseline
		growth := delta - prevDelta
		prevDelta = delta
		assert.LessOrEqual(t, growth, int64(fileStatusVmHWMSlopePages),
			"D=%d: baseline-subtracted ΔVmHWM grew %d pages, more than the %d-page per-distinct-file slope",
			d, growth, fileStatusVmHWMSlopePages)
	}
}

// TestFileStatusPerfCPU asserts the Resource Usage CPU
// bound: per-query CPU time (user+sys) is bounded by a
// constant multiple of the response size — the work per
// result object is the fixed field set the contract
// fixes, with no scanning of unrelated state — verified
// by measuring the CPU delta around queries of growing
// response size and asserting the delta grows linearly
// with the number of result objects.
func TestFileStatusPerfCPU(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping the CPU matrix in short mode")
	}
	if runtime.GOOS != "linux" {
		t.Skip("per-query CPU time is measured from /proc/self/stat")
	}
	r, _, call := rcNewRun(t, "vfs/file-status")
	// Response sizes, in result objects per response: the
	// CPU per query is measured at each size over enough
	// repetitions that the delta crosses the counter's
	// clock-tick granularity.
	sizes := []int{1, fileStatusCPUStep, 4 * fileStatusCPUStep}
	perQuery := make([]time.Duration, len(sizes))
	for i, size := range sizes {
		paths := make([]string, size)
		for j := range paths {
			paths[j] = filepath.Join(r.Fremote.Root(), fmt.Sprintf("cpu-%d-%04d.txt", i, j))
		}
		in := rc.Params{
			"fs":    fs.ConfigString(r.Fremote),
			"paths": paths,
		}
		// Warm-up: pay the cold read's index-entry
		// allocation for every distinct path before
		// measuring, and discard the first repetitions so
		// the measurement starts from a steady state.
		for rep := 0; rep < fileStatusCPUWarmup; rep++ {
			_, err := call.Fn(context.Background(), in)
			require.NoError(t, err, "warm-up at response size %d, repetition %d", size, rep)
		}
		start, err := fileStatusProcessCPUTime()
		require.NoError(t, err)
		for rep := 0; rep < fileStatusCPUReps; rep++ {
			_, err := call.Fn(context.Background(), in)
			require.NoError(t, err, "measured repetition %d at response size %d", rep, size)
		}
		end, err := fileStatusProcessCPUTime()
		require.NoError(t, err)
		perQuery[i] = (end - start) / time.Duration(fileStatusCPUReps)
	}
	// The CPU per query grows linearly with the response
	// size: the per-query CPU at each size is bounded by
	// the size's multiple of the single-path per-query
	// CPU, plus slack for the fixed per-query cost.
	for i := 1; i < len(perQuery); i++ {
		bound := time.Duration(int64(sizes[i])*int64(perQuery[0])) + fileStatusCPUSlack
		assert.LessOrEqual(t, perQuery[i], bound,
			"per-query CPU at response size %d (%v) exceeds the linear bound %v", sizes[i], perQuery[i], bound)
	}
}

// fileStatusLoadMatrix is the declared load matrix for
// the WSS/MRC curve: mount sizes × query rates.
var fileStatusLoadMatrix = []struct {
	mountSize int
	queryRate int
}{
	{mountSize: 10, queryRate: 10},
	{mountSize: 50, queryRate: 50},
	{mountSize: fileStatusWSSQuerySet, queryRate: 100},
}

// TestFileStatusPerfWSSMRC traces the WSS/MRC curve —
// the (population, RSS, miss-ratio) curve, not a scalar
// — under the declared load matrix of mount sizes ×
// query rates. The population is varied via
// pre-populated cache entries and a fixed,
// pre-generated warm query set, with a fresh population
// per point; peak RSS and per-query ΔM are asserted
// flat in the mount's file count and the query count.
func TestFileStatusPerfWSSMRC(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping the WSS/MRC matrix in short mode")
	}
	if runtime.GOOS != "linux" {
		t.Skip("RSS is measured from the procfs")
	}
	// The fixed universe the warm query set is drawn
	// from, larger than any population in the matrix.
	universe := make([]string, fileStatusWSSUniverse)
	for i := range universe {
		universe[i] = fmt.Sprintf("wss-%04d.txt", i)
	}
	var firstRSS int64 = -1
	for _, point := range fileStatusLoadMatrix {
		// A fresh population per point: cold-miss behavior
		// is measured per point, not carried across
		// points, so each point runs as its own subtest
		// with its own mount.
		t.Run(fmt.Sprintf("population=%d/rate=%d", point.mountSize, point.queryRate), func(t *testing.T) {
			r, vfs, call := fileStatusNewRun(t, "vfs/file-status")
			// Pre-populate the point's cache entries.
			for i := 0; i < point.mountSize; i++ {
				name := universe[i]
				require.NoError(t, vfs.WriteFile("/"+name, []byte(name), 0o644))
			}
			// The warm query set, fixed across points, so
			// the miss ratio falls as the population
			// grows: a query naming a path the population
			// does not hold is a cold miss.
			querySet := universe[:fileStatusWSSQuerySet]
			misses := 0
			for q := 0; q < fileStatusWSSQueries; q++ {
				i := q % len(querySet)
				if i >= point.mountSize {
					misses++
				}
				in := rc.Params{
					"fs":    fs.ConfigString(r.Fremote),
					"paths": []string{filepath.Join(r.Fremote.Root(), querySet[i])},
				}
				_, err := call.Fn(context.Background(), in)
				require.NoError(t, err, "query %d", q)
				// Hold the point's declared query rate.
				time.Sleep(time.Second / time.Duration(point.queryRate))
			}
			rss, err := fileStatusStatmResidentPages()
			require.NoError(t, err)
			if firstRSS < 0 {
				firstRSS = rss
			}
			missRatio := float64(misses) / float64(fileStatusWSSQueries)
			t.Logf("WSS/MRC point: population=%d queryRate=%d rssPages=%d missRatio=%.4f",
				point.mountSize, point.queryRate, rss, missRatio)
			// Peak RSS is flat in the mount's file count
			// and the query count: the resource the read
			// uses does not scale with either.
			assert.LessOrEqual(t, rss, firstRSS+int64(fileStatusRSSFlatPages),
				"population %d: RSS %d pages exceeds the first point's %d pages by more than the %d-page flatness tolerance",
				point.mountSize, rss, firstRSS, fileStatusRSSFlatPages)
		})
	}
}
