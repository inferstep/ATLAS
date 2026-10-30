// Which files, and which lines of them, the model has been shown
// (AgentContext.BodySeen and BodyShown), and the AgentContext methods that
// record and answer it.

package main

import "sort"

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
func (c *AgentContext) RecordBodySeen(path string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.markBodySeenLocked(path)
	c.BodyShown[path] = &bodyCoverage{whole: true}
}

// RecordBodyRead marks lines from..to (1-based, inclusive) of a file of total
// lines as shown by a read. A read of every line shows the whole file, and a
// later partial read does not take that back.
func (c *AgentContext) RecordBodyRead(path string, from, to, total int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.markBodySeenLocked(path)
	cov := c.BodyShown[path]
	if cov == nil {
		cov = &bodyCoverage{}
		c.BodyShown[path] = cov
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

func (c *AgentContext) markBodySeenLocked(path string) {
	if c.BodySeen == nil {
		c.BodySeen = make(map[string]bool)
	}
	if c.BodyShown == nil {
		c.BodyShown = make(map[string]*bodyCoverage)
	}
	c.BodySeen[path] = true
}

// LineWasShown reports whether line n (1-based) of a file was shown to the
// model. A file with no coverage record counts as shown: whether it was
// opened at all is WasBodySeen's question.
func (c *AgentContext) LineWasShown(path string, n int) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	cov := c.BodyShown[path]
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
func (c *AgentContext) ShownSpans(path string) [][2]int {
	c.mu.Lock()
	defer c.mu.Unlock()
	cov := c.BodyShown[path]
	if cov == nil || cov.whole {
		return nil
	}
	return append([][2]int(nil), cov.spans...)
}

// bodySeenPaths returns every file whose contents the model has been shown,
// sorted so a caller that reports one of them reports the same one each time.
func (c *AgentContext) bodySeenPaths() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, 0, len(c.BodySeen))
	for p, ok := range c.BodySeen {
		if ok {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

// WasBodySeen reports whether the model has actually been shown this file's
// contents. Distinct from WasFileRead, which outline_file also satisfies.
func (c *AgentContext) WasBodySeen(path string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.BodySeen[path]
}
