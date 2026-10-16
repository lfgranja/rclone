# Data Model: Per-file VFS cache status reporting

**Date**: 2026-10-01  
**Feature**: Per-file VFS cache status reporting  
**Spec**: `/home/luis/development/rclone/specs/001-vfs-file-status/spec.md`

## Core Entities

### 1. FileStatus
The primary entity representing the complete state of a file in a mounted remote.

**Fields**:
```go
type FileStatus struct {
    Path     string      `json:"path"`     // The path being queried (always present)
    Local    LocalState  `json:"local"`    // Local cache state
    Upload   UploadState `json:"upload"`   // Upload state
    Bytes    ByteInfo    `json:"bytes"`    // Byte count information
    Error    *StatusError `json:"error,omitempty"` // Error if query failed
}
```

### 2. LocalState
Represents the local cache state of a file.

**Values**:
- `fully_held` - File is completely cached locally
- `partly_held` - File is partially cached (some bytes available)
- `not_held` - File is not cached locally
- `underivable` - Mount holds a cache entry but cannot derive how many bytes are held
- `download_failed` - Download failed (some bytes may still be cached)
- `not_regular_file` - Path is not a regular file (e.g. a link, on a mount started to show links)

**Validation Rules**:
- Only one value can be present at a time
- `download_failed` indicates a download failure while `partly_held` indicates incomplete download
- `not_regular_file` takes precedence for non-regular files
- `underivable` is produced only when a cache entry exists and the held bytes cannot be
  derived from it — never by an incomplete file, a transfer in progress, or an underivable
  total size (which is a byte quantity marker instead)

### 3. UploadState
Represents the upload state of a file.

**Values**:
- `uploaded` - File has been successfully uploaded
- `awaiting_upload` - File is queued for upload but not started
- `uploading` - File is currently being uploaded
- `upload_failed` - Upload failed on the current mount instance (retry policy is internal to the mount, not reported)
- `not_applicable` - This file is not in scope for upload on a mount that does track
  uploads, such as one never written through the mount (content already on remote)
- `not_tracked` - Mount keeps no cache at all, so it has no upload queue

**Validation Rules**:
- Only one value can be present at a time
- `not_applicable`, `not_tracked` and the byte-quantity marker `not_measurable` are three
  distinct values with three distinct meanings and are mutually exclusive with each other
  and with the other states
- `upload_failed` indicates a failure reported by the running mount instance; it is not carried
  across a restart
- `not_tracked` is reported only on a mount that keeps no cache at all, never per file on a
  mount that does keep one

### 4. ByteInfo
Contains byte count information for the file.

Each quantity is a union of a non-negative number and one of two fixed string markers, so a
field can hold a measurement or the statement that there is not one. There is no sentinel
number and no absent field.

**Fields**:
```go
type ByteValue struct {
    Number  int64   // A real measurement, 0 included
    Marker  string  // "" when Number is set; otherwise "not_determinable" or "not_measurable"
}

type ByteInfo struct {
    UnbrokenRun ByteValue `json:"unbroken_run"` // Unbroken run of held bytes from offset 0
    TotalHeld   ByteValue `json:"total_held"`   // Total bytes held anywhere in the file
    TotalSize   ByteValue `json:"total_size"`   // Total file size
}
```

Serialised, each quantity is a JSON number or a JSON string:

```json
{ "unbroken_run": 0, "total_held": 0, "total_size": 1024000 }
{ "unbroken_run": "not_determinable", "total_held": "not_determinable", "total_size": "not_determinable" }
{ "unbroken_run": "not_measurable", "total_held": "not_measurable", "total_size": "not_measurable" }
```

**Markers**:
- `not_determinable` - The mount cannot derive this quantity. Carried by `total_size` where the
  file's size is not derivable from what the mount holds, and by `unbroken_run` and `total_held` where
  the local fact is itself `underivable`.
- `not_measurable` - The entry asked about is not a regular file, so the quantity has
  nothing to measure. Carried by all three in that one case.

The two markers live in a namespace disjoint from the one the values of `LocalState` and
`UploadState` live in: a marker appears only in a byte quantity, a fact's value only as that
fact's value. This is why the marker is spelled `not_measurable` and not `not_applicable` —
`not_applicable` is `UploadState`'s own value and means something unrelated, and it never
appears in a byte quantity.

A quantity the mount can state is that number, 0 included: 0 held bytes is a measurement of
a mount holding none of the file, and a zero-byte file whose cache entry is held is
`fully_held`, not incomplete.

