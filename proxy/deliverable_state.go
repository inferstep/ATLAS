// DeliverableState, the ledger's record of one path: the bytes last seen on
// disk, the verdict about exact bytes, the checkpoint and the tombstone; and
// the ceilings on what the ledger keeps in memory and re-reads to hash.

package main

// DeliverableState is one canonical path's observed state. Four different
// things are kept apart deliberately and must never be collapsed: what is on
// disk now, what evidence exists about some exact bytes, whether a restorable
// checkpoint is available, and task correctness -- which this type never
// claims and never stores.
type DeliverableState struct {
	Path        string // canonical workspace-relative
	CurrentHash string // sha256 of the bytes last observed on disk
	CurrentSize int
	Generation  int // increments on every observed mutation

	// The verdict and the EXACT bytes it describes. A verdict whose
	// ValidatedHash differs from CurrentHash is historical, never current.
	ValidationKind   ValidationKind
	ValidationStatus ValidationStatus
	ValidationDetail string
	ValidatedHash    string

	// At most one prior demonstrably-passed version. Bytes are held in
	// memory only, under explicit ceilings.
	CheckpointHash        string
	CheckpointBytes       []byte
	CheckpointUnavailable string // why there is none, when there is none
	// CheckpointKind is the kind of check the checkpoint's pass came from.
	// Restoration compares kinds: a structural pass is not evidence about
	// syntax, so swapping one for the other would be a guess dressed as
	// recovery.
	CheckpointKind ValidationKind
	// Recovered records that the CURRENT bytes were put there by system
	// recovery rather than by the model. It is a server-side fact like the
	// rest of the ledger and never reaches the model, SSE or the wire.
	Recovered bool

	// Tombstones. A deliberately removed file must never be resurrected, so
	// the state is recorded and restoration is prohibited outright.
	Tombstoned        bool
	TombstoneReason   string // "deleted" | "moved:<canonical dst>"
	RestoreProhibited bool
}

// CurrentValidation reports the verdict ONLY when it describes the bytes
// currently recorded. Anything else is historical evidence and is reported as
// unknown, which fails closed.
func (d *DeliverableState) CurrentValidation() (ValidationKind, ValidationStatus) {
	if d == nil || d.ValidatedHash == "" || d.ValidatedHash != d.CurrentHash {
		return ValidationKindUnknown, ValidationUnknown
	}
	return d.ValidationKind, d.ValidationStatus
}

// Checkpoint ceilings. Stage-1 artifacts were 1.3-7.2 KiB and the largest
// fenced payload observed live was 10.8 KiB, so 256 KiB is ~25x the worst
// case while bounding a pathological file, and 2 MiB caps a session at
// roughly eight such files. Over budget fails closed: the observation is
// kept, the bytes are not, and the reason is recorded.
// maxLedgerReadBytes bounds what the ledger will re-read to hash. A file above
// it is not hashed at all, so its verdict reads as unknown rather than as a
// stale claim about bytes nobody looked at.
const (
	maxCheckpointFileBytes    = 256 * 1024
	maxCheckpointSessionBytes = 2 * 1024 * 1024
	maxLedgerReadBytes        = 8 * 1024 * 1024
)
