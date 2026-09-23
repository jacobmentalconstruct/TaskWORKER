// Package core implements the transport-independent authoritative worker,
// lifecycle, queue, and observers. Backend and Store remain inward-facing ports.
// Only the future serve adapter constructs a production worker.
package core