**Validation Rules**:
- For `fully_held`: `unbroken_run` == `total_size` == `total_held`, unless `total_size` is `not_determinable`, when `unbroken_run` == `total_held`
- For `not_held`: `unbroken_run` == 0, `total_held` == 0, `total_size` is the file size or `not_determinable`
- For `partly_held`: `unbroken_run` > 0, `unbroken_run` < `total_size`, `total_held` > 0, `total_held` >= `unbroken_run`, `total_held` <= `total_size`, unless `total_size` is `not_determinable`
- For `download_failed`: `unbroken_run` and `total_held` reflect the bytes actually cached, `total_size` is the file size or `not_determinable` where the size is not derivable, and 0 <= `unbroken_run` <= `total_held`
- For `underivable`: `unbroken_run` and `total_held` both carry the `not_determinable` marker
- For `not_regular_file`: all three carry the `not_measurable` marker
- No quantity is ever omitted, so a consumer reads one object shape for every state

### 5. StatusError
Represents errors that can occur during status queries.

**Fields**:
```go
type StatusError struct {
    Code    string `json:"code"`    // Stable machine-readable error code
    Message string `json:"message"` // Human-readable description, not part of the contract
}
```

**Error Codes** - the closed set published by spec `FR-006`:

| Code | Meaning |
|------|---------|
| `no_active_mount` | The request named a mount, or reached one, that has none running |
| `outside_any_mount` | The path lies outside the mount the call acts on |
| `ambiguous_mount` | The request names at least one path but more than one, and no mount |
| `directory_not_file` | The path names a directory |
| `empty_path` | The request carried an empty or absent `paths` array; decided before any mount is consulted |
| `param_invalid` | The request body is malformed or carries an invalid parameter value — whole-request, not a per-path result |
| `not_found` | The path names no entry the mount can see |
| `unaccountable_path` | The mount holds no current listing of the path's parent, so it cannot say anything about it |
| `mount_unreachable` | The control interface could not be reached at all; produced by the command, since no request arrives to be answered |

Codes deliberately absent:
- `unreadable_path` - the mount cannot observe the caller's permissions (spec `FR-012`), so
  such a path is answered on its ordinary terms
- `query_failed` - a catch-all naming nothing would not be a code a caller can act on
  differently; an authentication failure is a control-interface outcome reported as the
  query-failed exit status, not as a per-path result (a malformed body or an invalid
  parameter value carries `param_invalid` above; an unknown parameter is ignored and is
  no failure at all)
- `param_not_found` - unknown parameters are ignored as read-only siblings (vfs/list, vfs/stats,
  vfs/queue) ignore them; mutating siblings (vfs/refresh, vfs/forget, vfs/poll-interval) reject unknown keys, this endpoint is read-only, so there is deliberately no code for one
- `not_regular_file` - a value of the local fact, not an error code
- `invalid_request` - a whole-request outcome, not a per-path code

A later release may add a code; consumers must tolerate one they do not recognise.

**Decision order** — a request matching more than one condition has one answer, fixed by
spec `FR-006`:

1. **Request shape.** Absent or empty `paths` is `empty_path`, before any mount is consulted
   and before any identifier is looked at. Reported once for the request, not once per path,
   since there are no paths.
2. **Identifier resolution.** An identifier matching no running mount is `no_active_mount`,
   whatever the paths turn out to be. No identifier with more than one path is
   `ambiguous_mount`. No identifier with exactly one path resolves that path against the
   registered mount points.
3. **Per path**, within the one mount the call resolved to: resolve the path (links
   followed) — `outside_any_mount`; establish what the path names from entries the mount
   currently holds — `unaccountable_path` or `not_found`; then classify what it is —
   `directory_not_file`, the non-regular-file value of the local fact, or the file's two
   facts.

A request rejected at level 1 or 2 produces no per-path results at all.

### 6. Path
Represents a file path within a mount context.

**Properties**:
- Resolved against mount point
- Symlinks in parent directories are resolved to identify mount
- Final component determines file type (regular file vs other)
- Must be absolute path within mount host filesystem

### 7. Mount
Represents a mounted remote context.

**Properties**:
- Identified by the name the report of running mounts gives it: the remote config name where
  one mount serves that name, and that name carrying the index suffix the report gives it
  where several do. Taken in the same parameter every sibling mount-targeted operation takes
  its mount in.
- Accepting the indexed form is new work, confined to this endpoint; the sibling operations
  continue to resolve by the bare config name and to report a shared name as ambiguous, so a
  caller can tell two such mounts apart through this feature and not through those
- Registered with mount point for path resolution, which is also what tells two mounts
  sharing one config name apart on a single-path request
- Maintains cache state and upload queue
- Supports various cache configurations (enabled/disabled, etc.)

### 8. ControlInterfaceConnection
The transport by which a caller reaches a mount, per spec `FR-034`.

**Properties**:
- HTTP connection to the mount's control server
- Address from `--rc-addr`, default `localhost:5572`; a `unix:///path/to/socket` address is
  reachable with no further flags
