package cli

import (
	"reflect"
	"testing"
)

func staticPortCandidates(t *testing.T, cfg Config) []string {
	t.Helper()
	_, target, _, err := staticLease(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return sshPortCandidates(target.Port, target.FallbackPorts)
}

func TestStaticLeaseExplicitStaticPortNeverFallsBack(t *testing.T) {
	cfg := baseConfig()
	cfg.Provider = staticProvider
	cfg.Static.Host = "127.0.0.1"
	cfg.Static.Port = "2224"
	if got, want := staticPortCandidates(t, cfg), []string{"2224"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("candidates=%v want %v", got, want)
	}
}

func TestStaticLeaseExplicitSSHPortNeverFallsBack(t *testing.T) {
	cfg := baseConfig()
	cfg.Provider = staticProvider
	cfg.Static.Host = "127.0.0.1"
	cfg.SSHPort = "2224"
	MarkSSHPortExplicit(&cfg)
	if got, want := staticPortCandidates(t, cfg), []string{"2224"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("candidates=%v want %v", got, want)
	}
}

func TestStaticLeaseDefaultPortKeepsFallback(t *testing.T) {
	cfg := baseConfig()
	cfg.Provider = staticProvider
	cfg.Static.Host = "build.example"
	if got, want := staticPortCandidates(t, cfg), []string{"2222", "22"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("candidates=%v want %v", got, want)
	}
}

func TestStaticLeaseExplicitFallbackPortsAreHonored(t *testing.T) {
	cfg := baseConfig()
	cfg.Provider = staticProvider
	cfg.Static.Host = "build.example"
	cfg.Static.Port = "2224"
	cfg.SSHFallbackPorts = []string{"2225"}
	cfg.sshFallbackPortsExplicit = true
	cfg.explicitSSHFallbackPorts = []string{"2225"}
	if got, want := staticPortCandidates(t, cfg), []string{"2224", "2225"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("candidates=%v want %v", got, want)
	}
}
