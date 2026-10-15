package main

import (
	"maps"
	"slices"
)

// sortedKeys gives a map's keys in sorted order. Go gives a map's keys in a
// new order each time, so a gate that names the first of several keys it meets
// would name another one from run to run, and one request would get two texts.
// A gate that names one key of a map takes them from here.
func sortedKeys[V any](m map[string]V) []string {
	return slices.Sorted(maps.Keys(m))
}
