package mail

import (
	"errors"
	"fmt"
)

var ErrMissingMessageBody = errors.New("missing message body")

// ErrAuthFailed 标记 IMAP 服务端明确拒绝认证 (授权码错误或被撤销)，区别于网络类瞬时故障。
var ErrAuthFailed = errors.New("imap authentication failed")

// authError 保留原始提示文案，同时可被 errors.Is(err, ErrAuthFailed) 识别。
type authError struct{ error }

func (e authError) Is(target error) bool { return target == ErrAuthFailed }
func (e authError) Unwrap() error        { return e.error }

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
