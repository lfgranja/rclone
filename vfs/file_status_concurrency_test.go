package vfs

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/rc"
	"github.com/rclone/rclone/fstest"
	"github.com/rclone/rclone/lib/israce"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const fileStatusConcurrencyChildEnv = "RCLONE_FILE_STATUS_CONCURRENCY_CHILD"

// fileStatusConcurrencyTimeout scales the fixed timeouts for the
// concurrent list/resolve/unmount workload when running under the race
// detector. The workload progresses under -race but takes
// roughly 8x longer: measured ~1.6s non-race vs 12-14s race, so the 15s
// budget that is comfortable without race sat right at the race runtime and
// intermittently tripped the outer context deadline. Scaling by the measured
// slowdown keeps the same safety margin; non-race behavior is unchanged.
func fileStatusConcurrencyTimeout(base time.Duration) time.Duration {
	if israce.Enabled {
		return base * 8
	}
	return base
}

func TestFileStatusConcurrentListResolveUnmount(t *testing.T) {
	if os.Getenv(fileStatusConcurrencyChildEnv) == "1" {
		testFileStatusConcurrentListResolveUnmount(t)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), fileStatusConcurrencyTimeout(15*time.Second))
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestFileStatusConcurrentListResolveUnmount$")
	cmd.Env = append(os.Environ(), fileStatusConcurrencyChildEnv+"=1")
	output, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("concurrent vfs/list, path resolution, and unmount timed out: %s", output)
	}
	require.NoError(t, err, "concurrent child output:\n%s", output)
}

func testFileStatusConcurrentListResolveUnmount(t *testing.T) {
	previousProcs := runtime.GOMAXPROCS(2)
	defer runtime.GOMAXPROCS(previousProcs)

	r := fstest.NewRun(t)
	vfs, shutdown := newFileStatusRegistryVFS(t, r.Fremote, nil)
	mountPoint := t.TempDir()
	RegisterMountPointForVFS(vfs, mountPoint)
	t.Cleanup(func() { UnregisterMountPointForVFS(vfs) })

	// Keep path resolution inside its registry scan long enough for vfs/list
	// and teardown to contend for the opposite lock order.
	const fillerCount = 8192
	fillerRoot := t.TempDir()
	fillerNames := make([]string, fillerCount)
	for i := range fillerNames {
		name := fmt.Sprintf("file-status-lock-order-%d", i)
		fillerNames[i] = name
		RegisterMountPoint(name, filepath.Join(fillerRoot, fmt.Sprintf("unused-%d", i)))
	}
	t.Cleanup(func() {
		for _, name := range fillerNames {
			UnregisterMountPoint(name)
		}
	})

	listCall := rc.Calls.Get("vfs/list")
	statusCall := rc.Calls.Get("vfs/file-status")
	require.NotNil(t, listCall)
	require.NotNil(t, statusCall)

	const readersPerSurface = 4
	const iterations = 100
	readerCount := readersPerSurface * 2
	ready := make(chan struct{}, readerCount+1)
	firstReadDone := make(chan struct{}, readerCount)
	start := make(chan struct{})
	results := make(chan error, readerCount+1)
	var workers sync.WaitGroup

	startWorker := func(run func() error) {
		workers.Add(1)
		go func() {
			defer workers.Done()
			ready <- struct{}{}
			<-start
			results <- run()
		}()
	}

	for range readersPerSurface {
		startWorker(func() error {
			for i := range iterations {
				out, err := listCall.Fn(context.Background(), nil)
				if i == 0 {
					firstReadDone <- struct{}{}
				}
				if err != nil {
					return err
				}
				if _, ok := out["vfses"].([]string); !ok {
					return fmt.Errorf("vfs/list returned no vfses array")
				}
			}
			return nil
		})
		startWorker(func() error {
			for i := range iterations {
				out, err := statusCall.Fn(context.Background(), rc.Params{"paths": []string{mountPoint}})
				if i == 0 {
					firstReadDone <- struct{}{}
				}
				if err != nil {
					return err
				}
				if _, ok := out["statuses"]; !ok {
					return fmt.Errorf("path status returned no statuses array")
				}
			}
			return nil
		})
	}

	workers.Add(1)
	go func() {
		defer workers.Done()
		ready <- struct{}{}
		<-start
		for range readerCount {
			<-firstReadDone
		}
		UnregisterMountPointForVFS(vfs)
		shutdown()
		results <- nil
	}()

	for range readerCount + 1 {
		<-ready
	}
	close(start)
	for range readerCount + 1 {
		select {
		case err := <-results:
			assert.NoError(t, err)
		case <-time.After(fileStatusConcurrencyTimeout(15 * time.Second)):
			t.Fatal("concurrent vfs/list, path status, and unmount did not finish")
		}
	}
	workers.Wait()
}

type fileStatusBlockingListFs struct {
	fs.Fs
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (f *fileStatusBlockingListFs) List(ctx context.Context, dir string) (fs.DirEntries, error) {
	f.once.Do(func() { close(f.started) })
	<-f.release
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return f.Fs.List(ctx, dir)
}

func TestFileStatusPathRequestHoldsVFSAcrossUnmount(t *testing.T) {
	r := fstest.NewRun(t)
	r.WriteObject(context.Background(), "file.txt", "content", time.Now())
	blockedFs := &fileStatusBlockingListFs{
		Fs:      r.Fremote,
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	vfs, shutdown := newFileStatusRegistryVFS(t, blockedFs, nil)
	mountPoint := t.TempDir()
	RegisterMountPointForVFS(vfs, mountPoint)
	t.Cleanup(func() { UnregisterMountPointForVFS(vfs) })
	statusCall := rc.Calls.Get("vfs/file-status")
	require.NotNil(t, statusCall)

	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(blockedFs.release) }) }
	done := make(chan struct {
		out rc.Params
		err error
	}, 1)
	requestFinished := false
	t.Cleanup(func() {
		release()
		if !requestFinished {
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Error("in-flight path status did not return during cleanup")
			}
		}
	})
	go func() {
		out, err := statusCall.Fn(context.Background(), rc.Params{"paths": []string{filepath.Join(mountPoint, "file.txt")}})
		done <- struct {
			out rc.Params
			err error
		}{out: out, err: err}
	}()

	select {
	case <-blockedFs.started:
	case <-time.After(5 * time.Second):
		t.Fatal("path status did not reach the controlled directory listing")
	}

	// The resolver has selected the VFS and is blocked inside its listing.
	// An unmount must not tear that instance down before the answer is assembled.
	UnregisterMountPointForVFS(vfs)
	shutdown()
	release()

	var response struct {
		out rc.Params
		err error
	}
	select {
	case response = <-done:
		requestFinished = true
	case <-time.After(5 * time.Second):
		t.Fatal("in-flight path status did not finish after releasing the listing")
	}
	require.NoError(t, response.err)
	statuses := decodeStatuses(t, response.out)
	require.Len(t, statuses, 1)
	assert.NotContains(t, statuses[0], "error", "the selected VFS remains usable until the request completes")
}
