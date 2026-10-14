package vfs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/cache"
	"github.com/rclone/rclone/fs/rc"
	"github.com/rclone/rclone/vfs/vfscommon"
	"github.com/rclone/rclone/vfs/vfscache/writeback"
)

const getVFSHelp = ` 
This command takes an "fs" parameter. If this parameter is not
supplied and if there is only one VFS in use then that VFS will be
used. If there is more than one VFS in use then the "fs" parameter
must be supplied.`

// mountPointsMu protects the mountPoints map.
var mountPointsMu sync.Mutex

// mountPoints maps the indexed VFS name (as returned by vfs/list)
// to its mount point path. A VFS created without a mount point
// (in-process, remote-control, or serve usage) is not registered
// here, so it will not appear in the map.
var mountPoints = map[string]string{}

// RegisterMountPoint registers a mount point for a VFS. The name
// must be one of the names returned by activeCacheEntries (the
// indexed form, e.g. "mydrive" or "mydrive[1]").
func RegisterMountPoint(name, mountPoint string) {
	mountPointsMu.Lock()
	defer mountPointsMu.Unlock()
	mountPoints[name] = mountPoint
}

// UnregisterMountPoint removes a mount point registration for a VFS.
func UnregisterMountPoint(name string) {
	mountPointsMu.Lock()
	defer mountPointsMu.Unlock()
	delete(mountPoints, name)
}

// GetMountPoint returns the mount point for a VFS by its indexed name,
// or empty string if the VFS has no registered mount point.
func GetMountPoint(name string) string {
	mountPointsMu.Lock()
	defer mountPointsMu.Unlock()
	return mountPoints[name]
}

// GetAllMountPoints returns a copy of the mountPoints map.
func GetAllMountPoints() map[string]string {
	mountPointsMu.Lock()
	defer mountPointsMu.Unlock()
	result := make(map[string]string, len(mountPoints))
	for k, v := range mountPoints {
		result[k] = v
	}
	return result
}

// RegisterMountPointForVFS finds the VFS in the active cache and
// registers its mount point using the correct indexed name.
func RegisterMountPointForVFS(vfs *VFS, mountPoint string) {
	activeMu.Lock()
	defer activeMu.Unlock()
	configName := fs.ConfigString(vfs.f)
	vfses := active[configName]
	for i, activeVFS := range vfses {
		if activeVFS == vfs {
			name := configName
			if len(vfses) > 1 {
				name = fmt.Sprintf("%s[%d]", configName, i)
			}
			mountPointsMu.Lock()
			mountPoints[name] = mountPoint
			mountPointsMu.Unlock()
			vfs.MountPoint = mountPoint
			return
		}
	}
}

// UnregisterMountPointForVFS finds the VFS in the active cache and
// unregisters its mount point using the correct indexed name.
func UnregisterMountPointForVFS(vfs *VFS) {
	activeMu.Lock()
	defer activeMu.Unlock()
	configName := fs.ConfigString(vfs.f)
	vfses := active[configName]
	for i, activeVFS := range vfses {
		if activeVFS == vfs {
			name := configName
			if len(vfses) > 1 {
				name = fmt.Sprintf("%s[%d]", configName, i)
			}
			mountPointsMu.Lock()
			delete(mountPoints, name)
			mountPointsMu.Unlock()
			vfs.MountPoint = ""
			return
		}
	}
}

// GetVFS gets a VFS with config name "fs" from the cache or returns an error.
//
// If "fs" is not set and there is one and only one VFS in the active
// cache then it returns it. This is for backwards compatibility.
//
// This deletes the "fs" parameter from in if it is valid
func getVFS(in rc.Params) (vfs *VFS, err error) {
	fsString, err := in.GetString("fs")
	if rc.IsErrParamNotFound(err) {
		var count int
		vfs, count = activeCacheEntries()
		if count == 1 {
			return vfs, nil
		} else if count == 0 {
			return nil, errors.New(`no VFS active and "fs" parameter not supplied`)
		}
		return nil, errors.New(`more than one VFS active - need "fs" parameter`)
	} else if err != nil {
		return nil, err
	}
	activeMu.Lock()
	defer activeMu.Unlock()
	fsString = cache.Canonicalize(fsString)
	activeVFS := active[fsString]
	if len(activeVFS) == 0 {
		return nil, fmt.Errorf("no VFS found with name %q", fsString)
	} else if len(activeVFS) > 1 {
		return nil, fmt.Errorf("more than one VFS active with name %q", fsString)
	}
	delete(in, "fs") // delete the fs parameter
	return activeVFS[0], nil
}

