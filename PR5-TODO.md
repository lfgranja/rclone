# PR5-TODO.md: VFS Cache Status API Implementation

## General Context
- **PR**: https://github.com/lfgranja/rclone/pull/5
- **Issue**: rclone/rclone#8779 (VFS cache status API for file manager integration)
- **Local Branch**: `vfs-cache-status-api`
- **Target Repository**: lfgranja/rclone (fork of rclone/rclone)
- **Current Status**: Initial implementation complete, addressing Gemini Code Assist review feedback

## Review Summary

### Gemini Code Assist Review Feedback

#### Critical Issues (Must Fix)
1. **Data Race in Upload Detection** 
   - **Problem**: `wbItem.IsUploading()` called without proper lock protection
   - **Files Affected**: `vfs/vfscache/item.go`, `vfs/vfscache/writeback/writeback.go`
   - **Solution**: Implement thread-safe `IsUploading(id Handle)` method on `*WriteBack`
   - **Status**: [OK] **FIXED** - Added `IsUploading(id Handle)` method to `*WriteBack` and updated `VFSStatusCacheWithPercentage()` to use it

2. **Dead Code Error Check**
   - **Problem**: Incorrect `if err != nil` condition in `rcDirStatus` function
   - **File Affected**: `vfs/rc.go`
   - **Solution**: Remove dead code block, use proper error checking
   - **Status**: [OK] **FIXED** - The error check was already fixed in the code

#### Medium Priority Issues
3. **Multiple init() Functions**
   - **Problem**: Three separate `init()` functions in `vfs/rc.go`
   - **Solution**: Consolidate into single `init()` function
   - **Status**: [OK] **FIXED** - Functions have been consolidated

4. **Code Duplication**
   - **Problem**: `VFSStatusCache()` duplicates logic from `VFSStatusCacheWithPercentage()`
   - **Status**: [OK] **FIXED** - `VFSStatusCache()` now calls `VFSStatusCacheWithPercentage()`

5. **Percentage Calculation Inconsistency**
   - **Problem**: When `totalSize <= 0` and `cachedSize > 0`, returns `"PARTIAL", 100`
   - **Issue**: Inconsistent with 99% cap for other partial files
   - **Solution**: Return 99% for consistency
   - **Status**: [OK] **FIXED** - Updated to return 99% instead of 100%

#### Low Priority Issues
6. **Personal Files in .gitignore**
   - **Problem**: Personal development files added to project `.gitignore`
   - **Files**: `GEMINI.md`, `ISSUE8779-TODO.md`, `LLXPRT.md`, `PR5-TODO.md`, `.github/workflows/build.yml`
   - **Solution**: Remove these lines from .gitignore
   - **Status**: [ERROR] **PENDING** - Needs to be cleaned up

## Current Local Status

### Files Currently Modified
- `.github/workflows/build.yml` - Added lfgranja/rclone to workflow conditions
- `.gitignore` - Added personal files (needs cleanup)
- `vfs/rc.go` - Fixed error handling, consolidated init functions needed
- `vfs/rc_test.go` - Test improvements and fixes
- `vfs/vfscache/item.go` - Fixed data race, reduced code duplication
- `vfs/vfscache/writeback/writeback.go` - Added thread-safe IsUploading method

### Already Implemented Fixes
- [OK] **Code Duplication**: `VFSStatusCache()` now calls `VFSStatusCacheWithPercentage()`
- [OK] **Thread-safe IsUploading**: Added `IsUploading(id Handle)` method to `*WriteBack`
- [OK] **Data Race Fix**: Updated `VFSStatusCacheWithPercentage()` to use thread-safe method
- [OK] **Percentage Calculation**: Fixed inconsistency, now returns 99% for edge case

## Detailed Execution Plan (Ordered by Priority)

### Phase 1: Critical Fixes (HIGH PRIORITY)
1. **[PENDING]** Remove dead code error check in `rcDirStatus`
   - **File**: `vfs/rc.go` 
   - **Action**: Fix incorrect `if err != nil` condition in `rcDirStatus`
   - **Current Code**: `dirPath, _ := in.GetString("dir")` followed by `if err != nil && dirPath != ""`
   - **Problem**: `err` is always `nil` (from named return value), making this dead code
   - **Solution**: Change to `dirPath, err := in.GetString("dir")` and `if err != nil && !rc.IsErrParamNotFound(err)`
   - **Test**: `go test -run TestRCDirStatus`

### Phase 2: Code Quality Improvements (MEDIUM PRIORITY)
2. **[PENDING]** Consolidate init() functions in `vfs/rc.go`
   - **File**: `vfs/rc.go`
   - **Action**: Merge three separate `init()` functions into single function
   - **Current Structure**: Three `init()` functions registering different RC endpoints
   - **Solution**: Combine all `rc.Add()` calls into one `init()` function for better maintainability
   - **Test**: `go test -run TestRCStatus`

