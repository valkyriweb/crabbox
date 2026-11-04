package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
)

type authenticatedControllerTestProvider struct {
	Provider
	scope string
	err   error
	calls *int
}

func (p authenticatedControllerTestProvider) Spec() ProviderSpec {
	return ProviderSpec{Name: "test-auth-controller"}
}
func (p authenticatedControllerTestProvider) ControllerProviderIdentity(ctx context.Context, _ Config) (string, bool, error) {
	*p.calls++
	if _, ok := ctx.Deadline(); !ok {
		panic("identity read lacks deadline")
	}
	return p.scope, true, p.err
}

func TestAuthenticatedControllerScopeIsReverifiedBeforeLifecycle(t *testing.T) {
	calls := 0
	p := authenticatedControllerTestProvider{scope: "verified-owner-profile", calls: &calls}
	RegisterProvider(&p)
	t.Cleanup(func() { delete(providerRegistry, p.Spec().Name) })
	cfg := baseConfig()
	cfg.Provider = p.Spec().Name
	_, scope, fixed, err := controllerProviderIdentityForConfig(cfg)
	if err != nil || !fixed || scope != p.scope {
		t.Fatalf("admission %q %v %v", scope, fixed, err)
	}
	t.Setenv(controllerProviderScopeEnv, scope)
	if err := validateControllerProviderScope(&cfg); err != nil || cfg.controllerProviderScope != scope {
		t.Fatalf("binding %q %v", cfg.controllerProviderScope, err)
	}
	p.scope = "changed-principal"
	if err := validateControllerProviderScope(&cfg); err == nil {
		t.Fatal("principal drift admitted")
	}
	p.err = errors.New("identity unavailable")
	if err := validateControllerProviderScope(&cfg); err == nil {
		t.Fatal("unavailable authority admitted")
	}
	if calls != 4 {
		t.Fatalf("identity calls=%d", calls)
	}
}

func TestControllerFixedCreateAttemptSurvivesReplayAndCancels(t *testing.T) {
	cfg := baseConfig()
	cfg.controllerProviderScope = "verified-owner-profile"
	const id = "cbx_abcdef123456"
	attempt := controllerFixedCreateAttemptID(cfg, id)
	if len(attempt) != 36 || attempt != controllerFixedCreateAttemptID(cfg, id) {
		t.Fatal("unstable attempt")
	}
	changed := cfg
	changed.controllerProviderScope = "other-owner"
	if attempt == controllerFixedCreateAttemptID(changed, id) || attempt == controllerFixedCreateAttemptID(cfg, "cbx_abcdef123457") {
		t.Fatal("attempt aliases foreign custody")
	}
	calls := 0
	client := &CoordinatorClient{BaseURL: "http://coordinator.test", Token: "fixture", Client: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["createAttemptID"] != attempt {
			t.Fatalf("incorrect bound attempt %v", body["createAttemptID"])
		}
		response := `{"lease":{"id":"` + id + `","provider":"serve","state":"provisioning"}}`
		if calls <= 2 {
			if r.Method != "PUT" || r.URL.Path != "/v1/leases/"+id {
				t.Fatalf("unexpected create %s %s", r.Method, r.URL.Path)
			}
		} else {
			if r.Method != "POST" || r.URL.Path != "/v1/leases/"+id+"/cancel-create" {
				t.Fatalf("unexpected cancel %s %s", r.Method, r.URL.Path)
			}
			response = `{"canceledCreate":{"version":1,"requestedLeaseID":"` + id + `","createAttemptID":"` + attempt + `","state":"canceled"}}`
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(response))}, nil
	})}}
	for range 2 {
		if _, err := client.EnsureLease(t.Context(), cfg, "fixture-public-key", true, id, "fixed"); err != nil {
			t.Fatal(err)
		}
	}
	backend := &coordinatorLeaseBackend{cfg: cfg, coord: client, rt: Runtime{Stderr: io.Discard}}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	err := backend.canceledCoordinatorLeaseCreateError(ctx, id, "fixed", attempt, true, context.Canceled)
	if !errors.Is(err, context.Canceled) || calls != 3 {
		t.Fatalf("cancel did not persist fence calls=%d err=%v", calls, err)
	}
	if controllerFixedCreateAttemptID(baseConfig(), id) != "" {
		t.Fatal("legacy fixed caller protocol changed")
	}
}

func (p authenticatedControllerTestProvider) RegisterFlags(*flag.FlagSet, Config) any { return nil }
func (p authenticatedControllerTestProvider) ApplyFlags(*Config, *flag.FlagSet, any) error {
	return nil
}

func TestAuthenticatedControllerConfigDiagnosticsStayOffline(t *testing.T) {
	path := isolatedConfigPath(t)
	calls := 0
	p := authenticatedControllerTestProvider{scope: "verified-owner-profile", calls: &calls}
	RegisterProvider(&p)
	t.Cleanup(func() { delete(providerRegistry, p.Spec().Name) })
	if err := os.WriteFile(path, []byte("provider: test-auth-controller\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	app := App{Stdout: &output, Stderr: io.Discard}
	if err := app.configShow([]string{"--json"}); err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatal("ordinary config diagnostics performed remote identity I/O")
	}
	output.Reset()
	if err := app.configShow([]string{"--json", "--controller-provider-identity"}); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || !strings.Contains(output.String(), "verified-owner-profile") {
		t.Fatalf("internal admission missing %d %s", calls, output.String())
	}
	p.err = errors.New("authenticated identity unavailable")
	output.Reset()
	if err := app.configShow([]string{"--json", "--controller-provider-identity"}); !errors.Is(err, p.err) {
		t.Fatalf("authenticated identity failure was hidden: %v", err)
	}
}
