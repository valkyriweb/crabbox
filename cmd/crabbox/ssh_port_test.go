package main

import (
	"slices"
	"testing"

	"github.com/openclaw/crabbox/internal/cli"
)

// These adapters share the remote 2222/22 policy, with the fork's loopback
// exclusion so forwarded endpoints cannot fall back to the operator's sshd.
func TestCloudProvidersRetainSSHPortFallback(t *testing.T) {
	for _, name := range []string{"aws", "azure", "gcp", "hetzner", "ovh", "nebius", "xcp-ng", "incus", "proxmox"} {
		t.Run(name, func(t *testing.T) {
			cfg := cli.BaseConfig()
			cfg.Provider = name
			provider, err := cli.ProviderFor(name)
			if err != nil {
				t.Fatal(err)
			}
			if defaults, ok := provider.(cli.ProviderConfigDefaulter); ok {
				if err := defaults.ApplyConfigDefaults(&cfg); err != nil {
					t.Fatal(err)
				}
			}
			for _, host := range []string{"192.0.2.10", "127.0.0.1", "::1", "localhost"} {
				target := cli.SSHTargetFromConfig(cfg, host)
				var wantFallbacks []string
				if host == "192.0.2.10" {
					wantFallbacks = []string{"22"}
				}
				if target.Port != "2222" || !slices.Equal(target.FallbackPorts, wantFallbacks) {
					t.Fatalf("%s target port=%s fallbacks=%v", host, target.Port, target.FallbackPorts)
				}
			}
		})
	}
}
