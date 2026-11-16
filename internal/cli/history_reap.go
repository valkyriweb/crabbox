package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// orphanedRunBlockedStage marks runs closed by `history reap`, so they can be
// told apart from command failures in `history --json`.
const orphanedRunBlockedStage = "client-orphaned"

// orphanedRunLeaselessGrace is the least silence required before a run that
// never got a lease is closed. Such a run has no lease state to prove that its
// client is gone, and a client can wait a long time for capacity.
const orphanedRunLeaselessGrace = 6 * time.Hour

type historyReapResult struct {
	ID          string `json:"id"`
	LeaseID     string `json:"leaseID,omitempty"`
	Phase       string `json:"phase,omitempty"`
	LastEventAt string `json:"lastEventAt,omitempty"`
	Action      string `json:"action"`
	Reason      string `json:"reason"`
}

// historyReap closes coordinator runs that still say "running" although the
// client that owned them is gone. A client that is killed (SIGKILL, host
// sleep, tool timeout) never reports the finish, and the coordinator keeps the
// record open forever. A run is reaped only when it has been silent for
// --older-than and none of its leases is still live, so a long command on a
// live lease is never touched. A run without a lease must be silent for at
// least orphanedRunLeaselessGrace.
func (a App) historyReap(ctx context.Context, args []string) error {
	fs := newFlagSet("history reap", a.Stderr)
	olderThan := fs.Duration("older-than", time.Hour, "reap runs with no event for at least this long")
	dryRun := fs.Bool("dry-run", false, "report what would be reaped without changing anything")
	limit := fs.Int("limit", 500, "maximum running runs to inspect")
	jsonOut := fs.Bool("json", false, "print JSON")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return Exit(2, "usage: crabbox history reap [--older-than 1h] [--limit 500] [--dry-run] [--json]")
	}
	if *olderThan < time.Minute {
		return Exit(2, "older-than must be at least 1m")
	}
	if *limit <= 0 {
		return Exit(2, "limit must be positive")
	}
	coord, err := configuredCoordinator()
	if err != nil {
		return err
	}
	runs, err := coord.Runs(ctx, "", "", "", "running", *limit)
	if err != nil {
		return err
	}
	now := time.Now()
	results := make([]historyReapResult, 0, len(runs))
	failed := 0
	for _, run := range runs {
		result := historyReapResult{ID: run.ID, LeaseID: run.LeaseID, Phase: run.Phase, LastEventAt: blank(run.LastEventAt, run.StartedAt)}
		result.Action, result.Reason = "skipped", orphanedRunSkipReason(ctx, coord, run, now, *olderThan)
		if result.Reason == "" {
			result.Action, result.Reason = "would-reap", "silent and no live lease"
			changed := ""
			if !*dryRun {
				changed = orphanedRunChangeReason(ctx, coord, run)
			}
			if changed != "" {
				result.Action, result.Reason = "skipped", changed
			} else if !*dryRun {
				message := fmt.Sprintf("crabbox history reap: the client never finished this run; no event since %s and no live lease. The command's real exit status is unknown.\n", result.LastEventAt)
				if _, err := coord.FinishRun(ctx, run.ID, 1, 0, 0, message, false, nil, nil, FailureClassification{BlockedStage: orphanedRunBlockedStage}, nil); err != nil {
					result.Action, result.Reason = "error", err.Error()
					failed++
				} else {
					result.Action = "reaped"
				}
			}
		}
		results = append(results, result)
	}
	if *jsonOut {
		if err := json.NewEncoder(a.Stdout).Encode(results); err != nil {
			return err
		}
	} else {
		for _, r := range results {
			fmt.Fprintf(a.Stdout, "%-10s %s lease=%s phase=%s last_event=%s reason=%s\n", r.Action, r.ID, blank(r.LeaseID, "-"), blank(r.Phase, "-"), blank(r.LastEventAt, "-"), r.Reason)
		}
	}
	if failed > 0 {
		return Exit(1, "history reap: %d of %d runs could not be closed", failed, len(results))
	}
	return nil
}

// orphanedRunSkipReason returns why a running run must be left alone, or ""
// when it is safe to close. Any doubt (bad timestamp, lease lookup error)
// keeps the run.
func orphanedRunSkipReason(ctx context.Context, coord *CoordinatorClient, run CoordinatorRun, now time.Time, olderThan time.Duration) string {
	last, err := time.Parse(time.RFC3339Nano, blank(run.LastEventAt, run.StartedAt))
	if err != nil {
		return "unreadable last event time"
	}
	if now.Sub(last) < olderThan {
		return "recent event"
	}
	leaseIDs := run.LeaseIDs
	if len(leaseIDs) == 0 && run.LeaseID != "" {
		leaseIDs = []string{run.LeaseID}
	}
	if len(leaseIDs) == 0 && now.Sub(last) < orphanedRunLeaselessGrace {
		return "no lease yet; kept until silent for " + orphanedRunLeaselessGrace.String()
	}
	for _, id := range leaseIDs {
		lease, err := coord.GetLease(ctx, id)
		if isCoordinatorNotFound(err) {
			continue
		}
		if err != nil {
			return "lease " + id + " lookup failed"
		}
		if !statusTerminalState(lease.State) {
			return "lease " + id + " is " + blank(lease.State, "live")
		}
	}
	return ""
}

// orphanedRunChangeReason reads the run again just before it is closed and
// returns why it must now be left alone. The coordinator cannot close a run on
// a condition, so this second read narrows the time in which a client that is
// still alive could report between the inspection and the close.
func orphanedRunChangeReason(ctx context.Context, coord *CoordinatorClient, seen CoordinatorRun) string {
	fresh, err := coord.Run(ctx, seen.ID)
	if err != nil {
		return "second read failed"
	}
	if fresh.State != "running" {
		return "already " + blank(fresh.State, "closed")
	}
	if fresh.LastEventAt != seen.LastEventAt || fresh.EventCount != seen.EventCount || fresh.Phase != seen.Phase ||
		fresh.LeaseID != seen.LeaseID || len(fresh.LeaseIDs) != len(seen.LeaseIDs) {
		return "changed during inspection"
	}
	return ""
}
