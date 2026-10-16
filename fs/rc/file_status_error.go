package rc

import "fmt"

// FileStatusError is the typed error the vfs/file-status endpoint
// returns for a rejection belonging to the whole request rather
// than to one path, such as empty_path, no_active_mount,
// ambiguous_mount or an invalid parameter value.
//
// It carries the error code, the human-readable message and the
// HTTP status the endpoint answers with, so the rc server's
// error writer can render it as the error object of the
// endpoint's contract instead of mapping it through the rc
// server error mapping, which would answer 500 and a plain
// string error.
type FileStatusError struct {
	// Code is the error code of the rejection, e.g. "empty_path".
	Code string
	// Message is the human-readable description of the rejection.
	Message string
	// Status is the HTTP status the endpoint answers with.
	Status int
}

// NewFileStatusError returns a FileStatusError carrying the given
// code, message and HTTP status.
func NewFileStatusError(code, message string, status int) *FileStatusError {
	return &FileStatusError{
		Code:    code,
		Message: message,
		Status:  status,
	}
}

// Error implements the error interface, reporting the code and
// the message.
func (e *FileStatusError) Error() string {
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// Render returns the response body the endpoint answers with for
// the rejection: a top-level object carrying the error object of
// the same shape a per-path error carries, with no path field.
// The rc server's error writer (writeFileStatusError, called from
// handlePost before its trailing write) renders this body with
// e.Status rather than the rc server error mapping status
// (contracts/api.md "Whole-Request Error Response").
func (e *FileStatusError) Render() Params {
	return Params{
		"error": Params{
			"code":    e.Code,
			"message": e.Message,
		},
	}
}