- Credentials, where the server requires any, are HTTP basic from `--rc-user` and
  `--rc-pass` — the same values `rclone rc` reads
- `vfs/file-status` is registered `NoAuth`, like `vfs/list`, `vfs/stats` and `vfs/queue`
- HTTP POST with a JSON body
- Established per request; no state about a contacted server is kept
- A connection that cannot be established is the `mount_unreachable` outcome

### 9. MountPointEntry
The mount point reported beside each entry of the report of running mounts.

**Properties**:
- Carries the path a mount is reachable at, or null where the mount has none
- Reported in a key of its own so that everything the report already returned is returned
  unchanged, per spec `FR-014`; that key is keyed by the names the report already returns, so
  a caller pairs a mount point with a mount by name and never by position
- Null for a VFS created without a mount point; such a VFS has no host
  interior, so no single-path request names a path inside it
- Two mounts sharing one config name carry separate mount points

## Relationships

### FileStatus → Mount
- Each FileStatus is associated with exactly one Mount
- Mount provides context for interpreting the status
- Mount configuration affects available states (e.g., `not_tracked` when caching disabled)

### FileStatus → Path  
- Each FileStatus describes exactly one Path
- Path is always included in the response for correlation
- Path resolution determines which Mount handles the query

### LocalState ↔ UploadState
- Two independent aspects of the same file's state
- Both are captured simultaneously from a single observation
- Affect exit status independently (LocalState takes precedence for exit code)

### ByteInfo → LocalState
- Byte counts are interpreted based on LocalState
- Different states have different valid ranges for byte values
- `not_regular_file` state has no meaningful byte counts, so all three carry
  `not_measurable`
- `underivable` state leaves the two held quantities carrying the `not_determinable` marker, since
  those are the bytes that could not be derived

## State Transitions

### Local State Transitions
- `not_held` → `partly_held` (partial download)
- `partly_held` → `fully_held` (download completes)
- `partly_held` → `download_failed` (download fails with partial progress)
- `fully_held` → `not_held` (cache eviction)
- `download_failed` → `partly_held` or `fully_held` (retry succeeds)

### Upload State Transitions

Transitions are trigger → guard → effect. A transition whose guard does not
hold does not occur; the reported value is the state the guard names instead.
The table is the authoritative statement of the value boundaries; the value
definitions in **UploadState** above carry the meaning of each state.

| From | Event | Guard (must hold) | To |
|------|-------|-------------------|----|
| `awaiting_upload` | upload starts | mount keeps a cache and an upload queue; file was written through the mount | `uploading` |
| `uploading` | upload completes | transfer finished successfully | `uploaded` |
| `uploading` | upload fails | transfer attempt failed | `upload_failed` |
| `upload_failed` | retry scheduled | mount still running; failure is in-memory state of this instance | `awaiting_upload` |
| any per-file state | mount started with no cache | `mount.cacheEnabled == false` — a mount-level guard, never per-file | `not_tracked` |
| `awaiting_upload` | file provenance is remote | `mount.cacheEnabled == true` AND the file was never written through this mount (content already on the remote) | `not_applicable` |

Rules the table fixes:

- **Retry policy is not part of the reported state.** `upload_failed` means
  exactly "the last attempt on this mount instance failed". Whether, when, or
  how often the mount retries is internal to the mount and is not reported,
  not bounded, and not part of the contract. There is no permanent-failure
  value: a file whose retries never succeed keeps reporting `upload_failed`
  for as long as the mount runs, and the failure is not carried across a
  mount restart.
- `not_applicable` and `not_tracked` are mount-level guards, mutually
  exclusive with every per-file value and with each other: a mount that keeps
  no cache reports `not_tracked` for every file and never reports a per-file
  upload value; a cache-keeping mount never reports `not_tracked`; a
  cache-keeping mount reports `not_applicable` only for files it never saw
  written through it.
- Byte quantities are reported on every transition outcome per `FR-002`; the
  marker rules for `not_regular_file` and `underivable` apply unchanged.
- Both facts in a pair describe one observation instant per `FR-022`: the
  local fact and the upload fact are read under the same lock, so a pair
  never combines a pre-transition local fact with a post-transition upload
  fact.

## Error Handling

### Error Precedence
Follows the order `FR-006` fixes, from the outermost failure inwards:

1. Transport failures — no server reached at all (`mount_unreachable`, reported by the
   command rather than by any result)
2. Request shape — an absent or empty `paths` array (`empty_path`). Whole-request, not
   per-path, and decided before any mount is consulted, so it is never ambiguous and never
   no-active-mount
3. Whole-request failures — malformed body or invalid parameter value (`param_invalid`),
   authentication failure. Not per-path results (an unknown parameter is ignored and is no
   failure at all)
