// Package engine is wisp's provider-neutral sandbox engine: it creates,
// boots, suspends, checkpoints, backs up and deletes Firecracker microVMs, and
// wakes them on demand. An API front end (internal/server serves the Sprites
// API) sits on top: it names sandboxes, authenticates callers and speaks its
// own protocol, and reaches a sandbox only through an *Engine.
//
// The engine knows sandboxes by ID (store.Record). Whatever a front end keeps
// beside a record (a name, a URL, labels) is stored with it but never read
// here, except through the hooks the front end installs: a Describer to name a
// record in events and the log, OnDelete for its part of a deletion, and
// SetGuestAPI for the handler a guest reaches over its host channel.
package engine
