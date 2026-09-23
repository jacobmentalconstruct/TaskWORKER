# Security policy

## Reporting a vulnerability

Please do not open a public issue for a security problem. Use this repository's
**Security** tab, choose **Report a vulnerability**, and describe what you found and
how to reproduce it. The report is private to the maintainer. This is a small
project maintained on a best-effort basis, so there is no guaranteed response time.

## Supported versions

Only the latest 0.1.x release is supported.

## What is in scope

TaskWorker is a local service. These are in scope:

- The loopback HTTP and event-stream API, including the browser-origin
  restrictions that keep other web pages from controlling it.
- The embedded browser UI.
- The MCP bridge and the Python client.
- The on-disk journal: corruption handling, recovery and the bounds on what is read.
- The release archives and their checksums.

## What is not a vulnerability

These are documented limits, not bugs (see `docs/limitations.md`):

- The service has **no authentication**. Any program running as any user on the same
  machine can control it. It accepts loopback connections only, and exposing it to a
  network is unsupported.
- Jobs, prompts and outputs are stored **unencrypted** in the data directory.
- The executables are not code-signed or notarized.
