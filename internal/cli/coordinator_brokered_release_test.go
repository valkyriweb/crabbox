package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const brokeredReleaseTestProviderName = "brokered-release-test"

type brokeredReleaseTestProvider struct{}

func (brokeredReleaseTestProvider) Spec() ProviderSpec {
	return ProviderSpec{
		Name:         brokeredReleaseTestProviderName,
		Kind:         ProviderKindSSHLease,
		Targets:      []TargetSpec{{OS: targetLinux}},
		Features:     FeatureSet{FeatureSSH},
		Coordinator:  CoordinatorSupported,
		BrokeredOnly: true,
	}
}
func (brokeredReleaseTestProvider) RegisterFlags(*flag.FlagSet, Config) any { return noProviderFlags{} }
func (brokeredReleaseTestProvider) ApplyFlags(*Config, *flag.FlagSet, any) error {
	return nil
}
func (p brokeredReleaseTestProvider) Configure(Config, Runtime) (Backend, error) {
	return runEnvProfileTestBackend{spec: p.Spec()}, nil
}

func init() { RegisterProvider(brokeredReleaseTestProvider{}) }

func brokeredReleaseTestBackend(server *httptest.Server, stderr *bytes.Buffer, spec ProviderSpec) *coordinatorLeaseBackend {
	return &coordinatorLeaseBackend{
		cfg:   Config{Provider: spec.Name, TargetOS: targetLinux},
		spec:  spec,
		coord: &CoordinatorClient{BaseURL: server.URL, Client: server.Client()},
		rt:    Runtime{Stderr: stderr},
	}
}

func claimBrokeredReleaseTestLease(t *testing.T, id, provider string) {
	t.Helper()
	if err := ClaimLeaseTargetForConfig(id, "brokered-slug", Config{Provider: provider}, Server{Provider: provider}, SSHTarget{}, time.Hour); err != nil {
		t.Fatal(err)
	}
}

func TestBrokeredDeferredReleaseRemovesClaimOnceCleanupConfirms(t *testing.T) {
	clearConfigEnv(t)
	configureCoordinatorReleaseTestTiming(t, time.Second, 0)
	const id = "cbx_abcdef123456"
	claimBrokeredReleaseTestLease(t, id, brokeredReleaseTestProviderName)
	deleting := true
	var gets atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/release"):
			_ = json.NewEncoder(w).Encode(map[string]any{"lease": CoordinatorLease{ID: id, Provider: brokeredReleaseTestProviderName, State: "released", CleanupStartedAt: "2026-10-22T09:40:00Z", ReleaseDeletesServer: &deleting}})
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/leases/"):
			gets.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{"lease": confirmedCoordinatorRelease(id, brokeredReleaseTestProviderName)})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	var stderr bytes.Buffer
	backend := brokeredReleaseTestBackend(server, &stderr, brokeredReleaseTestProvider{}.Spec())
	outcome, err := backend.ReleaseLeaseWithOutcome(t.Context(), ReleaseLeaseRequest{Lease: LeaseTarget{LeaseID: id, Server: Server{Provider: brokeredReleaseTestProviderName}}, Force: true, DeferProviderCleanupObservation: true})
	if err != nil {
		t.Fatalf("release: %v\n%s", err, stderr.String())
	}
	if !outcome.Terminal || gets.Load() == 0 {
		t.Fatalf("outcome=%+v gets=%d; want terminal after observing cleanup\n%s", outcome, gets.Load(), stderr.String())
	}
	if _, exists, err := ReadLeaseClaimWithPresence(id); err != nil || exists {
		t.Fatalf("claim exists=%t err=%v; want removed", exists, err)
	}
	if strings.Contains(stderr.String(), "remains pending") {
		t.Fatalf("unexpected pending warning:\n%s", stderr.String())
	}
}

