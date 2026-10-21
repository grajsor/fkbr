package api

import "errors"

// errorCodes gives sentinel errors stable codes so they survive being sent
// between nodes and to clients; receivers rebuild an error that still
// matches with errors.Is.
var errorCodes = []struct {
	err  error
	code string
}{
	{ErrUnknownNode, "unknown_node"},
	{ErrNodeOffline, "node_offline"},
	{ErrUnknownRoot, "unknown_root"},
	{ErrBadPath, "bad_path"},
	{ErrUnknownJob, "unknown_job"},
	{ErrReadOnly, "read_only"},
	{ErrNotAllowed, "not_allowed"},
	{ErrNotFound, "not_found"},
}

// ErrorCode returns the code of the sentinel err wraps, or "".
func ErrorCode(err error) string {
	for _, c := range errorCodes {
		if errors.Is(err, c.err) {
			return c.code
		}
	}
	return ""
}

// ErrorForCode returns the sentinel for a code, or nil.
func ErrorForCode(code string) error {
	for _, c := range errorCodes {
		if c.code == code {
			return c.err
		}
	}
	return nil
}

// CodedError is an error received from elsewhere: its message as sent,
// unwrapping to the sentinel its code names.
type CodedError struct {
	Msg  string
	Code string
}

func (e *CodedError) Error() string { return e.Msg }
func (e *CodedError) Unwrap() error { return ErrorForCode(e.Code) }
