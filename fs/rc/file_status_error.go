package rc

import "fmt"

// FileStatusError is a whole-request rejection of the vfs/file-status endpoint.
type FileStatusError struct {
	// Code is the error code of the rejection, e.g. "empty_path".
	Code string
	// Message is the human-readable description of the rejection.
	Message string
	// Status is the HTTP status the endpoint answers with.
	Status int
}

// NewFileStatusError returns a FileStatusError with the given code, message and status.
func NewFileStatusError(code, message string, status int) *FileStatusError {
	return &FileStatusError{
		Code:    code,
		Message: message,
		Status:  status,
	}
}

// Error returns the error code and message.
func (e *FileStatusError) Error() string {
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// Render returns the endpoint's error object for the rejection.
func (e *FileStatusError) Render() Params {
	return Params{
		"error": Params{
			"code":    e.Code,
			"message": e.Message,
		},
	}
}