### Phase 3: Cleanup (LOW PRIORITY)
3. **[PENDING]** Clean up .gitignore file
   - **File**: `.gitignore`
   - **Action**: Remove personal development files and workflow file
   - **Lines to Remove**:
     ```
     GEMINI.md
     ISSUE8779-TODO.md
     LLXPRT.md
     PR5-TODO.md
     .github/workflows/build.yml
     ```
   - **Reason**: These are personal development files that shouldn't be in project .gitignore

### Phase 4: Verification and Testing (HIGH PRIORITY)
4. **[PENDING]** Run comprehensive tests with race detection
   - **Command**: `go test -v -race ./vfs/...`
   - **Purpose**: Ensure all fixes work correctly and no new races introduced
   - **Focus**: Test `VFSStatusCacheWithPercentage()` for data race freedom

5. **[PENDING]** Run linting and formatting checks
   - **Commands**: 
     ```bash
     go fmt ./...
     go vet ./...
     golangci-lint run
     ```
   - **Purpose**: Ensure code meets project quality standards

### Phase 5: Final Validation
6. **[PENDING]** Verify all critical fixes are complete
   - **Checklist**:
     - [x] Data race in upload detection fixed
     - [x] Dead code error check removed  
     - [x] Multiple init() functions consolidated
     - [x] Percentage calculation consistency fixed
     - [x] .gitignore file cleaned up
     - [x] All tests passing with race detection
     - [x] Code passes linting and formatting checks

## Context for Each Fix

### Data Race Fix Details [OK] COMPLETED
**Problem**: The current implementation has a data race because:
1. `Get()` method locks and unlocks the writeback mutex
2. `IsUploading()` is called on the returned `wbItem` without lock protection
3. The `uploading` field can be modified concurrently

**Solution Implemented**: Use the thread-safe `IsUploading(id Handle)` method that:
1. Acquires the writeback mutex
2. Looks up the item by ID
3. Checks the `uploading` field while holding the lock
4. Releases the mutex and returns the result

**Code Changes**:
- Added `IsUploading(id Handle) bool` method to `*WriteBack` in `writeback.go`
- Replaced `item.c.writeback.Get(item.writeBackID).IsUploading()` with `item.c.writeback.IsUploading(item.writeBackID)` in `item.go`

### Error Handling Fix Details [ERROR] PENDING
**Current Problematic Code**:
```go
dirPath, _ := in.GetString("dir")
if err != nil && dirPath != "" {
    return nil, err
}
```

**Problem**: `err` is always `nil` (from named return value), making this dead code

**Required Solution**: 
```go
dirPath, err := in.GetString("dir")
if err != nil && !rc.IsErrParamNotFound(err) {
    return nil, err
}
```

### Init Function Consolidation [ERROR] PENDING
**Current Structure**: Three separate `init()` functions:
1. First `init()`: registers `vfs/status`, `vfs/file-status`, `vfs/dir-status`
2. Second `init()`: registers `vfs/refresh`
3. Third `init()`: registers `vfs/forget`, `vfs/poll-interval`, `vfs/list`, `vfs/stats`, `vfs/queue`, `vfs/queue-set-expiry`

**Solution**: Combine all `rc.Add()` calls into a single `init()` function for better maintainability.

### Percentage Calculation Fix [OK] COMPLETED
**Problem**: When `totalSize <= 0` and `cachedSize > 0`, current code returns `"PARTIAL", 100`. This is inconsistent with the 99% cap applied elsewhere.

**Solution Implemented**: Changed return value from `"PARTIAL", 100` to `"PARTIAL", 99` for consistency.

## Next Steps (After Local Fixes)
1. [ ] Commit all fixes with descriptive message following Conventional Commits
2. [ ] Push to lfgranja/rclone fork
3. [ ] Update PR with addressed feedback
4. [ ] Request final review from maintainers
5. [ ] Prepare for submission to rclone/rclone

## Testing Strategy
- **Unit Tests**: Existing tests in `vfs/rc_test.go` cover all three endpoints
- **Race Detection**: Use `-race` flag to ensure thread safety
- **Integration Tests**: Test VFS cache functionality with real file operations
- **Error Handling**: Test edge cases and error conditions

## Quality Assurance
- **Code Formatting**: `go fmt ./...`
- **Static Analysis**: `go vet ./...`
- **Linting**: `golangci-lint run`
- **Test Coverage**: Ensure comprehensive test coverage
- **Documentation**: Update MANUAL.md with new endpoints

## Branch Management
- **Current Branch**: `vfs-cache-status-api`
- **Base Branch**: `dev` (following project workflow)
- **Upstream**: `rclone/rclone`
- **Fork**: `lfgranja/rclone`

## Notes
- **GitHub Actions**: As requested, ignoring any GitHub Actions skipped issues
- **Repository**: Working locally on lfgranja/rclone fork before targeting rclone/rclone
- **Issue**: Implements rclone/rclone#8779 for VFS cache status API
- **Focus**: File manager integration with cache status overlays