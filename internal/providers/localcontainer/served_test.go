package localcontainer

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

type servedRecordingRunner struct{ args []string }

func TestServedHostKeyWaitsForNodeToolchain(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("container readiness script requires a POSIX shell")
	}
	for _, fixture := range []struct {
		name        string
		nodeVersion string
		npm         bool
		brokenNPM   bool
		ready       bool
	}{
		{name: "missing-node", npm: true},
		{name: "wrong-node", nodeVersion: "v22.0.0", npm: true},
		{name: "missing-npm", nodeVersion: servedNodeVersion},
		{name: "broken-npm", nodeVersion: servedNodeVersion, npm: true, brokenNPM: true},
		{name: "ready", nodeVersion: servedNodeVersion, npm: true, ready: true},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			bin := t.TempDir()
			tools := map[string]string{"cat": "printf '%s\\n' 'ssh-ed25519 AAAA fixture'"}
			if fixture.nodeVersion != "" {
				tools["node"] = "printf '%s\\n' '" + fixture.nodeVersion + "'"
			}
			if fixture.npm {
				tools["npm"] = "exit 0"
				if fixture.brokenNPM {
					tools["npm"] = "exit 1"
				}
			}
			for name, script := range tools {
				if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			runner := servedRuntimeFunc(func(ctx context.Context, req core.LocalCommandRequest) (core.LocalCommandResult, error) {
				if len(req.Args) < 3 || req.Args[0] != "exec" || req.Args[1] != "container" {
					t.Fatalf("unexpected command: %v", req.Args)
				}
				command := exec.CommandContext(ctx, "/bin/sh", append([]string{"-c", `exec "$@"`, "fixture"}, req.Args[2:]...)...)
				command.Env = []string{"PATH=" + bin}
				output, err := command.Output()
				return core.LocalCommandResult{Stdout: string(output)}, err
			})
			runtime := NewServedRuntime(core.Config{}, core.Runtime{Exec: runner})
			key, err := runtime.HostKey(t.Context(), ServedContainer{ID: "container"})
			if fixture.ready {
				if err != nil || key != "ssh-ed25519 AAAA fixture" {
					t.Fatalf("key=%q error=%v", key, err)
				}
			} else if err == nil || key != "" {
				t.Fatalf("toolchain is not ready: key=%q error=%v", key, err)
			}
		})
	}
}

func (r *servedRecordingRunner) Run(_ context.Context, req core.LocalCommandRequest) (core.LocalCommandResult, error) {
	r.args = append([]string(nil), req.Args...)
	return core.LocalCommandResult{Stdout: "container-id\n"}, nil
}
func TestServedCreationUsesSharedBootstrapWithoutHostMounts(t *testing.T) {
	runner := &servedRecordingRunner{}
	cfg := core.Config{LocalContainer: core.LocalContainerConfig{Runtime: "docker", Image: "example:stable", CPUs: 3, Memory: "6g", Network: "host", DockerSocket: true, Volumes: []string{"/:/host"}}, TTL: time.Hour, IdleTimeout: time.Minute}
	r := NewServedRuntime(cfg, core.Runtime{Exec: runner})
	if r.b.cfg.LocalContainer.Network != "bridge" || r.b.cfg.LocalContainer.DockerSocket || len(r.b.cfg.LocalContainer.Volumes) != 0 {
		t.Fatal("served policy inherited unsafe local-container options")
	}
	_, _, err := r.b.createContainerWithOptions(context.Background(), r.b.cfg, "name", "cbx_123456abcdef", "", "ssh-ed25519 AAAA", "", false, &ServedContainerOptions{Deadline: 123, Labels: map[string]string{"provider": "serve", "crabbox.serve": "true", "crabbox.serve.machine": "machine"}})
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Join(runner.args, "\n")
	for _, expected := range []string{"CRABBOX_MANAGED_HOME=1", "--rm", "--cpu-shares\n2", "--cpus\n3", "--memory\n6g", "127.0.0.1::2222", "provider=serve", "crabbox.serve.machine=machine", servedNodeInstallScript, "umask 022", "CRABBOX_SERVE_DEADLINE=123"} {
		if !strings.Contains(args, expected) {
			t.Errorf("missing %q", expected)
		}
	}
	for _, arg := range runner.args {
		if arg == "-v" || arg == "--privileged" || arg == "host" {
			t.Errorf("unsafe argument %q", arg)
		}
	}
}

func TestServedRuntimeRejectsForeignImageBeforeExecution(t *testing.T) {
	runner := &servedRecordingRunner{}
	r := NewServedRuntime(core.Config{LocalContainer: core.LocalContainerConfig{Runtime: "docker", Image: "configured:stable"}}, core.Runtime{Exec: runner})
	r.preparedImageID = "sha256:prepared"
	_, err := r.Create(context.Background(), "cbx_123456abcdef", "ssh-ed25519 AAAA", 1, 1024, "foreign:latest", nil, ServedContainerOptions{})
	if err == nil || !strings.Contains(err.Error(), "serve_image_mismatch") {
		t.Fatal(err)
	}
	if len(runner.args) != 0 {
		t.Fatal("foreign image reached runtime", runner.args)
	}
}

// Served leases regenerate host keys per lease; ordinary local-container leases must keep the shared bootstrap unchanged.
func TestServedHostKeyPolicyStaysOutOfOrdinaryBootstrap(t *testing.T) {
	for _, served := range []string{"ssh-keygen -A", "rm -f /etc/ssh/ssh_host_*"} {
		if strings.Contains(bootstrapScript, served) {
			t.Fatalf("served host-key policy %q leaked into ordinary local-container bootstrap", served)
		}
	}
}