func init() {
	rc.Add(rc.Call{
		Path:  "vfs/refresh",
		Fn:    rcRefresh,
		Title: "Refresh the directory cache.",
		Help: `
This reads the directories for the specified paths and freshens the
directory cache.

If no paths are passed in then it will refresh the root directory.

    rclone rc vfs/refresh

Otherwise pass directories in as dir=path. Any parameter key
starting with dir will refresh that directory, e.g.

    rclone rc vfs/refresh dir=home/junk dir2=data/misc

If the parameter recursive=true is given the whole directory tree
will get refreshed. This refresh will use --fast-list if enabled.
` + getVFSHelp,
	})
}

func rcRefresh(ctx context.Context, in rc.Params) (out rc.Params, err error) {
	vfs, err := getVFS(in)
	if err != nil {
		return nil, err
	}

	root, err := vfs.Root()
	if err != nil {
		return nil, err
	}
	getDir := func(path string) (*Dir, error) {
		path = strings.Trim(path, "/")
		segments := strings.Split(path, "/")
		var node Node = root
		for _, s := range segments {
			if dir, ok := node.(*Dir); ok {
				node, err = dir.stat(s)
				if err != nil {
					return nil, err
				}
			}
		}
		if dir, ok := node.(*Dir); ok {
			return dir, nil
		}
		return nil, EINVAL
	}

	recursive := false
	{
		const k = "recursive"

		if v, ok := in[k]; ok {
			s, ok := v.(string)
			if !ok {
				return out, fmt.Errorf("value must be string %q=%v", k, v)
			}
			recursive, err = strconv.ParseBool(s)
			if err != nil {
				return out, fmt.Errorf("invalid value %q=%v", k, v)
			}
			delete(in, k)
		}
	}

	result := map[string]string{}
	if len(in) == 0 {
		if recursive {
			err = root.readDirTree()
		} else {
			err = root.readDir()
		}
		if err != nil {
			result[""] = err.Error()
		} else {
			result[""] = "OK"
		}
	} else {
		for k, v := range in {
			path, ok := v.(string)
			if !ok {
				return out, fmt.Errorf("value must be string %q=%v", k, v)
			}
			if strings.HasPrefix(k, "dir") {
				dir, err := getDir(path)
				if err != nil {
					result[path] = err.Error()
				} else {
					if recursive {
						err = dir.readDirTree()
					} else {
						err = dir.readDir()
					}
					if err != nil {
						result[path] = err.Error()
					} else {
						result[path] = "OK"
					}
				}
			} else {
				return out, fmt.Errorf("unknown key %q", k)
			}
		}
	}
	out = rc.Params{
		"result": result,
	}
	return out, nil
}

// Add remote control for the VFS
func init() {
	rc.Add(rc.Call{
		Path:  "vfs/forget",
		Fn:    rcForget,
		Title: "Forget files or directories in the directory cache.",
		Help: `
This forgets the paths in the directory cache causing them to be
re-read from the remote when needed.

If no paths are passed in then it will forget all the paths in the
directory cache.

    rclone rc vfs/forget

Otherwise pass files or dirs in as file=path or dir=path.  Any
parameter key starting with file will forget that file and any
starting with dir will forget that dir, e.g.

    rclone rc vfs/forget file=hello file2=goodbye dir=home/junk
` + getVFSHelp,
	})
}

func rcForget(ctx context.Context, in rc.Params) (out rc.Params, err error) {
	vfs, err := getVFS(in)
	if err != nil {
		return nil, err
	}

	root, err := vfs.Root()
	if err != nil {
		return nil, err
	}

	forgotten := []string{}
	if len(in) == 0 {
		root.ForgetAll()
	} else {
		for k, v := range in {
			path, ok := v.(string)
			if !ok {
				return out, fmt.Errorf("value must be string %q=%v", k, v)
			}
			path = strings.Trim(path, "/")
			if strings.HasPrefix(k, "file") {
				root.ForgetPath(path, fs.EntryObject)
			} else if strings.HasPrefix(k, "dir") {
				root.ForgetPath(path, fs.EntryDirectory)
			} else {
				return out, fmt.Errorf("unknown key %q", k)
			}
			forgotten = append(forgotten, path)
		}
	}
	out = rc.Params{
		"forgotten": forgotten,
	}
	return out, nil
}

