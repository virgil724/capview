# Code Signing Options for `capview`

> **Decision guide only.** This document compares Authenticode signing options so the repository owner can choose a path manually. No certificates are purchased or applied for automatically.

## Why Signing Matters for `capview`

`capview` distributes Windows executables and a native COM/DirectShow virtual camera DLL:

- `capview-windows-amd64.exe` / `capview-cli-windows-amd64.exe` (and `capview.exe` / `capview-cli.exe` inside `capview-windows-amd64.zip`)
- `capview_vcam.dll` (loaded into Windows 11 FrameServer `svchost.exe` and Windows 10 DirectShow host apps)
- `capview-windows-arm64.exe` / `capview-cli-windows-arm64.exe`

Without an Authenticode signature, first-time users see a **Microsoft Defender SmartScreen ("Windows protected your PC")** warning, and hardened Windows 11 machines with **WDAC / HVCI** policies may refuse to load an unsigned `capview_vcam.dll` inside `svchost.exe`.

---

## Comparison of Options

| Option | Cost | Trust Level / SmartScreen | CI Automation Fit | Availability (Individual Developer in Taiwan) | Verdict |
| :--- | :--- | :--- | :--- | :--- | :--- |
| **SignPath Foundation (Free OSS)** | **$0** | **EV-grade** — immediate SmartScreen bypass | **Excellent** — official GitHub Actions integration (`signpath/github-action-submit-signing-request@v3`) + SignPath GitHub App | **Available globally** (requires OSI-approved license — `capview` is MIT — and pure OSS build on GitHub-hosted runners) | **Top Recommendation** |
| **Azure Artifact Signing** *(formerly Azure Trusted Signing)* | **$9.99/month** Basic (5,000 signings/mo; $0.005 overage) | **Immediate SmartScreen bypass** (cloud HSM, short-lived Public Trust certs) | **Excellent** — GitHub Actions OIDC / Azure CLI | **Blocked for Taiwan individuals** — Public Trust individual validation is currently limited to **US & Canada residents**; organizations require 3+ years of verifiable history in supported countries | Not directly available to Taiwan individual devs |
| **Certum Open Source Code Signing (Cloud / SimplySign)** | **~€49 (~$58)/year** | **OV-grade** — requires building SmartScreen download reputation per cert/release | **Poor for unattended CI** — SimplySign cloud signing uses mobile app TOTP approval | **Available globally** to individual OSS developers (no physical USB token required) | Viable fallback only for manual local release signing |
| **Commercial OV / EV Certificates** *(DigiCert, Sectigo, SSL.com)* | OV **~$100–250/yr**<br>EV **~$380–600+/yr** | OV: SmartScreen reputation needed<br>EV: **Immediate SmartScreen bypass** | **Moderate** — requires cloud HSM (e.g. DigiCert KeyLocker / SSL.com eSigner) or self-hosted runner with FIPS hardware token | OV available to individuals (varies by CA); EV mostly **registered businesses only** | Overkill / costly for a personal OSS project |
| **Unsigned (Current State)** | **$0** | SmartScreen warning on first download; strict WDAC/HVCI may block `capview_vcam.dll` in FrameServer | N/A (CI signing hooks cleanly skip) | Always available | Acceptable for early/draft releases only |

---

## Recommended Path: SignPath Foundation

**SignPath Foundation** is the best fit for `capview`:

1. **Free ($0)** for qualifying open-source projects.
2. **EV-grade reputation** (immediate SmartScreen trust without paying $400+/year or forming a company).
3. **Zero private-key handling in GitHub Secrets**: keys stay in SignPath's managed HSM; GitHub Actions only uploads the built artifact and downloads the signed output.
4. **Eligibility alignment**: `capview` is MIT-licensed, builds deterministically from source on GitHub-hosted runners (`windows-latest`), and has no proprietary binary blobs.

---

## Owner Next-Step Checklist (Matching `.github/workflows/ci.yml`)

`.github/workflows/ci.yml` already ships **both** opt-in signing hooks before `capview-windows-amd64.zip` is assembled and before `SHA256SUMS.txt` is generated:

- **Tag-only execution**: Signing hooks are gated by `github.event_name == 'push' && startsWith(github.ref, 'refs/tags/v')` — they **never** run on pull requests or pushes to `main`, so PR CI never stalls or fails on release signing once credentials are configured.
- **Mutual exclusion**: If both PFX and SignPath credentials are configured at the same time, the PFX (`signtool.exe`) hook runs and the SignPath Foundation hook is automatically skipped (with a clear log notice) so binaries are never double-signed or overwritten.
- **Safe no-op default**: Until you configure one of the paths below, tag builds cleanly skip signing and produce unsigned draft release artifacts.

### Path A — Enable SignPath Foundation (Recommended)

1. Apply for a free open-source account at [SignPath Foundation](https://signpath.org/) using the `virgil724/capview` repository (MIT license).
2. Install the **SignPath GitHub App** on `virgil724/capview` and configure a SignPath **Project** (e.g. slug `capview`), **Signing Policy** (e.g. `release-signing`), and **Artifact Configuration** that signs `.exe` and `.dll` files inside the uploaded GitHub Actions zip artifact.
3. In GitHub **Settings → Secrets and variables → Actions**:
   - **Variables**:
     - `SIGNPATH_ORG_ID` — your SignPath organization ID *(required to activate hook)*
     - `SIGNPATH_PROJECT_SLUG` — optional *(defaults to `capview`)*
     - `SIGNPATH_SIGNING_POLICY_SLUG` — optional *(defaults to `release-signing`)*
   - **Secrets**:
     - `SIGNPATH_API_TOKEN` — your SignPath CI submitter API token *(required to activate hook)*
4. Push a `v*` release tag (creating a **draft** GitHub Release) and verify the signed binaries inside `capview-windows-amd64.zip` with `Get-AuthenticodeSignature` before publishing.

### Path B — Enable PFX / `signtool.exe` (Self-Managed Certificate)

If you later obtain a PFX-compatible code-signing certificate:

1. Base64-encode the `.pfx` file (single line, no wrapping headers).
2. In GitHub **Settings → Secrets and variables → Actions**:
   - **Secrets**:
     - `CODESIGN_PFX_BASE64` — base64-encoded `.pfx` *(required to activate hook)*
     - `CODESIGN_PFX_PASSWORD` — `.pfx` passphrase *(required to activate hook)*
   - **Variables**:
     - `CODESIGN_TIMESTAMP_URL` — optional RFC 3161 timestamp server *(defaults to `http://timestamp.digicert.com`)*
3. CI automatically decodes the PFX into `RUNNER_TEMP`, signs all 5 Windows targets (`SHA256` digest + RFC 3161 timestamp), and deletes the temporary `.pfx` in an `if: always()` cleanup step.
