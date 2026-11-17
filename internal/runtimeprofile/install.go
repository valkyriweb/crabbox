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
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Install operates only in a private root supplied by the existing lease owner.
// It downloads nothing, runs no installer, and never starts a harness. The owner
// supplies the reviewed bundle stream and an isolated exact-version probe.
type InstallLimits struct {
	CompressedBytes, ExpandedBytes int64
	Files                          int
}
type VersionProbe func(context.Context, string) (string, error)
type InstalledBundle struct{ ProfileDigest, BundleSHA256, Version, Directory string }
type installedManifest struct {
	ProfileDigest string            `json:"profileDigest"`
	Files         map[string]string `json:"files"`
}

func profileDigest(p Profile) string {
	b, _ := json.Marshal(p)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
func contextCopy(ctx context.Context, dst io.Writer, src io.Reader) (int64, error) {
	return io.Copy(dst, contextReader{ctx, src})
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(b []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(b)
}

func Install(ctx context.Context, p Profile, privateRoot string, bundle io.Reader, limits InstallLimits, probe VersionProbe) (InstalledBundle, error) {
	var result InstalledBundle
	if err := p.Validate(); err != nil {
		return result, err
	}
	if limits.CompressedBytes <= 0 || limits.CompressedBytes > 1<<40 || limits.ExpandedBytes <= 0 || limits.ExpandedBytes > 1<<40 || limits.Files <= 0 || probe == nil || bundle == nil {
		return result, errors.New("explicit bundle limits, stream and version probe required")
	}
	stat, err := os.Lstat(privateRoot)
	if err != nil || !stat.IsDir() || stat.Mode()&os.ModeSymlink != 0 || stat.Mode().Perm()&0077 != 0 {
		return result, errors.New("private owner root required")
	}
	root, err := os.OpenRoot(privateRoot)
	if err != nil {
		return result, err
	}
	defer root.Close()
	digest := profileDigest(p)
	result = InstalledBundle{ProfileDigest: digest, BundleSHA256: p.Bundle.SHA256, Version: p.Bundle.Version, Directory: filepath.Join(privateRoot, digest)}
	stage, err := os.MkdirTemp(privateRoot, ".runtime-stage-")
	if err != nil {
		return InstalledBundle{}, err
	}
	defer os.RemoveAll(stage)
	archive, err := os.OpenFile(filepath.Join(stage, "bundle.tgz"), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return InstalledBundle{}, err
	}
	defer archive.Close()
	hash := sha256.New()
	n, err := contextCopy(ctx, io.MultiWriter(archive, hash), io.LimitReader(bundle, limits.CompressedBytes+1))
	if err != nil || n > limits.CompressedBytes {
		return InstalledBundle{}, errors.New("bundle exceeds compressed limit or read was canceled")
	}
	if hex.EncodeToString(hash.Sum(nil)) != p.Bundle.SHA256 {
		return InstalledBundle{}, errors.New("runtime bundle checksum mismatch")
	}
	if _, err = archive.Seek(0, io.SeekStart); err != nil {
		return InstalledBundle{}, err
	}
	payload := filepath.Join(stage, "payload")
	if err = os.Mkdir(payload, 0700); err != nil {
		return InstalledBundle{}, err
	}
	confined, err := os.OpenRoot(payload)
	if err != nil {
		return InstalledBundle{}, err
	}
	defer confined.Close()
	files, err := extractBundle(ctx, archive, confined, limits)
	if err != nil {
		return InstalledBundle{}, err
	}
	execInfo, err := confined.Lstat(p.Bundle.Executable)
	if err != nil || !execInfo.Mode().IsRegular() || execInfo.Mode().Perm()&0100 == 0 {
		return InstalledBundle{}, errors.New("reviewed runtime executable is absent or nonexecutable")
	}
	manifest, _ := json.Marshal(installedManifest{ProfileDigest: digest, Files: files})
	if len(manifest) > 1<<20 {
		return InstalledBundle{}, errors.New("runtime receipt exceeds bound")
	}
	// Rebuild the expected manifest from the checksum-pinned archive even on
	// reuse. A writable receipt can never become its own integrity authority.
	if _, err := root.Lstat(digest); err == nil {
		if err = verifyInstalled(ctx, root, digest, p, digest, limits, manifest); err != nil {
			return InstalledBundle{}, err
		}
		actual, err := probe(ctx, filepath.Join(result.Directory, filepath.FromSlash(p.Bundle.Executable)))
		if err != nil || actual != p.Bundle.Version {
			return InstalledBundle{}, errors.New("installed runtime exact-version probe failed")
		}
		if err = ctx.Err(); err != nil {
			return InstalledBundle{}, err
		}
		if err = verifyInstalled(ctx, root, digest, p, digest, limits, manifest); err != nil {
			return InstalledBundle{}, err
		}
		return result, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return InstalledBundle{}, err
	}
	actual, err := probe(ctx, filepath.Join(payload, filepath.FromSlash(p.Bundle.Executable)))
	if err != nil || actual != p.Bundle.Version {
		return InstalledBundle{}, errors.New("runtime exact-version probe failed")
	}
	if err = ctx.Err(); err != nil {
		return InstalledBundle{}, err
	}
	receipt, err := confined.OpenFile(".bundle-receipt.json", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return InstalledBundle{}, err
	}
	_, err = receipt.Write(manifest)
	if err == nil {
		err = receipt.Sync()
	}
	closeErr := receipt.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return InstalledBundle{}, err
	}
	stageRoot, err := os.OpenRoot(stage)
	if err != nil {
		return InstalledBundle{}, err
	}
	err = verifyInstalled(ctx, stageRoot, "payload", p, digest, limits, manifest)
	stageRoot.Close()
	if err != nil {
		return InstalledBundle{}, err
	}
	// Renaming a nonempty immutable directory never replaces another completed
	// installation. A racing owner must verify the already-published tree.
	if err = os.Rename(payload, result.Directory); err != nil {
		if verifyErr := verifyInstalled(ctx, root, digest, p, digest, limits, manifest); verifyErr != nil {
			return InstalledBundle{}, fmt.Errorf("runtime publication failed: %w", err)
		}
	}
	return result, nil
}

func extractBundle(ctx context.Context, source io.Reader, root *os.Root, limits InstallLimits) (map[string]string, error) {
	zipped, err := gzip.NewReader(source)
	if err != nil {
		return nil, errors.New("runtime bundle must be gzip tar")
	}
	defer zipped.Close()
	// Limit all decompressed bytes, including headers, padding and junk after tar.
	limited := &io.LimitedReader{R: contextReader{ctx, zipped}, N: limits.ExpandedBytes + 1}
	archive := tar.NewReader(limited)
	files := map[string]string{}
	seen := map[string]bool{}
	count := 0
	logicalBytes := int64(0)
	for {
		header, err := archive.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, errors.New("invalid or oversized runtime archive")
		}
		count++
		if count > limits.Files {
			return nil, errors.New("runtime archive entry limit exceeded")
		}
		name := header.Name
		if header.Typeflag == tar.TypeDir {
			name = strings.TrimSuffix(name, "/")
		}
		if len(name) > 1024 || !relative(name) || name == ".bundle-receipt.json" || seen[name] {
			return nil, errors.New("unsafe or duplicate runtime archive path")
		}
		seen[name] = true
		if header.Typeflag == tar.TypeDir {
			if err = root.MkdirAll(name, 0700); err != nil {
				return nil, err
			}
			continue
		}
		if header.Typeflag != tar.TypeReg || header.Size < 0 || header.Size > limits.ExpandedBytes {
			return nil, errors.New("runtime archive permits only regular files/directories")
		}
		if header.Size > limits.ExpandedBytes-logicalBytes {
			return nil, errors.New("runtime logical byte limit exceeded")
		}
		logicalBytes += header.Size
		if err = root.MkdirAll(filepath.Dir(name), 0700); err != nil {
			return nil, err
		}
		file, err := root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600|os.FileMode(header.Mode)&0100)
		if err != nil {
			return nil, err
		}
		hash := sha256.New()
		_, err = contextCopy(ctx, io.MultiWriter(file, hash), archive)
		if err == nil {
			err = file.Sync()
		}
		closeErr := file.Close()
		if err == nil {
			err = closeErr
		}
		if err != nil {
			return nil, err
		}
		files[name] = hex.EncodeToString(hash.Sum(nil))
	}
	if _, err = io.Copy(io.Discard, limited); err != nil || limited.N <= 0 {
		return nil, errors.New("runtime expanded byte limit exceeded")
	}
	if len(files) == 0 {
		return nil, errors.New("empty runtime bundle")
	}
	return files, nil
}

