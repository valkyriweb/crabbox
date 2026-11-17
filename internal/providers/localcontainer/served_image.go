package localcontainer

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
)

// v2 adds the pinned Node.js toolchain; v3 adds the C and Rust toolchains;
// v4 adds jq; v5 adds Go, pip, venv, pytest, pnpm and the shared libraries
// headless browsers need; v6 keeps the apt package lists so a lease can run
// apt-get install without apt-get update first.
// Each bump invalidates older prepared images.
const servedPreparationRecipe = "v6"

// servedToolchainPackages are installed only into prepared serve images, on
// top of bootstrapPackages, so leases can build C, Rust and Go code, run Python
// tests in a venv or with the distribution pytest, and process JSON with jq.
// Go, pip and pytest come from the base distribution, not from a pinned release.
const servedToolchainPackages = "build-essential pkg-config libssl-dev xz-utils unzip jq golang-go python3-pip python3-venv python3-pytest"

// servedToolchainTools are the commands the readiness probe requires before it
// treats a base image as already prepared; it also requires OpenSSL headers
// (pkg-config openssl), working cargo/rustc, the pinned pnpm and a Python that
// can create a venv.
const servedToolchainTools = "cc make pkg-config xz unzip jq cargo rustc go pip3 pytest pnpm"

// servedBrowserLibraries are the shared libraries and one metric-compatible
// font family that headless Chromium needs. Browser binaries are not baked in:
// they are large and tied to the version of the test framework a repository
// pins, so a lease downloads them (a serve cache volume keeps them between
// leases). The names are the pre-64-bit-time_t ones; the install script picks
// the "t64" variant where the distribution renamed the package.
const servedBrowserLibraries = "libasound2 libatk-bridge2.0-0 libatk1.0-0 libatspi2.0-0 libcairo2 libcups2 libdbus-1-3 libdrm2 libfontconfig1 libfreetype6 libgbm1 libglib2.0-0 libnspr4 libnss3 libpango-1.0-0 libx11-6 libxcb1 libxcomposite1 libxdamage1 libxext6 libxfixes3 libxkbcommon0 libxrandr2 fonts-liberation"

// servedBrowserLibrariesInstallScript installs servedBrowserLibraries. It runs
// after apt-get update.
const servedBrowserLibrariesInstallScript = `browser_packages=""
for browser_package in ` + servedBrowserLibraries + `; do
  if apt-cache show "${browser_package}t64" >/dev/null 2>&1; then browser_package="${browser_package}t64"; fi
  browser_packages="$browser_packages $browser_package"
done
apt-get install -y --no-install-recommends $browser_packages`

// servedPnpmVersion is the pinned pnpm release; the readiness probe and the
// install script must agree on it. This line of pnpm is plain JavaScript, so
// one tarball serves every architecture. A repository that pins another pnpm
// in its packageManager field gets that version from pnpm itself.
const servedPnpmVersion = "11.28.12"

// servedPnpmInstallScript installs the pinned, SHA256-verified pnpm tarball
// into the pinned Node.js prefix without running package scripts, and links it
// into /usr/local/bin. It runs after servedNodeInstallScript. It is idempotent
// and fails closed (set -eu).
const servedPnpmInstallScript = `pnpm_version=` + servedPnpmVersion + `
pnpm_sha256=d0c42f9232c9685056b432f8b07631bbabd245d40ede9a982919c7a7ada40aa7
if [ "$(/opt/node/bin/pnpm --version 2>/dev/null || true)" != "$pnpm_version" ]; then
  pnpm_tmp="$(mktemp -d)"
  curl -fsSL --retry 3 -o "$pnpm_tmp/pnpm.tgz" "https://registry.npmjs.org/pnpm/-/pnpm-$pnpm_version.tgz"
  echo "$pnpm_sha256  $pnpm_tmp/pnpm.tgz" | sha256sum -c -
  npm_config_cache="$pnpm_tmp/cache" npm_config_update_notifier=false /opt/node/bin/npm install -g --prefix /opt/node --no-audit --no-fund --ignore-scripts "$pnpm_tmp/pnpm.tgz"
  rm -rf "$pnpm_tmp"
fi
for pnpm_bin in pnpm pnpx; do ln -sf "/opt/node/bin/$pnpm_bin" "/usr/local/bin/$pnpm_bin"; done
[ "$(/usr/local/bin/pnpm --version)" = "$pnpm_version" ]`

