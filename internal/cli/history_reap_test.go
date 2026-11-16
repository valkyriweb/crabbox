package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// reapFixture serves nine running runs. Three are orphans; two more look like
// orphans until the second read shows that their client is still alive.
func reapFixture(t *testing.T, finishStatus int) (*atomic.Int32, func() map[string]map[string]any) {
	t.Helper()
	now := time.Now().UTC()
	at := func(age time.Duration) string { return now.Add(-age).Format(time.RFC3339) }
	runs := []map[string]any{
		{"id": "run_orphan_released", "leaseID": "cbx_released", "leaseIDs": []string{"cbx_released"}, "state": "running", "phase": "command", "startedAt": at(5 * time.Hour), "lastEventAt": at(3 * time.Hour)},
		{"id": "run_orphan_gone", "leaseID": "cbx_gone", "state": "running", "phase": "sync", "startedAt": at(4 * time.Hour), "lastEventAt": at(2 * time.Hour)},
		{"id": "run_live_lease", "leaseID": "cbx_active", "state": "running", "phase": "command", "startedAt": at(9 * time.Hour), "lastEventAt": at(8 * time.Hour)},
		{"id": "run_recent", "leaseID": "cbx_released", "state": "running", "phase": "command", "startedAt": at(20 * time.Minute), "lastEventAt": at(10 * time.Minute)},
		{"id": "run_waiting_for_capacity", "state": "running", "phase": "leasing", "startedAt": at(2 * time.Hour)},
		{"id": "run_lookup_fails", "leaseID": "cbx_error", "state": "running", "phase": "command", "startedAt": at(3 * time.Hour), "lastEventAt": at(3 * time.Hour)},
		{"id": "run_never_leased", "state": "running", "phase": "leasing", "startedAt": at(7 * time.Hour), "lastEventAt": at(7 * time.Hour)},
		{"id": "run_client_reports_late", "leaseID": "cbx_released", "state": "running", "phase": "command", "startedAt": at(5 * time.Hour), "lastEventAt": at(3 * time.Hour), "eventCount": 4},
		{"id": "run_client_finishes_late", "leaseID": "cbx_released", "state": "running", "phase": "command", "startedAt": at(5 * time.Hour), "lastEventAt": at(3 * time.Hour)},
	}
	// The second read sees what a client that is still alive did meanwhile.
	secondRead := map[string]map[string]any{
		"run_client_reports_late":  {"id": "run_client_reports_late", "leaseID": "cbx_released", "state": "running", "phase": "command", "startedAt": at(5 * time.Hour), "lastEventAt": at(0), "eventCount": 5},
		"run_client_finishes_late": {"id": "run_client_finishes_late", "leaseID": "cbx_released", "state": "succeeded", "phase": "command", "startedAt": at(5 * time.Hour), "lastEventAt": at(3 * time.Hour)},
	}
	for _, run := range runs {
		if _, ok := secondRead[run["id"].(string)]; !ok {
			secondRead[run["id"].(string)] = run
		}
	}
	secondReads := &atomic.Int32{}
	var mu sync.Mutex
	finished := map[string]map[string]any{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/runs":
			if r.URL.Query().Get("state") != "running" {
				t.Errorf("reap must list only running runs: %s", r.URL.RawQuery)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"runs": runs})
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/runs/"):
			run, ok := secondRead[strings.TrimPrefix(r.URL.Path, "/v1/runs/")]
			if !ok {
				t.Errorf("unexpected second read %s", r.URL.Path)
			}
			secondReads.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{"run": run})
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/leases/"):
			state := map[string]string{"cbx_released": "released", "cbx_active": "active"}[strings.TrimPrefix(r.URL.Path, "/v1/leases/")]
			switch {
			case state != "":
				_, _ = fmt.Fprintf(w, `{"lease":{"id":"x","state":%q}}`, state)
			case strings.HasSuffix(r.URL.Path, "cbx_gone"):
				http.Error(w, `{"error":"not_found"}`, http.StatusNotFound)
			default:
				http.Error(w, `{"error":"boom"}`, http.StatusInternalServerError)
			}
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/finish"):
			if finishStatus != http.StatusOK {
				http.Error(w, `{"error":"nope"}`, finishStatus)
				return
			}
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/runs/"), "/finish")
			var body map[string]any
			raw, _ := io.ReadAll(r.Body)
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Errorf("finish body: %v", err)
			}
			mu.Lock()
			finished[id] = body
			mu.Unlock()
			_, _ = fmt.Fprintf(w, `{"run":{"id":%q,"state":"failed"}}`, id)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.String())
			http.Error(w, "unexpected", http.StatusTeapot)
		}
	}))
	t.Cleanup(server.Close)
	t.Setenv("CRABBOX_COORDINATOR", server.URL)
	t.Setenv("CRABBOX_COORDINATOR_TOKEN", "")
	return secondReads, func() map[string]map[string]any {
		mu.Lock()
		defer mu.Unlock()
		copied := map[string]map[string]any{}
		for id, body := range finished {
			copied[id] = body
		}
		return copied
	}
}

