package gmail

import "fmt"

// SendOutcomeError means the caller must reconcile Sent Items before retrying.
// Accepted distinguishes confirmed provider acceptance from an unknown outcome.
type SendOutcomeError struct {
	Accepted bool
	Err      error
}

func (e *SendOutcomeError) Error() string {
	outcome := "unknown"
	if e.Accepted {
		outcome = "accepted"
	}
	return fmt.Sprintf("send outcome %s; inspect Sent Items before retrying: %v", outcome, e.Err)
}
func (e *SendOutcomeError) Unwrap() error { return e.Err }
