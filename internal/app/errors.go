// Package app defines the use-case interface and the DTOs shared by every
// entry point (HTTP API, CLI, MCP, desktop). The JSON shapes declared here are
// the v0 contract described in docs/v0.md §7.7 and §13; no other package may
// define wire types.
package app

import (
	"errors"
	"fmt"
	"net/http"
)

// Code is a stable machine-readable error code (docs/v0.md §7.7, contract).
type Code string

const (
	CodeInvalidArgument       Code = "INVALID_ARGUMENT"
	CodeNotFound              Code = "NOT_FOUND"
	CodeConflict              Code = "CONFLICT"
	CodePathNotAllowed        Code = "PATH_NOT_ALLOWED"
	CodeUnsupportedSource     Code = "UNSUPPORTED_SOURCE"
	CodeSourceUnavailable     Code = "SOURCE_UNAVAILABLE"
	CodeSourceBlocked         Code = "SOURCE_BLOCKED"
	CodeNetworkError          Code = "NETWORK_ERROR"
	CodeToolMissing           Code = "TOOL_MISSING"
	CodeToolFailed            Code = "TOOL_FAILED"
	CodeChecksumMismatch      Code = "CHECKSUM_MISMATCH"
	CodeMediaInvalid          Code = "MEDIA_INVALID"
	CodeNoAudioStream         Code = "NO_AUDIO_STREAM"
	CodeASRFailed             Code = "ASR_FAILED"
	CodeModelNotConfigured    Code = "MODEL_NOT_CONFIGURED"
	CodeModelAuthFailed       Code = "MODEL_AUTH_FAILED"
	CodeModelNotFound         Code = "MODEL_NOT_FOUND"
	CodeModelRateLimited      Code = "MODEL_RATE_LIMITED"
	CodeModelUnavailable      Code = "MODEL_UNAVAILABLE"
	CodeModelOutputInvalid    Code = "MODEL_OUTPUT_INVALID"
	CodeCitationInvalid       Code = "CITATION_INVALID"
	CodeDiskFull              Code = "DISK_FULL"
	CodeEngineUnavailable     Code = "ENGINE_UNAVAILABLE"
	CodeEngineVersionMismatch Code = "ENGINE_VERSION_MISMATCH"
	CodeInternal              Code = "INTERNAL"

	// CodeUnauthorized is a transport-level code used only by the HTTP layer
	// for a missing or wrong token; it never appears on a job.
	CodeUnauthorized Code = "UNAUTHORIZED"
)

type codeInfo struct {
	retryable  bool
	userAction bool
	httpStatus int
	exitCode   int
}

// codeTable mirrors the tables in docs/v0.md §7.7 (flags) and §13.4 (exit codes).
var codeTable = map[Code]codeInfo{
	CodeInvalidArgument:       {false, true, http.StatusBadRequest, 2},
	CodeNotFound:              {false, true, http.StatusNotFound, 3},
	CodeConflict:              {false, true, http.StatusConflict, 4},
	CodePathNotAllowed:        {false, true, http.StatusForbidden, 7},
	CodeUnsupportedSource:     {false, true, http.StatusBadRequest, 2},
	CodeSourceUnavailable:     {false, true, http.StatusBadGateway, 10},
	CodeSourceBlocked:         {true, true, http.StatusBadGateway, 10},
	CodeNetworkError:          {true, false, http.StatusBadGateway, 10},
	CodeToolMissing:           {true, true, http.StatusServiceUnavailable, 6},
	CodeToolFailed:            {true, false, http.StatusInternalServerError, 1},
	CodeChecksumMismatch:      {true, false, http.StatusBadGateway, 6},
	CodeMediaInvalid:          {false, true, http.StatusUnprocessableEntity, 1},
	CodeNoAudioStream:         {false, true, http.StatusUnprocessableEntity, 1},
	CodeASRFailed:             {true, false, http.StatusInternalServerError, 1},
	CodeModelNotConfigured:    {true, true, http.StatusPreconditionFailed, 7},
	CodeModelAuthFailed:       {true, true, http.StatusBadGateway, 7},
	CodeModelNotFound:         {true, true, http.StatusBadGateway, 1},
	CodeModelRateLimited:      {true, false, http.StatusBadGateway, 1},
	CodeModelUnavailable:      {true, false, http.StatusBadGateway, 1},
	CodeModelOutputInvalid:    {true, false, http.StatusBadGateway, 1},
	CodeCitationInvalid:       {true, false, http.StatusBadGateway, 1},
	CodeDiskFull:              {true, true, http.StatusInsufficientStorage, 1},
	CodeEngineUnavailable:     {true, false, http.StatusServiceUnavailable, 5},
	CodeEngineVersionMismatch: {true, true, http.StatusConflict, 5},
	CodeInternal:              {true, false, http.StatusInternalServerError, 1},
	CodeUnauthorized:          {false, true, http.StatusUnauthorized, 7},
}