4. Identifier resolution (`no_active_mount`, `ambiguous_mount`), before any path is examined
5. Per-path containment (`outside_any_mount`), before existence is established
6. Per-path existence (`unaccountable_path`, `not_found`), before the entry is classified
7. Per-path classification (`directory_not_file`, the non-regular-file value of the local
   fact) and finally the file's two facts
8. Mount reported but with no state — `not_held`, `not_tracked`, `not_applicable`

### Error Recovery
- Most errors are permanent for the given query
- Mount restart may resolve some transient errors
- Client should retry on transient errors with exponential backoff

## Validation Rules

### Consistency Constraints
- LocalState and UploadState must come from the same instant
- Byte counts must match the declared LocalState
- Error states are mutually exclusive with successful states

### Business Rules
- A file can be both not fully cached and not yet uploaded
- Failed transfers are reported as their own state
- Empty files (0 bytes) are reported as fully held when cached
- Non-regular files carry `not_measurable` on every byte quantity rather than a number;
  `not_applicable` is the upload fact's value and never appears in a byte quantity
- On a mount that keeps no cache, every file carries `not_tracked` and no file can exit
  successfully, so every file whose local fact is a content value takes the
  upload-not-complete exit status (12) there; a non-regular file keeps the
  not-fully-held-locally status (11) per FR-027/FR-032/SC-021

### Performance Constraints
- State read must complete within 100ms
- No network access during state queries
- No blocking of ongoing transfers
- No blocking on a cache reset of the file being read: the record is read the way the mount's
  own per-file reads of it are read, and a record a reset is mid-rewrite on is reported as
  `underivable` rather than as the momentary value caught
- Bounded by local disk work on that file and by the cache's index lock, which entry creation
  holds across local metadata I/O — never by a transfer or a reset

## Data Flow

### Query Processing
1. Establish connection to the control server — failure is `mount_unreachable`
2. Path resolution → Mount identification
3. Mount state access → Cache and upload queue inspection
4. State calculation → Local and upload state determination
5. Byte count calculation → Held and total bytes computation, or the marker for a quantity
   that cannot be stated
6. Error handling → Appropriate error code assignment
7. Response construction → JSON serialization

### Mount Integration
- Queries are read-only operations
- No modification of mount state or cache: reaching a file's record brings its entry into the
  mount's in-memory index where it is not already there, which is what the mount's own
  existing per-file reads do, and writes nothing to the record or to cached content
- No impact on ongoing transfers
- Minimal locking for state access
- Connection state is not retained between requests

## Serialization Format

### JSON Structure
```json
{
  "path": "/path/to/file",
  "local": "fully_held",
  "upload": "uploaded",
  "bytes": {
    "unbroken_run": 1024000,
    "total_held": 1024000,
    "total_size": 1024000
  }
}
```

A quantity the mount cannot state, and one with nothing to measure, are JSON strings rather
than JSON numbers:

```json
{
  "path": "/path/to/file",
  "local": "partly_held",
  "upload": "awaiting_upload",
  "bytes": {
    "unbroken_run": 512000,
    "total_held": 512000,
    "total_size": "not_determinable"
  }
}
```

```json
{
  "path": "/path/to/link",
  "local": "not_regular_file",
  "upload": "not_applicable",
  "bytes": {
    "unbroken_run": "not_measurable",
    "total_held": "not_measurable",
    "total_size": "not_measurable"
  }
}
```

The last example carries both near-identical strings in one object, which is exactly why the
two namespaces are disjoint: `"upload"` holds the upload fact's value, meaning this file is
not in scope for upload on a mount that does track uploads, while the three byte quantities
hold the marker, meaning they have nothing to measure because the entry is not a regular
file. Neither string ever appears in the other's fields.

### Error Structure
```json
{
  "path": "/path/to/file",
  "error": {
    "code": "not_found",
    "message": "File does not exist in mount"
  }
}
```

### Multi-path Response
A JSON object with a single `statuses` key carrying the array of individual
FileStatus objects, each with its own path field for correlation.

## Security Considerations

### Access Control
- Status queries follow existing mount access rules
- `vfs/file-status` is registered `NoAuth`, like `vfs/list`, `vfs/stats` and `vfs/queue`, so
  it answers on a server started with authentication enabled to any caller who may reach the
  server
- Where the server does require authentication, it is HTTP basic from `--rc-user` and
  `--rc-pass`
- Read-only access to mount state
- No sensitive information exposure

### Data Privacy
- Only exposes cache and transfer metadata
- No file content exposure
- No user credential information

## Extensibility

### Future State Values
- LocalState and UploadState designed to accommodate new values
- ByteInfo structure supports additional metrics
- Error codes namespace for future expansion

### Interface Stability
- JSON format designed to be forward-compatible
- New fields added without breaking existing consumers
- Error codes remain stable once defined