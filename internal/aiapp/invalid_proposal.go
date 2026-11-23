package aiapp

import (
	"errors"
	"fmt"
)

// A received response was examined and rejected; this is not an unknown
// transport outcome. Its already reported usage must still be settled.
var ErrInvalidProposal = errors.New("invalid model proposal")

type invalidProposalError struct {
	Path   string
	Reason string
}

func (e *invalidProposalError) Error() string {
	return fmt.Sprintf("%s at %s: %s", ErrInvalidProposal, e.Path, e.Reason)
}
func (e *invalidProposalError) Unwrap() error { return ErrInvalidProposal }

func invalidProposal(path, reason string) error {
	return &invalidProposalError{Path: path, Reason: reason}
}
