package serve

import (
	"context"
	"flag"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/localcontainer"
)

func writeCPUFile(t *testing.T, body string, age time.Duration) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "host-cpu")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	at := time.Now().Add(-age)
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestReadHostCPUFile(t *testing.T) {
	for _, tc := range []struct {
		name     string
		body     string
		age      time.Duration
		ok       bool
		fraction float64
		cores    float64
	}{
		{"node usage", "12.5 40\n", 0, true, 0.3125, 40},
		{"over capacity clamps", "44 40", 0, true, 1, 40},
		{"idle", "0 40", 0, true, 0, 40},
		{"stale writer", "12 40", 31 * time.Second, false, 0, 0},
		{"just fresh", "12 40", 29 * time.Second, true, 0.3, 40},
		{"small future skew", "12 40", -3 * time.Second, true, 0.3, 40},
		{"far future mtime", "12 40", -time.Hour, false, 0, 0},
		{"zero cores", "1 0", 0, false, 0, 0},
		{"negative usage", "-1 40", 0, false, 0, 0},
		{"one field", "12", 0, false, 0, 0},
		{"extra field", "1 2 3", 0, false, 0, 0},
		{"not numbers", "busy idle", 0, false, 0, 0},
		{"nan", "NaN 40", 0, false, 0, 0},
		{"empty", "", 0, false, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fraction, cores, ok := readHostCPUFile(writeCPUFile(t, tc.body, tc.age), time.Now())
			if ok != tc.ok || fraction != tc.fraction || cores != tc.cores {
				t.Fatalf("got %v %v %v, want %v %v %v", fraction, cores, ok, tc.fraction, tc.cores, tc.ok)
			}
		})
	}
	if _, _, ok := readHostCPUFile(filepath.Join(t.TempDir(), "missing"), time.Now()); ok {
		t.Fatal("missing file read as valid")
	}
}

// Owned lease CPU is a share of the file's host cores, not of the cores this
// sandboxed process sees.
func TestHostCPUFileSubtractsOwnedCPUFromHostCores(t *testing.T) {
	path := writeCPUFile(t, "20 40", 0)
	h := &hostSampler{cpuFile: path}
	owned := []localcontainer.ServedContainer{{ID: "owned", Running: true, Labels: map[string]string{"crabbox.serve": "true", "crabbox.serve.machine": "machine"}}}
	stats := availabilityStatsFunc(func(_ context.Context, _ []localcontainer.ServedContainer) (float64, error) { return 12, nil })
	signals := fixedHostSignals(0)
	signals.cpu = h.hostCPU
	s := h.sampleSignals(t.Context(), knownInventory(owned), stats, "machine", signals)
	if s.cpu == nil || s.containerCPUUnknown {
		t.Fatalf("missing CPU: %+v", s)
	}
	if want := 0.5 - 12.0/40; *s.cpu != want {
		t.Fatalf("non-serve CPU %v, want %v", *s.cpu, want)
	}
}

func TestNoHostCPUFileKeepsNumCPUDenominator(t *testing.T) {
	if got, want := (&hostSampler{}).hostCores(), float64(runtime.NumCPU()); got != want {
		t.Fatalf("cores %v, want %v", got, want)
	}
}

func TestHostCPUFileUnknownWhenStale(t *testing.T) {
	h := &hostSampler{cpuFile: writeCPUFile(t, "1 40", time.Minute)}
	if got := h.hostCPU(t.Context()); got != nil {
		t.Fatalf("stale file gave %v", *got)
	}
}

func TestHostCPUFileFlagAndEnv(t *testing.T) {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	p, err := availabilityFlags(fs, core.ServeAvailabilityConfig{HostCPUFile: "/from/config"}, false)
	if err != nil || p.hostCPUFile != "/from/config" {
		t.Fatalf("config: %v %q", err, p.hostCPUFile)
	}
	t.Setenv("CRABBOX_SERVE_HOST_CPU_FILE", "/from/env")
	fs = flag.NewFlagSet("serve", flag.ContinueOnError)
	p, err = availabilityFlags(fs, core.ServeAvailabilityConfig{}, false)
	if err != nil || p.hostCPUFile != "/from/env" {
		t.Fatalf("env: %v %q", err, p.hostCPUFile)
	}
	if err := fs.Parse([]string{"--host-cpu-file", "/from/flag"}); err != nil || p.hostCPUFile != "/from/flag" {
		t.Fatalf("flag: %v %q", err, p.hostCPUFile)
	}
}
