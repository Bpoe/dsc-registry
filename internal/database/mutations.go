package database

// MutationOutcome describes a committed change and is valid only on success.
type MutationOutcome uint8

const (
	MutationUnchanged MutationOutcome = iota
	MutationCreated
	MutationUpdated
)
