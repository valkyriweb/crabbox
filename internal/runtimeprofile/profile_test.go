package runtimeprofile

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func fixture() Profile {
	return Profile{SchemaVersion: 1, ID: "example-runtime", Runtime: "example-agent", OS: "linux", Architecture: "arm64", Bundle: Bundle{URL: "https://example.invalid/runtime.tgz", SHA256: strings.Repeat("a", 64), Version: "1.2.3", Executable: "bin/agent"}, StateDirectory: "state/example-agent", RequiredCapabilities: []Capability{TextHTTP, Tools}, EndpointGeneration: 1}
}
func TestReviewedProfileRequiresNativeCapabilities(t *testing.T) {
	p := fixture()
	data, _ := json.Marshal([]Profile{p})
	catalog, err := ReadCatalog(strings.NewReader(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := catalog.Resolve(p.ID, []Capability{TextHTTP}); err == nil {
		t.Fatal("one-turn proof admitted a tool runtime")
	}
	got, d, err := catalog.Resolve(p.ID, []Capability{TextHTTP, Tools})
	if err != nil || len(d) != 64 {
		t.Fatal(err)
	}
	got.RequiredCapabilities[0] = WebSocket
	again, d2, err := catalog.Resolve(p.ID, []Capability{TextHTTP, Tools})
	if err != nil || again.RequiredCapabilities[0] != TextHTTP || d2 != d {
		t.Fatal("catalog mutated through returned profile")
	}
	if _, _, err := catalog.Resolve("unreviewed", []Capability{TextHTTP, Tools}); err == nil {
		t.Fatal("unreviewed artifact admitted")
	}
}
func TestProfileRejectsUnpinnedOrUnsafeInputs(t *testing.T) {
	for _, change := range []func(*Profile){func(p *Profile) { p.Bundle.Version = "1.2.3-beta.1" }, func(p *Profile) { p.Bundle.URL = "https://user:secret@example.invalid/a" }, func(p *Profile) { p.Bundle.URL += "?token=forbidden" }, func(p *Profile) { p.Bundle.Executable = "../agent" }, func(p *Profile) { p.StateDirectory = "/home/shared" }, func(p *Profile) { p.Bundle.SHA256 = "latest" }, func(p *Profile) { p.RequiredCapabilities = nil }, func(p *Profile) { p.EndpointGeneration = 2 }} {
		p := fixture()
		change(&p)
		if p.Validate() == nil {
			t.Fatal("unsafe profile accepted")
		}
	}
	if _, err := ReadCatalog(strings.NewReader(`[{"command":"arbitrary shell"}]`)); err == nil {
		t.Fatal("unknown request fields accepted")
	}
	data, _ := json.Marshal([]Profile{fixture()})
	if _, err := ReadCatalog(strings.NewReader(string(data) + ` {}`)); err == nil {
		t.Fatal("trailing input accepted")
	}
}
func TestAttachmentBindsOwnerAndActualRuntimeEvidence(t *testing.T) {
	a := Attachment{Version: 1, LeaseID: "cbx_abcdef123456", ResourceID: "machine/owned", ProviderScope: "verified-owner", ProfileDigest: strings.Repeat("a", 64), EndpointGeneration: 1, ProcessID: "process-1", SessionID: "session-1", SSH: SSHTransport{Host: "cbx_abcdef123456", Port: 22, User: "crabbox", HostKey: "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", CredentialReference: "lease-transport"}}
	now := time.Now()
	expected := AttachmentExpectation{LeaseID: a.LeaseID, ResourceID: a.ResourceID, ProviderScope: a.ProviderScope, ProfileDigest: a.ProfileDigest, SSH: a.SSH, ExpiresAt: now.Add(time.Minute)}
	validate := func(a Attachment) error { return a.Validate(expected, now) }
	if err := validate(a); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*Attachment){func(a *Attachment) { a.LeaseID = "cbx_abcdef123457" }, func(a *Attachment) { a.ProviderScope = "foreign" }, func(a *Attachment) { a.ResourceID = "machine/foreign" }, func(a *Attachment) { a.ProfileDigest = strings.Repeat("b", 64) }, func(a *Attachment) { a.ProcessID = "" }, func(a *Attachment) { a.SSH.Host = "donor-host" }, func(a *Attachment) { a.EndpointGeneration = 2 }} {
		bad := a
		change(&bad)
		if validate(bad) == nil {
			t.Fatal("foreign or unproven attachment accepted")
		}
	}
}
