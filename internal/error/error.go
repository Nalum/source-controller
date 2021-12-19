package error

// StallingError is the reconciliation stalled state error. It contains an error
// and a reason for the stalled condition.
type StallingError struct {
	// Reason is the stalled condition reason string.
	Reason string
	// Err is the error that caused stalling. This can be used as the message in
	// stalled condition.
	Err error
}

// Error implements error interface.
func (se *StallingError) Error() string {
	return se.Err.Error()
}

// Unwrap returns the underlying error.
func (se *StallingError) Unwrap() error {
	return se.Err
}