// Error is the error object returned by every entry point (contract).
type Error struct {
	Code               Code           `json:"code"`
	Message            string         `json:"message"`
	Stage              string         `json:"stage,omitempty"`
	Retryable          bool           `json:"retryable"`
	UserActionRequired bool           `json:"user_action_required"`
	Hint               string         `json:"hint,omitempty"`
	Details            map[string]any `json:"details,omitempty"`

	cause error
}

// ErrorEnvelope is the JSON body of a failed request: {"error": {...}}.
type ErrorEnvelope struct {
	Error *Error `json:"error"`
}

func (e *Error) Error() string {
	if e.Stage != "" {
		return fmt.Sprintf("%s (%s): %s", e.Code, e.Stage, e.Message)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func (e *Error) Unwrap() error { return e.cause }

// E builds an Error with the retryable / user_action_required flags taken
// from the contract table.
func E(code Code, format string, args ...any) *Error {
	info, ok := codeTable[code]
	if !ok {
		info = codeTable[CodeInternal]
	}
	msg := format
	if len(args) > 0 {
		msg = fmt.Sprintf(format, args...)
	}
	return &Error{Code: code, Message: msg, Retryable: info.retryable, UserActionRequired: info.userAction}
}

// WithHint returns e with a user-facing hint.
func (e *Error) WithHint(hint string) *Error { e.Hint = hint; return e }

// WithStage returns e tagged with the pipeline stage it happened in.
func (e *Error) WithStage(stage string) *Error { e.Stage = stage; return e }

// WithDetail returns e with one extra details entry.
func (e *Error) WithDetail(key string, value any) *Error {
	if e.Details == nil {
		e.Details = map[string]any{}
	}
	e.Details[key] = value
	return e
}

// WithCause attaches an underlying error for errors.Is / errors.As; it is not
// serialized.
func (e *Error) WithCause(err error) *Error { e.cause = err; return e }

// AsError converts any error into an *Error; unknown errors become INTERNAL.
// It returns nil for a nil error.
func AsError(err error) *Error {
	if err == nil {
		return nil
	}
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return E(CodeInternal, "%s", err.Error()).WithCause(err)
}

// CodeOf returns the code of err, or INTERNAL for a non-app error.
func CodeOf(err error) Code {
	if e := AsError(err); e != nil {
		return e.Code
	}
	return ""
}

// IsCode reports whether err is an *Error with the given code.
func IsCode(err error, code Code) bool {
	var e *Error
	return errors.As(err, &e) && e.Code == code
}

// HTTPStatus maps an error code to the HTTP status used by the API.
func HTTPStatus(code Code) int {
	if info, ok := codeTable[code]; ok {
		return info.httpStatus
	}
	return http.StatusInternalServerError
}

// CLI exit codes (docs/v0.md §13.4, contract).
const (
	ExitOK           = 0
	ExitInternal     = 1
	ExitUsage        = 2
	ExitNotFound     = 3
	ExitConflict     = 4
	ExitEngine       = 5
	ExitToolNotReady = 6
	ExitConfig       = 7
	ExitJobFailed    = 8
	ExitJobCancelled = 9
	ExitNetwork      = 10
	ExitWaitTimeout  = 11
)

// ExitCode maps an error code to the CLI exit code.
func ExitCode(code Code) int {
	if info, ok := codeTable[code]; ok {
		return info.exitCode
	}
	return ExitInternal
}

// KnownCodes lists every job-level error code, in contract order.
func KnownCodes() []Code {
	return []Code{
		CodeInvalidArgument, CodeNotFound, CodeConflict, CodePathNotAllowed,
		CodeUnsupportedSource, CodeSourceUnavailable, CodeSourceBlocked,
		CodeNetworkError, CodeToolMissing, CodeToolFailed, CodeChecksumMismatch,
		CodeMediaInvalid, CodeNoAudioStream, CodeASRFailed, CodeModelNotConfigured,
		CodeModelAuthFailed, CodeModelNotFound, CodeModelRateLimited,
		CodeModelUnavailable, CodeModelOutputInvalid, CodeCitationInvalid,
		CodeDiskFull, CodeEngineUnavailable, CodeEngineVersionMismatch, CodeInternal,
	}
}
