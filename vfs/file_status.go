package vfs

import (
	"github.com/rclone/rclone/lib/ranges"
	"github.com/rclone/rclone/vfs/vfscache"
)

// fileStatusOf derives the FileStatus for a single path from the
// mount's persisted per-file record. It is called with the VFS lock
// not held. The record is probed — never created — so a status-only
// read changes no published cache total (FR-005); it takes the item's
// own snapshot for a single observation instant (FR-022) and never
// blocks on a transfer or reset condition (FR-026): a record a reset
// is mid-rewrite on is reported as underivable. The two facts are
// derived independently from the same snapshot — the 36 pairs of
// FR-022 have no cross-fact coupling. The cacheOff flag is the
// mount-level guard for not_tracked (FR-011). The isLink flag
// indicates the final path component is a symlink on a mount started
// to show links; the local fact becomes not_regular_file with all
// three byte quantities not_measurable (FR-032).
func fileStatusOf(v *VFS, cachePath string, isLink, cacheOff bool) FileStatus {
	// Non-regular file: mount started to show links, final component
	// is a symlink. The local fact is not_regular_file with all three
	// byte quantities not_measurable (FR-032). The upload fact is
	// derived on its own terms — the mount never wrote the file, so
	// not_applicable on a cache-keeping mount, not_tracked on a
	// cache-off mount.
	if isLink {
		return FileStatus{
			Local: FileLocalNotRegularFile,
			Upload: func() FileUploadState {
				if cacheOff {
					return FileUploadNotTracked
				}
				return FileUploadNotApplicable
			}(),
			Bytes: FileByteInfo{
				TotalKnown: false, // total_size rendered as not_measurable by ToParams
			},
			IsDir: false,
		}
	}

	// Cache-off mount: the mount keeps no cache at all (FR-011). No
	// file has a cache entry, so the local fact is not_held with true
	// zero held bytes (a measurement, not a missing value, FR-002),
	// total_size known from the directory entry the mount holds, and
	// the upload fact is the mount-level guard not_tracked.
	if cacheOff {
		size := int64(-1)
		// Best-effort size from the remote; if unavailable the
		// byte info will carry not_determinable for total_size.
		if o, err := v.f.NewObject(v.ctx, cachePath); err == nil && o != nil {
			size = o.Size()
		}
		return FileStatus{
			Local:  FileLocalNotHeld,
			Upload: FileUploadNotTracked,
			Bytes: FileByteInfo{
				UnbrokenRun: 0,
				TotalHeld:   0,
				TotalSize:   size,
				TotalKnown:  size >= 0,
			},
			IsDir: false,
		}
	}

	// Cache-keeping mount: probe the file's record. The probe brings
	// no entry into the cache index, so the published cache totals
	// are unchanged by the read; where the persisted record survives
	// on disk it is still accounted for (FR-030/FR-031).
	snap, ok := v.cache.ProbeSnapshot(cachePath)
	if !ok {
		// No record at all. A synthesized metadata file is reported
		// from its readable in-memory node rather than from the
		// absent item; anything else holds no copy of the file.
		if status, found := fileStatusMetadata(v, cachePath); found {
			return status
		}
		size := int64(-1)
		if o, err := v.f.NewObject(v.ctx, cachePath); err == nil && o != nil {
			size = o.Size()
		}
		return FileStatus{
			Local:  FileLocalNotHeld,
			Upload: FileUploadNotApplicable, // never written through this mount
			Bytes: FileByteInfo{
				UnbrokenRun: 0,
				TotalHeld:   0,
				TotalSize:   size,
				TotalKnown:  size >= 0,
			},
			IsDir: false,
		}
	}

	// A reset is rewriting the record. Per FR-026 the read does not
	// wait for it; the local fact is underivable with both held
	// quantities not_determinable, and total_size not_determinable
	// since it cannot be derived without waiting either. The upload
	// fact keeps the mount-level value for a file whose upload cannot
	// be observed mid-reset.
	if snap.Resetting {
		return FileStatus{
			Local:  FileLocalUnderivable,
			Upload: FileUploadNotApplicable,
			Bytes: FileByteInfo{
				TotalKnown: false,
			},
			IsDir: false,
		}
	}

	// Normal case: derive both facts from the single snapshot.
	return fileStatusFromSnapshot(v, snap)
}