// servedRustupVersion pins rustup-init; the stable toolchain it installs is
// shared read-only under /opt/rust.
const servedRustupVersion = "1.29.3"

// servedRustInstallScript installs a pinned, SHA256-verified rustup-init and
// the minimal stable toolchain under /opt/rust as root. The toolchain binaries
// themselves (not rustup proxies) are linked into /usr/local/bin, so lease users
// run cargo and rustc without RUSTUP_HOME and keep a per-user ~/.cargo for the
// registry and build cache. The image environment is left unchanged. It is
// idempotent and fails closed (set -eu).
const servedRustInstallScript = `rustup_version=` + servedRustupVersion + `
case "$(uname -m)" in
  x86_64|amd64) rust_target=x86_64-unknown-linux-gnu; rustup_sha256=b1c8cb14e30889e7e2999e6b69e62d9e9b9ffc0f4df8180564d306d9680e6c55 ;;
  aarch64|arm64) rust_target=aarch64-unknown-linux-gnu; rustup_sha256=9f608c142e0e38a78957d0661ba1dfaf0b6fb37184cfaa4b9a0aa57af6184282 ;;
  *) echo "unsupported architecture for Rust: $(uname -m)" >&2; exit 1 ;;
esac
export RUSTUP_HOME=/opt/rust/rustup CARGO_HOME=/opt/rust/cargo
if ! /opt/rust/cargo/bin/rustup which cargo >/dev/null 2>&1; then
  rust_tmp="$(mktemp -d)"
  curl -fsSL --retry 3 -o "$rust_tmp/rustup-init" "https://static.rust-lang.org/rustup/archive/$rustup_version/$rust_target/rustup-init"
  echo "$rustup_sha256  $rust_tmp/rustup-init" | sha256sum -c -
  chmod 0755 "$rust_tmp/rustup-init"
  "$rust_tmp/rustup-init" -y --no-modify-path --profile minimal --default-toolchain stable
  rm -rf "$rust_tmp"
fi
rust_bin="$(dirname "$(/opt/rust/cargo/bin/rustup which cargo)")"
for rust_tool in cargo rustc rustdoc rustfmt; do
  if [ -x "$rust_bin/$rust_tool" ]; then ln -sf "$rust_bin/$rust_tool" "/usr/local/bin/$rust_tool"; fi
done
ln -sf /opt/rust/cargo/bin/rustup /usr/local/bin/rustup
rm -rf /opt/rust/cargo/registry /opt/rust/cargo/git
chmod -R a+rX,go-w /opt/rust
unset RUSTUP_HOME CARGO_HOME
/usr/local/bin/cargo --version >/dev/null
/usr/local/bin/rustc --version >/dev/null`

// servedNodeVersion is the pinned Node.js release; the readiness probe and the
// install script must agree on it.
const servedNodeVersion = "v24.22.1"

// servedNodeInstallScript installs the pinned official Node.js tarball under
// /opt/node and links it into /usr/local/bin, which is on PATH for login and
// non-login SSH sessions. It is idempotent and fails closed (set -eu) when the
// architecture is unsupported or the SHA256 does not match.
const servedNodeInstallScript = `node_version=` + servedNodeVersion + `
case "$(uname -m)" in
  x86_64|amd64) node_arch=x64; node_sha256=eb6728e77c576a35162c122c45ccadf224d89ff00abc8dae46cc2897c6002d7c ;;
  aarch64|arm64) node_arch=arm64; node_sha256=33c37be234ba8bee041961047b19160f4d7d0f164e8e246f78bdd10feff148b3 ;;
  *) echo "unsupported architecture for Node.js: $(uname -m)" >&2; exit 1 ;;
esac
if [ ! -x /opt/node/bin/node ] || [ ! -x /opt/node/bin/npm ] || [ "$(/opt/node/bin/node --version)" != "$node_version" ]; then
  node_tmp="$(mktemp -d)"
  curl -fsSL --retry 3 -o "$node_tmp/node.tgz" "https://nodejs.org/dist/$node_version/node-$node_version-linux-$node_arch.tar.gz"
  echo "$node_sha256  $node_tmp/node.tgz" | sha256sum -c -
  rm -rf /opt/node
  mkdir -p /opt/node
  tar -xzf "$node_tmp/node.tgz" -C /opt/node --strip-components=1
  rm -rf "$node_tmp"
fi
for node_bin in node npm npx corepack; do ln -sf "/opt/node/bin/$node_bin" "/usr/local/bin/$node_bin"; done
/usr/local/bin/node --version >/dev/null`

