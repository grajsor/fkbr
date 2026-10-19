package remote

import (
	"net/http"

	"fkbr/internal/api"
)

var statusForCode = map[string]int{
	"unknown_node": http.StatusNotFound,
	"node_offline": http.StatusServiceUnavailable,
	"unknown_root": http.StatusNotFound,
	"bad_path":     http.StatusBadRequest,
	"unknown_job":  http.StatusNotFound,
	"read_only":    http.StatusForbidden,
	"not_allowed":  http.StatusForbidden,
}

type wireError struct {
	Error string `json:"error"`
	Code  string `json:"code,omitempty"`
}

func encodeError(err error) (wireError, int) {
	code := api.ErrorCode(err)
	status, ok := statusForCode[code]
	if !ok {
		status = http.StatusInternalServerError
	}
	return wireError{err.Error(), code}, status
}

func decodeError(w wireError) error {
	return &api.CodedError{Msg: w.Error, Code: w.Code}
}