func verifyInstalled(ctx context.Context, root *os.Root, directory string, p Profile, digest string, limits InstallLimits, expected []byte) error {
	info, err := root.Lstat(directory)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("runtime publication is not an owned directory")
	}
	tree, err := root.OpenRoot(directory)
	if err != nil {
		return err
	}
	defer tree.Close()
	file, err := tree.Open(".bundle-receipt.json")
	if err != nil {
		return errors.New("runtime receipt absent")
	}
	defer file.Close()
	receipt, err := io.ReadAll(io.LimitReader(file, (1<<20)+1))
	if err != nil || !bytes.Equal(receipt, expected) {
		return errors.New("runtime receipt differs from pinned bundle")
	}
	var manifest installedManifest
	decoder := json.NewDecoder(bytes.NewReader(expected))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&manifest); err != nil || manifest.ProfileDigest != digest || len(manifest.Files) == 0 {
		return errors.New("runtime receipt changed")
	}
	if err := fs.WalkDir(tree.FS(), ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return errors.New("unexpected runtime symlink")
		}
		if !entry.IsDir() && name != ".bundle-receipt.json" {
			if _, ok := manifest.Files[name]; !ok {
				return errors.New("unexpected installed runtime file")
			}
		}
		return nil
	}); err != nil {
		return err
	}
	total := int64(0)
	var names []string
	for name := range manifest.Files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if !relative(name) {
			return errors.New("unsafe runtime receipt path")
		}
		info, err := tree.Lstat(name)
		if err != nil || !info.Mode().IsRegular() {
			return errors.New("installed runtime file changed")
		}
		total += info.Size()
		if total > limits.ExpandedBytes || len(names) > limits.Files {
			return errors.New("installed runtime exceeds bounds")
		}
		file, err := tree.Open(name)
		if err != nil {
			return err
		}
		hash := sha256.New()
		_, err = contextCopy(ctx, hash, io.LimitReader(file, info.Size()+1))
		file.Close()
		if err != nil || hex.EncodeToString(hash.Sum(nil)) != manifest.Files[name] {
			return errors.New("installed runtime checksum changed")
		}
	}
	info, err = tree.Lstat(p.Bundle.Executable)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0100 == 0 {
		return errors.New("installed runtime executable changed")
	}
	return nil
}
