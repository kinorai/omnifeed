// Package failover composes two domain.Searchers into one: a primary that
// answers every query, and a backup asked only when the primary's answer is an
// error, empty, thin, or degraded. The composite is itself a domain.Searcher,
// so a longer chain is nesting — failover(a, failover(b, c)) — and the
// transports never learn that more than one searcher exists.
//
// The pieces are kept apart on purpose: Policy decides (pure), merge combines
// two rankings (pure), and Searcher only runs the two calls and reports what
// happened.
package failover

import (
	"context"
	"log/slog"
	"strconv"
	"time"

	"github.com/kinorai/omnifeed/internal/domain"
	"github.com/kinorai/omnifeed/internal/observability"
)

// Config configures the Searcher.
type Config struct {
	Primary domain.Searcher
	Backup  domain.Searcher
	Policy  Policy
	// PrimaryBudget, when > 0, bounds how long the primary may take before the
	// backup is asked instead. Without it a hung primary spends its whole
	// timeout-and-retry budget first (tens of seconds on SearXNG) and the
	// backup only starts after that. A primary cut off by the budget counts
	// as an error, so the backup replaces it.
	PrimaryBudget time.Duration
	Logger        *slog.Logger
	// Metrics, when non-nil, receives one omnifeed_search_fallbacks_total
	// increment per search handed to the backup.
	Metrics *observability.Metrics
}

// Searcher answers with Primary, and with Backup when Policy says so.
type Searcher struct {
	primary       domain.Searcher
	backup        domain.Searcher
	policy        Policy
	primaryBudget time.Duration
	logger        *slog.Logger
	metrics       *observability.Metrics
	name          string
}

// New returns a Searcher wired with the given config.
func New(cfg Config) *Searcher {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Searcher{
		primary:       cfg.Primary,
		backup:        cfg.Backup,
		policy:        cfg.Policy,
		primaryBudget: cfg.PrimaryBudget,
		logger:        cfg.Logger,
		metrics:       cfg.Metrics,
		name:          cfg.Primary.Name() + "+" + cfg.Backup.Name(),
	}
}

// Name joins the members' names, e.g. "searxng+tinyfish". It is the searcher
// label on omnifeed_search_requests_total, so a dashboard can tell a
// deployment with a backup from one without.
// Computed once in New: transports read it on every search.
func (s *Searcher) Name() string { return s.name }

// Search asks the primary, and the backup when the policy calls for it.
//
// When the backup fails too, or replaces a failed primary with nothing, the
// primary's answer stands — its results if it had any, its error if not. The
// caller then sees the original failure, with its reason (quota_exhausted,
// timeout, ...) intact: an empty success would read as "nothing exists" where
// the truth is "retry later".
func (s *Searcher) Search(ctx context.Context, query string, opts domain.SearchOptions) (domain.SearchReport, error) {
	report, err := s.searchPrimary(ctx, query, opts)
	decision := s.policy.Decide(ctx, opts.Limit, report, err)
	if decision.Action == Keep {
		return report, err
	}

	backup, backupErr := s.backup.Search(ctx, query, opts)
	s.observe(ctx, decision, report, err, backup, backupErr)
	if backupErr != nil || (err != nil && len(backup.Results) == 0) {
		return report, err
	}
	if decision.Action == Replace {
		return backup, nil
	}
	return domain.SearchReport{
		Results:        merge(report.Results, backup.Results, opts.Limit),
		DegradedReason: backup.DegradedReason,
	}, nil
}

// searchPrimary asks the primary within PrimaryBudget, when one is set.
func (s *Searcher) searchPrimary(ctx context.Context, query string, opts domain.SearchOptions) (domain.SearchReport, error) {
	if s.primaryBudget <= 0 {
		return s.primary.Search(ctx, query, opts)
	}
	budgetCtx, cancel := context.WithTimeout(ctx, s.primaryBudget)
	defer cancel()
	return s.primary.Search(budgetCtx, query, opts)
}

// observe records one handoff: the metric always, and a log line that says
// why the primary was not enough and how the backup did.
func (s *Searcher) observe(ctx context.Context, decision Decision,
	report domain.SearchReport, err error, backup domain.SearchReport, backupErr error) {
	outcome := "served"
	switch {
	case backupErr != nil && ctx.Err() != nil:
		// The caller hung up while the backup ran: not the backup's failure.
		outcome = "canceled"
	case backupErr != nil:
		outcome = "failed"
	case len(backup.Results) == 0:
		outcome = "empty"
	}
	if s.metrics != nil {
		s.metrics.ObserveSearchFallback(s.primary.Name(), s.backup.Name(), string(decision.Trigger), observability.Reason(err), outcome)
	}

	attrs := []any{
		"primary", s.primary.Name(),
		"backup", s.backup.Name(),
		"trigger", string(decision.Trigger),
		"cause", cause(report, err),
		"primary_results", len(report.Results),
		"query_id", observability.QueryIDFrom(ctx),
	}
	switch outcome {
	case "canceled":
		s.logger.InfoContext(ctx, "search backup canceled by caller, primary answer stands", attrs...)
		return
	case "failed":
		s.logger.WarnContext(ctx, "search backup failed, primary answer stands",
			append(attrs, "backup_reason", observability.Reason(backupErr), "err", backupErr)...)
		return
	case "empty":
		s.logger.WarnContext(ctx, "search backup found nothing, primary answer stands", attrs...)
		return
	}
	s.logger.InfoContext(ctx, "search served by backup",
		append(attrs, "backup_results", len(backup.Results))...)
}

// cause says in a few words what was wrong with the primary's answer.
func cause(report domain.SearchReport, err error) string {
	switch {
	case err != nil:
		return observability.Reason(err)
	case report.Degraded():
		return report.DegradedReason
	default:
		return strconv.Itoa(len(report.Results)) + " results"
	}
}
