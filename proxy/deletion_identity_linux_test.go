//go:build linux

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// --- The held reference ------------------------------------------------------
//
// What the approval binds to is a kernel reference to the inspected object,
// not two readings of numbers. These tests pin that: the reference answers
// from the object it holds, a recycled inode number cannot pass, and every
// path that ends an approval's life releases exactly one descriptor.

// openFDs counts this process's open descriptors. Every lifecycle test below
// asserts the count is unchanged once the path under test has run.
func openFDs(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatalf("cannot read /proc/self/fd: %v", err)
	}
	return len(entries)
}

// statInfo is an os.FileInfo whose Sys() reports chosen device and inode
// numbers -- the only way to present a RECYCLED number on a filesystem that
// does not recycle them, which is how the link-count guard is proved without
// an ext4 mount.
type statInfo struct {
	os.FileInfo
	st *syscall.Stat_t
}

func (s statInfo) Sys() interface{} { return s.st }

func lstatT(t *testing.T, p string) *syscall.Stat_t {
	t.Helper()
	var st syscall.Stat_t
	if err := syscall.Lstat(p, &st); err != nil {
		t.Fatalf("lstat %s: %v", p, err)
	}
	return &st
}

func TestHeldReferenceRefusesARecycledNumber(t *testing.T) {
	dir := t.TempDir()
	for _, c := range []struct {
		name   string
		make   func(p string)
		remake func(p string)
	}{
		{"file", func(p string) { os.WriteFile(p, []byte("A\n"), 0o644) }, func(p string) { os.WriteFile(p, []byte("A\n"), 0o644) }},
		{"symlink", func(p string) { os.Symlink("real", p) }, func(p string) { os.Symlink("real", p) }},
		{"dir", func(p string) { os.Mkdir(p, 0o755) }, func(p string) { os.Mkdir(p, 0o755) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := filepath.Join(dir, c.name)
			c.make(p)
			before := openFDs(t)
			h, err := pinObject(p)
			if err != nil {
				t.Fatalf("pin: %v", err)
			}
			info, _ := os.Lstat(p)
			if !h.stillTheObjectAt(info) {
				t.Fatal("the held object was not recognised at its own path")
			}
			pinned := lstatT(t, p)
			// The object loses its last name. Whatever the path shows now,
			// even an entry wearing the held object's exact numbers, is not it.
			os.Remove(p)
			c.remake(p)
			recycled := statInfo{FileInfo: info, st: pinned}
			if h.stillTheObjectAt(recycled) {
				t.Error("a recycled device+inode passed as the held object")
			}
			if now, _ := os.Lstat(p); h.stillTheObjectAt(now) {
				t.Error("the replacement passed as the held object")
			}
			h.release()
			h.release() // idempotent
			if h.stillTheObjectAt(info) {
				t.Error("a released reference answered true")
			}
			if got := openFDs(t); got != before {
				t.Errorf("descriptors: %d before, %d after", before, got)
			}
		})
	}
}

func TestHeldReferenceFollowsNothing(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "real.py")
	os.WriteFile(target, []byte("A\n"), 0o644)
	link := filepath.Join(dir, "l.py")
	os.Symlink("real.py", link)
	h, err := pinObject(link)
	if err != nil {
		t.Fatalf("pin: %v", err)
	}
	defer h.release()
	linkInfo, _ := os.Lstat(link)
	targetInfo, _ := os.Lstat(target)
	if !h.stillTheObjectAt(linkInfo) {
		t.Error("the link itself was not held")
	}
	if h.stillTheObjectAt(targetInfo) {
		t.Error("the reference followed the link to its target")
	}
	// Removing the target does not touch the held link's identity; the
	// content check (link text) is what would notice a retarget.
	os.Remove(target)
	if now, _ := os.Lstat(link); !h.stillTheObjectAt(now) {
		t.Error("a dangling link is still the same link object")
	}
}

// --- The object-identity matrix, end to end through the handshake and tool --
//
// One driver for every case: inspect and ask, let the case disturb the disk
// while the prompt is up, answer, run the real tool, and read the disk.
func driveApprovedDeletion(t *testing.T, dir, rel, sess string, churn func()) (gone bool, res *ToolResult) {
	t.Helper()
	ctx, cancel := deletePermCtx(t, sess, dir)
	defer cancel()
	ctx.StreamFn = func(string, interface{}) {}
	args := json.RawMessage(`{"path":"` + rel + `"}`)
	done := make(chan bool, 1)
	go func() { done <- awaitPermission(ctx, "delete_file", "call_m", args) }()
	waitForPending(t, sess, "call_m")
	if churn != nil {
		churn()
	}
	postDecision(t, `{"session_id":"`+sess+`","tool_call_id":"call_m","decision":"allow"}`)
	if !<-done {
		t.Fatal("the approval did not come through")
	}
	res = executeToolCall("delete_file", args, ctx)
	_, err := os.Lstat(filepath.Join(dir, rel))
	return os.IsNotExist(err), res
}