func reapActions(t *testing.T, out []byte) map[string]historyReapResult {
	t.Helper()
	var results []historyReapResult
	if err := json.Unmarshal(out, &results); err != nil {
		t.Fatalf("decode reap JSON: %v: %s", err, out)
	}
	byID := map[string]historyReapResult{}
	for _, result := range results {
		byID[result.ID] = result
	}
	return byID
}

func TestHistoryReapClosesOnlyOrphanedRuns(t *testing.T) {
	_, finished := reapFixture(t, http.StatusOK)
	var stdout, stderr bytes.Buffer
	app := App{Stdout: &stdout, Stderr: &stderr}
	if err := app.history(context.Background(), []string{"reap", "--json"}); err != nil {
		t.Fatal(err)
	}
	got := reapActions(t, stdout.Bytes())
	want := map[string]string{
		"run_orphan_released":      "reaped",
		"run_orphan_gone":          "reaped",
		"run_never_leased":         "reaped",
		"run_live_lease":           "skipped",
		"run_recent":               "skipped",
		"run_waiting_for_capacity": "skipped",
		"run_lookup_fails":         "skipped",
		"run_client_reports_late":  "skipped",
		"run_client_finishes_late": "skipped",
	}
	if len(got) != len(want) {
		t.Fatalf("results = %#v", got)
	}
	for id, action := range want {
		if got[id].Action != action {
			t.Errorf("%s: action=%q reason=%q, want %q", id, got[id].Action, got[id].Reason, action)
		}
	}
	if reason := got["run_live_lease"].Reason; !strings.Contains(reason, "cbx_active is active") {
		t.Errorf("live lease reason = %q", reason)
	}
	if reason := got["run_client_reports_late"].Reason; reason != "changed during inspection" {
		t.Errorf("late report reason = %q", reason)
	}
	if reason := got["run_client_finishes_late"].Reason; reason != "already succeeded" {
		t.Errorf("late finish reason = %q", reason)
	}
	closed := finished()
	if len(closed) != 3 {
		t.Fatalf("finished runs = %v", closed)
	}
	body := closed["run_orphan_released"]
	if body["exitCode"] != float64(1) || body["blockedStage"] != orphanedRunBlockedStage {
		t.Fatalf("finish body = %v", body)
	}
	if log, _ := body["log"].(string); !strings.Contains(log, "exit status is unknown") {
		t.Fatalf("finish log = %q", log)
	}
}

func TestHistoryReapDryRunChangesNothing(t *testing.T) {
	secondReads, finished := reapFixture(t, http.StatusOK)
	var stdout, stderr bytes.Buffer
	app := App{Stdout: &stdout, Stderr: &stderr}
	if err := app.history(context.Background(), []string{"reap", "--dry-run"}); err != nil {
		t.Fatal(err)
	}
	if len(finished()) != 0 || secondReads.Load() != 0 {
		t.Fatalf("dry run finished runs: %v, second reads: %d", finished(), secondReads.Load())
	}
	out := stdout.String()
	if strings.Count(out, "would-reap ") != 5 || strings.Count(out, "skipped ") != 4 || !strings.Contains(out, "would-reap run_orphan_gone lease=cbx_gone phase=sync") {
		t.Fatalf("dry run output:\n%s", out)
	}
}

func TestHistoryReapReportsRunsItCouldNotClose(t *testing.T) {
	reapFixture(t, http.StatusForbidden)
	var stdout, stderr bytes.Buffer
	app := App{Stdout: &stdout, Stderr: &stderr}
	err := app.history(context.Background(), []string{"reap", "--json"})
	if err == nil || !strings.Contains(err.Error(), "3 of 9 runs could not be closed") {
		t.Fatalf("err = %v", err)
	}
	if got := reapActions(t, stdout.Bytes()); got["run_orphan_gone"].Action != "error" || got["run_recent"].Action != "skipped" {
		t.Fatalf("results = %#v", got)
	}
}

func TestHistoryReapRejectsUnsafeArguments(t *testing.T) {
	for _, args := range [][]string{{"reap", "--older-than", "30s"}, {"reap", "--limit", "0"}, {"reap", "run_123"}} {
		var stdout, stderr bytes.Buffer
		app := App{Stdout: &stdout, Stderr: &stderr}
		if err := app.history(context.Background(), args); err == nil {
			t.Errorf("%v: expected an error", args)
		}
	}
}
