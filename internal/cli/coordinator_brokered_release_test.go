package cli

import (
	"bytes"
	"encoding/json"
	"flag"
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