func TestBrokeredReleaseNotFoundForgetsLocalClaim(t *testing.T) {
	clearConfigEnv(t)
	configureCoordinatorReleaseTestTiming(t, time.Second, 0)
	const id = "cbx_abcdef123457"
	claimBrokeredReleaseTestLease(t, id, brokeredReleaseTestProviderName)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"not_found"}`))
	}))
	defer server.Close()
	var stderr bytes.Buffer
	backend := brokeredReleaseTestBackend(server, &stderr, brokeredReleaseTestProvider{}.Spec())
	outcome, err := backend.ReleaseLeaseWithOutcome(t.Context(), ReleaseLeaseRequest{Lease: LeaseTarget{LeaseID: id, Server: Server{Provider: brokeredReleaseTestProviderName}}, Force: true})
	if err != nil {
		t.Fatalf("release: %v\n%s", err, stderr.String())
	}
	if !outcome.ForgottenLocally {
		t.Fatalf("outcome=%+v; want forgotten locally", outcome)
	}
	if _, exists, err := ReadLeaseClaimWithPresence(id); err != nil || exists {
		t.Fatalf("claim exists=%t err=%v; want removed", exists, err)
	}
	if !strings.Contains(stderr.String(), "removing local claim") {
		t.Fatalf("missing forget message:\n%s", stderr.String())
	}
}

func TestBrokeredReleaseNotFoundKeepsClaimUnlessLeaseIsAbsent(t *testing.T) {
	notFound := func(w http.ResponseWriter) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"not_found"}`))
	}
	for _, tc := range []struct {
		name    string
		release func(http.ResponseWriter, *http.Request)
		get     func(http.ResponseWriter, *http.Request, string)
	}{
		{
			name:    "owner mismatch: release 404 but the lease still reads",
			release: func(w http.ResponseWriter, _ *http.Request) { notFound(w) },
			get: func(w http.ResponseWriter, _ *http.Request, id string) {
				_ = json.NewEncoder(w).Encode(map[string]any{"lease": CoordinatorLease{ID: id, Provider: brokeredReleaseTestProviderName, State: "active"}})
			},
		},
		{
			name:    "untyped catch-all 404",
			release: func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) },
			get:     func(w http.ResponseWriter, _ *http.Request, _ string) { notFound(w) },
		},
		{
			name: "other 404 error code",
			release: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"error":"machine_not_found"}`))
			},
			get: func(w http.ResponseWriter, _ *http.Request, _ string) { notFound(w) },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearConfigEnv(t)
			configureCoordinatorReleaseTestTiming(t, time.Second, 0)
			const id = "cbx_abcdef12345a"
			claimBrokeredReleaseTestLease(t, id, brokeredReleaseTestProviderName)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/release") {
					tc.release(w, r)
					return
				}
				tc.get(w, r, id)
			}))
			defer server.Close()
			var stderr bytes.Buffer
			backend := brokeredReleaseTestBackend(server, &stderr, brokeredReleaseTestProvider{}.Spec())
			outcome, _ := backend.ReleaseLeaseWithOutcome(t.Context(), ReleaseLeaseRequest{Lease: LeaseTarget{LeaseID: id, Server: Server{Provider: brokeredReleaseTestProviderName}}, Force: true})
			if outcome.ForgottenLocally {
				t.Fatalf("outcome=%+v; want the claim kept", outcome)
			}
			if _, exists, err := ReadLeaseClaimWithPresence(id); err != nil || !exists {
				t.Fatalf("claim exists=%t err=%v; want kept\n%s", exists, err, stderr.String())
			}
		})
	}
}

func TestNonBrokeredReleaseNotFoundKeepsLocalClaim(t *testing.T) {
	clearConfigEnv(t)
	configureCoordinatorReleaseTestTiming(t, time.Second, 0)
	const id = "cbx_abcdef123458"
	claimBrokeredReleaseTestLease(t, id, "aws")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"not_found"}`))
	}))
	defer server.Close()
	var stderr bytes.Buffer
	backend := brokeredReleaseTestBackend(server, &stderr, ProviderSpec{Name: "aws"})
	if _, err := backend.ReleaseLeaseWithOutcome(t.Context(), ReleaseLeaseRequest{Lease: LeaseTarget{LeaseID: id, Server: Server{Provider: "aws"}}, Force: true}); err == nil {
		t.Fatal("release succeeded; want coordinator 404 to fail for a non-brokered provider")
	}
	if _, exists, err := ReadLeaseClaimWithPresence(id); err != nil || !exists {
		t.Fatalf("claim exists=%t err=%v; want kept", exists, err)
	}
}

