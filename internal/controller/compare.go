package controller

import "sort"

// equalStringSets reports whether a and b contain the same elements, treating a
// nil slice and an empty slice as equal and ignoring order. Drift comparisons
// must use this instead of reflect.DeepEqual: the Admin API returns array
// columns as empty (non-nil) slices with a server-chosen order, so a CR that
// leaves a field unset (nil) or lists values in a different order would
// otherwise be flagged as perpetually drifted, causing an endless
// reconcile→PATCH loop.
func equalStringSets(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	if len(a) == 0 {
		return true
	}
	ca := append([]string(nil), a...)
	cb := append([]string(nil), b...)
	sort.Strings(ca)
	sort.Strings(cb)
	for i := range ca {
		if ca[i] != cb[i] {
			return false
		}
	}
	return true
}