func getDuration(k string, v any) (time.Duration, error) {
	s, ok := v.(string)
	if !ok {
		return 0, fmt.Errorf("value must be string %q=%v", k, v)
	}
	interval, err := fs.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("parse duration: %w", err)
	}
	return interval, nil
}

func getInterval(in rc.Params) (time.Duration, bool, error) {
	k := "interval"
	v, ok := in[k]
	if !ok {
		return 0, false, nil
	}
	interval, err := getDuration(k, v)
	if err != nil {
		return 0, true, err
	}
	if interval < 0 {
		return 0, true, errors.New("interval must be >= 0")
	}
	delete(in, k)
	return interval, true, nil
}

func getTimeout(in rc.Params) (time.Duration, error) {
	k := "timeout"
	v, ok := in[k]
	if !ok {
		return 10 * time.Second, nil
	}
	timeout, err := getDuration(k, v)
	if err != nil {
		return 0, err
	}
	delete(in, k)
	return timeout, nil
}

func getStatus(vfs *VFS, in rc.Params) (out rc.Params, err error) {
	for k, v := range in {
		return nil, fmt.Errorf("invalid parameter: %s=%s", k, v)
	}
	vfs.pollMu.Lock()
	defer vfs.pollMu.Unlock()
	return rc.Params{
		"enabled":   vfs.Opt.PollInterval != 0,
		"supported": vfs.pollChan != nil,
		"interval": map[string]any{
			"raw":     vfs.Opt.PollInterval,
			"seconds": time.Duration(vfs.Opt.PollInterval) / time.Second,
			"string":  vfs.Opt.PollInterval.String(),
		},
	}, nil
}

func setPollInterval(vfs *VFS, interval, timeout time.Duration) (timeoutHit bool, err error) {
	vfs.pollMu.Lock()
	defer vfs.pollMu.Unlock()
	if vfs.ctx.Err() != nil {
		return false, errors.New("VFS is shutting down")
	}
	if vfs.pollChan == nil {
		return false, errors.New("poll-interval is not supported by this remote")
	}
	var timeoutChan <-chan time.Time
	if timeout > 0 {
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		timeoutChan = timer.C
	}
	select {
	case vfs.pollChan <- interval:
		vfs.Opt.PollInterval = fs.Duration(interval)
	case <-timeoutChan:
		timeoutHit = true
	case <-vfs.ctx.Done():
		return false, errors.New("VFS is shutting down")
	}
	return timeoutHit, nil
}

func init() {
	rc.Add(rc.Call{
		Path:  "vfs/poll-interval",
		Fn:    rcPollInterval,
		Title: "Get the status or update the value of the poll-interval option.",
		Help: `
Without any parameter given this returns the current status of the
poll-interval setting.

When the interval=duration parameter is set, the poll-interval value
is updated and the polling function is notified.
Setting interval=0 disables poll-interval.

    rclone rc vfs/poll-interval interval=5m

The timeout=duration parameter can be used to specify a time to wait
for the current poll function to apply the new value.
If timeout is less or equal 0, which is the default, wait indefinitely.

The new poll-interval value will only be active when the timeout is
not reached.

If poll-interval is updated or disabled temporarily, some changes
might not get picked up by the polling function, depending on the
used remote.
` + getVFSHelp,
	})
}

func rcPollInterval(ctx context.Context, in rc.Params) (out rc.Params, err error) {
	vfs, err := getVFS(in)
	if err != nil {
		return nil, err
	}

	interval, intervalPresent, err := getInterval(in)
	if err != nil {
		return nil, err
	}
	timeout, err := getTimeout(in)
	if err != nil {
		return nil, err
	}
	for k, v := range in {
		return nil, fmt.Errorf("invalid parameter: %s=%s", k, v)
	}
	if !intervalPresent {
		vfs.pollMu.Lock()
		supported := vfs.pollChan != nil
		vfs.pollMu.Unlock()
		if !supported {
			return nil, errors.New("poll-interval is not supported by this remote")
		}
		return getStatus(vfs, in)
	}
	timeoutHit, err := setPollInterval(vfs, interval, timeout)
	if err != nil {
		return nil, err
	}
	out, err = getStatus(vfs, in)
	if out != nil {
		out["timeout"] = timeoutHit
	}
	return
}

