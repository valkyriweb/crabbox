package runtimeprofile

import (
	"context"
	"errors"
	"time"
)

// RuntimeEvidence comes from the existing lease owner's actual endpoint probe,
// not the acquire payload. No process is started by this producer.
type RuntimeEvidence struct {
	Version, ProcessID, SessionID string
	EndpointGeneration            int
}
type RuntimeProbe func(context.Context, InstalledBundle) (RuntimeEvidence, error)
type ReadyReceipt struct {
	Attachment         Attachment `json:"attachment"`
	TransportReference string     `json:"transportReference"`
	ExpiresAt          time.Time  `json:"expiresAt"`
}

// ProduceReceipt keeps route/frontdoor/candidate details in the existing owner's
// transport registry. The short-lived opaque reference must name that immutable
// full transport, not just a private key or a persistent machine registration.
// There is deliberately no orchestrator UI consumer or registry implementation here.
func ProduceReceipt(ctx context.Context, p Profile, installed InstalledBundle, expected AttachmentExpectation, transportReference string, probe RuntimeProbe, clock func() time.Time) (ReadyReceipt, error) {
	if err := p.Validate(); err != nil {
		return ReadyReceipt{}, err
	}
	if clock == nil || probe == nil || !identifier.MatchString(transportReference) || installed.ProfileDigest != profileDigest(p) || installed.ProfileDigest != expected.ProfileDigest || installed.BundleSHA256 != p.Bundle.SHA256 || installed.Version != p.Bundle.Version || installed.Directory == "" {
		return ReadyReceipt{}, errors.New("verified bundle, owner transport and runtime probe required")
	}
	if !expected.ExpiresAt.After(clock()) {
		return ReadyReceipt{}, errors.New("owned lease has expired")
	}
	evidence, err := probe(ctx, installed)
	if err != nil {
		return ReadyReceipt{}, errors.New("owned runtime endpoint probe failed")
	}
	if err = ctx.Err(); err != nil {
		return ReadyReceipt{}, err
	}
	if evidence.Version != p.Bundle.Version {
		return ReadyReceipt{}, errors.New("runtime endpoint version differs from reviewed bundle")
	}
	attachment := Attachment{Version: 1, LeaseID: expected.LeaseID, ResourceID: expected.ResourceID, ProviderScope: expected.ProviderScope, ProfileDigest: expected.ProfileDigest, EndpointGeneration: evidence.EndpointGeneration, ProcessID: evidence.ProcessID, SessionID: evidence.SessionID, SSH: expected.SSH}
	if err = attachment.Validate(expected, clock()); err != nil {
		return ReadyReceipt{}, err
	}
	return ReadyReceipt{Attachment: attachment, TransportReference: transportReference, ExpiresAt: expected.ExpiresAt}, nil
}
