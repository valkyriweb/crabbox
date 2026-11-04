package cli

import (
	"reflect"
	"testing"
)

func TestNonStaticLoopbackLeaseNeverFallsBackTo22(t *testing.T) {
	cfg := baseConfig()
	cfg.Provider = "aws"
	target := sshTargetForLeaseConnection(cfg, "127.0.0.1", "crabbox", "2203", "", nil)
	if got, want := sshPortCandidates(target.Port, target.FallbackPorts), []string{"2203"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("candidates=%v want %v", got, want)
	}
	remote := sshTargetForLeaseConnection(cfg, "192.0.2.10", "crabbox", "2203", "", nil)
	if got, want := sshPortCandidates(remote.Port, remote.FallbackPorts), []string{"2203", "22"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("remote candidates=%v want %v", got, want)
	}
}

func TestLoopbackLeaseHonorsExplicitFallbackPorts(t *testing.T) {
	cfg := baseConfig()
	cfg.Provider = "aws"
	cfg.SSHFallbackPorts = []string{"22"}
	cfg.sshFallbackPortsExplicit = true
	cfg.explicitSSHFallbackPorts = []string{"22"}
	target := sshTargetForLeaseConnection(cfg, "127.0.0.1", "crabbox", "2203", "", nil)
	if got, want := sshPortCandidates(target.Port, target.FallbackPorts), []string{"2203", "22"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("candidates=%v want %v", got, want)
	}
}