func TestApprovalObjectIdentityMatrix(t *testing.T) {
	cases := []struct {
		name     string
		setup    func(dir string) string
		churn    func(dir, rel string)
		wantGone bool
	}{
		// Replacements: a new object wearing the old one's face.
		{"file replaced with identical bytes", mkFile, func(d, r string) { os.Remove(filepath.Join(d, r)); mkFile(d) }, false},
		{"symlink replaced with the same text", mkLink, func(d, r string) { os.Remove(filepath.Join(d, r)); mkLink(d) }, false},
		{"empty directory replaced by another", mkDir, func(d, r string) { os.Remove(filepath.Join(d, r)); mkDir(d) }, false},
		// In-place changes the reference alone would not see.
		{"file bytes changed in place", mkFile, func(d, r string) { os.WriteFile(filepath.Join(d, r), []byte("B\n"), 0o644) }, false},
		{"symlink retargeted", mkLink, func(d, r string) { os.Remove(filepath.Join(d, r)); os.Symlink("other.py", filepath.Join(d, r)) }, false},
		{"directory gained a child", mkDir, func(d, r string) { os.WriteFile(filepath.Join(d, r, "x"), []byte("x"), 0o644) }, false},
		{"file became a directory", mkFile, func(d, r string) { os.Remove(filepath.Join(d, r)); os.Mkdir(filepath.Join(d, r), 0o755) }, false},
		{"file became a symlink", mkFile, func(d, r string) { os.Remove(filepath.Join(d, r)); os.Symlink("real.py", filepath.Join(d, r)) }, false},
		// Unchanged: the approval is honoured.
		{"unchanged file", mkFile, func(string, string) {}, true},
		{"unchanged symlink", mkLink, func(string, string) {}, true},
		{"unchanged empty directory", mkDir, func(string, string) {}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			os.WriteFile(filepath.Join(dir, "real.py"), []byte("A\n"), 0o644)
			rel := c.setup(dir)
			before := openFDs(t)
			gone, res := driveApprovedDeletion(t, dir, rel, "sess-mx-"+strings.ReplaceAll(c.name, " ", "-"),
				func() { c.churn(dir, rel) })
			if gone != c.wantGone {
				t.Errorf("%s: deleted=%v want %v (success=%v err=%.60s)", c.name, gone, c.wantGone, res.Success, res.Error)
			}
			if !c.wantGone && res.Success {
				t.Errorf("%s: a stale approval reported success", c.name)
			}
			if got := openFDs(t); got != before {
				t.Errorf("%s: descriptors %d before, %d after", c.name, before, got)
			}
			// Whatever survives on disk is the replacement, never touched.
			if !c.wantGone {
				if _, err := os.Lstat(filepath.Join(dir, rel)); err != nil {
					t.Errorf("%s: the replacement was removed", c.name)
				}
			}
		})
	}
}

func mkFile(dir string) string {
	os.WriteFile(filepath.Join(dir, "f.py"), []byte("A\n"), 0o644)
	return "f.py"
}
func mkLink(dir string) string { os.Symlink("real.py", filepath.Join(dir, "l.py")); return "l.py" }
func mkDir(dir string) string  { os.Mkdir(filepath.Join(dir, "d"), 0o755); return "d" }

// Alias spellings of one path are one canonical identity: approved as one
// spelling, removable as another.
func TestApprovalAliasSpellingsAreOneIdentity(t *testing.T) {
	dir := t.TempDir()
	mkFile(dir)
	ctx, cancel := deletePermCtx(t, "sess-alias", dir)
	defer cancel()
	ctx.StreamFn = func(string, interface{}) {}
	before := openFDs(t)
	done := make(chan bool, 1)
	go func() { done <- awaitPermission(ctx, "delete_file", "call_a", json.RawMessage(`{"path":"./f.py"}`)) }()
	waitForPending(t, "sess-alias", "call_a")
	postDecision(t, `{"session_id":"sess-alias","tool_call_id":"call_a","decision":"allow"}`)
	if !<-done {
		t.Fatal("approval did not come through")
	}
	res := executeToolCall("delete_file", json.RawMessage(`{"path":"f.py"}`), ctx)
	if !res.Success {
		t.Fatalf("the alias spelling was not honoured: %s", res.Error)
	}
	if _, err := os.Lstat(filepath.Join(dir, "f.py")); !os.IsNotExist(err) {
		t.Error("the approved file survived")
	}
	if got := openFDs(t); got != before {
		t.Errorf("descriptors %d before, %d after", before, got)
	}
}

