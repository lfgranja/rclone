package vfs

import (
	"github.com/rclone/rclone/lib/ranges"
	"github.com/rclone/rclone/vfs/vfscache"
)

// fileStatusOf derives the FileStatus for a single path from the
// mount's persisted per-file record. It is called with the VFS lock
// not held; it takes the item's own lock for a single observation
// instant (FR-022). It never blocks on a transfer or reset condition
// (FR-026); if the record is mid-rewrite the local fact is reported
// as underivable. The two facts are derived independently from the
// same record snapshot — the 36 pairs of FR-022 have no cross-fact
// coupling. The cacheOff flag is the mount-level guard for
// not_tracked (FR-011). The isLink flag indicates the final path
// component is a symlink on a mount started to show links; the local
// fact becomes not_regular_file with all three byte quantities
// not_measurable (FR-032).
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

	// Cache-keeping mount: look up the item in the cache. The item
	// may not exist (file never accessed through the mount), or it
	// may exist with various states.
	item := v.cache.Item(cachePath)
	if item == nil {
		// No cache entry at all. The mount holds no copy of the
		// file — local fact not_held, 0 held bytes (true zero,
		// FR-002), total_size from the remote object if available.
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

	// Item exists: check if it's being reset. Per FR-026 the read
	// does not wait for the reset; the local fact is underivable.
	if item.IsBeingReset() {
		return fileStatusFromItem(item, cachePath, true, v)
	}

	// Normal case: derive both facts from the item's current record.
	return fileStatusFromItem(item, cachePath, false, v)
}

// fileStatusFromItem derives the FileStatus from an item's record
// snapshot. The underivable flag forces the local fact to
// underivable with both held quantities not_determinable (used when
// beingReset is true, FR-026).
func fileStatusFromItem(item *vfscache.Item, cachePath string, underivable bool, v *VFS) FileStatus {
	// Get a consistent snapshot of the item's persisted info.
	rs := item.GetRanges()
	size := item.GetSizeLocked()
	dirty := item.IsDirty()
	writeBackID := item.GetWriteBackID()
	fingerprint := item.GetFingerprint()

	// Determine held bytes from the ranges.
	var unbrokenRun, totalHeld int64
	var totalKnown bool
	if !underivable {
		if rs != nil {
			unbrokenRun = unbrokenRunFrom(rs, 0, size)
			totalHeld = rs.Size()
		}
		if size >= 0 {
			totalKnown = true
		}
	} else if size >= 0 {
		// Underivable: both held quantities are not_determinable
		// (rendered by ToParams). Total size is still a measurement
		// where the mount knows it (FR-002, FR-004).
		totalKnown = true
	}

	// Local fact derivation.
	var local FileLocalState
	if underivable {
		local = FileLocalUnderivable
	} else if rs == nil || rs.Size() == 0 {
		// No ranges held.
		local = FileLocalNotHeld
	} else if size >= 0 && rs.Present(ranges.Range{Pos: 0, Size: size}) {
		// Entire file present.
		local = FileLocalFullyHeld
	} else {
		// Partially held.
		local = FileLocalPartlyHeld
	}

	// Check for download failure: if downloaders have accumulated
	// errors, the download failed. The held quantities still
	// describe the bytes that did arrive (FR-028).
	if local != FileLocalUnderivable {
		errCount, lastErr := item.GetDownloadersError()
		if errCount > 0 && lastErr != nil {
			local = FileLocalDownloadFailed
		}
	}

	// Upload fact derivation.
	var upload FileUploadState
	if dirty {
		// File has local modifications not yet uploaded.
		if writeBackID != 0 {
			// There is a writeback queue entry.
			if v.cache.WritebackUploading(writeBackID) {
				upload = FileUploadUploading
			} else {
				upload = FileUploadAwaitingUpload
			}
		} else {
			// Dirty but no writeback ID — upload failed on this
			// mount instance and is not currently queued (FR-003,
			// FR-028). The failure is reported for the life of the
			// mount.
			upload = FileUploadUploadFailed
		}
	} else if fingerprint != "" {
		// Not dirty and has fingerprint: upload completed.
		upload = FileUploadUploaded
	} else {
		// Not dirty, no fingerprint: never written through this
		// mount.
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