func useFastCapacityWait(t *testing.T) {
	t.Helper()
	oldInitial, oldMax := coordinatorCapacityWaitInitialDelay, coordinatorCapacityWaitMaxDelay
	coordinatorCapacityWaitInitialDelay, coordinatorCapacityWaitMaxDelay = time.Millisecond, time.Millisecond
	t.Cleanup(func() { coordinatorCapacityWaitInitialDelay, coordinatorCapacityWaitMaxDelay = oldInitial, oldMax })
}

var busyCapacityRejection = CoordinatorHTTPError{StatusCode: http.StatusConflict, Message: `{"error":"serve_capacity_unavailable","details":[{"online":true,"allocatedLeases":8}]}`}

func TestBrokeredAcquisitionWaitsForCapacity(t *testing.T) {
	useFastCapacityWait(t)
	for _, tc := range []struct {
		name      string
		cfg       Config
		spec      ProviderSpec
		busy      error
		wantCalls int
		wantErr   bool
	}{
		{name: "brokered waits", spec: brokeredReleaseTestProvider{}.Spec(), busy: busyCapacityRejection, wantCalls: 3},
		{name: "legacy rejection without details waits", spec: brokeredReleaseTestProvider{}.Spec(), busy: CoordinatorHTTPError{StatusCode: http.StatusConflict, Message: `{"error":"serve_capacity_unavailable"}`}, wantCalls: 3},
		{name: "no machines registered fails fast", spec: brokeredReleaseTestProvider{}.Spec(), busy: CoordinatorHTTPError{StatusCode: http.StatusConflict, Message: `{"error":"serve_capacity_unavailable","details":[]}`}, wantCalls: 1, wantErr: true},
		{name: "no machine online fails fast", spec: brokeredReleaseTestProvider{}.Spec(), busy: CoordinatorHTTPError{StatusCode: http.StatusConflict, Message: `{"error":"serve_capacity_unavailable","details":[{"online":false}]}`}, wantCalls: 1, wantErr: true},
		{name: "no-wait opt out", cfg: Config{Serve: ServeConfig{NoCapacityWait: true}}, spec: brokeredReleaseTestProvider{}.Spec(), busy: busyCapacityRejection, wantCalls: 1, wantErr: true},
		{name: "fallback skips wait", cfg: Config{Serve: ServeConfig{Fallback: "aws"}}, spec: brokeredReleaseTestProvider{}.Spec(), busy: busyCapacityRejection, wantCalls: 1, wantErr: true},
		{name: "other providers unchanged", spec: ProviderSpec{Name: "aws"}, busy: busyCapacityRejection, wantCalls: 1, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stderr bytes.Buffer
			b := &coordinatorLeaseBackend{cfg: tc.cfg, spec: tc.spec, rt: Runtime{Stderr: &stderr}}
			calls := 0
			_, err := b.waitForBrokeredCapacity(t.Context(), func(context.Context) (LeaseTarget, error) {
				calls++
				if calls < 3 {
					return LeaseTarget{}, tc.busy
				}
				return LeaseTarget{LeaseID: "cbx_abcdef123459"}, nil
			})
			if calls != tc.wantCalls || (err != nil) != tc.wantErr {
				t.Fatalf("calls=%d err=%v; want calls=%d err=%t", calls, err, tc.wantCalls, tc.wantErr)
			}
			if tc.wantCalls > 1 && !strings.Contains(stderr.String(), `phase="awaiting-capacity"`) {
				t.Fatalf("missing progress line:\n%s", stderr.String())
			}
		})
	}
}

