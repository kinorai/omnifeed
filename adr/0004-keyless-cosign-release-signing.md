# ADR 0004: Releases are signed keylessly with cosign

- **Status:** accepted (PR #72)
- **Date:** 2026-10-25
- **Scope:** the release job (`.github/workflows/ci.yml`) and `.goreleaser.yaml`

## Context

omnifeed is public, and others pull its release archives and the `kinorai/omnifeed`
images. Each release ships `checksums.txt`, and the release notes say it "contains
SHA-256 hashes for every artifact in this release". That proves an archive was not
corrupted in transit. It does not prove who built it: anyone who can replace the
archives can replace `checksums.txt` too. SBOMs are already generated (goreleaser
`sboms:` with syft), but nothing was signed: the release job had
`permissions: contents: write` only, and no step used cosign or artifact attestations.

## Decision

goreleaser signs every release with **keyless cosign** (Sigstore):

- **Archives:** `signs:` runs `cosign sign-blob` on `checksums.txt` and writes one
  Sigstore bundle, `checksums.txt.sigstore.json`. `checksums.txt` covers every
  archive, so signing it signs them all.
- **Images:** `docker_signs:` runs `cosign sign` on each multi-arch manifest by digest.
  The signature is pushed next to the image on Docker Hub.
- **Identity:** the release job's GitHub OIDC token (`id-token: write`). Fulcio issues a
  short-lived certificate for the workflow, and Rekor logs the signature. There is no
  key to store, rotate or leak.
- **Tooling:** `sigstore/cosign-installer`, pinned by commit SHA like every other
  action. The config follows goreleaser's own `.goreleaser.yaml` and its cosign v3
  guide.

The identity is the release workflow on `main`: the versioning and release jobs run
only on `main`. A looser match, such as a regexp on the repo URL, would also accept a
signature made by any workflow on any branch of the repo.

To verify an archive:

```sh
cosign verify-blob --bundle checksums.txt.sigstore.json \
  --certificate-identity https://github.com/kinorai/omnifeed/.github/workflows/ci.yml@refs/heads/main \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  checksums.txt
sha256sum --ignore-missing -c checksums.txt
```

To verify an image:

```sh
cosign verify kinorai/omnifeed:<version> \
  --certificate-identity https://github.com/kinorai/omnifeed/.github/workflows/ci.yml@refs/heads/main \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

## Options considered

| Option | Why not chosen |
|---|---|
| cosign with a stored key pair | A long-lived secret to keep in the repo's secrets and rotate. Keyless gives the same check with nothing to leak |
| GitHub artifact attestations (`actions/attest-build-provenance`) | Also keyless. goreleaser's documented path is cosign, which signs the archives and the images from the same config, and the check needs no GitHub CLI |
| No signing, checksums only | Integrity without origin, as before |

## Consequences

- A user can check that an archive or an image was built by this repo's release
  workflow, not only that it arrived intact.
- The release job can mint an OIDC token (`id-token: write`). It holds that
  permission only in the release job.
- Signatures are public: the Rekor transparency log records each one with the workflow
  identity.
- The signing steps run only on a real release, so PR CI does not exercise them. The
  first release after the merge is the first test, and PR #72's own `fix:` commit cuts
  one. If a signing step fails, the tag is already pushed and the GitHub Release is
  incomplete. Check that the `.sigstore.json` asset and the image signature exist, and
  that both commands above pass.

## Sources

- goreleaser, signing: <https://goreleaser.com/customization/sign/>
- goreleaser, signing Docker images: <https://goreleaser.com/customization/docker_sign/>
- goreleaser, cosign v3: <https://goreleaser.com/blog/cosign-v3/>
