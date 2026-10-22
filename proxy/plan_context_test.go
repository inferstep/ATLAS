package main

import (
	"fmt"
	"maps"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
)

// More files were read than fit into the context of a revised plan. The
// planner sees the first of them in sorted path order, every time, not
// whatever the map's order gives.
func TestTheFilesReadThatFillAPlansContextAreTheSameEachTime(t *testing.T) {
	dir := t.TempDir()
	ctx := NewAgentContext(dir, Tier2Medium)
	for n := 11; n >= 0; n-- {
		ctx.RecordFileRead(filepath.Join(dir, fmt.Sprintf("f%02d.py", n)), "x = 1\n")
	}
	want := []string{"f00.py", "f01.py", "f02.py", "f03.py", "f04.py", "f05.py", "f06.py", "f07.py"}
	for try := 1; try <= 100; try++ {
		pctx := map[string]string{}
		addFilesReadToPlanContext(ctx, pctx, 8)
		if got := slices.Sorted(maps.Keys(pctx)); !reflect.DeepEqual(got, want) {
			t.Fatalf("try %d: %v, want %v", try, got, want)
		}
	}
}