func TestBrokeredCapacityWaitStopsAtBudget(t *testing.T) {
	useFastCapacityWait(t)
	b := &coordinatorLeaseBackend{cfg: Config{Serve: ServeConfig{CapacityWait: 20 * time.Millisecond}}, spec: brokeredReleaseTestProvider{}.Spec(), rt: Runtime{Stderr: &bytes.Buffer{}}}
	var lastDeadline time.Time
	started := time.Now()
	_, err := b.waitForBrokeredCapacity(t.Context(), func(ctx context.Context) (LeaseTarget, error) {
		lastDeadline, _ = ctx.Deadline()
		return LeaseTarget{}, busyCapacityRejection
	})
	if !isCoordinatorCapacityUnavailable(err) {
		t.Fatalf("err=%v; want the capacity rejection after the budget", err)
	}
	if limit := started.Add(20*time.Millisecond + coordinatorCapacityAttemptGrace + time.Second); lastDeadline.IsZero() || lastDeadline.After(limit) {
		t.Fatalf("last attempt deadline=%v; want bounded by budget plus grace (%v)", lastDeadline, limit)
	}
}

func TestBrokeredCapacityWaitDefaultsToTenMinutes(t *testing.T) {
	useFastCapacityWait(t)
	b := &coordinatorLeaseBackend{spec: brokeredReleaseTestProvider{}.Spec(), rt: Runtime{Stderr: &bytes.Buffer{}}}
	ctx, cancel := context.WithCancel(t.Context())
	var deadline time.Time
	calls := 0
	_, _ = b.waitForBrokeredCapacity(ctx, func(attemptCtx context.Context) (LeaseTarget, error) {
		calls++
		if calls == 2 {
			deadline, _ = attemptCtx.Deadline()
			cancel()
		}
		return LeaseTarget{}, busyCapacityRejection
	})
	if want := time.Now().Add(10*time.Minute + coordinatorCapacityAttemptGrace); deadline.IsZero() || deadline.After(want) || deadline.Before(want.Add(-time.Minute)) {
		t.Fatalf("attempt deadline=%v; want about %v", deadline, want)
	}
}

func TestBrokeredCapacityWaitHonorsContextCancellation(t *testing.T) {
	oldInitial := coordinatorCapacityWaitInitialDelay
	coordinatorCapacityWaitInitialDelay = time.Hour
	t.Cleanup(func() { coordinatorCapacityWaitInitialDelay = oldInitial })
	b := &coordinatorLeaseBackend{spec: brokeredReleaseTestProvider{}.Spec(), rt: Runtime{Stderr: &bytes.Buffer{}}}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	calls := 0
	done := make(chan error, 1)
	go func() {
		_, err := b.waitForBrokeredCapacity(ctx, func(context.Context) (LeaseTarget, error) {
			calls++
			return LeaseTarget{}, busyCapacityRejection
		})
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) || calls != 1 {
			t.Fatalf("err=%v calls=%d; want context deadline after one attempt", err, calls)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("capacity wait ignored context cancellation")
	}
}

