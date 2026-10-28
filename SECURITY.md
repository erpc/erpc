# Security Policy

## Reporting a vulnerability

Report vulnerabilities privately through [GitHub Security Advisories](https://github.com/erpc/erpc/security/advisories/new). Do not open a public issue, pull request, or discussion.

Include:

- affected version or commit
- configuration needed to reproduce
- steps or a proof of concept
- impact you observed

We acknowledge reports within 3 business days and keep you updated until a fix ships. Once a fix is released, we publish an advisory and credit you unless you ask us not to.

## Supported versions

Only the latest release receives security fixes. Upgrade to the newest release before reporting.

## Verifying releases

Release binaries and Docker images carry GitHub build provenance attestations:

```sh
gh attestation verify erpc_linux_x86_64 --repo erpc/erpc
gh attestation verify oci://ghcr.io/erpc/erpc:<version> --repo erpc/erpc
```
