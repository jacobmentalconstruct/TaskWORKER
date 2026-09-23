// Package journal implements core.Store with a versioned, checksummed, synced
// append-only journal and OS-backed exclusive ownership. See docs/persistence.md
// for the binary format, finite storage reserves, and platform durability limits.
package journal