func init() {
	rc.Add(rc.Call{
		Path:   "vfs/list",
		NoAuth: true,
		Title:  "List active VFSes.",
		Help: `
This lists the active VFSes.

It returns a list under the key "vfses" where the values are the VFS
names that could be passed to the other VFS commands in the "fs"
parameter.

It also returns a "mountPoints" object mapping each VFS name to its
mount point path, or null for a VFS created without a mount point
(in-process, remote-control, or serve usage).`,
		Fn: rcList,
	})
}

func rcList(ctx context.Context, in rc.Params) (out rc.Params, err error) {
	activeMu.Lock()
	defer activeMu.Unlock()
	var names = []string{}
	mountPointsMap := make(map[string]any)
	for name, vfses := range active {
		if len(vfses) == 1 {
			names = append(names, name)
			// Check if this VFS has a mount point
			mountPoint := GetMountPoint(name)
			if mountPoint != "" {
				mountPointsMap[name] = mountPoint
			} else {
				mountPointsMap[name] = nil
			}
		} else {
			for i := range vfses {
				indexedName := fmt.Sprintf("%s[%d]", name, i)
				names = append(names, indexedName)
				mountPoint := GetMountPoint(indexedName)
				if mountPoint != "" {
					mountPointsMap[indexedName] = mountPoint
				} else {
					mountPointsMap[indexedName] = nil
				}
			}
		}
	}
	out = rc.Params{}
	out["vfses"] = names
	out["mountPoints"] = mountPointsMap
	return out, nil
}

func init() {
	rc.Add(rc.Call{
		Path:   "vfs/stats",
		NoAuth: true,
		Title:  "Stats for a VFS.",
		Help: `
This returns stats for the selected VFS.

    {
        // Status of the disk cache - only present if --vfs-cache-mode > off
        "diskCache": {
            "bytesUsed": 0,
            "erroredFiles": 0,
            "files": 0,
            "hashType": 1,
            "outOfSpace": false,
            "path": "/home/user/.cache/rclone/vfs/local/mnt/a",
            "pathMeta": "/home/user/.cache/rclone/vfsMeta/local/mnt/a",
            "uploadsInProgress": 0,
            "uploadsQueued": 0
        },
        "fs": "/mnt/a",
        "inUse": 1,
        // Status of the in memory metadata cache
        "metadataCache": {
            "dirs": 1,
            "files": 0
        },
        // Options as returned by options/get
        "opt": {
            "CacheMaxAge": 3600000000000,
            // ...
            "WriteWait": 1000000000
        }
    }

` + getVFSHelp,
		Fn: rcStats,
	})
}

func rcStats(ctx context.Context, in rc.Params) (out rc.Params, err error) {
	vfs, err := getVFS(in)
	if err != nil {
		return nil, err
	}
	return vfs.Stats(), nil
}

func init() {
	rc.Add(rc.Call{
		Path:   "vfs/queue",
		NoAuth: true,
		Title:  "Queue info for a VFS.",
		Help: strings.ReplaceAll(`
This returns info about the upload queue for the selected VFS.

This is only useful if |--vfs-cache-mode| > off. If you call it when
the |--vfs-cache-mode| is off, it will return an empty result.

    {
        "queue": // an array of files queued for upload
        [
            {
                "name":      "file",   // string: name (full path) of the file,
                "id":        123,      // integer: id of this item in the queue,
                "size":      79,       // integer: size of the file in bytes
                "expiry":    1.5       // float: time until file is eligible for transfer, lowest goes first
                "tries":     1,        // integer: number of times we have tried to upload
                "delay":     5.0,      // float: seconds between upload attempts
                "uploading": false,    // boolean: true if item is being uploaded
            },
       ],
    }

The |expiry| time is the time until the file is eligible for being
uploaded in floating point seconds. This may go negative. As rclone
only transfers |--transfers| files at once, only the lowest
|--transfers| expiry times will have |uploading| as |true|. So there
may be files with negative expiry times for which |uploading| is
|false|.

`, "|", "`") + getVFSHelp,
		Fn: rcQueue,
	})
}

