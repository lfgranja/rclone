package vfs

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/fserrors"
	"github.com/rclone/rclone/fs/rc"
	"github.com/rclone/rclone/fstest"
	"github.com/rclone/rclone/vfs/vfscommon"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	fileStatusFailureWriteBackDelay = fs.Duration(24 * time.Hour)
	fileStatusFailureWaitTimeout    = 5 * time.Second
)

type fileStatusFailureControl struct {
	mu                sync.Mutex
	downloadPlans     map[string][]fileStatusDownloadFault
	downloadFallbacks map[string]fileStatusDownloadFault
	uploadSteps       []fileStatusUploadStep
	uploadAttempts    int
	downloadErrors    chan string
	uploadStarted     chan fileStatusUploadAttempt
}

type fileStatusDownloadFault struct {
	after int64
	err   error
}

type fileStatusUploadStep struct {
	err     error
	release <-chan struct{}
}

type fileStatusUploadAttempt struct {
	remote string
	number int
}

func newFileStatusFailureControl() *fileStatusFailureControl {
	return &fileStatusFailureControl{
		downloadPlans:     make(map[string][]fileStatusDownloadFault),
		downloadFallbacks: make(map[string]fileStatusDownloadFault),
		downloadErrors:    make(chan string, 8),
		uploadStarted:     make(chan fileStatusUploadAttempt, 16),
	}
}

func (control *fileStatusFailureControl) failDownloadPlan(remote string, first, fallback fileStatusDownloadFault) {
	control.mu.Lock()
	control.downloadPlans[remote] = []fileStatusDownloadFault{first}
	control.downloadFallbacks[remote] = fallback
	control.mu.Unlock()
}

func (control *fileStatusFailureControl) clearDownloadPlan(remote string) {
	control.mu.Lock()
	delete(control.downloadPlans, remote)
	delete(control.downloadFallbacks, remote)
	control.mu.Unlock()
}

func (control *fileStatusFailureControl) takeDownloadFault(remote string) (fileStatusDownloadFault, bool) {
	control.mu.Lock()
	defer control.mu.Unlock()
	if plan := control.downloadPlans[remote]; len(plan) != 0 {
		fault := plan[0]
		if len(plan) == 1 {
			delete(control.downloadPlans, remote)
		} else {
			control.downloadPlans[remote] = plan[1:]
		}
		return fault, true
	}
	fault, ok := control.downloadFallbacks[remote]
	return fault, ok
}

func (control *fileStatusFailureControl) setUploadSteps(steps ...fileStatusUploadStep) {
	control.mu.Lock()
	control.uploadSteps = append([]fileStatusUploadStep(nil), steps...)
	control.mu.Unlock()
}

