# Secret scan — 2026-11-11

> Date: 2026-11-11. Tool: gitleaks 8.30.1, default rules plus `.gitleaks.toml`
> (the `minisign-secret-key` rule). Commit scanned: `efe61c8`, the tip of `master`.
> Scope: every branch, `origin/master` and all 17 tags — 33 refs, 453 commits, of
> which 18 are merges.

## Scope

| Area | Command | Result |
|---|---|---|
| Tree | `git archive efe61c8 \| tar -x -C "$T"; cd "$T" && gitleaks dir -c "$CFG" --redact --no-banner .` | 1 finding, allowlisted |
| History | `gitleaks git -c "$CFG" --redact --no-banner --log-opts="--all --full-history -m" .` | 2 findings, both allowlisted |
| Commit messages | `git log --all --format='%H%n%B' \| gitleaks stdin -c "$CFG" --redact --no-banner` | none |
| Project-specific sweep | `git log --all -p \| grep -E 'untrusted comment: minisign (encrypted )?secret key\|ctx7sk-\|BEGIN [A-Z ]*PRIVATE KEY'` and a grep for `RECARTO_(SERVER\|SYNC)_KEY=` / `R2_*=` values | none |

The tree is scanned from a `git archive` export, so only tracked files are in scope.
Ignored local files (`.env`, agent configs) are never published and are not scanned.

## Findings

| Rule | Location | Commit | Value | Resolution |
|---|---|---|---|---|
| `generic-api-key` | `tools/embedgen/main.go:20` (`tokenSHA`) | `a27878d`, and the merge commit `1d47011` that carries it | `REDACTED` | False positive: the SHA-256 checksum of the pinned potion-base-8M `tokenizer.json`. Allowlisted in `.gitleaks.toml`. |

No other finding. The full-history `-m` run reports the same line in `a27878d` and in
the merge diff of `1d47011`; that is one false positive, not a second finding.

## Re-run

Run these from the repository root; they report no leaks:

```sh
CFG="$PWD/.gitleaks.toml"
T=$(mktemp -d)
git archive efe61c8 | tar -x -C "$T"
(cd "$T" && gitleaks dir -c "$CFG" --redact --no-banner .)
gitleaks git -c "$CFG" --redact --no-banner --log-opts="--all --full-history -m" .
git log --all --format='%H%n%B' | gitleaks stdin -c "$CFG" --redact --no-banner
```