func rcQueue(ctx context.Context, in rc.Params) (out rc.Params, err error) {
	vfs, err := getVFS(in)
	if err != nil {
		return nil, err
	}
	if vfs.cache == nil {
		return nil, nil
	}
	return vfs.cache.Queue(), nil
}

func init() {
	rc.Add(rc.Call{
		Path:  "vfs/queue-set-expiry",
		Title: "Set the expiry time for an item queued for upload.",
		Help: strings.ReplaceAll(`

Use this to adjust the |expiry| time for an item in the upload queue.
You will need to read the |id| of the item using |vfs/queue| before
using this call.

You can then set |expiry| to a floating point number of seconds from
now when the item is eligible for upload. If you want the item to be
uploaded as soon as possible then set it to a large negative number (eg
-1000000000). If you want the upload of the item to be delayed
for a long time then set it to a large positive number.

Setting the |expiry| of an item which has already has started uploading
will have no effect - the item will carry on being uploaded.

This will return an error if called with |--vfs-cache-mode| off or if
the |id| passed is not found.

This takes the following parameters

- |fs| - select the VFS in use (optional)
- |id| - a numeric ID as returned from |vfs/queue|
- |expiry| - a new expiry time as floating point seconds
- |relative| - if set, expiry is to be treated as relative to the current expiry (optional, boolean)

This returns an empty result on success, or an error.

`, "|", "`") + getVFSHelp,
		Fn: rcQueueSetExpiry,
	})
}

func rcQueueSetExpiry(ctx context.Context, in rc.Params) (out rc.Params, err error) {
	vfs, err := getVFS(in)
	if err != nil {
		return nil, err
	}
	if vfs.cache == nil {
		return nil, rc.NewErrParamInvalid(errors.New("can't call this unless using the VFS cache"))
	}

	// Read input values
	id, err := in.GetInt64("id")
	if err != nil {
		return nil, err
	}
	expiry, err := in.GetFloat64("expiry")
	if err != nil {
		return nil, err
	}
	relative, err := in.GetBool("relative")
	if err != nil && !rc.IsErrParamNotFound(err) {
		return nil, err
	}

	// Set expiry
	var refTime time.Time
	if !relative {
		refTime = time.Now()
	}
	err = vfs.cache.QueueSetExpiry(writeback.Handle(id), refTime, time.Duration(float64(time.Second)*expiry))
	return nil, err
}

func init() {
	rc.Add(rc.Call{
		Path:   "vfs/file-status",
		NoAuth: true,
		Title:  "Report the cache and transfer state of files on a VFS mount.",
		Help: `
This reports, for each path in the "paths" parameter, whether
the file is held in the local cache and the state of its
upload to the remote.

It takes an "fs" parameter naming the mount and a "paths"
parameter holding the file paths to report on, e.g.

    rclone rc vfs/file-status fs=mydrive paths=["/mnt/mydrive/file.txt"]

It returns the report under the key "statuses" where the values
are the per-path result objects.
` + getVFSHelp,
		Fn: rcFileStatus,
	})
}

