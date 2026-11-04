package serve

import (
	"encoding/json"
	core "github.com/openclaw/crabbox/internal/cli"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestServeControllerIdentity(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != "GET" || r.URL.Path != "/v1/whoami" {
			t.Errorf("unexpected effect %s %s", r.Method, r.URL.Path)
			w.WriteHeader(500)
			return
		}
		owner, org := "alice@example.com", "example-org"
		switch r.Header.Get("Authorization") {
		case "Bearer fixture-v1", "Bearer fixture-v2":
		case "Bearer fixture-other":
			owner = "bob@example.com"
		case "Bearer fixture-org":
			org = "other-org"
		default:
			w.WriteHeader(401)
			return
		}
		_ = json.NewEncoder(w).Encode(core.CoordinatorWhoami{Owner: owner, Org: org, Auth: "token"})
	}))
	defer server.Close()
	cfg := core.Config{Provider: "serve", Coordinator: server.URL, CoordToken: "fixture-v1", TargetOS: core.TargetLinux, Architecture: "amd64", ServerType: "cpu2", ServerTypeExplicit: true, TTL: 5 * time.Minute, SSHUser: "crabbox", WorkRoot: "/work/crabbox"}
	scope, fixed, err := (Provider{}).ControllerProviderIdentity(t.Context(), cfg)
	if err != nil || !fixed || !strings.HasPrefix(scope, "serve-v1:sha256:") {
		t.Fatalf("identity %q %v %v", scope, fixed, err)
	}
	rotated := cfg
	rotated.CoordToken = "fixture-v2"
	same, _, err := (Provider{}).ControllerProviderIdentity(t.Context(), rotated)
	if err != nil || same != scope {
		t.Fatalf("same-principal rotation changed custody: %q %v", same, err)
	}
	for _, tc := range []struct {
		name   string
		change func(*core.Config)
	}{
		{"principal", func(c *core.Config) { c.CoordToken = "fixture-other" }},
		{"org", func(c *core.Config) { c.CoordToken = "fixture-org" }},
		{"machine", func(c *core.Config) { c.Serve.Machine = "other-donor" }},
		{"architecture-explicitness", func(c *core.Config) { core.MarkArchitectureExplicit(c) }},
		{"guest-access", func(c *core.Config) { c.WorkRoot = "/different-root" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changed := cfg
			tc.change(&changed)
			actual, _, err := (Provider{}).ControllerProviderIdentity(t.Context(), changed)
			if err != nil || actual == scope {
				t.Fatalf("scope failed to bind %s: %q %v", tc.name, actual, err)
			}
		})
	}
	for _, change := range []func(*core.Config){
		func(c *core.Config) { c.ServerType = "cpu8"; c.ServerTypeExplicit = true },
		func(c *core.Config) { c.Class = "fast" },
		func(c *core.Config) { c.Profile = "approved-profile" },
		func(c *core.Config) { c.TTL = 10 * time.Minute; c.IdleTimeout = time.Minute },
	} {
		changed := cfg
		change(&changed)
		actual, _, err := (Provider{}).ControllerProviderIdentity(t.Context(), changed)
		if err != nil || actual != scope {
			t.Fatalf("authorized per-workspace override changed custody: %q %v", actual, err)
		}
	}
	if strings.Contains(scope, "fixture") || strings.Contains(scope, "alice") {
		t.Fatal("identity disclosed credential or principal")
	}
	before := calls.Load()
	for _, tc := range []struct {
		name   string
		change func(*core.Config)
	}{
		{"command", func(c *core.Config) { c.CoordTokenCommand = []string{"must-not-execute"} }},
		{"missing-token", func(c *core.Config) { c.CoordToken = "" }},
		{"fallback", func(c *core.Config) { c.Serve.Fallback = "other" }},
		{"query", func(c *core.Config) { c.Coordinator += "?secret=forbidden" }},
		{"desktop", func(c *core.Config) { c.Desktop = true }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changed := cfg
			tc.change(&changed)
			if _, fixed, err := (Provider{}).ControllerProviderIdentity(t.Context(), changed); err == nil || fixed {
				t.Fatal("invalid identity admitted")
			}
		})
	}
	if calls.Load() != before {
		t.Fatal("invalid configuration performed remote I/O")
	}
}

func TestServeControllerRejectsUnauthenticatedOrAdminIdentity(t *testing.T) {
	for _, identity := range []core.CoordinatorWhoami{{}, {Owner: "unknown", Auth: "token"}, {Owner: "alice@example.com", Auth: "admin", Admin: true}, {Owner: "alice@example.com"}} {
		t.Run(identity.Owner+identity.Auth, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _ = json.NewEncoder(w).Encode(identity) }))
			defer server.Close()
			cfg := core.Config{Coordinator: server.URL, CoordToken: "fixture"}
			if _, fixed, err := (Provider{}).ControllerProviderIdentity(t.Context(), cfg); err == nil || fixed {
				t.Fatal("untrusted principal admitted")
			}
		})
	}
}