type servedImage struct {
	ID     string `json:"Id"`
	Config struct {
		Cmd        []string
		Entrypoint []string
		User       string
		Env        []string
	}
}

func (r *ServedRuntime) inspectImage(ctx context.Context, ref string) (servedImage, error) {
	result, err := r.b.docker(ctx, []string{"image", "inspect", ref}, nil, nil)
	if err != nil {
		return servedImage{}, err
	}
	var images []servedImage
	if err := json.Unmarshal([]byte(result.Stdout), &images); err != nil {
		return servedImage{}, err
	}
	if len(images) != 1 || images[0].ID == "" {
		return servedImage{}, fmt.Errorf("missing image identity for %s", ref)
	}
	return images[0], nil
}

func servedPreparedTag(baseID string) string {
	sum := sha256.Sum256([]byte(baseID + "\n" + bootstrapPackages + " " + servedToolchainPackages + "\n" + servedPreparationRecipe))
	return fmt.Sprintf("crabbox-serve-prepared:%x", sum[:8])
}

func (r *ServedRuntime) Prepare(ctx context.Context) error {
	if err := r.EnsureImage(ctx); err != nil {
		return err
	}
	r.StartPreparation(ctx)
	r.WaitPreparation()
	return nil
}

// EnsureImage retains startup's fatal configured-image validation, without
// making reconciliation wait for optional package installation.
func (r *ServedRuntime) EnsureImage(ctx context.Context) error {
	r.imageMu.Lock()
	r.preparedImageID = ""
	r.baseImageID = ""
	r.baseImage = servedImage{}
	r.imageMu.Unlock()
	base, err := r.inspectImage(ctx, r.Image())
	if err != nil {
		if _, err := r.b.docker(ctx, []string{"pull", r.Image()}, r.b.rt.Stderr, r.b.rt.Stderr); err != nil {
			return err
		}
		base, err = r.inspectImage(ctx, r.Image())
		if err != nil {
			return err
		}
	}
	r.imageMu.Lock()
	r.baseImageID, r.baseImage = base.ID, base
	r.imageMu.Unlock()
	return nil
}

// servedPreparationRetryInterval is the pause between a failed preparation and
// the next attempt.
var servedPreparationRetryInterval = 10 * time.Minute

// servedPreparationTimeout bounds one preparation attempt, including the
// docker commit. Twenty minutes was too short under nested Docker in gVisor
// (lue-dl360-worker-01): the install took about 12 minutes and the commit of
// the ~2 GB layer was still running when the deadline killed it, every retry.
// CRABBOX_SERVE_PREPARE_TIMEOUT (a Go duration, e.g. "90m") overrides it.
var servedPreparationTimeout = 60 * time.Minute

func servedPreparationDeadline() time.Duration {
	if raw := strings.TrimSpace(os.Getenv("CRABBOX_SERVE_PREPARE_TIMEOUT")); raw != "" {
		if d, err := time.ParseDuration(raw); err == nil && d > 0 {
			return d
		}
	}
	return servedPreparationTimeout
}