// fileStatusFromSnapshot derives the FileStatus from one coherent
// per-file observation. The local fact, the upload fact and the byte
// quantities all come from the same snapshot, so a reported pair
// always describes one moment (FR-022) and never tears across
// separate reads of the record.
func fileStatusFromSnapshot(v *VFS, snap vfscache.ItemSnapshot) FileStatus {
	size := snap.Size
	totalKnown := size >= 0

	// Determine held bytes from the snapshot's ranges.
	var unbrokenRun, totalHeld int64
	if snap.Rs != nil {
		totalHeld = snap.Rs.Size()
	}
	switch {
	case size < 0:
		// The total is unknown: the unbroken run is the cached
		// prefix from offset zero, whatever follows the gap being
		// counted in the total held only (FR-002).
		unbrokenRun = unbrokenPrefix(snap.Rs)
	case size > 0:
		unbrokenRun = unbrokenRunFrom(snap.Rs, 0, size)
	}

	// Local fact derivation.
	var local FileLocalState
	switch {
	case size == 0:
		// A file with no bytes to be present has all of its bytes
		// present (FR-002a).
		local = FileLocalFullyHeld
	case snap.DownloadErr != nil && totalKnown:
		// A failed download takes the failure value rather than
		// partly_held, the held quantities still describing the
		// bytes that did arrive (FR-028). It needs a known size to
		// state the quantities against.
		local = FileLocalDownloadFailed
	case snap.Rs == nil || totalHeld == 0:
		// No ranges held.
		local = FileLocalNotHeld
	case size >= 0 && snap.Rs.Present(ranges.Range{Pos: 0, Size: size}):
		// Entire file present.
		local = FileLocalFullyHeld
	default:
		// Partially held, including a zero prefix where only later
		// bytes are cached.
		local = FileLocalPartlyHeld
	}

	// Upload fact derivation.
	var upload FileUploadState
	switch {
	case snap.UploadErr != nil:
		// The last upload attempt on this mount instance failed. A
		// retry being scheduled clears nothing; only a later
		// successful transfer does (FR-028).
		upload = FileUploadUploadFailed
	case snap.Dirty:
		// Local modifications not yet uploaded.
		if snap.WriteBackID != 0 && v.cache.WritebackUploading(snap.WriteBackID) {
			upload = FileUploadUploading
		} else {
			upload = FileUploadAwaitingUpload
		}
	case snap.UploadCompleted:
		// This mount instance completed the upload of the current
		// content. A file merely read into the cache was never
		// written through the mount and stays not_applicable.
		upload = FileUploadUploaded
	default:
		// Never written through this mount.
		upload = FileUploadNotApplicable
	}

	return FileStatus{
		Local:  local,
		Upload: upload,
		Bytes: FileByteInfo{
			UnbrokenRun: unbrokenRun,
			TotalHeld:   totalHeld,
			TotalSize:   size,
			TotalKnown:  totalKnown,
		},
		IsDir: false,
	}
}

// fileStatusMetadata reports a synthesized metadata file from its
// readable in-memory node. Asking for the path is what brings the
// entry into being and it holds real content once there, so it takes
// its values on the same terms as any other path — fully held with
// real quantities — rather than not_found or not_held. Its upload
// fact is reported like any other path's: the mount never wrote it.
// It reports false where the path is not a metadata file or names no
// readable regular file.
func fileStatusMetadata(v *VFS, cachePath string) (FileStatus, bool) {
	if _, found := v.isMetadataFile(cachePath); !found {
		return FileStatus{}, false
	}
	node, err := v.Stat(cachePath)
	if err != nil || node == nil || !node.IsFile() {
		return FileStatus{}, false
	}
	size := node.Size()
	if size < 0 {
		return FileStatus{}, false
	}
	return FileStatus{
		Local:  FileLocalFullyHeld,
		Upload: FileUploadNotApplicable,
		Bytes: FileByteInfo{
			UnbrokenRun: size,
			TotalHeld:   size,
			TotalSize:   size,
			TotalKnown:  true,
		},
		IsDir: false,
	}, true
}

// unbrokenRunFrom returns the length of the unbroken run of
// present bytes starting at offset, using the Ranges FindMissing
// method. The unbroken run ends at the first missing byte.
func unbrokenRunFrom(rs ranges.Ranges, offset, size int64) int64 {
	if rs == nil || size <= 0 {
		return 0
	}
	// FindMissing returns the first missing range starting from
	// the given offset. Its Pos is the end of the unbroken run.
	missing := rs.FindMissing(ranges.Range{Pos: offset, Size: size})
	if missing.Pos <= offset {
		// The first byte is missing
		return 0
	}
	return missing.Pos - offset
}

// unbrokenPrefix returns the length of the unbroken run of present
// bytes from offset zero where the total size is unknown: the reach
// of the first range where it starts at zero, else 0. The ranges are
// sorted and coalesced, so the first range is the lowest one.
func unbrokenPrefix(rs ranges.Ranges) int64 {
	if len(rs) == 0 || rs[0].Pos != 0 {
		return 0
	}
	return rs[0].Size
}
