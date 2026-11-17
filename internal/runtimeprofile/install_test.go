package runtimeprofile

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func bundleFixture(t *testing.T, headers ...*tar.Header) (Profile, []byte) {
	t.Helper()
	var data bytes.Buffer
	zip := gzip.NewWriter(&data)
	archive := tar.NewWriter(zip)
	for _, h := range headers {
		if err := archive.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			if _, err := archive.Write(bytes.Repeat([]byte("x"), int(h.Size))); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zip.Close(); err != nil {
		t.Fatal(err)
	}
	p := fixture()
	sum := sha256.Sum256(data.Bytes())
	p.Bundle.SHA256 = hex.EncodeToString(sum[:])
	return p, data.Bytes()
}
func installRoot(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "owned")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	return root
}
func installLimits() InstallLimits {
	return InstallLimits{CompressedBytes: 1 << 20, ExpandedBytes: 1 << 20, Files: 20}
}
func TestInstallVerifiedBundleIdempotenceAndTamperDetection(t *testing.T) {
	p, b := bundleFixture(t, &tar.Header{Name: "bin/agent", Mode: 0700, Typeflag: tar.TypeReg, Size: 8})
	root := installRoot(t)
	calls := 0
	probe := func(ctx context.Context, path string) (string, error) {
		calls++
		if data, err := os.ReadFile(path); err != nil || string(data) != "xxxxxxxx" {
			t.Fatal("unverified executable", err)
		}
		return p.Bundle.Version, nil
	}
	first, err := Install(context.Background(), p, root, bytes.NewReader(b), installLimits(), probe)
	if err != nil {
		t.Fatal(err)
	}
	again, err := Install(context.Background(), p, root, nil, installLimits(), probe)
	if err == nil {
		t.Fatal("missing stream contract accepted")
	}
	again, err = Install(context.Background(), p, root, bytes.NewReader(b), installLimits(), probe)
	if err != nil || again != first || calls != 2 {
		t.Fatal("idempotent installation", err)
	}
	if err = os.WriteFile(filepath.Join(first.Directory, "bin/agent"), []byte("changed"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err = Install(context.Background(), p, root, bytes.NewReader(b), installLimits(), probe); err == nil {
		t.Fatal("tampered installation reused")
	}
}
func TestInstallRejectsUnsafeArchivesBeforeProbe(t *testing.T) {
	for _, header := range []*tar.Header{{Name: "../escape", Typeflag: tar.TypeReg, Size: 1}, {Name: "/absolute", Typeflag: tar.TypeReg, Size: 1}, {Name: "bin/agent", Typeflag: tar.TypeSymlink, Linkname: "outside"}, {Name: "bin/agent", Typeflag: tar.TypeLink, Linkname: "other"}, {Name: ".bundle-receipt.json", Typeflag: tar.TypeReg, Size: 1}} {
		t.Run(strings.ReplaceAll(header.Name, "/", "_"), func(t *testing.T) {
			p, b := bundleFixture(t, header)
			called := false
			root := installRoot(t)
			_, err := Install(context.Background(), p, root, bytes.NewReader(b), installLimits(), func(context.Context, string) (string, error) { called = true; return p.Bundle.Version, nil })
			if err == nil || called {
				t.Fatal("unsafe archive admitted", err)
			}
			entries, _ := os.ReadDir(root)
			if len(entries) != 0 {
				t.Fatal("staging leaked")
			}
		})
	}
}
func TestInstallLimitsHashVersionCancellationAndDuplicatePaths(t *testing.T) {
	p, b := bundleFixture(t, &tar.Header{Name: "bin/agent", Mode: 0700, Typeflag: tar.TypeReg, Size: 8})
	for _, kind := range []string{"checksum", "compressed", "expanded", "files", "version", "cancel"} {
		t.Run(kind, func(t *testing.T) {
			candidate := p
			limits := installLimits()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch kind {
			case "checksum":
				candidate.Bundle.SHA256 = strings.Repeat("0", 64)
			case "compressed":
				limits.CompressedBytes = 1
			case "expanded":
				limits.ExpandedBytes = 1
			case "files":
				limits.Files = 0
			case "cancel":
				cancel()
			}
			root := installRoot(t)
			_, err := Install(ctx, candidate, root, bytes.NewReader(b), limits, func(context.Context, string) (string, error) { return "wrong", errors.New("fixture") })
			if err == nil {
				t.Fatal("failure accepted")
			}
			entries, _ := os.ReadDir(root)
			if len(entries) != 0 {
				t.Fatal("staging leaked")
			}
		})
	}
	p, b = bundleFixture(t, &tar.Header{Name: "bin/agent", Mode: 0700, Typeflag: tar.TypeReg, Size: 1}, &tar.Header{Name: "bin/agent", Mode: 0700, Typeflag: tar.TypeReg, Size: 1})
	if _, err := Install(context.Background(), p, installRoot(t), bytes.NewReader(b), installLimits(), func(context.Context, string) (string, error) { t.Fatal("duplicate reached probe"); return "", nil }); err == nil {
		t.Fatal("duplicate accepted")
	}
}

func TestProbeCannotPublishModifiedOrAdditionalFiles(t *testing.T) {
	for _, kind := range []string{"changed", "added"} {
		t.Run(kind, func(t *testing.T) {
			p, b := bundleFixture(t, &tar.Header{Name: "bin/agent", Mode: 0700, Typeflag: tar.TypeReg, Size: 8})
			root := installRoot(t)
			_, err := Install(context.Background(), p, root, bytes.NewReader(b), installLimits(), func(_ context.Context, path string) (string, error) {
				target := path
				if kind == "added" {
					target = filepath.Join(filepath.Dir(path), "unexpected")
				}
				if err := os.WriteFile(target, []byte("changed"), 0600); err != nil {
					t.Fatal(err)
				}
				return p.Bundle.Version, nil
			})
			if err == nil {
				t.Fatal("probe modified verified tree")
			}
			entries, _ := os.ReadDir(root)
			if len(entries) != 0 {
				t.Fatal("failed install retained")
			}
		})
	}
}

func TestInstallConventionalDirectoryHeaders(t *testing.T) {
	p, b := bundleFixture(t, &tar.Header{Name: "bin/", Typeflag: tar.TypeDir, Mode: 0700}, &tar.Header{Name: "bin/agent", Typeflag: tar.TypeReg, Mode: 0700, Size: 8})
	if _, err := Install(context.Background(), p, installRoot(t), bytes.NewReader(b), installLimits(), func(context.Context, string) (string, error) { return p.Bundle.Version, nil }); err != nil {
		t.Fatal(err)
	}
}

// Encode a tiny PAX sparse tar directly: archive/tar reads sparse files but its
// public writer deliberately does not produce them.
func TestExtractionBoundsSparseLogicalBytesBeforeSecondFile(t *testing.T) {
	var raw bytes.Buffer
	block := func(name string, kind byte, size int64) []byte {
		var b bytes.Buffer
		w := tar.NewWriter(&b)
		if err := w.WriteHeader(&tar.Header{Name: name, Mode: 0700, Typeflag: tar.TypeReg, Size: size, Format: tar.FormatUSTAR}); err != nil {
			t.Fatal(err)
		}
		h := append([]byte(nil), b.Bytes()[:512]...)
		h[156] = kind
		for i := 148; i < 156; i++ {
			h[i] = ' '
		}
		sum := 0
		for _, c := range h {
			sum += int(c)
		}
		copy(h[148:156], []byte(fmt.Sprintf("%06o\x00 ", sum)))
		return h
	}
	record := func(key, value string) string {
		body := key + "=" + value + "\n"
		n := len(body) + 2
		for {
			text := fmt.Sprintf("%d %s", n, body)
			if len(text) == n {
				return text
			}
			n = len(text)
		}
	}
	for _, name := range []string{"first", "second"} {
		pax := record("GNU.sparse.map", "0,1") + record("GNU.sparse.size", "8192") + record("GNU.sparse.numblocks", "1")
		raw.Write(block("PaxHeaders/"+name, tar.TypeXHeader, int64(len(pax))))
		raw.WriteString(pax)
		raw.Write(make([]byte, (512-len(pax)%512)%512))
		raw.Write(block(name, tar.TypeReg, 1))
		raw.WriteByte('x')
		raw.Write(make([]byte, 511))
	}
	raw.Write(make([]byte, 1024))
	var compressed bytes.Buffer
	zip := gzip.NewWriter(&compressed)
	zip.Write(raw.Bytes())
	zip.Close()
	directory := installRoot(t)
	root, err := os.OpenRoot(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	_, err = extractBundle(context.Background(), bytes.NewReader(compressed.Bytes()), root, InstallLimits{CompressedBytes: 1 << 20, ExpandedBytes: 12000, Files: 10})
	if err == nil || !strings.Contains(err.Error(), "logical byte limit") {
		t.Fatal("missing logical extraction bound", err)
	}
	first, err := os.Stat(filepath.Join(directory, "first"))
	if err != nil || first.Size() != 8192 {
		t.Fatal("fixture was not decoded as sparse", err)
	}
	if _, err = os.Stat(filepath.Join(directory, "second")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("over-budget sparse file was created", err)
	}
}

func TestReuseProbeCannotInvalidateVerifiedTree(t *testing.T) {
	for _, kind := range []string{"modified", "added"} {
		t.Run(kind, func(t *testing.T) {
			p, b := bundleFixture(t, &tar.Header{Name: "bin/agent", Mode: 0700, Typeflag: tar.TypeReg, Size: 8})
			root := installRoot(t)
			probe := func(context.Context, string) (string, error) { return p.Bundle.Version, nil }
			if _, err := Install(context.Background(), p, root, bytes.NewReader(b), installLimits(), probe); err != nil {
				t.Fatal(err)
			}
			_, err := Install(context.Background(), p, root, bytes.NewReader(b), installLimits(), func(_ context.Context, path string) (string, error) {
				if kind == "added" {
					path = filepath.Join(filepath.Dir(path), "unexpected")
				}
				if err := os.WriteFile(path, []byte("changed"), 0700); err != nil {
					t.Fatal(err)
				}
				return p.Bundle.Version, nil
			})
			if err == nil {
				t.Fatal("reuse probe invalidated verified installation")
			}
		})
	}
}

func TestReuseRejectsFileAndReceiptRewrite(t *testing.T) {
	for _, duringProbe := range []bool{false, true} {
		t.Run(fmt.Sprint(duringProbe), func(t *testing.T) {
			p, b := bundleFixture(t, &tar.Header{Name: "bin/agent", Mode: 0700, Typeflag: tar.TypeReg, Size: 8})
			root := installRoot(t)
			probe := func(context.Context, string) (string, error) { return p.Bundle.Version, nil }
			installed, err := Install(context.Background(), p, root, bytes.NewReader(b), installLimits(), probe)
			if err != nil {
				t.Fatal(err)
			}
			rewrite := func() {
				receiptPath := filepath.Join(installed.Directory, ".bundle-receipt.json")
				data, err := os.ReadFile(receiptPath)
				if err != nil {
					t.Fatal(err)
				}
				var receipt installedManifest
				if err = json.Unmarshal(data, &receipt); err != nil {
					t.Fatal(err)
				}
				changed := []byte("changed")
				sum := sha256.Sum256(changed)
				receipt.Files["bin/agent"] = hex.EncodeToString(sum[:])
				data, _ = json.Marshal(receipt)
				if err = os.WriteFile(filepath.Join(installed.Directory, "bin/agent"), changed, 0700); err != nil {
					t.Fatal(err)
				}
				if err = os.WriteFile(receiptPath, data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if duringProbe {
				probe = func(context.Context, string) (string, error) { rewrite(); return p.Bundle.Version, nil }
			} else {
				rewrite()
			}
			if _, err = Install(context.Background(), p, root, bytes.NewReader(b), installLimits(), probe); err == nil {
				t.Fatal("rewritten payload/receipt replaced pinned archive authority")
			}
		})
	}
}