// StartPreparation uses the agent lifetime, never a single lease's deadline.
// Only one preparation runs per agent; image selection remains nonblocking.
func (r *ServedRuntime) StartPreparation(ctx context.Context) {
	r.imageMu.Lock()
	if r.preparing || ctx.Err() != nil || r.baseImage.ID == "" {
		r.imageMu.Unlock()
		return
	}
	r.preparing = true
	r.preparationCtx = ctx
	r.preparationDone = make(chan struct{})
	base, done := r.baseImage, r.preparationDone
	r.imageMu.Unlock()
	go func() {
		bounded, cancel := context.WithTimeout(ctx, servedPreparationDeadline())
		defer cancel()
		id, err := r.prepareImage(bounded, base)
		if err != nil && r.b.rt.Stderr != nil {
			fmt.Fprintf(r.b.rt.Stderr, "warning: serve image preparation failed; leases will bootstrap at start: %s\n", strings.ReplaceAll(err.Error(), "\n", "; "))
		}
		r.imageMu.Lock()
		if err == nil {
			r.preparedImageID = id
		}
		r.preparing = false
		close(done)
		r.imageMu.Unlock()
		// A package mirror that was down at agent start must not leave the
		// machine without its prepared image until the next agent restart.
		if err != nil && ctx.Err() == nil {
			time.AfterFunc(servedPreparationRetryInterval, func() { r.retryPreparation(ctx) })
		}
	}()
}

// retryPreparation runs after a failed preparation. It does nothing when the
// agent stopped, the base image changed hands to a newer preparation, or
// another path already produced the prepared image.
func (r *ServedRuntime) retryPreparation(ctx context.Context) {
	r.imageMu.RLock()
	current := r.preparationCtx == ctx && r.preparedImageID == ""
	r.imageMu.RUnlock()
	if current {
		r.StartPreparation(ctx)
	}
}

func (r *ServedRuntime) WaitPreparation() {
	r.imageMu.RLock()
	done := r.preparationDone
	r.imageMu.RUnlock()
	if done != nil {
		<-done
	}
}

func (r *ServedRuntime) preparedImageMissing(ctx context.Context, id string) bool {
	result, err := r.b.docker(ctx, []string{"image", "inspect", id}, nil, nil)
	if err == nil {
		return false
	}
	message := strings.ToLower(result.Stderr + "\n" + err.Error())
	return strings.Contains(message, "no such image") || strings.Contains(message, "image not known")
}

func (r *ServedRuntime) cleanupPreparationContainer(name string) error {
	cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := r.b.docker(cleanup, []string{"rm", "-f", name}, nil, r.b.rt.Stderr)
	return err
}

