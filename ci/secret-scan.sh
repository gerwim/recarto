#!/bin/sh
# Secret scan of everything a push publishes: every commit reachable from HEAD, merges
# diffed against each parent (-m), and every commit message. The working tree is not
# scanned: git-ignored local files (.env, agent configs) are never published.
# Rules and allowlist: .gitleaks.toml. Report: docs/validation/2026-11-11-secret-scan.md
set -eu

gitleaks git -c .gitleaks.toml --redact --no-banner --log-opts="HEAD --full-history -m" .

msgs=$(mktemp)
trap 'rm -f "$msgs"' EXIT
git log --format='%H%n%B' HEAD >"$msgs"
gitleaks stdin -c .gitleaks.toml --redact --no-banner <"$msgs"
