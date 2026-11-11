// Which files, and which lines of them, the model has been shown
// (fileHistory.BodySeen and BodyShown), and the fileHistory methods that
// record and answer it.

package app

import (
	"path/filepath"
	"sort"
)

// bodyCoverage is the part of a file the model has been shown: all of it, or
// 1-based inclusive line spans.
type bodyCoverage struct {
	whole bool
	spans [][2]int
}

// RecordBodySeen marks a file's contents as having been shown to the model in
// full: written or edited by it, or delivered to it. See BodySeen for why this
// is not the same as being in the read cache. It also ends any partial-read
// record, because the line numbers of an earlier read no longer hold for the
// new bytes.
func (f *fileHistory) RecordBodySeen(path string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.markBodySeenLocked(path)
	f.BodyShown[path] = &bodyCoverage{whole: true}
}

// RecordBodyRead marks lines from..to (1-based, inclusive) of a file of total
// lines as shown by a read. A read of every line shows the whole file, and a
// later partial read does not take that back.
func (f *fileHistory) RecordBodyRead(path string, from, to, total int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.markBodySeenLocked(path)
	cov := f.BodyShown[path]
	if cov == nil {
		cov = &bodyCoverage{}
		f.BodyShown[path] = cov
	}
	if cov.whole {
		return
	}
	if from <= 1 && to >= total {
		cov.whole, cov.spans = true, nil
		return
	}
	if to >= from {
		cov.spans = append(cov.spans, [2]int{from, to})
	}
}

func (f *fileHistory) markBodySeenLocked(path string) {
	if f.BodySeen == nil {
		f.BodySeen = make(map[string]bool)
	}
	if f.BodyShown == nil {
		f.BodyShown = make(map[string]*bodyCoverage)
	}
	f.BodySeen[path] = true
}

// LineWasShown reports whether line n (1-based) of a file was shown to the
// model. A file with no coverage record counts as shown: whether it was
// opened at all is WasBodySeen's question.
func (f *fileHistory) LineWasShown(path string, n int) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	cov := f.BodyShown[path]
	if cov == nil || cov.whole {
		return true
	}
	for _, s := range cov.spans {
		if n >= s[0] && n <= s[1] {
			return true
		}
	}
	return false
}

// ShownSpans returns the line spans a partly read file has shown, or nil for a
// file shown whole or not recorded.
func (f *fileHistory) ShownSpans(path string) [][2]int {
	f.mu.Lock()
	defer f.mu.Unlock()
	cov := f.BodyShown[path]
	if cov == nil || cov.whole {
		return nil
	}
	return append([][2]int(nil), cov.spans...)
}

// bodySeenPaths returns every file whose contents the model has been shown,
// sorted so a caller that reports one of them reports the same one each time.
func (f *fileHistory) bodySeenPaths() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.BodySeen))
	for p, ok := range f.BodySeen {
		if ok {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

// bodySeenBaseNames returns the base names of the files whose contents the
// model has been shown.
func (f *fileHistory) bodySeenBaseNames() map[string]bool {
	seen := map[string]bool{}
	f.mu.Lock()
	defer f.mu.Unlock()
	for path, ok := range f.BodySeen {
		if ok {
			seen[filepath.Base(path)] = true
		}
	}
	return seen
}

// WasBodySeen reports whether the model has actually been shown this file's
// contents. Distinct from WasFileRead, which outline_file also satisfies.
func (f *fileHistory) WasBodySeen(path string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.BodySeen[path]
}
