package mail

import (
	"errors"
	"fmt"
)

var ErrMissingMessageBody = errors.New("missing message body")

type MessageReadFailure struct {
	Ref       MessageRef
	Err       error
	Permanent bool
}

type BatchReadError struct {
	Failures []MessageReadFailure
}

func (failure *BatchReadError) Error() string {
	return fmt.Sprintf("failed to read %d message bodies", len(failure.Failures))
}

func (failure *BatchReadError) ErrorFor(ref MessageRef) error {
	for _, item := range failure.Failures {
		candidate := item.Ref
		if ref.UIDValidity == 0 {
			candidate.UIDValidity = 0
		}
		if candidate.CacheKey() == ref.CacheKey() {
			return item.Err
		}
	}
	return nil
}
