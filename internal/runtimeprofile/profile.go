// Package runtimeprofile defines reviewed runtime bundles and ephemeral attach
// receipts. It neither provisions resources nor owns their lifecycle.
package runtimeprofile

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"golang.org/x/crypto/ssh"
	"io"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"
)

type Capability string

const (
	TextHTTP  Capability = "text-http"
	Tools     Capability = "tools"
	WebSocket Capability = "websocket"
)

type Bundle struct {
	URL        string `json:"url"`
	SHA256     string `json:"sha256"`
	Version    string `json:"version"`
	Executable string `json:"executable"`
}

type Profile struct {
	SchemaVersion        int          `json:"schemaVersion"`
	ID                   string       `json:"id"`
	Runtime              string       `json:"runtime"`
	OS                   string       `json:"os"`
	Architecture         string       `json:"architecture"`
	Bundle               Bundle       `json:"bundle"`
	StateDirectory       string       `json:"stateDirectory"`
	RequiredCapabilities []Capability `json:"requiredCapabilities"`
	EndpointGeneration   int          `json:"endpointGeneration"`
}

var identifier = regexp.MustCompile(`^[a-z][a-z0-9-]{0,63}$`)
var digest = regexp.MustCompile(`^[a-f0-9]{64}$`)
var stableVersion = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(?:\+[A-Za-z0-9.-]+)?$`)

func relative(value string) bool {
	return value != "" && value != "." && !strings.ContainsAny(value, "\\\x00\r\n") && !strings.HasPrefix(value, "/") && path.Clean(value) == value && value != ".." && !strings.HasPrefix(value, "../")
}

func (p Profile) Validate() error {
	endpoint, err := url.Parse(p.Bundle.URL)
	if p.SchemaVersion != 1 || !identifier.MatchString(p.ID) || !identifier.MatchString(p.Runtime) || p.OS != "linux" || !slices.Contains([]string{"amd64", "arm64"}, p.Architecture) || p.EndpointGeneration != 1 {
		return errors.New("unsupported runtime profile identity or endpoint generation")
	}
	if err != nil || endpoint.Scheme != "https" || endpoint.Hostname() == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.ForceQuery || endpoint.Fragment != "" || strings.Contains(p.Bundle.URL, "#") || !digest.MatchString(p.Bundle.SHA256) || !stableVersion.MatchString(p.Bundle.Version) {
		return errors.New("runtime bundle requires a pinned stable version and credential-free HTTPS URL with SHA256")
	}
	if !relative(p.Bundle.Executable) || !relative(p.StateDirectory) {
		return errors.New("runtime paths must stay relative to the owned lease root")
	}
	if len(p.RequiredCapabilities) == 0 {
		return errors.New("runtime inference capabilities must be explicit")
	}
	seen := map[Capability]bool{}
	for _, c := range p.RequiredCapabilities {
		if !slices.Contains([]Capability{TextHTTP, Tools, WebSocket}, c) || seen[c] {
			return errors.New("unknown or duplicate runtime capability")
		}
		seen[c] = true
	}
	return nil
}

// ReadCatalog only accepts the operator-reviewed catalog. An acquire request
// supplies an ID, never a replacement artifact, shell command or secret.
func ReadCatalog(reader io.Reader) (Catalog, error) {
	decoder := json.NewDecoder(io.LimitReader(reader, 1<<20))
	decoder.DisallowUnknownFields()
	var profiles []Profile
	if err := decoder.Decode(&profiles); err != nil {
		return Catalog{}, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return Catalog{}, errors.New("catalog must contain exactly one JSON value")
	}
	catalog := Catalog{profiles: map[string]Profile{}}
	for _, p := range profiles {
		if err := p.Validate(); err != nil {
			return Catalog{}, fmt.Errorf("profile %q: %w", p.ID, err)
		}
		if _, exists := catalog.profiles[p.ID]; exists {
			return Catalog{}, errors.New("duplicate runtime profile")
		}
		catalog.profiles[p.ID] = p
	}
	return catalog, nil
}

type Catalog struct{ profiles map[string]Profile }

// Resolve refuses capability inflation: HTTP smoke authority cannot be used to
// admit a runtime that needs native tool or WebSocket inference.
func (c Catalog) Resolve(id string, available []Capability) (Profile, string, error) {
	p, ok := c.profiles[id]
	if !ok {
		return Profile{}, "", errors.New("runtime profile is not in the reviewed catalog")
	}
	for _, required := range p.RequiredCapabilities {
		if !slices.Contains(available, required) {
			return Profile{}, "", fmt.Errorf("runtime requires unsupported inference capability %s", required)
		}
	}
	p.RequiredCapabilities = slices.Clone(p.RequiredCapabilities)
	encoded, err := json.Marshal(p)
	if err != nil {
		return Profile{}, "", err
	}
	sum := sha256.Sum256(encoded)
	return p, hex.EncodeToString(sum[:]), nil
}

// Attachment carries evidence and argv-safe data, not a shell SSH command or
// persistent machine registration. The resource controller retains custody;
// detaching the UI must never be interpreted as permission to release a lease.
type Attachment struct {
	Version            int          `json:"version"`
	LeaseID            string       `json:"leaseId"`
	ResourceID         string       `json:"resourceId"`
	ProviderScope      string       `json:"providerScope"`
	ProfileDigest      string       `json:"profileDigest"`
	EndpointGeneration int          `json:"endpointGeneration"`
	ProcessID          string       `json:"processId"`
	SessionID          string       `json:"sessionId"`
	SSH                SSHTransport `json:"ssh"`
}

type SSHTransport struct {
	Host    string `json:"host"`
	Port    int    `json:"port"`
	User    string `json:"user"`
	HostKey string `json:"hostKey"`
	// Reference is resolved by the existing owner transport; private key bytes,
	// paths to a generic auth home and arbitrary SSH flags are never accepted.
	CredentialReference string `json:"credentialReference"`
}

type AttachmentExpectation struct {
	LeaseID, ResourceID, ProviderScope, ProfileDigest string
	SSH                                               SSHTransport
	ExpiresAt                                         time.Time
}

func (a Attachment) Validate(expected AttachmentExpectation, now time.Time) error {
	leaseID, resourceID, scope, profileDigest := expected.LeaseID, expected.ResourceID, expected.ProviderScope, expected.ProfileDigest
	if !expected.ExpiresAt.After(now) {
		return errors.New("lease attachment has expired")
	}
	if a.SSH != expected.SSH {
		return errors.New("attachment changed the pinned owner SSH transport")
	}
	key, _, _, rest, err := ssh.ParseAuthorizedKey([]byte(a.SSH.HostKey))
	if err != nil || len(rest) != 0 || key.Type() != ssh.KeyAlgoED25519 {
		return errors.New("attachment requires a valid pinned ED25519 host key")
	}
	if a.Version != 1 || a.EndpointGeneration != 1 || a.LeaseID != leaseID || a.ResourceID != resourceID || a.ProviderScope != scope || a.ProfileDigest != profileDigest || !digest.MatchString(a.ProfileDigest) {
		return errors.New("attachment does not match the owned lease, profile or endpoint generation")
	}
	if leaseID == "" || resourceID == "" || scope == "" || a.ProcessID == "" || a.SessionID == "" {
		return errors.New("attachment requires verified resource and runtime process/session evidence")
	}
	if a.SSH.Host != a.LeaseID || a.SSH.Port != 22 || a.SSH.User != "crabbox" || !strings.HasPrefix(a.SSH.HostKey, "ssh-ed25519 ") || !identifier.MatchString(a.SSH.CredentialReference) {
		return errors.New("attachment requires the lease-pinned owner SSH transport")
	}
	return nil
}