// Two hard-linked names are one object and two identities: an approval for
// one name is not an approval for the other, and taking it for the other name
// spends and releases it.
func TestHardLinkedNamesStaySeparatedByCanonicalPath(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.py")
	os.WriteFile(a, []byte("A\n"), 0o644)
	if err := os.Link(a, filepath.Join(dir, "b.py")); err != nil {
		t.Skipf("hard links unsupported here: %v", err)
	}
	ctx, cancel := deletePermCtx(t, "sess-hl", dir)
	defer cancel()
	ctx.StreamFn = func(string, interface{}) {}
	before := openFDs(t)
	done := make(chan bool, 1)
	go func() { done <- awaitPermission(ctx, "delete_file", "call_h", json.RawMessage(`{"path":"a.py"}`)) }()
	waitForPending(t, "sess-hl", "call_h")
	postDecision(t, `{"session_id":"sess-hl","tool_call_id":"call_h","decision":"allow"}`)
	if !<-done {
		t.Fatal("approval did not come through")
	}
	if _, ok := takeDeleteApproval(ctx, filepath.Join(dir, "b.py")); ok {
		t.Fatal("an approval for a.py was handed out for b.py")
	}
	if _, ok := takeDeleteApproval(ctx, a); ok {
		t.Fatal("the grant survived being taken for the wrong path")
	}
	if got := openFDs(t); got != before {
		t.Errorf("descriptors %d before, %d after: the spent grant was not released", before, got)
	}
	for _, n := range []string{"a.py", "b.py"} {
		if _, err := os.Lstat(filepath.Join(dir, n)); err != nil {
			t.Errorf("%s was removed without its own approval", n)
		}
	}
}

// No reference, no prompt, no deletion.
func TestIdentityUnavailableFailsClosedBeforeAsking(t *testing.T) {
	dir := t.TempDir()
	mkFile(dir)
	saved := pinObjectFn
	pinObjectFn = func(string) (*objectHandle, error) { return nil, errObjectIdentityUnavailable }
	defer func() { pinObjectFn = saved }()

	ctx, cancel := deletePermCtx(t, "sess-noid", dir)
	defer cancel()
	prompts := 0
	var mu sync.Mutex
	ctx.StreamFn = func(et string, _ interface{}) {
		if et == "permission_request" {
			mu.Lock()
			prompts++
			mu.Unlock()
		}
	}
	before := openFDs(t)
	if awaitPermission(ctx, "delete_file", "call_n", json.RawMessage(`{"path":"f.py"}`)) {
		t.Fatal("a deletion was approved with no held object")
	}
	mu.Lock()
	defer mu.Unlock()
	if prompts != 0 {
		t.Errorf("%d prompt(s) were shown for an object that could not be held", prompts)
	}
	if _, ok := pendingPermissions.Load(permKey("sess-noid", "call_n")); ok {
		t.Error("a pending permission was left behind")
	}
	if _, err := os.Lstat(filepath.Join(dir, "f.py")); err != nil {
		t.Error("the file was removed")
	}
	if got := openFDs(t); got != before {
		t.Errorf("descriptors %d before, %d after", before, got)
	}
	// And a target that somehow carries no reference never matches.
	var d deleteTarget
	info, _ := os.Lstat(filepath.Join(dir, "f.py"))
	if d.identityMatches(deleteTarget{info: info, Canonical: filepath.Join(dir, "f.py")}) {
		t.Error("a target with no held reference matched")
	}
}

// --- Every way an approval's life can end releases its reference once -------

