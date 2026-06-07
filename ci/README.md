# Release infrastructure (maintainer runbook)

This directory holds the CI release setup for `recarto update`. A tagged commit
triggers `.gitlab-ci.yml`, which cross-compiles the three binaries, signs a release
manifest with minisign, and uploads everything to a Cloudflare R2 bucket served at
`releases.gerwim.com`. The `recarto update` client reads from there and applies an
update only if the minisign signature **and** the sha256 verify.

```
  Your Mac (once) ── extract macOS SDK ──► ci/osxcross-sdk/MacOSX*.sdk.tar.xz
        │
        └─ docker build -f ci/builder.Dockerfile  ──►  osxcross bakes the SDK INTO the image
                                                            │
                                                docker push to GitLab registry
                                                            │
   git tag X.Y.Z ──► GitLab CI pulls the image ──► build (zig: linux/windows,
                     oa64-clang: darwin) ──► hash + merge index.json ──► minisign
                     sign ──► upload binaries + index.json + index.json.minisig to R2
```

The macOS SDK is **never committed** (Apple's license restricts redistribution, and
it would bloat the repo). It is consumed once, at image-build time, and lives only
inside the builder image in your private registry — so the ephemeral CI containers
get it from the image, not from the repo. `ci/osxcross-sdk/` is gitignored.

---

## One-time setup

### 1. minisign keypair (already done)

Generated passphrase-less so CI can sign non-interactively:

```sh
minisign -GW -p recarto-minisign.pub -s recarto-minisign.key
```

- The **public** key is embedded in the binary at `internal/update/pubkey.go`
  (`ManifestPublicKey`). Rotating it requires shipping a new binary.
- The **secret** key (`recarto-minisign.key`) is gitignored. It goes into the
  `RECARTO_MINISIGN_KEY` CI variable (step 5). Never commit it.

### 2. Extract the macOS SDK (on a Mac, once)

osxcross needs Apple's SDK (the `CoreFoundation`/`Security` frameworks that
`crypto/x509` links). On a Mac with Xcode **or** the Command Line Tools installed:

```sh
git clone https://github.com/tpoechtrager/osxcross
cd osxcross
# From a full Xcode .xip:
./tools/gen_sdk_package_pbzx.sh           # → MacOSX<version>.sdk.tar.xz
# …or from the Command Line Tools only:
# ./tools/gen_sdk_package_tools.sh
```

Copy the resulting tarball into the repo build context:

```sh
mkdir -p ci/osxcross-sdk
cp /path/to/MacOSX14.sdk.tar.xz ci/osxcross-sdk/
```

### 3. Build and push the builder image (once; repeat on toolchain/SDK changes)

```sh
docker login registry.gerwim.nl
docker build -f ci/builder.Dockerfile -t registry.gerwim.nl/gerwim/recarto/builder:latest .
docker push registry.gerwim.nl/gerwim/recarto/builder:latest
```

This image carries Go, zig (linux/windows), osxcross + the SDK (darwin), `minisign`,
and the `aws` CLI. `.gitlab-ci.yml`'s `build` and `publish` jobs reference it via
`$BUILDER_IMAGE`. Rebuild and push only when you bump Go/zig, update osxcross, or
refresh the macOS SDK — otherwise every release reuses the existing image.

> Verified locally: the zig linux/windows builds and the full
> build → hash → merge → minisign-sign pipeline run inside this image, and a
> minisign-CLI signature is accepted by recarto's own verifier. The darwin/osxcross
> compile itself is exercised the first time the real image (with your SDK) runs.

### 4. Cloudflare R2

1. Create an R2 bucket. Its name is the value you set as `R2_BUCKET`.
2. Attach the custom domain `releases.gerwim.com` to the bucket, served at the
   bucket **root**, and enable public read. This is what makes
   `https://releases.gerwim.com/recarto/index.json` resolve to the object
   `recarto/index.json` in the bucket. (If the domain is mapped to a sub-path
   instead of the root, every client fetch 404s.)
3. Create an R2 API token with **Object Read & Write** on the bucket → note the
   Access Key ID and Secret Access Key.
4. Note your Cloudflare **Account ID**; the S3 endpoint is
   `https://<account_id>.r2.cloudflarestorage.com` (built in CI from `R2_ACCOUNT_ID`).

### 5. GitLab CI/CD variables

Settings → CI/CD → Variables. Mark them **Protected** so they are only exposed to
protected tag pipelines (also protect your release tags under
Settings → Repository → Protected tags, e.g. the wildcard `*`).

| Variable | Type | Masked | Value |
|---|---|---|---|
| `RECARTO_MINISIGN_KEY` | **File** | no¹ | full contents of `recarto-minisign.key` |
| `R2_ACCESS_KEY_ID` | Variable | yes | R2 token Access Key ID |
| `R2_SECRET_ACCESS_KEY` | Variable | yes | R2 token Secret Access Key |
| `R2_ACCOUNT_ID` | Variable | yes | Cloudflare account id |
| `R2_BUCKET` | Variable | no | bucket name |

¹ The minisign secret key is **multi-line**, and GitLab masked variables must be
single-line — so it must be a **File** type variable (CI sees `$RECARTO_MINISIGN_KEY`
as a path to a temp file, which `minisign -S -s` reads directly). Keep its branch/tag
exposure restricted via Protected.

---

## Cutting a release

```sh
git tag X.Y.Z           # plain semver, no leading "v" (matches the version stamp)
git push origin X.Y.Z
```

The pipeline runs `test → build → publish`. `build` produces
`releases/{linux-amd64/recarto, darwin-arm64/recarto, windows-amd64/recarto.exe}`;
`publish` fetches the current `index.json` (absent on the first release), merges the
new version in, signs it, and uploads the versioned binaries (cached forever) plus
`index.json` + `index.json.minisig` (short TTL) to `recarto/` in the bucket.

Verify afterward:

```sh
curl -fsS https://releases.gerwim.com/recarto/index.json | head
recarto update --check          # exit 0 = current, 10 = update available
```

---

## Troubleshooting

- **`update` 404s / can't find the manifest** — `releases.gerwim.com` isn't mapped to
  the bucket root. Confirm `https://releases.gerwim.com/recarto/index.json` is
  directly fetchable.
- **`aws s3 cp` integrity/checksum errors on R2** — newer aws CLI versions send
  default checksums R2 rejects. Pin the CLI in the image, or set
  `AWS_REQUEST_CHECKSUM_CALCULATION=when_required` in the `publish` job.
- **darwin link errors (`Security`/`CoreFoundation`/`resolv`)** — the SDK in the image
  is missing or too old for the build. Rebuild the builder image with a current
  macOS SDK (steps 2–3).
- **`minisign` signature rejected by the client** — the embedded `ManifestPublicKey`
  no longer matches the signing key. They are a pair; rotating one requires the other
  (and a new binary release).
