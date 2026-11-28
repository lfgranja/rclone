package vfs

import "github.com/rclone/rclone/fs/rc"

// Byte-quantity markers for values the mount cannot state.
const (
	fileStatusMarkerNotDeterminable = "not_determinable"
	fileStatusMarkerNotMeasurable   = "not_measurable"
)

// Exit statuses for reported states; failures go through the error path.
const (
	fileStatusExitSuccess           = 0
	fileStatusExitNotFullyHeld      = 11
	fileStatusExitUploadNotComplete = 12
)

// FileLocalState is the local-cache fact reported for a file.
type FileLocalState string

// Local-cache fact values.
const (
	// FileLocalFullyHeld reports a fully cached file, including a zero-byte file.
	FileLocalFullyHeld FileLocalState = "fully_held"
	// FileLocalPartlyHeld reports a file held in part.
	FileLocalPartlyHeld FileLocalState = "partly_held"
	// FileLocalNotHeld reports a file with no cache entry and zero held bytes.
	FileLocalNotHeld FileLocalState = "not_held"
	// FileLocalDownloadFailed reports a file whose download failed.
	FileLocalDownloadFailed FileLocalState = "download_failed"
	// FileLocalUnderivable reports a cache entry with no derivable held bytes.
	FileLocalUnderivable FileLocalState = "underivable"
	// FileLocalNotRegularFile reports a path that is not a regular file.
	FileLocalNotRegularFile FileLocalState = "not_regular_file"
)

// FileUploadState is the upload fact reported for a file.
type FileUploadState string

// Upload fact values.
const (
	// FileUploadUploaded reports a completed upload.
	FileUploadUploaded FileUploadState = "uploaded"
	// FileUploadAwaitingUpload reports an upload queued but not started.
	FileUploadAwaitingUpload FileUploadState = "awaiting_upload"
	// FileUploadUploading reports an upload in progress.
	FileUploadUploading FileUploadState = "uploading"
	// FileUploadUploadFailed reports an upload that failed on this mount.
	FileUploadUploadFailed FileUploadState = "upload_failed"
	// FileUploadNotApplicable reports a file outside the mount's upload scope.
	FileUploadNotApplicable FileUploadState = "not_applicable"
	// FileUploadNotTracked reports a mount that keeps no cache or upload queue.
	FileUploadNotTracked FileUploadState = "not_tracked"
)

// FileByteInfo holds the byte quantities reported for a file.
type FileByteInfo struct {
	// UnbrokenRun is the held byte run from offset zero.
	UnbrokenRun int64
	// TotalHeld is the number of bytes held anywhere in the file.
	TotalHeld int64
	// TotalSize is the file size, valid only when TotalKnown is true.
	TotalSize int64
	// TotalKnown reports whether the mount knows the total size.
	TotalKnown bool
}

// FileStatus is the report for one path the mount can answer.
type FileStatus struct {
	// Local is the local-cache fact of the file.
	Local FileLocalState
	// Upload is the upload fact of the file.
	Upload FileUploadState
	// Bytes are the file's byte quantities.
	Bytes FileByteInfo
	// IsDir reports whether the path names a directory.
	IsDir bool
}

// ToParams returns the per-path state result with local, upload and bytes keys.
func (s FileStatus) ToParams() rc.Params {
	var unbrokenRun, totalHeld, totalSize any
	switch s.Local {
	case FileLocalNotRegularFile:
		// All three quantities carry not_measurable.
		unbrokenRun = fileStatusMarkerNotMeasurable
		totalHeld = fileStatusMarkerNotMeasurable
		totalSize = fileStatusMarkerNotMeasurable
	case FileLocalUnderivable:
		// The two held quantities carry not_determinable.
		unbrokenRun = fileStatusMarkerNotDeterminable
		totalHeld = fileStatusMarkerNotDeterminable
		totalSize = s.totalSize()
	default:
		unbrokenRun = s.Bytes.UnbrokenRun
		totalHeld = s.Bytes.TotalHeld
		totalSize = s.totalSize()
	}
	return rc.Params{
		"local":  s.Local,
		"upload": s.Upload,
		"bytes": rc.Params{
			"unbroken_run": unbrokenRun,
			"total_held":   totalHeld,
			"total_size":   totalSize,
		},
	}
}

// totalSize returns the total size or the not_determinable marker.
func (s FileStatus) totalSize() any {
	if !s.Bytes.TotalKnown {
		return fileStatusMarkerNotDeterminable
	}
	return s.Bytes.TotalSize
}

// ExitCode returns the exit status for the reported state.
func (s FileStatus) ExitCode(cacheOff bool) int {
	if cacheOff {
		if s.Local == FileLocalNotRegularFile {
			return fileStatusExitNotFullyHeld
		}
		return fileStatusExitUploadNotComplete
	}
	if s.Local == FileLocalFullyHeld &&
		(s.Upload == FileUploadUploaded || s.Upload == FileUploadNotApplicable) {
		return fileStatusExitSuccess
	}
	if s.Local != FileLocalFullyHeld {
		return fileStatusExitNotFullyHeld
	}
	return fileStatusExitUploadNotComplete
}