func (control *fileStatusFailureControl) upload(ctx context.Context, remote string) error {
	control.mu.Lock()
	control.uploadAttempts++
	attempt := fileStatusUploadAttempt{remote: remote, number: control.uploadAttempts}
	step := fileStatusUploadStep{}
	if len(control.uploadSteps) != 0 {
		step = control.uploadSteps[0]
		control.uploadSteps = control.uploadSteps[1:]
	}
	control.mu.Unlock()

	control.uploadStarted <- attempt
	if step.release != nil {
		select {
		case <-step.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return step.err
}

func (control *fileStatusFailureControl) uploadAttemptCount() int {
	control.mu.Lock()
	defer control.mu.Unlock()
	return control.uploadAttempts
}

type fileStatusFailureFs struct {
	fs.Fs
	control *fileStatusFailureControl
}

func (f *fileStatusFailureFs) Features() *fs.Features {
	features := *f.Fs.Features()
	// Force writeback through the wrapped Put/Update methods.
	features.Copy = nil
	features.Move = nil
	features.PutStream = nil
	return &features
}

func (f *fileStatusFailureFs) List(ctx context.Context, dir string) (fs.DirEntries, error) {
	entries, err := f.Fs.List(ctx, dir)
	if err != nil {
		return nil, err
	}
	for i, entry := range entries {
		if object, ok := entry.(fs.Object); ok {
			entries[i] = &fileStatusFailureObject{Object: object, control: f.control}
		}
	}
	return entries, nil
}

func (f *fileStatusFailureFs) NewObject(ctx context.Context, remote string) (fs.Object, error) {
	object, err := f.Fs.NewObject(ctx, remote)
	if object == nil {
		return nil, err
	}
	return &fileStatusFailureObject{Object: object, control: f.control}, err
}

func (f *fileStatusFailureFs) Put(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) (fs.Object, error) {
	if err := f.control.upload(ctx, src.Remote()); err != nil {
		return nil, err
	}
	object, err := f.Fs.Put(ctx, in, src, options...)
	if object == nil {
		return nil, err
	}
	return &fileStatusFailureObject{Object: object, control: f.control}, err
}

type fileStatusFailureObject struct {
	fs.Object
	control *fileStatusFailureControl
}

func (o *fileStatusFailureObject) Open(ctx context.Context, options ...fs.OpenOption) (io.ReadCloser, error) {
	reader, err := o.Object.Open(ctx, options...)
	if err != nil {
		return nil, err
	}
	fault, ok := o.control.takeDownloadFault(o.Remote())
	if !ok {
		return reader, nil
	}
	return &fileStatusFailureReader{
		ReadCloser: reader,
		remaining:  fault.after,
		err:        fserrors.NoLowLevelRetryError(fault.err),
		remote:     o.Remote(),
		failed:     o.control.downloadErrors,
	}, nil
}

func (o *fileStatusFailureObject) Update(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) error {
	if err := o.control.upload(ctx, o.Remote()); err != nil {
		return err
	}
	return o.Object.Update(ctx, in, src, options...)
}

type fileStatusFailureReader struct {
	io.ReadCloser
	remaining int64
	err       error
	remote    string
	failed    chan<- string
}

func (r *fileStatusFailureReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if r.remaining == 0 {
		select {
		case r.failed <- r.remote:
		default:
		}
		return 0, r.err
	}
	if int64(len(p)) > r.remaining {
		p = p[:int(r.remaining)]
	}
	n, err := r.ReadCloser.Read(p)
	r.remaining -= int64(n)
	return n, err
}

type fileStatusFailureFixture struct {
	control *fileStatusFailureControl
	remote  *fileStatusFailureFs
	vfs     *VFS
	call    *rc.Call
}

func newFileStatusFailureFixture(t *testing.T, opt *vfscommon.Options) (*fstest.Run, *fileStatusFailureFixture) {
	t.Helper()
	if *fstest.RemoteName != "" {
		t.Skip("Skipping test on non local remote")
	}
	r := fstest.NewRun(t)
	control := newFileStatusFailureControl()
	fixture := &fileStatusFailureFixture{
		control: control,
		remote:  &fileStatusFailureFs{Fs: r.Fremote, control: control},
	}
	t.Cleanup(func() {
		fixture.close(t)
	})
	fixture.start(t, opt)
	return r, fixture
}

func (fixture *fileStatusFailureFixture) start(t *testing.T, opt *vfscommon.Options) {
	t.Helper()
	localOpt := *opt
	localOpt.DirCacheTime = fs.Duration(time.Hour)
	fixture.vfs = New(context.Background(), fixture.remote, &localOpt)
	mountPoint := fixture.remote.Root()
	RegisterMountPointForVFS(fixture.vfs, mountPoint)
	require.Equal(t, mountPoint, GetMountPoint(fs.ConfigString(fixture.remote)))
	fixture.call = rc.Calls.Get("vfs/file-status")
	require.NotNil(t, fixture.call)
	require.Equal(t, fs.ConfigString(fixture.remote.Fs), fs.ConfigString(fixture.remote))
}

func (fixture *fileStatusFailureFixture) restart(t *testing.T, opt *vfscommon.Options) {
	t.Helper()
	UnregisterMountPointForVFS(fixture.vfs)
	fixture.vfs.Shutdown()
	fixture.vfs = nil
	fixture.start(t, opt)
}

func (fixture *fileStatusFailureFixture) close(t *testing.T) {
	t.Helper()
	if fixture.vfs == nil {
		return
	}
	vfs := fixture.vfs
	fixture.vfs = nil
	UnregisterMountPointForVFS(vfs)
	vfs.Shutdown()
	require.NoError(t, vfs.CleanUp())
}

func fileStatusFailureResult(t *testing.T, r *fstest.Run, fixture *fileStatusFailureFixture, path string) map[string]any {
	t.Helper()
	doc := fileStatusStateCall(t, fixture.call, fs.ConfigString(r.Fremote), []string{path})
	return fileStatusStateResult(t, fileStatusStateStatuses(t, doc), path)
}

func requireFileStatusFailureByte(t *testing.T, status map[string]any, field string, want int64) {
	t.Helper()
	bytes, ok := status["bytes"].(map[string]any)
	require.True(t, ok, "status has no byte object: %v", status)
	got, ok := bytes[field].(float64)
	require.True(t, ok, "byte field %q is not a JSON number: %v", field, bytes[field])
	assert.Equal(t, float64(want), got, "byte field %q", field)
}

func waitForFileStatusUploadState(t *testing.T, r *fstest.Run, fixture *fileStatusFailureFixture, path, want string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(fileStatusFailureWaitTimeout)
	for {
		status := fileStatusFailureResult(t, r, fixture, path)
		if status["upload"] == want {
			return status
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for upload state %q; last status: %v", want, status["upload"])
			return status
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func waitForFileStatusUploadSettled(t *testing.T, r *fstest.Run, fixture *fileStatusFailureFixture, path string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(fileStatusFailureWaitTimeout)
	for {
		status := fileStatusFailureResult(t, r, fixture, path)
		if status["upload"] != "uploading" {
			return status
		}
		if time.Now().After(deadline) {
			t.Fatalf("upload did not leave the in-flight state; last status: %v", status["upload"])
			return status
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func receiveFileStatusUploadAttempt(t *testing.T, control *fileStatusFailureControl) fileStatusUploadAttempt {
	t.Helper()
	select {
	case attempt := <-control.uploadStarted:
		return attempt
	case <-time.After(fileStatusFailureWaitTimeout):
		t.Fatal("timed out waiting for an upload attempt")
		return fileStatusUploadAttempt{}
	}
}

func fileStatusFailureResultDuringUpload(t *testing.T, r *fstest.Run, fixture *fileStatusFailureFixture, path string) map[string]any {
	t.Helper()
	result := make(chan struct {
		out rc.Params
		err error
	}, 1)
	go func() {
		out, err := fixture.call.Fn(context.Background(), rc.Params{
			"fs":    fs.ConfigString(r.Fremote),
			"paths": []string{path},
		})
		result <- struct {
			out rc.Params
			err error
		}{out: out, err: err}
	}()
	select {
	case response := <-result:
		require.NoError(t, response.err)
		raw, err := json.Marshal(response.out)
		require.NoError(t, err)
		var doc map[string]any
		require.NoError(t, json.Unmarshal(raw, &doc))
		return fileStatusStateResult(t, fileStatusStateStatuses(t, doc), path)
	case <-time.After(2 * time.Second):
		t.Fatal("file-status endpoint blocked on an in-flight upload")
		return nil
	}
}

func TestFileStatusPartialDownloadFailureClearsAfterSuccessfulRetry(t *testing.T) {
	const (
		name    = "partial-download.txt"
		content = "0123456789abcdefghijklmnopqrstuvwxyz"
		partial = 7
	)
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

	failed := fileStatusFailureResult(t, r, fixture, path)
	assert.Equal(t, "download_failed", failed["local"])
	requireFileStatusFailureByte(t, failed, "unbroken_run", partial)
	requireFileStatusFailureByte(t, failed, "total_held", partial)
	requireFileStatusFailureByte(t, failed, "total_size", int64(len(content)))

	fixture.control.clearDownloadPlan(name)
	got, err := fixture.vfs.ReadFile(name)
	require.NoError(t, err)
	assert.Equal(t, content, string(got))
	recovered := fileStatusFailureResult(t, r, fixture, path)
	assert.Equal(t, "fully_held", recovered["local"])
}

func TestFileStatusUploadFailureRetriesAndClearsAfterSuccess(t *testing.T) {
	const name, content = "upload-retry.txt", "dirty upload data"
	releaseRetry := make(chan struct{})
	var releaseOnce sync.Once
	unblockRetry := func() { releaseOnce.Do(func() { close(releaseRetry) }) }
	defer unblockRetry()

	r, fixture := newFileStatusFailureFixture(t, fileStatusStateOpt(vfscommon.CacheModeFull, fileStatusFailureWriteBackDelay))
	fixture.control.setUploadSteps(
		fileStatusUploadStep{err: errors.New("injected upload failure")},
		fileStatusUploadStep{release: releaseRetry},
	)
	writeFileStatusStateThrough(t, fixture.vfs, name, content)
	path := fileStatusStatePath(r, name)

	drainFileStatusStateWriteback(t, fixture.vfs)
	first := receiveFileStatusUploadAttempt(t, fixture.control)
	assert.Equal(t, 1, first.number)
	assert.Contains(t, first.remote, name)
	failed := waitForFileStatusUploadSettled(t, r, fixture, path)
	assert.Equal(t, "upload_failed", failed["upload"])

	drainFileStatusStateWriteback(t, fixture.vfs)
	second := receiveFileStatusUploadAttempt(t, fixture.control)
	assert.Equal(t, 2, second.number)
	assert.Contains(t, second.remote, name)
	retrying := fileStatusFailureResult(t, r, fixture, path)
	assert.Equal(t, "upload_failed", retrying["upload"])
	unblockRetry()

	uploaded := waitForFileStatusUploadState(t, r, fixture, path, "uploaded")
	assert.Equal(t, "fully_held", uploaded["local"])
	assert.Equal(t, 2, fixture.control.uploadAttemptCount())
}

func TestFileStatusRestartRequeuesDirtyContent(t *testing.T) {
	const name, content = "restart-dirty.txt", "local content awaiting upload"
	opt := fileStatusStateOpt(vfscommon.CacheModeFull, fileStatusFailureWriteBackDelay)
	r, fixture := newFileStatusFailureFixture(t, opt)
	writeFileStatusStateThrough(t, fixture.vfs, name, content)
	path := fileStatusStatePath(r, name)

	beforeContent, err := fixture.vfs.ReadFile(name)
	require.NoError(t, err)
	assert.Equal(t, content, string(beforeContent))
	before := fileStatusFailureResult(t, r, fixture, path)
	assert.Equal(t, "fully_held", before["local"])
	assert.Equal(t, "awaiting_upload", before["upload"])
	assert.Zero(t, fixture.control.uploadAttemptCount())

	fixture.restart(t, opt)
	listFileStatusStateRoot(t, fixture.vfs)
	afterContent, err := fixture.vfs.ReadFile(name)
	require.NoError(t, err)
	assert.Equal(t, content, string(afterContent))
	after := fileStatusFailureResult(t, r, fixture, path)
	assert.Equal(t, "fully_held", after["local"])
	assert.Equal(t, "awaiting_upload", after["upload"])
	assert.Zero(t, fixture.control.uploadAttemptCount())
}

func TestFileStatusUploadFailureBecomesAwaitingAfterRestart(t *testing.T) {
	const name, content = "restart-failed-upload.txt", "dirty content after failed upload"
	opt := fileStatusStateOpt(vfscommon.CacheModeFull, fileStatusFailureWriteBackDelay)
	r, fixture := newFileStatusFailureFixture(t, opt)
	fixture.control.setUploadSteps(fileStatusUploadStep{err: errors.New("injected upload failure")})
	writeFileStatusStateThrough(t, fixture.vfs, name, content)
	path := fileStatusStatePath(r, name)

	drainFileStatusStateWriteback(t, fixture.vfs)
	attempt := receiveFileStatusUploadAttempt(t, fixture.control)
	assert.Equal(t, 1, attempt.number)
	assert.Contains(t, attempt.remote, name)
	failed := waitForFileStatusUploadSettled(t, r, fixture, path)
	assert.Equal(t, "upload_failed", failed["upload"])

	fixture.restart(t, opt)
	listFileStatusStateRoot(t, fixture.vfs)
	awaiting := fileStatusFailureResult(t, r, fixture, path)
	assert.Equal(t, "fully_held", awaiting["local"])
	assert.Equal(t, "awaiting_upload", awaiting["upload"])
}

func TestFileStatusUploadInFlight(t *testing.T) {
	const name, content = "upload-in-flight.txt", "writeback must not block status"
	releaseUpload := make(chan struct{})
	var releaseOnce sync.Once
	unblockUpload := func() { releaseOnce.Do(func() { close(releaseUpload) }) }
	defer unblockUpload()

	r, fixture := newFileStatusFailureFixture(t, fileStatusStateOpt(vfscommon.CacheModeFull, fileStatusFailureWriteBackDelay))
	fixture.control.setUploadSteps(fileStatusUploadStep{release: releaseUpload})
	writeFileStatusStateThrough(t, fixture.vfs, name, content)
	path := fileStatusStatePath(r, name)
	drainFileStatusStateWriteback(t, fixture.vfs)
	attempt := receiveFileStatusUploadAttempt(t, fixture.control)
	assert.Contains(t, attempt.remote, name)

	status := fileStatusFailureResultDuringUpload(t, r, fixture, path)
	assert.Equal(t, "fully_held", status["local"])
	assert.Equal(t, "uploading", status["upload"])
	requireFileStatusFailureByte(t, status, "unbroken_run", int64(len(content)))
	requireFileStatusFailureByte(t, status, "total_held", int64(len(content)))
	unblockUpload()

	waitForFileStatusUploadState(t, r, fixture, path, "uploaded")
}

func TestFileStatusMultipleOutstandingUploads(t *testing.T) {
	const (
		firstName     = "outstanding-one.txt"
		firstContent  = "first dirty file"
		secondName    = "outstanding-two.txt"
		secondContent = "second dirty file"
	)
	r, fixture := newFileStatusFailureFixture(t, fileStatusStateOpt(vfscommon.CacheModeFull, fileStatusFailureWriteBackDelay))
	writeFileStatusStateThrough(t, fixture.vfs, firstName, firstContent)
	writeFileStatusStateThrough(t, fixture.vfs, secondName, secondContent)

	firstPath := fileStatusStatePath(r, firstName)
	secondPath := fileStatusStatePath(r, secondName)
	doc := fileStatusStateCall(t, fixture.call, fs.ConfigString(r.Fremote), []string{firstPath, secondPath})
	statuses := fileStatusStateStatuses(t, doc)
	require.Len(t, statuses, 2)
	for _, test := range []struct {
		path    string
		content string
	}{
		{firstPath, firstContent},
		{secondPath, secondContent},
	} {
		status := fileStatusStateResult(t, statuses, test.path)
		assert.Equal(t, "fully_held", status["local"], "path=%s", test.path)
		assert.Equal(t, "awaiting_upload", status["upload"], "path=%s", test.path)
		requireFileStatusFailureByte(t, status, "total_size", int64(len(test.content)))
	}
	assert.Zero(t, fixture.control.uploadAttemptCount())
}
