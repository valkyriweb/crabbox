package cli

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestCurrentLeaseClientIdentityFromEnv(t *testing.T) {
	for _, name := range append(append([]string{"CRABBOX_CLIENT_HOST"}, leaseClientSessionEnv...), leaseClientAgentEnv...) {
		t.Setenv(name, "")
	}
	t.Setenv("PI_SESSION_ID", "session-123")
	t.Setenv("AI_AGENT", "example-agent")
	t.Setenv("CRABBOX_CLIENT_HOST", "build host\n01")

	got := currentLeaseClientIdentity()
	want := LeaseClientIdentity{Session: "session-123", Host: "buildhost01", Agent: "example-agent"}
	if got == nil || *got != want {
		t.Fatalf("identity=%+v want %+v", got, want)
	}

	t.Setenv("CRABBOX_SESSION_ID", "explicit")
	if got := currentLeaseClientIdentity(); got.Session != "explicit" {
		t.Fatalf("session=%q want explicit override", got.Session)
	}
}

func TestCurrentLeaseClientIdentityEmpty(t *testing.T) {
	for _, name := range append(append([]string{"CRABBOX_CLIENT_HOST"}, leaseClientSessionEnv...), leaseClientAgentEnv...) {
		t.Setenv(name, "")
	}
	previous := leaseClientHostname
	leaseClientHostname = func() (string, error) { return "", errors.New("no host") }
	t.Cleanup(func() { leaseClientHostname = previous })
	if got := currentLeaseClientIdentity(); got != nil {
		t.Fatalf("identity=%+v want nil", got)
	}
	if got := sanitizeLeaseClientValue(strings.Repeat("a", 300)); len(got) != leaseClientIdentityMaxLen {
		t.Fatalf("len=%d want %d", len(got), leaseClientIdentityMaxLen)
	}
}

func TestCoordinatorLeaseOwnerShownInList(t *testing.T) {
	cfg := baseConfig()
	cfg.Provider = "aws"
	server, _, _ := leaseToServerTarget(CoordinatorLease{
		ID:             "cbx_123",
		Keep:           true,
		Owner:          "alice@example.com",
		ClientIdentity: &LeaseClientIdentity{Session: "s-1", Host: "ci-host", Agent: "example-agent"},
	}, cfg)
	var out bytes.Buffer
	if err := renderServerList(&out, []Server{server}); err != nil {
		t.Fatal(err)
	}
	want := " owner=alice@example.com session=s-1 client_host=ci-host agent=example-agent"
	if !strings.Contains(out.String(), want) {
		t.Fatalf("list=%q want %q", out.String(), want)
	}

	legacy, _, _ := leaseToServerTarget(CoordinatorLease{ID: "cbx_456"}, cfg)
	if got := leaseOwnerSummary(legacy.Labels); got != "" {
		t.Fatalf("legacy summary=%q want empty", got)
	}
}

func TestCoordinatorLeaseSizeSummaryFollowsExplicitType(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  Config
		want string
	}{
		{"class", Config{Class: "beast", ServerType: "c7a.48xlarge"}, "class=beast preferred_type=c7a.48xlarge"},
		{"explicit type", Config{Class: "beast", ServerType: "cpu2", ServerTypeExplicit: true}, "class=- type=cpu2 fallback_class=beast"},
	} {
		if got := coordinatorLeaseSizeSummary(tc.cfg); got != tc.want {
			t.Errorf("%s: got %q want %q", tc.name, got, tc.want)
		}
	}
}
