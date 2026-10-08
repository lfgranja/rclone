package vfs

import "github.com/rclone/rclone/fs/rc"

// The two byte-quantity markers, spelled exactly as the
// vfs/file-status endpoint serialises them (data-model.md
// "ByteInfo"). The markers live in a namespace of their
// own, disjoint from the values of either fact: a marker
// appears only as the value of a byte quantity, and a
// fact's value only as that fact's value (FR-002).
const (
	fileStatusMarkerNotDeterminable = "not_determinable"
	fileStatusMarkerNotMeasurable   = "not_measurable"
)

// The exit statuses FR-027 publishes for a result that
// reports a state: success, not fully held locally and
// upload not complete. The usage error (2) and the query
// failure (1) belong to failures to answer the query at
// all, which the endpoint reports as errors rather than
// as states, so no FileStatus maps to them (FR-027).
const (
	fileStatusExitSuccess           = 0
	fileStatusExitNotFullyHeld      = 11
	fileStatusExitUploadNotComplete = 12
)

// FileLocalState is the local-cache fact of a file: how
// much of the file the mount holds locally (FR-002,
// FR-004). Each value is published with its exact wire
// spelling in contracts/api.md "State Value Definitions",
// and describes one observation instant (FR-022).
type FileLocalState string

// The values of the local-cache fact.
const (
	// FileLocalFullyHeld reports a file the mount holds
	// in the cache in full, a zero-byte file included.
	FileLocalFullyHeld FileLocalState = "fully_held"
	// FileLocalPartlyHeld reports a file the mount holds
	// in part, its held bytes described by the byte
	// quantities.
	FileLocalPartlyHeld FileLocalState = "partly_held"
	// FileLocalNotHeld reports a file the mount holds no
	// cache entry for; 0 held bytes are a true
	// measurement of a mount holding none of the file,
	// not a missing value (FR-002).
	FileLocalNotHeld FileLocalState = "not_held"
	// FileLocalDownloadFailed reports a file whose
	// download failed, the byte quantities describing the
	// bytes that did arrive (FR-028).
	FileLocalDownloadFailed FileLocalState = "download_failed"
	// FileLocalUnderivable reports a file the mount holds
	// a cache entry for but cannot derive held bytes
	// from, the two held quantities carrying the
	// not_determinable marker. It is reported in exactly
	// this one case (FR-004).
	FileLocalUnderivable FileLocalState = "underivable"
	// FileLocalNotRegularFile reports a path that is not
	// a regular file, such as a link on a mount started
	// to show links, all three byte quantities carrying
	// the not_measurable marker (FR-032).
	FileLocalNotRegularFile FileLocalState = "not_regular_file"
)

// FileUploadState is the upload fact of a file: the state
// of its upload to the remote (FR-003). The value is
// derived per file from what the mount durably records
// about the file's content, never from the cache mode the
// mount was started with, and describes the same
// observation instant as the local fact (FR-022).
type FileUploadState string

// The values of the upload fact.
const (
	// FileUploadUploaded reports a file whose upload
	// completed.
	FileUploadUploaded FileUploadState = "uploaded"
	// FileUploadAwaitingUpload reports a file queued for
	// upload but not started.
	FileUploadAwaitingUpload FileUploadState = "awaiting_upload"
	// FileUploadUploading reports a file currently being
	// uploaded.
	FileUploadUploading FileUploadState = "uploading"
	// FileUploadUploadFailed reports a file whose upload
	// failed on the current mount instance; retry
	// scheduling is internal to the mount and is not part
	// of the reported state (FR-003).
	FileUploadUploadFailed FileUploadState = "upload_failed"
	// FileUploadNotApplicable reports a file not in scope
	// for upload on a mount that does track uploads, such
	// as one never written through the mount. It is a
	// mount-level guard, never a per-file observation, and
	// is mutually exclusive with every per-file value
	// (FR-003).
	FileUploadNotApplicable FileUploadState = "not_applicable"
	// FileUploadNotTracked reports a mount that keeps no
	// cache at all and so has no upload queue. It is a
	// mount-level guard, reported for every file on such a
	// mount and never on one that keeps a cache (FR-003,
	// FR-011).
	FileUploadNotTracked FileUploadState = "not_tracked"
)