// rcFileStatus reports the local cache and upload state of the
// files named in the "paths" parameter.
func rcFileStatus(ctx context.Context, in rc.Params) (out rc.Params, err error) {
	// Decision order level 1: Request shape validation
	// Check paths parameter exists and is an array
	pathsRaw, ok := in["paths"]
	if !ok {
		return nil, rc.NewFileStatusError("empty_path", "paths is empty or an entry is not absolute", 400)
	}

	// Handle both []any and []string (the latter is what Go produces for JSON arrays of strings)
	var pathsSlice []any
	switch v := pathsRaw.(type) {
	case []any:
		pathsSlice = v
	case []string:
		pathsSlice = make([]any, len(v))
		for i, s := range v {
			pathsSlice[i] = s
		}
	default:
		return nil, rc.NewFileStatusError("param_invalid", "paths must be an array", 400)
	}

	if len(pathsSlice) == 0 {
		return nil, rc.NewFileStatusError("empty_path", "paths is empty or an entry is not absolute", 400)
	}

	// Convert to string slice, checking for non-string entries
	paths := make([]string, len(pathsSlice))
	for i, v := range pathsSlice {
		s, ok := v.(string)
		if !ok {
			return nil, rc.NewFileStatusError("param_invalid", "paths entries must be strings", 400)
		}
		paths[i] = s
	}

	// Check all paths are absolute (after cleaning)
	for _, path := range paths {
		cleaned := filepath.Clean(path)
		if !filepath.IsAbs(cleaned) {
			return nil, rc.NewFileStatusError("empty_path", "paths is empty or an entry is not absolute", 400)
		}
	}

	// Decision order level 2: Identifier resolution
	// Get the fs parameter if provided
	fsString, err := in.GetString("fs")
	hasFsParam := err == nil
	if err != nil && !rc.IsErrParamNotFound(err) {
		return nil, err
	}

	var vfsInstance *VFS
	var vfsName string

	if hasFsParam {
		// Identifier provided - resolve it
		activeMu.Lock()
		fsString = cache.Canonicalize(fsString)
		activeVFS := active[fsString]
		activeMu.Unlock()

		if len(activeVFS) == 0 {
			return nil, rc.NewFileStatusError("no_active_mount", fmt.Sprintf("no VFS found with name %q", fsString), 400)
		} else if len(activeVFS) > 1 {
			// Check if fsString contains an index suffix like "name[1]"
			// For now, if multiple VFSes with same name and no index, it's ambiguous
			// But the indexed name should have been provided
			return nil, rc.NewFileStatusError("no_active_mount", fmt.Sprintf("more than one VFS active with name %q", fsString), 400)
		}
		vfsInstance = activeVFS[0]
		vfsName = fsString

		// A VFS without an explicit mount point uses its remote's root
		// as the implicit mount point (for in-process/RC/serve VFSes
		// created without a mount point). This allows the endpoint to
		// serve path-based queries against the remote's root.
		if vfsInstance.MountPoint == "" {
			root, err := vfsInstance.Root()
			if err != nil {
				return nil, rc.NewFileStatusError("unaccountable_path", fmt.Sprintf("cannot access VFS root: %v", err), 400)
			}
			vfsInstance.MountPoint = root.Path()
		}
	} else {
		// No identifier provided
		if len(paths) > 1 {
			return nil, rc.NewFileStatusError("ambiguous_mount", "multiple paths provided but no fs parameter", 400)
		}
		// Single path - resolve against mount points.
		singlePath := filepath.Clean(paths[0])
		vfsInstance, vfsName = resolveVFSForPath(singlePath)
		if vfsInstance == nil {
			// Path didn't resolve to a VFS. This is a per-path
			// outside_any_mount result (the call reached the mount
			// level but the path is outside all registered mount
			// points). Per FR-006 decision order level 3, this is
			// a per-path result, not a whole-request rejection.
			result := buildErrorResult(singlePath, "outside_any_mount", "path lies outside any registered mount point")
			out = rc.Params{"statuses": []any{result}}
			return out, nil
		}
	}

	// Now we have a single VFS to query
	// Decision order level 3: Per-path classification
	var results []map[string]any
	for _, path := range paths {
		cleaned := filepath.Clean(path)
		result := classifyPath(vfsInstance, vfsName, cleaned, hasFsParam)
		results = append(results, result)
	}

	out = rc.Params{
		"statuses": results,
	}
	return out, nil
}

// resolveVFSForPath finds the VFS that owns the given path by checking
// registered mount points. Returns the VFS and its indexed name, or nil if
// the path is not inside any registered mount point.
func resolveVFSForPath(path string) (*VFS, string) {
	mountPointsMu.Lock()
	defer mountPointsMu.Unlock()

	// Find the longest matching mount point
	var bestMatch string
	var bestName string
	var bestVFS *VFS

	for name, mountPoint := range mountPoints {
		if mountPoint == "" {
			continue
		}
		cleanMP := filepath.Clean(mountPoint)
		cleanPath := filepath.Clean(path)

		// Check if path is inside this mount point
		if isPathInsideMountPoint(cleanPath, cleanMP) {
			// Prefer the longest mount point (most specific)
			if len(cleanMP) > len(bestMatch) {
				bestMatch = cleanMP
				bestName = name
			}
		}
	}

	if bestName == "" {
		return nil, ""
	}

	// Find the VFS for this name
	activeMu.Lock()
	defer activeMu.Unlock()

	// Parse the name to get config name and index
	configName, index := parseIndexedName(bestName)
	vfses := active[configName]
	if index >= 0 && index < len(vfses) {
		bestVFS = vfses[index]
	} else if len(vfses) == 1 && index == -1 {
		bestVFS = vfses[0]
	}

	return bestVFS, bestName
}

