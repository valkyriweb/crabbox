# Reviewed runtime bundle installation (library only)

The existing resource owner can call `runtimeprofile.Install` inside a private,
lease-owned root. It accepts a reviewed stable profile, an already-authorized
bundle stream, explicit compressed/expanded/entry bounds, and an isolated exact
version probe. It does not fetch artifacts, execute vendor installers, copy an
auth home, start an agent, register a machine, or own/release a lease.

The producer checks the entire compressed SHA256 before extraction. Extraction
permits only regular files and directories; rejects traversal, absolute paths,
links, special files, duplicate paths and reserved receipt paths; strips
privileged/write permissions; and bounds compressed bytes, all decompressed
bytes and entries. Publication follows exact-version probing and verification
of the extracted file hashes. Failed/canceled attempts remove their staging
root. A completed content-addressed profile directory is reused only after
rebuilding expected hashes from the supplied checksum-pinned archive, checking
every file before and after probing, and rejecting changed receipts, added files
and symlinks. Reuse still requires the bundle stream; an owner may supply its
verified local artifact cache instead of another download. The private root and probe callback are trusted owner capabilities, not
untrusted request inputs; callers must not share the root with another agent or
run the probe with host credentials. There is no new background process.

`ProduceReceipt` combines that installation evidence with the existing owner's
lease expectation and a live runtime endpoint probe. It requires matching
version, process/session evidence and endpoint generation. Its transport reference
names an immutable complete route held by the existing owner's ephemeral
transport registry, including any frontdoor or relay and all pinned host keys.
The receipt contains no private key, auth home, shell command or local binary
path. It must never be treated as a persistent machine registration.

This PR supplies the installer and receipt producer, tested with synthetic
archives and mocked version/endpoint probes. It does not supply a transport
registry, orchestrator UI consumer, native harness launcher, reviewed runnable catalog or
complete bundles. Existing stable artifact pins still need frozen dependency
bundles and applicable publisher verification. Native tools/WebSocket requirements
must pass catalog capability checks before admission; an HTTP proof is not a
native harness qualification. Actual remote use remains through the existing
controller and its approval gates. No live infrastructure changes are included.
