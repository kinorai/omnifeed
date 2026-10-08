package domain

// Document metadata keys that more than one layer reads. Engines and the
// registry write them; the response cache and the transports read them.
const (
	// PartialKey is "true" on a document an engine returned successfully but
	// incompletely (e.g. a Reddit morechildren round was blocked mid-crawl).
	// PartialReasonKey carries the classified reason. Partial documents are
	// never cached: the next call should get a chance at the whole thing.
	PartialKey       = "partial"
	PartialReasonKey = "partial_reason"

	// FallbackFromKey names the dedicated engine that failed when the generic
	// fallback rendered the document instead. A fallback render is a stand-in,
	// so it is never cached either.
	FallbackFromKey = "fallback_from"
)
