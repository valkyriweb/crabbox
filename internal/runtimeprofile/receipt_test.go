package runtimeprofile

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestReceiptRequiresActualRuntimeEvidenceAndOwnerTransport(t *testing.T) {
	p := fixture()
	installed := InstalledBundle{ProfileDigest: profileDigest(p), BundleSHA256: p.Bundle.SHA256, Version: p.Bundle.Version, Directory: "/owned/runtime"}
	now := time.Now()
	expected := AttachmentExpectation{LeaseID: "cbx_abcdef123456", ResourceID: "machine/owned", ProviderScope: "owner-scope", ProfileDigest: installed.ProfileDigest, ExpiresAt: now.Add(time.Hour), SSH: SSHTransport{Host: "cbx_abcdef123456", Port: 22, User: "crabbox", HostKey: "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", CredentialReference: "lease-auth"}}
	evidence := RuntimeEvidence{Version: p.Bundle.Version, ProcessID: "process-1", SessionID: "session-1", EndpointGeneration: 1}
	produce := func() (ReadyReceipt, error) {
		return ProduceReceipt(context.Background(), p, installed, expected, "owner-route", func(context.Context, InstalledBundle) (RuntimeEvidence, error) { return evidence, nil }, func() time.Time { return now })
	}
	receipt, err := produce()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(receipt)
	if strings.Contains(string(b), installed.Directory) || receipt.TransportReference != "owner-route" {
		t.Fatal("receipt exposed local runtime path")
	}
	for _, kind := range []string{"generation", "version", "process", "expired", "profile"} {
		saved := evidence
		savedExpected := expected
		switch kind {
		case "generation":
			evidence.EndpointGeneration = 2
		case "version":
			evidence.Version = "other"
		case "process":
			evidence.ProcessID = ""
		case "expired":
			expected.ExpiresAt = now
		case "profile":
			expected.ProfileDigest = strings.Repeat("b", 64)
		}
		if _, err := produce(); err == nil {
			t.Fatal("unproven receipt accepted", kind)
		}
		evidence = saved
		expected = savedExpected
	}
}