// parseIndexedName parses a name like "mydrive[1]" into ("mydrive", 1)
// or returns ("mydrive", -1) if no index suffix.
func parseIndexedName(name string) (string, int) {
	if strings.HasSuffix(name, "]") {
		bracket := strings.LastIndex(name, "[")
		if bracket > 0 {
			indexStr := name[bracket+1 : len(name)-1]
			if idx, err := strconv.Atoi(indexStr); err == nil {
				return name[:bracket], idx
			}
		}
	}
	return name, -1
}

// isPathInsideMountPoint checks if path is inside mountPoint (at component boundary)
func isPathInsideMountPoint(path, mountPoint string) bool {
	if path == mountPoint {
		return true
	}
	// Check if path starts with mountPoint + separator
	// Handle root mount point "/" specially to avoid "//" prefix
	var prefix string
	if mountPoint == "/" {
		prefix = "/"
	} else {
		prefix = mountPoint + string(filepath.Separator)
	}
	return strings.HasPrefix(path, prefix)
}

// classifyPath classifies a single path against a VFS and returns the result object.
// If hasFsParam is true, the caller provided an "fs" identifier, so we use the
// VFS's remote root as the effective mount point when no explicit mount point exists.
func classifyPath(vfsInstance *VFS, vfsName, path string, hasFsParam bool) map[string]any {
	// Determine the effective mount point
	mountPoint := vfsInstance.MountPoint
	if mountPoint == "" && hasFsParam {
		// VFS has no explicit mount point but we have an fs
		// identifier - use the remote's root as the implicit
		// mount point (for in-process/RC/serve VFSes created
		// without a mount point). This allows the endpoint to
		// serve path-based queries against the remote's root.
		root, err := vfsInstance.Root()
		if err != nil {
			return buildErrorResult(path, "unaccountable_path", fmt.Sprintf("cannot access VFS root: %v", err))
		}
		mountPoint = root.Path()
		if mountPoint == "" {
			// Root directory of remote has empty path - treat as "/"
			mountPoint = "/"
		}
	}

	if mountPoint == "" {
		// VFS has no mount point and no fs identifier - path is outside
		return buildErrorResult(path, "outside_any_mount", "VFS has no mount point")
	}

	cleanMP := filepath.Clean(mountPoint)
	cleanPath := filepath.Clean(path)

	if !isPathInsideMountPoint(cleanPath, cleanMP) {
		return buildErrorResult(path, "outside_any_mount", "path lies outside the mount point")
	}

	// Check for overlapping mount points (path inside more than one)
	// Only check for overlap when we don't have an fs identifier (identifier-less resolution)
	// When fs is provided, we already know which VFS we're querying
	if !hasFsParam && isPathInMultipleMountPoints(cleanPath) {
		return buildErrorResult(path, "ambiguous_mount", "path lies inside multiple mount points")
	}

	// Get the relative path within the mount
	relPath, err := filepath.Rel(cleanMP, cleanPath)
	if err != nil {
		return buildErrorResult(path, "outside_any_mount", "path is not relative to mount point")
	}

	// Check if the path exists in the VFS
	root, err := vfsInstance.Root()
	if err != nil {
		return buildErrorResult(path, "unaccountable_path", "cannot access VFS root")
	}

	// Walk the path to find the entry
	node, parentDir, err := walkToEntry(root, relPath)
	if err != nil {
		if os.IsNotExist(err) {
			// Check if parent is known
			if parentDir != nil {
				// Parent directory is known - check if the file exists on the remote
				// This handles the case where a file was added to the remote after the
				// directory was cached (FR-008, FR-033)
				cachePath := relPath
				if o, err := vfsInstance.f.NewObject(vfsInstance.ctx, cachePath); err == nil && o != nil {
					// File exists on remote but not in cache - treat as regular file with no cache entry
					// Construct status directly to avoid cache lookup which might return a stale item
					cacheOff := vfsInstance.Opt.CacheMode == vfscommon.CacheModeOff
					size := o.Size()
					var upload FileUploadState
					if cacheOff {
						upload = FileUploadNotTracked
					} else {
						upload = FileUploadNotApplicable
					}
					status := FileStatus{
						Local:  FileLocalNotHeld,
						Upload: upload,
						Bytes: FileByteInfo{
							UnbrokenRun: 0,
							TotalHeld:   0,
							TotalSize:   size,
							TotalKnown:  true,
						},
						IsDir: false,
					}
					return buildStatusResult(path, status, cacheOff)
				}
				// File doesn't exist on remote either
				return buildErrorResult(path, "not_found", "file does not exist in mount")
			}
			return buildErrorResult(path, "unaccountable_path", "parent directory not known")
		}
		return buildErrorResult(path, "unaccountable_path", err.Error())
	}

	// Check if it's a directory
	if node != nil && node.IsDir() {
		return buildErrorResult(path, "directory_not_file", "path names a directory")
	}

	// Check if it's a symlink (non-regular file)
	if node != nil && vfsInstance.Opt.Links && isSymlink(node) {
		// Non-regular file
		return buildNonRegularFileResult(vfsInstance, path)
	}

	// Regular file - get its status
	cachePath := relPath
	if cachePath == "." {
		cachePath = ""
	}
	isLink := false // Final component is not a symlink (we checked above)
	cacheOff := vfsInstance.Opt.CacheMode == vfscommon.CacheModeOff

	status := fileStatusOf(vfsInstance, cachePath, isLink, cacheOff)
	return buildStatusResult(path, status, cacheOff)
}