func TestServeCapacityWaitEnvironment(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("CRABBOX_SERVE_CAPACITY_WAIT", "90s")
	t.Setenv("CRABBOX_SERVE_NO_WAIT", "true")
	cfg := baseConfig()
	if err := applyEnv(&cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Serve.CapacityWait != 90*time.Second || !cfg.Serve.NoCapacityWait {
		t.Fatalf("serve=%+v; want capacityWait=90s noCapacityWait=true", cfg.Serve)
	}
	t.Setenv("CRABBOX_SERVE_CAPACITY_WAIT", "-1s")
	if err := applyEnv(&cfg); err == nil {
		t.Fatal("negative CRABBOX_SERVE_CAPACITY_WAIT accepted")
	}
}

func TestServeCapacityWaitConfigFile(t *testing.T) {
	clearConfigEnv(t)
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("serve:\n  capacityWait: 3m\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := readFileConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg := baseConfig()
	if err := applyFileConfig(&cfg, file); err != nil {
		t.Fatal(err)
	}
	if cfg.Serve.CapacityWait != 3*time.Minute {
		t.Fatalf("capacityWait=%s; want 3m", cfg.Serve.CapacityWait)
	}
}

func TestCoordinatorStopBySlugReleasesCanonicalLeaseID(t *testing.T) {
	if runParallelCLIContract(t, 0) {
		return
	}
	clearConfigEnv(t)
	dir := t.TempDir()
	installRecordingSSH(t, dir)
	t.Setenv("CRABBOX_OWNER", "test@example.com")
	const id = "cbx_abcdef12345b"
	if err := ClaimLeaseTargetForConfig(id, "ended-slug", Config{Provider: "aws"}, Server{Provider: "aws"}, SSHTarget{}, time.Hour); err != nil {
		t.Fatal(err)
	}
	var releasePaths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/release") {
			releasePaths = append(releasePaths, r.URL.Path)
			_ = json.NewEncoder(w).Encode(map[string]any{"lease": confirmedCoordinatorRelease(id, "aws")})
			return
		}
		if strings.Contains(r.URL.Path, id) {
			_ = json.NewEncoder(w).Encode(map[string]any{"lease": confirmedCoordinatorRelease(id, "aws")})
			return
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"not_found"}`))
	}))
	defer server.Close()
	t.Setenv("CRABBOX_COORDINATOR", server.URL)
	t.Setenv("CRABBOX_COORDINATOR_TOKEN", "user-token")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var stderr bytes.Buffer
	err := (App{Stdout: io.Discard, Stderr: &stderr}).stop(ctx, []string{"--provider", "aws", "--id", "ended-slug"})
	if err != nil {
		t.Fatalf("stop: %v\n%s", err, stderr.String())
	}
	if len(releasePaths) != 1 || releasePaths[0] != "/v1/leases/"+id+"/release" {
		t.Fatalf("release paths=%v; want the canonical lease ID\n%s", releasePaths, stderr.String())
	}
}

func TestBrokeredReleaseStillPendingAfterObservationKeepsClaim(t *testing.T) {
	clearConfigEnv(t)
	configureCoordinatorReleaseTestTiming(t, time.Second, 0)
	old := brokeredReleaseObservationTimeout
	brokeredReleaseObservationTimeout = 50 * time.Millisecond
	t.Cleanup(func() { brokeredReleaseObservationTimeout = old })
	const id = "cbx_abcdef12345c"
	claimBrokeredReleaseTestLease(t, id, brokeredReleaseTestProviderName)
	deleting := true
	pending := CoordinatorLease{ID: id, Provider: brokeredReleaseTestProviderName, State: "released", CleanupStartedAt: "2026-10-22T09:40:00Z", ReleaseDeletesServer: &deleting}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"lease": pending})
	}))
	defer server.Close()
	var stderr bytes.Buffer
	backend := brokeredReleaseTestBackend(server, &stderr, brokeredReleaseTestProvider{}.Spec())
	outcome, err := backend.ReleaseLeaseWithOutcome(t.Context(), ReleaseLeaseRequest{Lease: LeaseTarget{LeaseID: id, Server: Server{Provider: brokeredReleaseTestProviderName}}, Force: true, DeferProviderCleanupObservation: true})
	if err != nil {
		t.Fatalf("release: %v\n%s", err, stderr.String())
	}
	if outcome.Terminal || outcome.ForgottenLocally {
		t.Fatalf("outcome=%+v; want pending cleanup to stay non-terminal", outcome)
	}
	if _, exists, err := ReadLeaseClaimWithPresence(id); err != nil || !exists {
		t.Fatalf("claim exists=%t err=%v; want kept while cleanup is pending", exists, err)
	}
	if !strings.Contains(stderr.String(), "remains pending") {
		t.Fatalf("missing pending warning:\n%s", stderr.String())
	}
}
