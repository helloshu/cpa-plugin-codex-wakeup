# Security Policy

## Supported versions

Only the latest published release receives security fixes.

## Reporting a vulnerability

Please use GitHub's private vulnerability reporting feature for this repository. Do not include OAuth tokens, Management Keys, credential files, or other secrets in a public issue.

When possible, include the affected plugin version, CLIProxyAPI version, operating system and architecture, a minimal reproduction, and sanitized logs.

## Security boundaries

The plugin reads Codex OAuth material only through CLIProxyAPI host callbacks and uses it for the requested upstream call. It does not persist access, refresh, or ID tokens. Task state contains only configuration, stable auth indexes, masked labels, counters, timestamps, and sanitized diagnostics.
