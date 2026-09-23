# Persistence decision

Implemented in `internal/adapters/journal`: a standard-library-only,
append-only, checksummed transaction journal implementing `core.Store`. This
document specifies the version 1 format and its recovery guarantees.

The authoritative state is the replay of committed events, including complete
request/instruction records and retained output. A create receipt commits with
its job acceptance. Queue membership, active job, pause state, job transitions,
errors, and final metadata are events, not separate uncoordinated files.

## Format and durability contract

`journal.Open(dataDir)` acquires `owner.lock` before opening `journal.bin`.
All binary integers are unsigned, **big endian**. The 64-byte file header is:

| Offset | Bytes | Value |
| --- | ---: | --- |
| 0 | 8 | ASCII `TWJRNL` followed by CR LF |
| 8 | 4 | Format version, 1 |
| 12 | 4 | Header length, 64 |
| 16 | 16 | Cryptographically random persistent store identity |
| 32 | 32 | SHA-256 of header bytes 0–31 |

The store identity is exposed as 32 lowercase hexadecimal characters. Every
following record has a 72-byte frame header and one `Commit` JSON payload:

| Offset | Bytes | Value |
| --- | ---: | --- |
| 0 | 4 | Frame version, 1 |
| 4 | 4 | Payload byte length, 1 through 16 MiB |
| 8 | 32 | SHA-256 of frame bytes 0–7 |
| 40 | 32 | SHA-256 of payload bytes |
| 72 | length | Canonical UTF-8 JSON `Commit`, with `version: 1` |

Checksums detect corruption, not tampering. Independently checksumming the length
prevents a damaged length from being mistaken for a torn final payload. Validate
the complete frame header before allocating payload memory. JSON is the exact
`encoding/json.Marshal` encoding of the version 1 typed commit: declaration field
order, normal Go string escaping, no whitespace or trailing bytes. Recovery
rejects unknown fields, duplicate fields, missing fields that change canonical
encoding, invalid cursor decimals, and unsupported versions. Version 1 is a new
format; there was no previous implemented journal to migrate.

Every commit has nonempty contiguous same-store events; receipts reference the
accepted job in that commit. Replay validates payload shape, version, event
sequence, state transitions, output offsets, and queue consistency.

Serialize appends, finish each complete record, then call `File.Sync` before
acknowledging mutations, publishing events, or dispatching accepted jobs. Do not
hold user-facing observer operations in this write path. A failed or ambiguous
write/sync poisons the handle; stop dispatch, cancel active inference, and require
recovery rather than guessing what reached storage. Publication may lag a durable
commit after a crash; replay and idempotency resolve that ambiguity.

Recovery scans the verified prefix. An incomplete final frame from a torn append
may be truncated back to the last valid offset, synced, and reported. A complete
frame with checksum mismatch, invalid JSON/schema/sequence, corrupt header, or
unknown version fails closed without modifying evidence. Never skip interior
corruption or manufacture a successful terminal result. Running/cancelling jobs
in recovered state are interrupted in a new commit before dispatch resumes.

Acquire an OS-backed exclusive ownership lock before opening/recovering the
journal, held for the entire service lifetime and automatically released on
process death. Windows opens the persistent lock file with `CreateFile` share mode
zero; Linux and macOS use nonblocking exclusive `flock`. File existence is not
ownership.
Ownership and restart behavior require native platform tests; compilation alone
cannot verify them. Network filesystems and multiple writers are unsupported.

`File.Sync` requests stable storage from the OS; power-loss durability ultimately
depends on filesystem/device behavior. Sync newly created journal/header and
parent directory metadata where the platform supports it. Explicitly document
any platform initialization durability gap; do not claim portable atomic rename
or directory sync guarantees. The implementation syncs the journal and its parent
directory on Linux/macOS, including parent entries of newly created directories.
Windows syncs the file but has no directory
sync through this implementation. Native power-loss/device tests have not run.
Use owner-only permissions where supported and
the user's private data directory on Windows. Data is plaintext, not encrypted.

## Bounds and recovery cost

No deletion, rotation, or compaction in the initial implementation. Replay cost
is linear in journal size; in-memory state is bounded by the contract limits.
Reject admission if its projected record crosses 192 MiB. Every append must fit
below 256 MiB **including** a recomputed reserve: 8 KiB for queue/framing controls,
plus each unfinished job's encoded size and 4 KiB overhead per remaining lifecycle
replacement (three for queued, two for running, one for cancelling), plus 64 KiB
metadata allowance per unfinished job. Backend final metadata without text is
limited to 32 KiB; the rest covers terminal error/timestamps/escaping overhead.
This is stronger than a fixed admission watermark: output cannot consume the
space needed by a running cancellation and its terminal replacement.

Definite pre-write size/admission rejection returns `limit_exceeded`, leaves bytes
and state unchanged, and does not poison the handle. Output encountering it stops
and the job fails after backend return using its reserved terminal space. Further
queue controls can exhaust the finite general reserve; a rejected control makes
no change. A write/sync failure returns `storage_failure`, poisons the handle, and
halts core mutation/dispatch. Never claim an undurable cancellation or result was
committed. No failure auto-deletes history. Tests exercise exact encoded storage
boundaries, JSON expansion, and cancellation after output budget exhaustion.

Recovery reports discarded incomplete-final-frame bytes in
`Journal.RecoveredTailBytes`, truncates only that tail, and syncs the truncation.
Complete corrupt frames and corrupt/incomplete file headers preserve all bytes
and fail closed. A failed initial header creation therefore requires operator
inspection; it is not silently replaced with a fresh identity.

The journal keeps current state and a record-offset index in memory. Replay reads
records from disk; it does not retain all historical full job replacements.

This trades indexing and long histories for a small inspectable first version.
A bounded store can fill; supported maintenance/compaction is future work, and
manual truncation is not a recovery procedure. A clean new data directory starts
a separate store ID/history, with the old directory retained for inspection.

## Alternatives and costs

| Option | Advantage | Cost / decision |
| --- | --- | --- |
| Synced framed journal | Standard library, no CGO, transactions align with events | Custom recovery/locking tests, linear replay, bounded history; selected |
| SQLite with native driver | Mature transactions, indexes, maintenance tools | C toolchain/CGO complicates cross-compilation; deferred |
| SQLite with pure-Go driver | CGO-free database semantics | External module graph and engine increase binary size; deferred |
| Whole-state JSON overwrite | Simple initial code | Rewrites output/history; platform replace/directory durability and atomic receipt/event coordination still need care; rejected |

No database benchmark or future release binary-size estimate is claimed.
The executable now links core, journal, HTTP/SSE, CLI and Ollama. Standard-library
storage preserves the six CGO-disabled build targets without a database library.

Reference: Go's [`File.Sync`](https://pkg.go.dev/os#File.Sync) contract.
