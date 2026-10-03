// Package modalpb is the part of the modal client's gRPC protocol that
// frontend/modal serves: the ModalClient and TaskCommandRouter services cut
// down to the RPCs it implements, with the client's own package names, type
// names and field numbers, so the wire format is the client's.
//
// The modal wheel (PyPI modal 1.6.0, Apache-2.0, LICENSE here) ships no .proto
// files, only generated Python with the descriptors embedded: gen.sh extracts
// them (prune.py), checks the subset against the originals (check.py) and
// compiles it. Do not edit the generated files; edit prune.py's KEEP and rerun gen.sh.
package modalpb

//go:generate ./gen.sh