func (r *ServedRuntime) prepareImage(ctx context.Context, base servedImage) (id string, err error) {
	probe := "for tool in " + bootstrapTools + " " + servedToolchainTools + " node npm; do command -v \"$tool\" >/dev/null 2>&1 || { command -v apt-get >/dev/null 2>&1 && { echo prepare; exit 0; }; echo skip; exit 0; }; done; [ \"$(node --version 2>/dev/null)\" = \"" + servedNodeVersion + "\" ] || { command -v apt-get >/dev/null 2>&1 && { echo prepare; exit 0; }; echo skip; exit 0; }; { pkg-config --exists openssl && cargo --version && rustc --version && python3 -c 'import ensurepip, venv'; } >/dev/null 2>&1 || { command -v apt-get >/dev/null 2>&1 && { echo prepare; exit 0; }; echo skip; exit 0; }; [ \"$(pnpm --version 2>/dev/null)\" = \"" + servedPnpmVersion + "\" ] || { command -v apt-get >/dev/null 2>&1 && { echo prepare; exit 0; }; echo skip; exit 0; }; echo ready"
	probeName := "crabbox-serve-probe-" + strings.ToLower(rand.Text())
	result, err := r.b.docker(ctx, []string{"run", "--rm", "--name", probeName, "--entrypoint", "/bin/sh", base.ID, "-c", probe}, nil, r.b.rt.Stderr)
	if err != nil {
		return "", errors.Join(err, r.cleanupPreparationContainer(probeName))
	}
	switch strings.TrimSpace(result.Stdout) {
	case "ready", "skip":
		return "", nil
	case "prepare":
	default:
		return "", fmt.Errorf("unexpected serve image tool probe: %q", result.Stdout)
	}
	tag := servedPreparedTag(base.ID)
	if prepared, err := r.inspectImage(ctx, tag); err == nil {
		return prepared.ID, nil
	}
	name := "crabbox-serve-prepare-" + strings.ToLower(rand.Text())
	committed := false
	defer func() {
		err = errors.Join(err, r.cleanupPreparationContainer(name))
		if err != nil && committed {
			cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			_, removeErr := r.b.docker(cleanup, []string{"image", "rm", tag}, nil, r.b.rt.Stderr)
			err = errors.Join(err, removeErr)
		}
	}()
	args := []string{"run", "--name", name}
	// Podman injects container=podman into every container's environment and
	// commit persists it, which fails the base-environment check below.
	// Keep an image-defined container= value intact.
	if isPodmanRuntime(r.b.cfg.LocalContainer.Runtime) && !slices.ContainsFunc(base.Config.Env, func(env string) bool { return strings.HasPrefix(env, "container=") }) {
		args = append(args, "--unsetenv", "container")
	}
	// Podman also copies the agent's HTTP(S)_PROXY/NO_PROXY variables into the
	// container (http_proxy=true by default) and commit persists them, which
	// fails the same check whenever the agent runs behind a proxy.
	if isPodmanRuntime(r.b.cfg.LocalContainer.Runtime) {
		args = append(args, "--http-proxy=false")
	}
	// Avoid overriding empty defaults: Docker commit merges empty --change values
	// back from the temporary container rather than clearing them.
	if base.Config.User != "" {
		args = append(args, "--user", "root")
	}
	if len(base.Config.Entrypoint) != 0 {
		args = append(args, "--entrypoint", "/bin/sh")
	}
	args = append(args, base.ID)
	if len(base.Config.Entrypoint) == 0 {
		args = append(args, "/bin/sh")
	}
	script := "set -eu; export DEBIAN_FRONTEND=noninteractive; apt-get update; apt-get install -y --no-install-recommends " + bootstrapPackages + " " + servedToolchainPackages + "\n" + servedBrowserLibrariesInstallScript + "\n" + servedNodeInstallScript + "\n" + servedPnpmInstallScript + "\n" + servedRustInstallScript + "\nrm -f /etc/ssh/ssh_host_*; apt-get clean"
	args = append(args, "-c", script)
	if _, err := r.b.docker(ctx, args, r.b.rt.Stderr, r.b.rt.Stderr); err != nil {
		return "", err
	}
	// A nil slice marshals as null, which Docker treats as a shell command.
	// Empty JSON arrays clear the command for ENTRYPOINT-only images.
	cmd, _ := json.Marshal(append([]string{}, base.Config.Cmd...))
	entrypoint, _ := json.Marshal(append([]string{}, base.Config.Entrypoint...))
	args = []string{"commit", "--change", "CMD " + string(cmd)}
	args = append(args, "--change", "ENTRYPOINT "+string(entrypoint))
	if base.Config.User != "" {
		args = append(args, "--change", "USER "+base.Config.User)
	}
	for _, label := range []string{"crabbox.serve.prepared=true", "crabbox.serve.base-reference=" + strconv.Quote(r.Image()), "crabbox.serve.base-image-id=" + strconv.Quote(base.ID)} {
		args = append(args, "--change", "LABEL "+label)
	}
	args = append(args, name, tag)
	committed = true
	if _, err := r.b.docker(ctx, args, nil, r.b.rt.Stderr); err != nil {
		return "", err
	}
	prepared, err := r.inspectImage(ctx, tag)
	if err != nil {
		return "", err
	}
	if !slices.Equal(base.Config.Cmd, prepared.Config.Cmd) || !slices.Equal(base.Config.Entrypoint, prepared.Config.Entrypoint) || base.Config.User != prepared.Config.User || !slices.Equal(base.Config.Env, prepared.Config.Env) {
		return "", fmt.Errorf("serve prepared image did not preserve base CMD, entrypoint, user, and environment")
	}
	return prepared.ID, nil
}