func TestHeldReferenceIsReleasedOnEveryPath(t *testing.T) {
	type flow func(t *testing.T, dir, sess string)
	ask := func(t *testing.T, ctx *AgentContext, sess, call, rel string) chan bool {
		t.Helper()
		done := make(chan bool, 1)
		go func() { done <- awaitPermission(ctx, "delete_file", call, json.RawMessage(`{"path":"`+rel+`"}`)) }()
		waitForPending(t, sess, call)
		return done
	}
	answer := func(t *testing.T, sess, call, decision string) {
		t.Helper()
		postDecision(t, fmt.Sprintf(`{"session_id":%q,"tool_call_id":%q,"decision":%q}`, sess, call, decision))
	}
	paths := map[string]flow{
		"approved and removed": func(t *testing.T, dir, sess string) {
			ctx, cancel := deletePermCtx(t, sess, dir)
			defer cancel()
			ctx.StreamFn = func(string, interface{}) {}
			done := ask(t, ctx, sess, "c", mkFile(dir))
			answer(t, sess, "c", "allow")
			if !<-done {
				t.Fatal("not allowed")
			}
			if res := executeToolCall("delete_file", json.RawMessage(`{"path":"f.py"}`), ctx); !res.Success {
				t.Fatalf("removal failed: %s", res.Error)
			}
		},
		"approved but stale": func(t *testing.T, dir, sess string) {
			ctx, cancel := deletePermCtx(t, sess, dir)
			defer cancel()
			ctx.StreamFn = func(string, interface{}) {}
			done := ask(t, ctx, sess, "c", mkFile(dir))
			os.WriteFile(filepath.Join(dir, "f.py"), []byte("B\n"), 0o644)
			answer(t, sess, "c", "allow")
			if !<-done {
				t.Fatal("not allowed")
			}
			if res := executeToolCall("delete_file", json.RawMessage(`{"path":"f.py"}`), ctx); res.Success {
				t.Fatal("a stale approval deleted")
			}
		},
		"denied": func(t *testing.T, dir, sess string) {
			ctx, cancel := deletePermCtx(t, sess, dir)
			defer cancel()
			ctx.StreamFn = func(string, interface{}) {}
			done := ask(t, ctx, sess, "c", mkFile(dir))
			answer(t, sess, "c", "deny")
			if <-done {
				t.Fatal("denied but allowed")
			}
		},
		"malformed decision": func(t *testing.T, dir, sess string) {
			ctx, cancel := deletePermCtx(t, sess, dir)
			defer cancel()
			ctx.StreamFn = func(string, interface{}) {}
			done := ask(t, ctx, sess, "c", mkFile(dir))
			answer(t, sess, "c", "maybe")
			if <-done {
				t.Fatal("a malformed decision allowed")
			}
		},
		"timeout": func(t *testing.T, dir, sess string) {
			t.Setenv("ATLAS_PERMISSION_TIMEOUT_SEC", "1")
			ctx, cancel := deletePermCtx(t, sess, dir)
			defer cancel()
			ctx.StreamFn = func(string, interface{}) {}
			done := ask(t, ctx, sess, "c", mkFile(dir))
			if <-done {
				t.Fatal("a timeout allowed")
			}
		},
		"request cancelled (client disconnect)": func(t *testing.T, dir, sess string) {
			ctx, cancel := deletePermCtx(t, sess, dir)
			ctx.StreamFn = func(string, interface{}) {}
			done := ask(t, ctx, sess, "c", mkFile(dir))
			cancel()
			if <-done {
				t.Fatal("a cancelled request allowed")
			}
		},
		"duplicate decision": func(t *testing.T, dir, sess string) {
			ctx, cancel := deletePermCtx(t, sess, dir)
			defer cancel()
			ctx.StreamFn = func(string, interface{}) {}
			done := ask(t, ctx, sess, "c", mkFile(dir))
			answer(t, sess, "c", "deny")
			if <-done {
				t.Fatal("denied but allowed")
			}
			w := postDecision(t, fmt.Sprintf(`{"session_id":%q,"tool_call_id":"c","decision":"allow"}`, sess))
			if w.Code != 404 {
				t.Errorf("a second decision was accepted: %d", w.Code)
			}
		},
		"preflight failure after the reference was taken": func(t *testing.T, dir, sess string) {
			// A non-empty directory is refused AFTER the object was held.
			os.Mkdir(filepath.Join(dir, "d"), 0o755)
			os.WriteFile(filepath.Join(dir, "d", "x"), []byte("x"), 0o644)
			ctx, cancel := deletePermCtx(t, sess, dir)
			defer cancel()
			ctx.StreamFn = func(string, interface{}) {}
			if awaitPermission(ctx, "delete_file", "c", json.RawMessage(`{"path":"d"}`)) {
				t.Fatal("a non-empty directory was approved")
			}
		},
		"capacity refusal": func(t *testing.T, dir, sess string) {
			ctx, cancel := deletePermCtx(t, sess, dir)
			defer cancel()
			ctx.StreamFn = func(string, interface{}) {}
			ctx.mu.Lock()
			ctx.deletionAttempts = map[string]*deletionAttempt{}
			for i := 0; i < maxTrackedDeletions; i++ {
				ctx.deletionAttempts[fmt.Sprintf("/x/%d", i)] = &deletionAttempt{Removed: true}
			}
			ctx.mu.Unlock()
			done := ask(t, ctx, sess, "c", mkFile(dir))
			answer(t, sess, "c", "allow")
			if !<-done {
				t.Fatal("not allowed")
			}
			res := executeToolCall("delete_file", json.RawMessage(`{"path":"f.py"}`), ctx)
			if res.Success {
				t.Fatal("an untrackable deletion was performed")
			}
			if _, err := os.Lstat(filepath.Join(dir, "f.py")); err != nil {
				t.Fatal("the file was removed at the tracking ceiling")
			}
		},
		"grant replaced by a later grant": func(t *testing.T, dir, sess string) {
			ctx, cancel := deletePermCtx(t, sess, dir)
			defer cancel()
			ctx.StreamFn = func(string, interface{}) {}
			done := ask(t, ctx, sess, "c1", mkFile(dir))
			answer(t, sess, "c1", "allow")
			<-done
			os.WriteFile(filepath.Join(dir, "g.py"), []byte("G\n"), 0o644)
			done = ask(t, ctx, sess, "c2", "g.py")
			answer(t, sess, "c2", "allow")
			<-done
			// Only the second grant is held; consuming it releases the last one.
			if res := executeToolCall("delete_file", json.RawMessage(`{"path":"g.py"}`), ctx); !res.Success {
				t.Fatalf("removal failed: %s", res.Error)
			}
			if _, err := os.Lstat(filepath.Join(dir, "f.py")); err != nil {
				t.Fatal("the replaced grant's file was removed")
			}
		},
		"terminal cleanup without execution": func(t *testing.T, dir, sess string) {
			ctx, cancel := deletePermCtx(t, sess, dir)
			defer cancel()
			ctx.StreamFn = func(string, interface{}) {}
			done := ask(t, ctx, sess, "c", mkFile(dir))
			answer(t, sess, "c", "allow")
			<-done
			releaseDeleteApproval(ctx) // what the loop's exit does
			if _, ok := takeDeleteApproval(ctx, filepath.Join(dir, "f.py")); ok {
				t.Fatal("a released approval was still available")
			}
			if _, err := os.Lstat(filepath.Join(dir, "f.py")); err != nil {
				t.Fatal("cleanup removed the file")
			}
		},
	}
	for name, run := range paths {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			sess := "sess-life-" + strings.ReplaceAll(name, " ", "-")
			before := openFDs(t)
			run(t, dir, sess)
			if got := openFDs(t); got != before {
				t.Errorf("%s: descriptors %d before, %d after", name, before, got)
			}
			if _, ok := pendingPermissions.Load(permKey(sess, "c")); ok {
				t.Errorf("%s: a pending permission was left behind", name)
			}
		})
	}
}

