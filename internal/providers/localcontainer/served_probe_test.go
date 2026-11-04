package localcontainer

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestServedNodeReadinessExecutesProbe(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("container readiness uses a POSIX shell")
	}
	for _, tc := range []struct {
		name        string
		nodeVersion string
		nodeFails   bool
		apt         bool
		want        string
	}{
		{name: "pinned", nodeVersion: servedNodeVersion, apt: true, want: "ready"},
		{name: "older", nodeVersion: "v22.0.0", apt: true, want: "prepare"},
		{name: "newer", nodeVersion: "v25.0.0", apt: true, want: "prepare"},
		{name: "missing", apt: true, want: "prepare"},
		{name: "broken", nodeVersion: servedNodeVersion, nodeFails: true, apt: true, want: "prepare"},
		{name: "unsupported package manager", nodeVersion: "v22.0.0", want: "skip"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin := t.TempDir()
			writeTool := func(name, body string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			for _, tool := range []string{"sshd", "git", "rsync", "curl", "sudo", "python3", "npm"} {
				writeTool(tool, "exit 0")
			}
			if tc.apt {
				writeTool("apt-get", "exit 91") // Installation must never run in this fixture.
			}
			if tc.nodeVersion != "" {
				body := fmt.Sprintf("printf '%%s\\n' '%s'", tc.nodeVersion)
				if tc.nodeFails {
					body = "exit 1"
				}
				writeTool("node", body)
			}
			var probeOutput string
			preparedLookup := false
			runner := servedRuntimeFunc(func(ctx context.Context, req core.LocalCommandRequest) (core.LocalCommandResult, error) {
				a := req.Args
				if len(a) == 3 && a[0] == "image" && a[1] == "inspect" {
					if a[2] == "configured:stable" {
						return core.LocalCommandResult{Stdout: servedImageFixture("sha256:base")}, nil
					}
					preparedLookup = true
					return core.LocalCommandResult{Stdout: servedImageFixture("sha256:prepared")}, nil
				}
				if len(a) > 2 && a[0] == "run" && a[1] == "--rm" {
					// Map only the container's absolute sshd path into our fixture;
					// all other command lookups and Node execution use the real shell.
					prefix := `command() {
  if [ "$2" = /usr/sbin/sshd ]; then
    [ -x "$FIXTURE_BIN/sshd" ]
  else
    /bin/sh -c 'command -v "$1"' fixture "$2"
  fi
}
`
					cmd := exec.CommandContext(ctx, "/bin/sh", "-c", prefix+a[len(a)-1])
					cmd.Env = []string{"PATH=" + bin, "FIXTURE_BIN=" + bin}
					out, err := cmd.CombinedOutput()
					probeOutput = strings.TrimSpace(string(out))
					return core.LocalCommandResult{Stdout: string(out)}, err
				}
				return core.LocalCommandResult{}, fmt.Errorf("unexpected container command: %v", a)
			})
			r := NewServedRuntime(
				core.Config{LocalContainer: core.LocalContainerConfig{Runtime: "docker", Image: "configured:stable"}},
				core.Runtime{Exec: runner, Stderr: io.Discard},
			)
			if err := r.Prepare(t.Context()); err != nil {
				t.Fatal(err)
			}
			if probeOutput != tc.want {
				t.Fatalf("readiness result = %q, want %q", probeOutput, tc.want)
			}
			if preparedLookup != (tc.want == "prepare") {
				t.Fatalf("prepared image lookup = %v for readiness %q", preparedLookup, probeOutput)
			}
		})
	}
}
