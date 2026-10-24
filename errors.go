// SPDX-License-Identifier: Apache-2.0 OR MIT

package latai

import (
	"encoding/json"
	"errors"
	"fmt"
)

// Error is a latai wire error: `{"code","message","hint","details"?}`. A run that failed also
// carries its whole Response.
type Error struct {
	Code    string          `json:"code"`
	Message string          `json:"message"`
	Hint    string          `json:"hint"`
	Details json.RawMessage `json:"details,omitempty"`
	// The response of a run whose `finish` is `error` (nil for an error before any request).
	Response *Response `json:"-"`
}

func (e *Error) Error() string {
	if e.Hint != "" {
		return fmt.Sprintf("latai %s: %s (%s)", e.Code, e.Message, e.Hint)
	}
	return fmt.Sprintf("latai %s: %s", e.Code, e.Message)
}

// Name is the code's name (`CeilingExceeded` for `E0206`), "" for a code this SDK does not know.
func (e *Error) Name() string { return codes[e.Code].name }

// Category is the name of the code's category (`compat` for `E0206`), "" when unknown.
func (e *Error) Category() string {
	if len(e.Code) < 3 {
		return ""
	}
	return categories[e.Code[:3]]
}

// Retryable says whether the same call may work later unchanged (a busy queue, a deadline).
func (e *Error) Retryable() bool { return codes[e.Code].retryable }

// Is makes errors.Is(err, &latai.Error{Code: latai.CodeUnknownPack}) match by code.
func (e *Error) Is(target error) bool {
	var t *Error
	return errors.As(target, &t) && t.Code == e.Code
}

// HasCode reports whether err is a latai error with this code.
func HasCode(err error, code string) bool {
	var e *Error
	return errors.As(err, &e) && e.Code == code
}

// LibraryError is returned when liblatai cannot be found or loaded.
type LibraryError struct {
	Tried []string
	Cause string
}

func (e *LibraryError) Error() string {
	return fmt.Sprintf("latai: cannot load liblatai (%s); tried %v; set LATAI_LIB, or run `go run github.com/hitl-systems/latai-go/cmd/latai-core`", e.Cause, e.Tried)
}

// wireError reads an error JSON; a call that failed without one still gives its code.
func wireError(rc int32, data []byte) *Error {
	e := &Error{}
	if len(data) == 0 || json.Unmarshal(data, e) != nil || e.Code == "" {
		code := fmt.Sprintf("E%04d", rc)
		e = &Error{Code: code, Message: fmt.Sprintf("call failed with code %d", rc), Hint: codes[code].hint}
	}
	return e
}