// FileByteInfo is the three byte quantities of a file's
// report (FR-002, FR-002a): the unbroken run of held
// bytes from offset zero, the total bytes held anywhere in
// the file, and the file's total size. Each quantity is a
// measurement, 0 included, or an explicit marker; a
// quantity the mount cannot state is never omitted and
// never stands in as 0 (FR-002). Which quantity carries
// which marker is fixed by the local fact the status
// reports, so the struct carries only the measurements and
// whether the size is known: ToParams renders the
// markers, and a TotalKnown of false renders total_size as
// the not_determinable marker rather than as 0.
type FileByteInfo struct {
	// UnbrokenRun is the length of the unbroken run of
	// held bytes starting at offset zero.
	UnbrokenRun int64
	// TotalHeld is the total number of bytes held anywhere
	// in the file.
	TotalHeld int64
	// TotalSize is the file's total size, a measurement
	// only where TotalKnown is true.
	TotalSize int64
	// TotalKnown reports whether the mount knows the
	// file's total size; where it does not, the report
	// carries the not_determinable marker for total_size,
	// never 0 standing in for a size the mount cannot
	// supply (FR-002).
	TotalKnown bool
}

// FileStatus is the report of one path the mount could
// answer: both facts of FR-022, the byte quantities of
// FR-002, and whether the path names a directory, all
// describing one observation instant. A path the mount
// cannot answer — a directory, an absent entry, a path
// outside the mount — is not a FileStatus; it is an
// error result, which the endpoint reports separately
// (contracts/api.md "Error Results").
type FileStatus struct {
	// Local is the local-cache fact of the file.
	Local FileLocalState
	// Upload is the upload fact of the file.
	Upload FileUploadState
	// Bytes are the file's byte quantities.
	Bytes FileByteInfo
	// IsDir reports whether the path names a directory,
	// which the endpoint answers with the directory_not_file
	// error result rather than with a state (FR-012).
	IsDir bool
}

// ToParams returns the per-path state result of the
// contract: the local fact, the upload fact and the bytes
// object, under the "local", "upload" and "bytes" keys
// (contracts/api.md "Response Format"). The caller adds
// the "path" key; error results carry an "error" object
// instead and are built separately. The byte quantities
// carry their markers where the local fact fixes them —
// not_determinable for the two held quantities of an
// underivable file, and not_measurable for all three
// quantities of a non-regular file — and total_size
// carries not_determinable where the mount does not know
// the file's size, so a consumer reads one object shape
// whatever the state (FR-002).
func (s FileStatus) ToParams() rc.Params {
	var unbrokenRun, totalHeld, totalSize any
	switch s.Local {
	case FileLocalNotRegularFile:
		// A non-regular file has no size and no byte
		// ranges for any of the three quantities to
		// describe, so all three carry the not_measurable
		// marker, the fields still being present (FR-032).
		unbrokenRun = fileStatusMarkerNotMeasurable
		totalHeld = fileStatusMarkerNotMeasurable
		totalSize = fileStatusMarkerNotMeasurable
	case FileLocalUnderivable:
		// The mount holds a cache entry it cannot derive
		// held bytes from, so both held quantities carry
		// the not_determinable marker; the size is a
		// measurement wherever the mount knows it (FR-002).
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

// totalSize returns the total_size byte quantity: the
// file's size where the mount knows it, and the
// not_determinable marker where it does not, so a caller
// can tell an empty file from a size the mount cannot
// supply (FR-002).
func (s FileStatus) totalSize() any {
	if !s.Bytes.TotalKnown {
		return fileStatusMarkerNotDeterminable
	}
	return s.Bytes.TotalSize
}

// ExitCode returns the exit status FR-027 maps a reported
// state to, so a script can branch without parsing the
// output. A file reported as fully held locally whose
// upload fact is uploaded or not_applicable is success,
// the mount never having written the file, its content
// being already on the remote. Otherwise the
// not-fully-held-locally status takes precedence over the
// upload-not-complete status, the local fact being the
// primitive the feature reports and the upload fact
// staying in the reported output for a caller that needs
// it. The rule has exactly one exception, which keeps the
// upload-not-complete status reachable: on a mount that
// keeps no cache (cacheOff) no file is ever fully held, so
// every file whose local fact is a content value takes the
// upload-not-complete status, while a non-regular file
// keeps the not-fully-held-locally status, its local fact
// asserting nothing about held content (FR-027, FR-032,
// SC-021).
//
// Failures to answer the query at all — a directory, an
// absent entry, a path outside the mount, an unreachable
// or ambiguous mount — are not states and take the query
// failure through the error path rather than through this
// map; the usage error is the command machinery's (FR-027).
func (s FileStatus) ExitCode(cacheOff bool) int {
	if cacheOff {
		// On a mount that keeps no cache the exception
		// covers every content value; only the non-regular
		// file, which asserts nothing about held content,
		// keeps the not-fully-held-locally status.
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
