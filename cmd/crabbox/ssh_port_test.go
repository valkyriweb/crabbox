package main

import (
	"slices"
	"testing"

	"github.com/openclaw/crabbox/internal/cli"
)

// These adapters bootstrap cloud and local VMs with the shared 2222/22 policy.
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
			// The fork drops the implicit port 22 fallback on loopback hosts
			// (loopbackSafeFallbackPorts): a port-forward would reach the
			// operator's own sshd.
			for host, want := range map[string][]string{"192.0.2.10": {"22"}, "127.0.0.1": {}} {
				target := cli.SSHTargetFromConfig(cfg, host)
				if target.Port != "2222" || !slices.Equal(target.FallbackPorts, want) {
					t.Fatalf("%s target port=%s fallbacks=%v, want %v", host, target.Port, target.FallbackPorts, want)
				}
			}
		})
	}
}
