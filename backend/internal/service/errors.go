package service

import (
	"errors"
	"fmt"
	"strings"
)

var (
	ErrInvalidTransition = errors.New("requested status transition is not allowed")
	ErrInvalidInput      = errors.New("business input validation failed")
	ErrForbidden         = errors.New("role is not permitted for this operation")
	ErrTwoPersonRequired = errors.New("submitter and approver must be different users")
	ErrImmutableState    = errors.New("record can no longer be edited in its current state")
	ErrUnauthorized      = errors.New("invalid username or password")
	ErrInactiveUser      = errors.New("user account is inactive")
)

// GateConflict describes one gate that blocks a joint dispatch, with the
// reason it cannot participate (locked, busy or changed concurrently).
type GateConflict struct {
	Code   string `json:"code"`
	Reason string `json:"reason"`
}

// GateConflictError rejects an entire joint dispatch step and lists every
// conflicting gate so the operator can resolve them before retrying.
type GateConflictError struct {
	Conflicts []GateConflict
}

func (e *GateConflictError) Error() string {
	parts := make([]string, 0, len(e.Conflicts))
	for _, conflict := range e.Conflicts {
		parts = append(parts, fmt.Sprintf("%s(%s)", conflict.Code, conflict.Reason))
	}
	return fmt.Sprintf("joint dispatch rejected, conflicting gates: %s", strings.Join(parts, ", "))
}
