# Served workspace controller admission and runtime profiles

The existing workspace controller remains the single resource lifecycle owner.
The serve adapter can now resolve an authenticated routing identity through the
optional `AuthenticatedControllerProviderContract` hook. It binds a normalized
coordinator endpoint, authenticated non-admin owner/org and guest routing/access
configuration, including architecture explicitness. Token secrets are excluded.
Every scoped child lifecycle entry rechecks
identity before backend creation; changed principals, orgs or configuration fail
closed. A same-principal token rotation between child operations preserves scope.
Authorized class/type/profile/TTL/idle overrides belong to the persisted workspace
request and exact coordinator create intent, not the baseline custody scope.
Ordinary config diagnostics remain offline. Internal identity reads have a
30-second deadline. Dynamic token commands are explicitly unsupported until one
resolved credential can be bound through both identity verification and child I/O.

Authenticated controller acquisitions attach a deterministic create-attempt ID
to the existing fixed-ID PUT route. The ID binds the controller scope and lease
ID; coordinator owner/org and exact request intent checks remain authoritative.
Cancellation uses the existing durable cancel-create protocol, including after an
ambiguous committed response. This prevents a late fixed create from acquiring
capacity after cancellation. Legacy fixed-ID callers retain their protocol.
No extra controller, reconciliation daemon or generic resource deletion is added.

`internal/runtimeprofile` is a strict local catalog and attachment contract:

- Only a reviewed catalog may supply bundle URL, stable version, SHA256, relative
  executable/state paths, architecture and endpoint generation. Acquire requests
  select a catalog ID rather than supplying a shell command or auth home.
- The exact profile digest binds admission. Required HTTP, tools and WebSocket
  inference capabilities must be explicitly supported. A one-turn HTTP proof key
  cannot admit a native runtime requiring tools or WebSockets.
- An attachment receipt binds the owned lease/resource/scope/profile, process and
  session evidence, unexpired lease and exact pinned owner SSH transport. It is
  typed data, not an evaluated SSH command or persistent machine registration.
  Detaching a UI does not release the resource.

## Deliberate remaining work

This patch does not download/install a bundle, launch a native harness, copy
credentials, register Herdr machines/tabs, implement a Herdr server transport,
claim native tool/WebSocket qualification, or issue inference credentials. No
real runtime bundles or fake placeholder pins are shipped. An installer must
verify bytes before safe bounded extraction and exact version probing, then
atomically publish a content-addressed installation. A trusted owner adapter
must obtain the process/session/host-key evidence; a receipt cannot create it.
The profile/attachment contracts are tested library inputs for that next adapter,
not an exposed ready/provisioned command. Herdr integration remains with its
existing operations owner; no upstream Herdr changes are included.

Validation is mocked/local. Production activation, real lease smoke and exact
credential registration retain their independent operator approval gates.

Local verification: full Go serve and runtimeprofile packages; focused CLI
controller identity, fixed acquire/replay and cancellation regressions; 154
Worker serve tests; Worker typecheck; touched Worker test lint/format and
`git diff --check`. All passed. Structured Auto Review is required before
publication. No real bundles, harness turns, live leases or keys were used.
