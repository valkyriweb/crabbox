package cli

import (
	"os"
	"strings"
)

// LeaseClientIdentity records which local session created a coordinator
// lease, so long-lived keep=true leases have a visible owner beyond the
// authenticated account.
type LeaseClientIdentity struct {
	Session string `json:"session,omitempty"`
	Host    string `json:"host,omitempty"`
	Agent   string `json:"agent,omitempty"`
}

const leaseClientIdentityMaxLen = 128

var (
	leaseClientSessionEnv = []string{"CRABBOX_SESSION_ID", "PI_SESSION_ID", "CODEX_THREAD_ID", "CLAUDE_SESSION_ID"}
	leaseClientAgentEnv   = []string{"CRABBOX_AGENT", "AI_AGENT"}
	leaseClientHostname   = os.Hostname
)

func currentLeaseClientIdentity() *LeaseClientIdentity {
	identity := LeaseClientIdentity{
		Session: firstLeaseClientEnv(leaseClientSessionEnv),
		Agent:   firstLeaseClientEnv(leaseClientAgentEnv),
		Host:    sanitizeLeaseClientValue(os.Getenv("CRABBOX_CLIENT_HOST")),
	}
	if identity.Host == "" {
		if host, err := leaseClientHostname(); err == nil {
			identity.Host = sanitizeLeaseClientValue(host)
		}
	}
	if identity == (LeaseClientIdentity{}) {
		return nil
	}
	return &identity
}

func firstLeaseClientEnv(names []string) string {
	for _, name := range names {
		if value := sanitizeLeaseClientValue(os.Getenv(name)); value != "" {
			return value
		}
	}
	return ""
}

func sanitizeLeaseClientValue(value string) string {
	value = strings.Map(func(r rune) rune {
		if r <= ' ' || r == 0x7f {
			return -1
		}
		return r
	}, value)
	if len(value) > leaseClientIdentityMaxLen {
		value = value[:leaseClientIdentityMaxLen]
	}
	return value
}

func applyLeaseOwnerLabels(labels map[string]string, lease CoordinatorLease) {
	if owner := strings.TrimSpace(lease.Owner); owner != "" {
		labels["owner"] = owner
	}
	if identity := lease.ClientIdentity; identity != nil {
		if identity.Session != "" {
			labels["client_session"] = identity.Session
		}
		if identity.Host != "" {
			labels["client_host"] = identity.Host
		}
		if identity.Agent != "" {
			labels["client_agent"] = identity.Agent
		}
	}
}

// leaseOwnerSummary renders owner fields for list and status text output.
func leaseOwnerSummary(labels map[string]string) string {
	var b strings.Builder
	for _, field := range [][2]string{{"owner", "owner"}, {"session", "client_session"}, {"client_host", "client_host"}, {"agent", "client_agent"}} {
		if value := strings.TrimSpace(labels[field[1]]); value != "" {
			b.WriteString(" " + field[0] + "=" + value)
		}
	}
	return b.String()
}

// coordinatorLeaseSizeSummary describes how the lease size was chosen. An
// explicit --type selects the machine; the class then only supplies fallback
// defaults (for example disk sizing), so it must not read as the chosen size.
func coordinatorLeaseSizeSummary(cfg Config) string {
	if cfg.ServerTypeExplicit && strings.TrimSpace(cfg.ServerType) != "" {
		return "class=- type=" + cfg.ServerType + " fallback_class=" + blank(cfg.Class, "-")
	}
	return "class=" + cfg.Class + " preferred_type=" + cfg.ServerType
}