// isPathInMultipleMountPoints checks if a path is inside more than one registered mount point.
func isPathInMultipleMountPoints(path string) bool {
	mountPointsMu.Lock()
	defer mountPointsMu.Unlock()

	count := 0
	for _, mountPoint := range mountPoints {
		if mountPoint == "" {
			continue
		}
		cleanMP := filepath.Clean(mountPoint)
		if isPathInsideMountPoint(path, cleanMP) {
			count++
			if count > 1 {
				return true
			}
		}
	}
	return false
}

// walkToEntry walks the VFS tree to find the entry for the given relative path.
// Returns the node, its parent directory, and any error.
func walkToEntry(root *Dir, relPath string) (Node, *Dir, error) {
	if relPath == "" || relPath == "." {
		return root, nil, nil // Root directory
	}

	segments := strings.Split(relPath, string(filepath.Separator))
	var node Node = root
	var parentDir *Dir

	for i, segment := range segments {
		if dir, ok := node.(*Dir); ok {
			parentDir = dir
			child, err := dir.stat(segment)
			if err != nil {
				if os.IsNotExist(err) {
					return nil, parentDir, err
				}
				return nil, parentDir, err
			}
			node = child
		} else {
			// Not a directory, can't traverse further
			return nil, parentDir, fmt.Errorf("not a directory")
		}
		// If this is the last segment, we found the entry
		if i == len(segments)-1 {
			return node, parentDir, nil
		}
	}
	return node, parentDir, nil
}

// isSymlink checks if a node is a symlink
func isSymlink(node Node) bool {
	if f, ok := node.(*File); ok {
		return f.IsSymlink()
	}
	return false
}

// buildErrorResult creates a per-path error result.
func buildErrorResult(path, code, message string) map[string]any {
	return map[string]any{
		"path": path,
		"error": map[string]any{
			"code":    code,
			"message": message,
		},
	}
}

// buildNonRegularFileResult creates a result for a non-regular file (symlink).
func buildNonRegularFileResult(vfsInstance *VFS, path string) map[string]any {
	cacheOff := vfsInstance.Opt.CacheMode == vfscommon.CacheModeOff
	status := FileStatus{
		Local:  FileLocalNotRegularFile,
		Upload: FileUploadNotApplicable,
		Bytes: FileByteInfo{
			TotalKnown: false,
		},
		IsDir: false,
	}
	if cacheOff {
		status.Upload = FileUploadNotTracked
	}
	return buildStatusResult(path, status, cacheOff)
}

// buildStatusResult creates a success result from a FileStatus.
func buildStatusResult(path string, status FileStatus, cacheOff bool) map[string]any {
	result := status.ToParams()
	result["path"] = path
	return result
}

// buildSinglePathResponse builds a response for a single path with an error.
func buildSinglePathResponse(path, code, message string) rc.Params {
	return rc.Params{
		"statuses": []map[string]any{
			buildErrorResult(path, code, message),
		},
	}
}
