package observability

import (
	"errors"
	"time"

	"github.com/kinorai/omnifeed/internal/domain"
)

// ErrorCode is the stable, caller-facing error code every transport reports for
// a failed fetch/search (MCP structuredContent.code, the Open WebUI loader's
// error metadata, the REST error body). It is deliberately coarser than
// FailureKind: FailureKind is the metric taxonomy and may grow, while these
// codes are a contract clients branch on. Documented in docs/errors.md.
type ErrorCode string

// The complete set of caller-facing error codes.
const (
	CodeRateLimited    ErrorCode = "rate_limited"
	CodeBlocked        ErrorCode = "blocked"
	CodeCaptcha        ErrorCode = "captcha"
	CodeTimeout        ErrorCode = "timeout"
	CodeThinContent    ErrorCode = "thin_content"
	CodeNotFound       ErrorCode = "not_found"
	CodeUpstreamError  ErrorCode = "upstream_error"
	CodeQuotaExhausted ErrorCode = "quota_exhausted"
	CodeInvalidRequest ErrorCode = "invalid_request"
)

// Failure is the caller-facing classification of a failed call.
type Failure struct {
	Code ErrorCode
	// Retryable says whether the same call may succeed if repeated later
	// (after RetryAfter, when set). False means retrying is wasted work.
	Retryable bool
	// RetryAfter is the back-off the upstream or omnifeed's own pacing asked
	// for; 0 when unknown.
	RetryAfter time.Duration
	// UpstreamStatus is the upstream HTTP status, 0 when none applies.
	UpstreamStatus int
}

// codeFor maps each FailureKind onto a code and its retryability. The table is
// mirrored in docs/errors.md — keep both in step.
var codeFor = map[domain.FailureKind]struct {
	code      ErrorCode
	retryable bool
}{
	domain.KindHTTP429:          {CodeRateLimited, true},
	domain.KindHTTP403:          {CodeBlocked, false},
	domain.KindBotBlock:         {CodeBlocked, false},
	domain.KindCaptcha:          {CodeCaptcha, false},
	domain.KindTimeout:          {CodeTimeout, true},
	domain.KindCanceled:         {CodeTimeout, true},
	domain.KindThinContent:      {CodeThinContent, false},
	domain.KindNotFound:         {CodeNotFound, false},
	domain.KindSiteError:        {CodeUpstreamError, true},
	domain.KindUpstreamError:    {CodeUpstreamError, true},
	domain.KindUpstreamRejected: {CodeUpstreamError, false},
	domain.KindBadResponse:      {CodeUpstreamError, true},
	domain.KindQuotaExhausted:   {CodeQuotaExhausted, true},
	domain.KindError:            {CodeUpstreamError, false},
}

// Classify maps a failed call's error onto the caller-facing Failure. A
// domain.InvalidRequestError anywhere in the chain is the caller's own mistake
// (invalid_request, not retryable); otherwise the FailureKind (see Reason)
// decides. Returns the zero Failure for a nil err.
func Classify(err error) Failure {
	if err == nil {
		return Failure{}
	}
	var ire *domain.InvalidRequestError
	if errors.As(err, &ire) {
		return Failure{Code: CodeInvalidRequest}
	}
	m, known := codeFor[domain.FailureKind(Reason(err))]
	if !known {
		m = codeFor[domain.KindError]
	}
	f := Failure{Code: m.code, Retryable: m.retryable}
	var fe *domain.FetchError
	if errors.As(err, &fe) {
		f.RetryAfter = fe.RetryAfter
		f.UpstreamStatus = fe.StatusCode
	}
	return f
}

// RetryAfterSeconds renders RetryAfter as whole seconds, rounded UP (a caller
// told "retry in 12s" for a 12.4s wait would be refused again); 0 when unknown.
func (f Failure) RetryAfterSeconds() int {
	secs := int(f.RetryAfter / time.Second)
	if f.RetryAfter%time.Second > 0 {
		secs++
	}
	return secs
}

// Fields renders the Failure as the machine-readable object transports embed
// in an error response: code and retryable always, retry_after_s and
// upstream_status only when known. Callers add their own context (url, …).
func (f Failure) Fields() map[string]any {
	out := map[string]any{"code": string(f.Code), "retryable": f.Retryable}
	if s := f.RetryAfterSeconds(); s > 0 {
		out["retry_after_s"] = s
	}
	if f.UpstreamStatus != 0 {
		out["upstream_status"] = f.UpstreamStatus
	}
	return out
}
