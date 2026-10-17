# Security Policy

## Supported Versions

Security updates are applied to the latest release on the `main` branch and the most recent tagged `v*.*.*` container and binary releases.

| Version | Supported |
| :--- | :--- |
| `latest` / `main` | :white_check_mark: |
| Latest `v*.*.*` tag | :white_check_mark: |
| Older versions | :x: |

---

## Reporting a Vulnerability

Please **do not** report security vulnerabilities through public GitHub issues.

Instead, please report vulnerabilities privately using [**GitHub Private Vulnerability Reporting**](https://github.com/fadlytabrani/proxtail/security/advisories/new) on this repository.

When reporting a vulnerability, please include:
1. A description of the issue and its potential impact.
2. Step-by-step instructions or a minimal `config.yaml` to reproduce the issue.
3. The version or container image digest (`ghcr.io/fadlytabrani/proxtail@...`) affected.

We aim to acknowledge vulnerability reports within **48 hours** and provide a remediation plan or patch release within **7 days**.

---

## Security Architecture & Hardening

`proxtail` is designed with least-privilege security defaults:

- **Userspace Networking (`tsnet`)**: Requires zero host kernel capabilities (`CAP_NET_ADMIN` is **not** needed) and no access to `/dev/net/tun`.
- **Unprivileged Numeric UID/GID (`10001:10001`)**: The container image runs as numeric UID/GID `10001:10001` and complies with the **Kubernetes Pod Security Standards `restricted` profile** (`runAsNonRoot: true`, `allowPrivilegeEscalation: false`, `readOnlyRootFilesystem: true`, `capabilities.drop: ["ALL"]`).
- **Ephemeral In-Memory State by Default**: Node keys and Tailscale state are stored in memory (`mem.Store`) by default so containers never leave sensitive state files on disk.
- **Secret Isolation**: Tailscale auth keys and OAuth client secrets are resolved from environment variables (`TS_AUTHKEY_<NAME>`, `TS_CLIENT_SECRET_<NAME>`) rather than requiring secrets to be committed in `config.yaml`.
- **Signed Container Images**: Official images published to `ghcr.io/fadlytabrani/proxtail` are cryptographically signed using [Sigstore Cosign](https://docs.sigstore.dev/cosign/overview/) keyless OIDC signing in GitHub Actions.
