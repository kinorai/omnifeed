package observability

import (
	"context"
	"log/slog"
	"time"
)

// DependencyCheck probes one dependency; nil means up.
type DependencyCheck func(ctx context.Context) error

// DependencyChecker probes each configured dependency on an interval and
// publishes omnifeed_dependency_up{dependency}. It replaces gating /readyz on
// crawl4ai and SearXNG: a dependency outage fails the calls that need it (and
// says so, classified), while the pod stays in rotation for everything else —
// the Reddit quota, the GitHub/HN/Bluesky engines and the cache keep serving.
type DependencyChecker struct {
	Checks   map[string]DependencyCheck
	Interval time.Duration // between rounds; defaults to 30s
	Timeout  time.Duration // per check; defaults to 5s
	Metrics  *Metrics
	Logger   *slog.Logger

	last map[string]bool // previous state per dependency, for transition logs
}

// CheckOnce runs every check once and publishes the results.
func (d *DependencyChecker) CheckOnce(ctx context.Context) {
	if d.last == nil {
		d.last = make(map[string]bool, len(d.Checks))
	}
	timeout := d.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	for name, check := range d.Checks {
		up := d.runCheck(ctx, name, check, timeout)
		d.Metrics.SetDependencyUp(name, up)
		if prev, seen := d.last[name]; (!seen && !up) || (seen && prev != up) {
			if up {
				d.logger().Info("dependency up", "dependency", name)
			} else {
				d.logger().Warn("dependency down", "dependency", name)
			}
		}
		d.last[name] = up
	}
}

func (d *DependencyChecker) runCheck(ctx context.Context, name string, check DependencyCheck, timeout time.Duration) (up bool) {
	// A panicking check counts as down, never as a dead process.
	defer func() {
		if v := recover(); v != nil {
			ObservePanic(PanicBackground, v)
			up = false
		}
	}()
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	err := check(cctx)
	if err != nil {
		d.logger().Debug("dependency check failed", "dependency", name, "err", err)
	}
	return err == nil
}

// Run checks immediately, then every Interval, until ctx ends.
func (d *DependencyChecker) Run(ctx context.Context) {
	interval := d.Interval
	if interval <= 0 {
		interval = 30 * time.Second
	}
	d.CheckOnce(ctx)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			d.CheckOnce(ctx)
		}
	}
}

func (d *DependencyChecker) logger() *slog.Logger {
	if d.Logger != nil {
		return d.Logger
	}
	return slog.Default()
}
