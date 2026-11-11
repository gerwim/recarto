# Security

## Reporting a vulnerability

Report privately through GitHub's private vulnerability reporting:
<https://github.com/gerwim/recarto/security/advisories/new> (or Security → Report a
vulnerability). Do not open a public issue.

Include the version (`recarto version`), steps to reproduce and the impact.

## Scope

The code in this repository, including the HTTP sync server (`recarto server`) and its
key-based authentication: `RECARTO_SERVER_KEY` on the server, `RECARTO_SYNC_KEY` on
clients.
