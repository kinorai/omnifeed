package domain

// InvalidRequestError marks a failure as the caller's mistake (a URL that is
// malformed, uses a forbidden scheme, or targets a private address) rather
// than anything upstream. Transports map it to the invalid_request error code
// via errors.As; Error() is the wrapped cause verbatim, so wrapping changes no
// message and no metric label.
type InvalidRequestError struct{ Err error }

func (e *InvalidRequestError) Error() string { return e.Err.Error() }

// Unwrap exposes the cause to errors.Is / errors.As.
func (e *InvalidRequestError) Unwrap() error { return e.Err }
