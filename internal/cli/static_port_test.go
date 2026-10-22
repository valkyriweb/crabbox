package cli

import (
	"io"
	"os"
	"path/filepath"
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

func TestStaticLeaseLoopbackDefaultPortNeverFallsBackTo22(t *testing.T) {
	for _, host := range []string{"127.0.0.1", "::1", "localhost"} {
		cfg := baseConfig()
		cfg.Provider = staticProvider
		cfg.Static.Host = host
		if got, want := staticPortCandidates(t, cfg), []string{"2222"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("host=%s candidates=%v want %v", host, got, want)
		}
	}
}

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

func TestStaticRemoteExplicitPortInputsNeverFallBack(t *testing.T) {
	for _, source := range []string{"flag", "environment", "config"} {
		t.Run(source, func(t *testing.T) {
			isolateTestUserDirs(t)
			cfg := baseConfig()
			cfg.Provider = staticProvider
			cfg.Static.Host = "build.example"
			switch source {
			case "flag":
				fs := newFlagSet("run", io.Discard)
				flags := registerLeaseCreateFlags(fs, cfg)
				if err := parseFlags(fs, []string{"--ssh-port", "2224"}); err != nil {
					t.Fatal(err)
				}
				if err := applyLeaseCreateFlagsForLease(&cfg, fs, flags, "cbx_abcdef123456"); err != nil {
					t.Fatal(err)
				}
			case "environment":
				t.Setenv("CRABBOX_SSH_PORT", "2224")
				if err := applyEnv(&cfg); err != nil {
					t.Fatal(err)
				}
			case "config":
				configPath := filepath.Join(t.TempDir(), "config.yaml")
				if err := os.WriteFile(configPath, []byte("ssh:\n  port: 2224\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				file, err := readFileConfig(configPath)
				if err != nil {
					t.Fatal(err)
				}
				if err := applyFileConfig(&cfg, file); err != nil {
					t.Fatal(err)
				}
			}
			if !IsSSHPortExplicit(&cfg) {
				t.Fatalf("%s input did not mark the SSH port explicit", source)
			}
			if got, want := staticPortCandidates(t, cfg), []string{"2224"}; !reflect.DeepEqual(got, want) {
				t.Fatalf("candidates=%v want %v", got, want)
			}
		})
	}
}
