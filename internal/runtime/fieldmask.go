package runtime

import (
	"slices"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// FieldSet reports whether an Update writes the field name, given the
// request's update_mask paths and whether the field was present on the wire.
//
// An empty mask means the fields the caller set. A non-empty mask names the
// fields to write and nothing else: a named field is written from its wire
// value, NULL when unset, which is the one way to put NULL into a nullable
// column through Update. Both the dispatcher and the emitted server decide
// with this function, so the two cannot drift on what "set" means.
func FieldSet(mask []string, name string, present bool) bool {
	if len(mask) == 0 {
		return present
	}
	for _, p := range mask {
		if p == name {
			return true
		}
	}
	return false
}

// CheckFieldMask reports an update_mask path that names no updatable column
// as InvalidArgument. A primary-key, identity or serial column is not
// updatable, and a misspelt path would otherwise be ignored while the
// request is answered OK.
func CheckFieldMask(paths, updatable []string) error {
	for _, p := range paths {
		if !slices.Contains(updatable, p) {
			return status.Errorf(codes.InvalidArgument, "update_mask names %q, which is not an updatable field", p)
		}
	}
	return nil
}
