package serve

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

// The versioned field order is a persisted identity contract. Secrets are never
// inputs: rotation under the same authenticated principal preserves custody.
type controllerScope struct {
	Endpoint             string `json:"endpoint"`
	Owner                string `json:"owner"`
	Org                  string `json:"org"`
	Machine              string `json:"machine"`
	Target               string `json:"target"`
	Architecture         string `json:"architecture"`
	ArchitectureExplicit bool   `json:"architectureExplicit"`
	Image                string `json:"image"`
	User                 string `json:"user"`
	WorkRoot             string `json:"workRoot"`
}

func (Provider) ControllerProviderIdentity(ctx context.Context, cfg core.Config) (string, bool, error) {
	if cfg.TargetOS != "" && cfg.TargetOS != core.TargetLinux || cfg.Desktop || cfg.Browser || cfg.Code || cfg.Tailscale.Enabled || strings.TrimSpace(cfg.Serve.Fallback) != "" {
		return "", false, core.Exit(2, "serve controller requires a Linux SSH lease without optional transports or provider fallback")
	}
	// A command may return a different principal on its next invocation. Until
	// admission can bind one resolved command credential through child I/O,
	// support only the immutable in-memory token used by both reads and writes.
	if strings.TrimSpace(cfg.CoordToken) == "" || len(cfg.CoordTokenCommand) != 0 {
		return "", false, core.Exit(3, "serve controller requires a static coordinator token; token-command identity binding is unsupported")
	}
	endpoint, err := url.Parse(strings.TrimRight(strings.TrimSpace(cfg.Coordinator), "/"))
	if err != nil || endpoint.Hostname() == "" || (endpoint.Scheme != "http" && endpoint.Scheme != "https") || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.ForceQuery || endpoint.Fragment != "" || endpoint.RawFragment != "" {
		return "", false, core.Exit(2, "serve controller requires an absolute coordinator HTTP(S) URL without userinfo, query or fragment")
	}
	endpoint.Host = strings.ToLower(endpoint.Host)
	client, err := core.NewCoordinatorClient(cfg)
	if err != nil {
		return "", false, err
	}
	identity, err := client.Whoami(ctx)
	if err != nil {
		return "", false, core.Exit(3, "serve controller could not authenticate coordinator identity")
	}
	owner, org := strings.TrimSpace(identity.Owner), strings.TrimSpace(identity.Org)
	if owner == "" || owner == "unknown" || identity.Admin || strings.TrimSpace(identity.Auth) == "" {
		return "", false, core.Exit(3, "serve controller requires an authenticated non-admin owner identity")
	}
	// Class, type, profile and durations are controller-authorized per-workspace
	// request parameters. The controller persists them and the coordinator binds
	// exact create intent. Including them here would reject legitimate overrides
	// at acquisition and produce a different scope on inspect/stop without flags.
	scope := controllerScope{Endpoint: endpoint.String(), Owner: owner, Org: org, Machine: strings.TrimSpace(cfg.Serve.Machine), Target: core.TargetLinux, Architecture: cfg.Architecture, ArchitectureExplicit: core.IsArchitectureExplicit(cfg), Image: cfg.Image, User: cfg.SSHUser, WorkRoot: cfg.WorkRoot}
	data, err := json.Marshal(scope)
	if err != nil {
		return "", false, err
	}
	sum := sha256.Sum256(data)
	return "serve-v1:sha256:" + hex.EncodeToString(sum[:]), true, nil
}
