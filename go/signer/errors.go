package signer

import "errors"

// Error codes (contract semantics §4). Remote transports map these 1:1.
const (
	CodeKeyNotFound       = "KEY_NOT_FOUND"
	CodeSummaryMismatch   = "SUMMARY_MISMATCH"
	CodePolicyRejected    = "POLICY_REJECTED"
	CodeSignerUnavailable = "SIGNER_UNAVAILABLE"
	CodeInternal          = "INTERNAL"
)

// Error is a typed signer error. Two Errors match under errors.Is when their
// codes are equal, so detail-carrying instances match the sentinels below.
type Error struct {
	Code   string
	Detail string
}

func (e *Error) Error() string {
	if e.Detail == "" {
		return e.Code
	}
	return e.Code + ": " + e.Detail
}

func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	return ok && t.Code == e.Code
}

var (
	ErrKeyNotFound       = &Error{Code: CodeKeyNotFound}
	ErrSummaryMismatch   = &Error{Code: CodeSummaryMismatch}
	ErrPolicyRejected    = &Error{Code: CodePolicyRejected}
	ErrSignerUnavailable = &Error{Code: CodeSignerUnavailable}
	ErrInternal          = &Error{Code: CodeInternal}
)

// NewError attaches detail to one of the sentinel codes.
func NewError(sentinel *Error, detail string) *Error {
	return &Error{Code: sentinel.Code, Detail: detail}
}

// CodeOf maps any error to its signer code; non-signer errors map to
// CodeInternal, nil to "".
func CodeOf(err error) string {
	if err == nil {
		return ""
	}
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return CodeInternal
}