// A decision and a cancellation racing each other end the approval exactly
// once, and the reference is released whichever wins. Run under -race.
func TestConcurrentDecisionAndCancellationReleaseOnce(t *testing.T) {
	before := openFDs(t)
	for i := 0; i < 25; i++ {
		dir := t.TempDir()
		mkFile(dir)
		sess := fmt.Sprintf("sess-race-%d", i)
		reqCtx, cancel := context.WithCancel(context.Background())
		ctx := &AgentContext{PassID: sess, Ctx: reqCtx, WorkingDir: dir,
			FilesRead: map[string]string{}, FileReadTimes: map[string]time.Time{}, SessionWrites: map[string]bool{}}
		ctx.StreamFn = func(string, interface{}) {}
		done := make(chan bool, 1)
		go func() { done <- awaitPermission(ctx, "delete_file", "c", json.RawMessage(`{"path":"f.py"}`)) }()
		waitForPending(t, sess, "c")
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			postDecision(t, fmt.Sprintf(`{"session_id":%q,"tool_call_id":"c","decision":"allow"}`, sess))
		}()
		go func() { defer wg.Done(); cancel() }()
		allowed := <-done
		wg.Wait()
		if allowed {
			// The grant holds the reference; consuming it releases it.
			releaseDeleteApproval(ctx)
		}
		if _, ok := pendingPermissions.Load(permKey(sess, "c")); ok {
			t.Fatalf("iteration %d: pending permission left behind", i)
		}
	}
	if got := openFDs(t); got != before {
		t.Errorf("descriptors %d before, %d after %d racing iterations", before, got, 25)
	}
}
