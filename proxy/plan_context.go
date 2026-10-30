package main

import "strings"

// addFilesReadToPlanContext adds the files this run has read to the context of
// a revised plan, until the context holds `limit` files.
//
// When more files were read than fit, they are taken in sorted order of their
// paths. The run keeps no order of reading (FilesRead is a map), and the map's
// own order would make which files the planner sees a matter of chance.
func addFilesReadToPlanContext(ctx *AgentContext, pctx map[string]string, limit int) {
	files := ctx.SnapshotFilesRead()
	for _, path := range sortedKeys(files) {
		if len(pctx) >= limit {
			break
		}
		// Use relative path if possible so the planner key matches
		// what the agent will pass to read_file/edit_file later.
		rel := path
		if strings.HasPrefix(path, ctx.WorkingDir+"/") {
			rel = strings.TrimPrefix(path, ctx.WorkingDir+"/")
		}
		s := files[path]
		if len(s) > 2000 {
			s = s[:2000] + "\n... (truncated)"
		}
		pctx[rel] = s
	}
}
