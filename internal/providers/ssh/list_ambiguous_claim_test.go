package ssh

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestStaticListWarnsInsteadOfFailingOnAmbiguousClaims(t *testing.T) {
	stubStaticArchitecture(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	for _, id := range []string{"static_old_a", "static_old_b"} {
		if err := core.ClaimLeaseTargetForConfig(id, "old-mbp", core.Config{Provider: "ssh"}, core.Server{Provider: "ssh"}, core.SSHTarget{}, time.Hour); err != nil {
			t.Fatal(err)
		}
	}
	cfg := core.Config{Provider: "ssh"}
	cfg.SSHUser = "crabbox"
	cfg.SSHPort = "2222"
	cfg.Static.Host = "127.0.0.1"
	cfg.Static.ID = "old-mbp"
	var stderr bytes.Buffer
	backend := NewStaticSSHLeaseBackend(Provider{}.Spec(), cfg, core.Runtime{Stderr: &stderr}).(*staticLeaseBackend)
	views, err := backend.List(context.Background(), core.ListRequest{})
	if err != nil {
		t.Fatalf("list failed: %v", err)
	}
	if len(views) != 1 {
		t.Fatalf("views=%#v; want the configured target", views)
	}
	if !strings.Contains(stderr.String(), "multiple claims match identifier old-mbp") {
		t.Fatalf("missing ambiguity warning:\n%s", stderr.String())
	}
}
