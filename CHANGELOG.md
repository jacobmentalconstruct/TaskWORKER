# Changelog

## 0.1.0

First release.

- One local service owns a bounded queue of jobs and their results. The browser UI,
  the command line, a Python client and an MCP bridge all watch and control the same
  jobs.
- Inference runs on Ollama 0.18.3 with models you already have installed.
- Streaming output, cancel with the partial output kept, retry, branch, and pause and
  resume of the queue.
- Idempotent creates: a lost reply can be resent and resolves to the original job.
- Jobs are journaled to disk. A job that was running when the service stopped is
  marked interrupted, never silently restarted.
- The job list can be sorted by created or last-updated time, in either direction.
- Release archives for Windows, macOS and Linux on amd64 and arm64, with
  `SHA256SUMS`.
