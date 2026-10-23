package localcontainer

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

// v2 adds the pinned Node.js toolchain; the bump invalidates v1 prepared images.
const servedPreparationRecipe = "v2"

// servedNodeInstallScript installs the pinned official Node.js tarball under
// /opt/node and links it into /usr/local/bin, which is on PATH for login and
// non-login SSH sessions. It is idempotent and fails closed (set -eu) when the
// architecture is unsupported or the SHA256 does not match.
const servedNodeInstallScript = `node_version=v24.22.1
case "$(uname -m)" in
  x86_64|amd64) node_arch=x64; node_sha256=eb6728e77c576a35162c122c45ccadf224d89ff00abc8dae46cc2897c6002d7c ;;
  aarch64|arm64) node_arch=arm64; node_sha256=33c37be234ba8bee041961047b19160f4d7d0f164e8e246f78bdd10feff148b3 ;;
  *) echo "unsupported architecture for Node.js: $(uname -m)" >&2; exit 1 ;;
esac
if [ ! -x /opt/node/bin/node ] || [ "$(/opt/node/bin/node --version)" != "$node_version" ]; then
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
	sum := sha256.Sum256([]byte(baseID + "\n" + bootstrapPackages + "\n" + servedPreparationRecipe))
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
		bounded, cancel := context.WithTimeout(ctx, 10*time.Minute)
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
	}()
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
	probe := "for tool in " + bootstrapTools + " node npm; do command -v \"$tool\" >/dev/null 2>&1 || { command -v apt-get >/dev/null 2>&1 && { echo prepare; exit 0; }; echo skip; exit 0; }; done; echo ready"
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
	script := "set -eu; export DEBIAN_FRONTEND=noninteractive; apt-get update; apt-get install -y --no-install-recommends " + bootstrapPackages + "; " + servedNodeInstallScript + "; rm -f /etc/ssh/ssh_host_*; rm -rf /var/lib/apt/lists/*"
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
