# Contributing

Issues are welcome: bug reports, questions, and results from platforms that have not
been tested (Windows arm64 and macOS on Intel). Please include the version, your
operating system and architecture, the Ollama version, and the steps to reproduce.

Pull requests that fix a bug are welcome. For anything larger, please open an issue
first so we can agree on the approach before you write it.

## Before you send a change

- Build and test as described in `docs/development.md`. CI runs the Go tests (with the
  race detector), the Python client tests, the browser-JS tests and the packaging
  checks on Linux, macOS and Windows.
- The core and the adapters use only the Go standard library, apart from the MCP
  bridge's official SDK. Please do not add dependencies.
- Add a test with a bug fix.
- The limitations block in `README.md` and `docs/limitations.md` are generated from
  `release/limitations.json`. Edit that file and run
  `python scripts/limitations.py --write`; do not edit the generated text by hand.

By contributing you agree that your work is released under the project's MIT licence.
