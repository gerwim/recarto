# Contributing to Recarto

## Issues and pull requests

Open issues and pull requests on GitHub against `master`:
<https://github.com/gerwim/recarto>.

## Licence of contributions

Recarto is under the MIT licence ([LICENSE](LICENSE)), and so is every contribution.
By opening a pull request you agree to that and confirm the contribution is yours to
submit. No CLA or sign-off is needed.

## How changes land

GitHub is a mirror of the main repository. Accepted changes are applied in the main
repository and reach GitHub through the mirror.

## Checks

A pull request must pass the same checks as CI. You need Go 1.26.4 and a C compiler.

```sh
gofmt -l .        # prints nothing
go vet ./...
go test ./...
```

The main repository also scans every commit for secrets with gitleaks; its rules and
allowlist are in [`.gitleaks.toml`](.gitleaks.toml).

## Dependencies and third-party notices

After a change to `go.mod`, or when Recarto's code starts importing another third-party
package, run this from the repository root (it needs network access) and commit
`internal/licenses/`:

```sh
go run ./tools/licenses
```

Until you do, `go test ./...` fails and says so.

If the tool names a module without a licence, read that module's licence files and add
its SPDX expression to `licenseIDs` in `tools/licenses/main.go`.

When `ZIG_VERSION` in `ci/builder.Dockerfile` changes, copy `lib/libc/musl/COPYRIGHT`,
`lib/libc/mingw/COPYING` and `LICENSE` from that Zig release into
`internal/licenses/texts/{musl,mingw-w64,zig}/`, and update the versions in
`internal/licenses/licenses.go`.
