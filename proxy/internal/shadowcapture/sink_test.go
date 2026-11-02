package shadowcapture

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// readRecords reads every JSONL record of the capture at path.
func readRecords(t *testing.T, path string) []map[string]interface{} {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("capture unreadable: %v", err)
	}
	var out []map[string]interface{}
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var m map[string]interface{}
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("malformed JSONL line %q: %v", line, err)
		}
		out = append(out, m)
	}
	return out
}

// Disabled is the deployed default: nothing opens, nothing is written.
func TestShadowSinkDisabledByDefault(t *testing.T) {
	dir := t.TempDir()
	// No capture is named.
	sink, err := Open(dir, "")
	if err != nil {
		t.Fatalf("disabled must not error: %v", err)
	}
	if sink != nil {
		t.Fatal("a sink was opened with no capture configured")
	}
	if !sink.Enabled() { // nil receiver must answer false, not panic
		// expected
	} else {
		t.Error("a nil sink reports enabled")
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Errorf("disabled mode created %d file(s)", len(entries))
	}
}

// The capture records how ATLAS judged the requests of each session, so only
// its owner may read it.
func TestShadowCaptureFileIsReadableByItsOwnerOnly(t *testing.T) {
	dir := t.TempDir()
	sink, err := Open(dir, "mode.jsonl")
	if err != nil {
		t.Fatalf("sink: %v", err)
	}
	t.Cleanup(func() { _ = sink.Close(context.Background(), time.Second) })
	info, err := os.Stat(filepath.Join(dir, "mode.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("the capture file has mode %o, want 600", info.Mode().Perm())
	}
}

// Initialization refuses anything it cannot own: an existing destination, or
// one outside the capture root.
func TestShadowSinkInitializationRefusals(t *testing.T) {
	t.Run("existing destination", func(t *testing.T) {
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "run.jsonl"), []byte("old\n"), 0o644)
		if _, err := Open(dir, "run.jsonl"); err == nil {
			t.Error("an existing capture was accepted; a mixed run would look like one run")
		}
	})
	for _, escape := range []string{"../outside.jsonl", "/etc/passwd", "sub/../../out.jsonl"} {
		t.Run("escape "+escape, func(t *testing.T) {
			dir := t.TempDir()
			if _, err := Open(dir, escape); err == nil {
				t.Errorf("%q escaped the capture root and was accepted", escape)
			}
		})
	}
}

// A full queue drops, counts, and never blocks the caller.
func TestShadowSinkDropsWhenFull(t *testing.T) {
	dir := t.TempDir()
	f, err := os.Create(filepath.Join(dir, "run.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	// The writer is not started, so nothing drains the queue while it fills.
	sink := newSink(f)
	for i := 0; i < QueueDepth*4; i++ {
		sink.Submit(map[string]interface{}{"record_kind": "x", "i": i})
	}
	go sink.run()
	if err := sink.Close(context.Background(), 5*time.Second); err != nil {
		t.Fatalf("close: %v", err)
	}
	recs := readRecords(t, filepath.Join(dir, "run.jsonl"))
	footer := recs[len(recs)-1]
	if footer["record_kind"] != "task_contract_shadow_footer" {
		t.Fatalf("last record is not a footer: %v", footer["record_kind"])
	}
	if footer["dropped"].(float64) <= 0 {
		t.Error("a saturated queue reported no drops")
	}
	if footer["accepted"].(float64) != float64(QueueDepth*4) {
		t.Errorf("accepted=%v", footer["accepted"])
	}
}

// Duplicate request IDs are a capture defect, never merged.
func TestShadowSinkFlagsDuplicateRequestIDs(t *testing.T) {
	dir := t.TempDir()
	sink, _ := Open(dir, "run.jsonl")
	sink.NoteRequest("req-1")
	sink.NoteRequest("req-2")
	sink.NoteRequest("req-1") // same id twice in one capture
	sink.Close(context.Background(), 5*time.Second)
	recs := readRecords(t, filepath.Join(dir, "run.jsonl"))
	footer := recs[len(recs)-1]
	if footer["duplicate_request_ids"].(float64) != 1 {
		t.Errorf("duplicate_request_ids=%v, want 1", footer["duplicate_request_ids"])
	}
}

// A capture with no footer is detectable as incomplete.
// An acquisition that never reached its close hook has no footer.
//
// The ungraceful fixture changed with the finalisation owner. It used to close
// the queue and wait for the writer, which is now exactly what a CLEAN close
// does -- the writer emits the footer after draining -- so closing the queue can
// no longer stand for a process that died. What a SIGKILL actually leaves is a
// file whose close hook never ran, which is what this now reads: the records on
// disk while the sink is still open.
func TestShadowCaptureWithoutFooterIsIncomplete(t *testing.T) {
	dir := t.TempDir()
	sink, _ := Open(dir, "run.jsonl")
	sink.Submit(map[string]interface{}{"record_kind": "task_contract_shadow_request"})
	waitWritten(t, sink, 1)
	recs := readRecords(t, filepath.Join(dir, "run.jsonl"))
	if len(recs) != 1 {
		t.Fatalf("%d records before any close, want 1", len(recs))
	}
	for _, r := range recs {
		if r["record_kind"] == "task_contract_shadow_footer" {
			t.Error("a capture wrote a footer without a close hook")
		}
	}
	sink.Close(context.Background(), 5*time.Second)
}

// waitWritten blocks until the writer has written n records.
func waitWritten(t *testing.T, s *Sink, n int64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for s.written.Load() < n {
		if time.Now().After(deadline) {
			t.Fatalf("writer wrote %d of %d records", s.written.Load(), n)
		}
		time.Sleep(time.Millisecond)
	}
}

// --- Submission lifecycle: open, closing, closed -----------------------------

// Submissions racing the close hook cannot panic, cannot block, and cannot
// leave the footer unable to account for them.
//
// A "check the flag, then send" pair is not enough on its own: close can land
// between the check and the send. Admission is therefore held for reading while
// a submitter decides AND enqueues, and the cutoff takes it for writing, so no
// submitter is inside when the queue closes.
func TestShadowSubmitRacesCloseWithoutPanic(t *testing.T) {
	dir := t.TempDir()
	sink, err := Open(dir, "race.jsonl")
	if err != nil {
		t.Fatalf("sink: %v", err)
	}
	const submitters = 16
	var wg sync.WaitGroup
	returned := make(chan struct{}, submitters)
	start := make(chan struct{})
	for i := 0; i < submitters; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			for j := 0; j < 64; j++ {
				sink.Submit(map[string]interface{}{
					"record_kind": "task_contract_shadow_gate", "i": i, "j": j})
			}
			returned <- struct{}{}
		}(i)
	}
	close(start)
	time.Sleep(2 * time.Millisecond) // let submitters get going
	if err := sink.Close(context.Background(), 5*time.Second); err != nil {
		t.Fatalf("close: %v", err)
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("a submitter never returned: submission blocked on closure")
	}
	if len(returned) != submitters {
		t.Errorf("%d of %d submitters returned", len(returned), submitters)
	}

	accepted, written, dropped := sink.accepted.Load(), sink.written.Load(), sink.dropped.Load()
	refused := sink.refused.Load()
	t.Logf("accepted=%d written=%d dropped=%d refused=%d", accepted, written, dropped, refused)
	if accepted != written+dropped {
		t.Errorf("accepted %d != written %d + dropped %d", accepted, written, dropped)
	}
	if accepted+refused != submitters*64 {
		t.Errorf("accepted %d + refused %d != %d submitted", accepted, refused, submitters*64)
	}
	recs := readRecords(t, filepath.Join(dir, "race.jsonl"))
	footer := recs[len(recs)-1]
	if footer["record_kind"] != "task_contract_shadow_footer" {
		t.Fatalf("last record is %v, not the footer", footer["record_kind"])
	}
	if got := int64(footer["written"].(float64)); int64(len(recs)-1) != got {
		t.Errorf("%d records before the footer, footer says %d written", len(recs)-1, got)
	}
}

// A record submitted after the cutoff is inert: no panic, not accepted, and
// not able to contradict a footer that has already been written.
func TestShadowSubmitAfterCleanCloseIsRefused(t *testing.T) {
	dir := t.TempDir()
	sink, err := Open(dir, "late.jsonl")
	if err != nil {
		t.Fatalf("sink: %v", err)
	}
	sink.Submit(map[string]interface{}{"record_kind": "task_contract_shadow_request"})
	if err := sink.Close(context.Background(), 5*time.Second); err != nil {
		t.Fatalf("close: %v", err)
	}
	before, err := os.ReadFile(filepath.Join(dir, "late.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	sink.Submit(map[string]interface{}{"record_kind": "task_contract_shadow_gate"})
	if got := sink.refused.Load(); got != 1 {
		t.Errorf("refused=%d, want 1", got)
	}
	if got := sink.accepted.Load(); got != 1 {
		t.Errorf("a post-cutoff record was accepted: accepted=%d", got)
	}
	after, err := os.ReadFile(filepath.Join(dir, "late.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Error("a post-cutoff record reached the finalised file")
	}
	// Closing again is idempotent and reports the same outcome.
	if err := sink.Close(context.Background(), time.Second); err != nil {
		t.Errorf("second close: %v", err)
	}
}

// --- Single-owner finalisation -----------------------------------------------

// A clean close: the writer drains, emits the footer as the final line, closes
// the descriptor, and stops. Nothing else writes to the file.
func TestShadowCleanCloseFinalisesExactlyOnce(t *testing.T) {
	dir := t.TempDir()
	before := runtime.NumGoroutine()
	sink, err := Open(dir, "clean.jsonl")
	if err != nil {
		t.Fatalf("sink: %v", err)
	}
	for i := 0; i < 50; i++ {
		sink.Submit(map[string]interface{}{
			"record_kind": "task_contract_shadow_gate", "i": i})
	}
	if err := sink.Close(context.Background(), 5*time.Second); err != nil {
		t.Fatalf("close: %v", err)
	}
	recs := readRecords(t, filepath.Join(dir, "clean.jsonl"))
	footer := recs[len(recs)-1]
	if footer["record_kind"] != "task_contract_shadow_footer" {
		t.Fatalf("last record is %v", footer["record_kind"])
	}
	for _, r := range recs[:len(recs)-1] {
		if r["record_kind"] == "task_contract_shadow_footer" {
			t.Error("more than one footer")
		}
	}
	if got := int64(footer["written"].(float64)); got != 50 || int64(len(recs)-1) != got {
		t.Errorf("footer written=%d, file holds %d records", got, len(recs)-1)
	}
	if got := footer["accepted"].(float64); got != 50 {
		t.Errorf("accepted=%v", got)
	}
	// The writer is finished and owns nothing further.
	select {
	case <-sink.done:
	default:
		t.Error("the writer goroutine is still running after a clean close")
	}
	if _, err := sink.f.Write([]byte("x")); err == nil {
		t.Error("the descriptor is still open after a clean close")
	}
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > before && time.Now().Before(deadline) {
		runtime.Gosched()
	}
	if after := runtime.NumGoroutine(); after > before {
		t.Errorf("goroutines %d -> %d after a clean close", before, after)
	}
}

// A writer that cannot finish within the hook's deadline leaves a file with no
// footer. The hook returns bounded and says the capture is incomplete; nothing
// manufactures a footer that a reader could mistake for a full acquisition.
//
// The writer here is simply never started, which is the same thing the hook can
// observe as a writer blocked in a filesystem call: no progress before the
// deadline. A write already inside a blocking syscall cannot be interrupted
// portably, so the hook never tries -- see the comment on close().
func TestShadowBlockedWriterDeadlineWritesNoFooter(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "stuck.jsonl")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sink := newSink(f) // writer never started: nothing drains the queue
	sink.Submit(map[string]interface{}{"record_kind": "task_contract_shadow_request"})

	started := time.Now()
	err = sink.Close(context.Background(), 150*time.Millisecond)
	elapsed := time.Since(started)
	if err == nil {
		t.Error("a capture that never finalised reported success")
	}
	if elapsed > 3*time.Second {
		t.Errorf("the close hook took %v, well past its deadline", elapsed)
	}
	b, rerr := os.ReadFile(path)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if strings.Contains(string(b), "task_contract_shadow_footer") {
		t.Error("a timed-out close manufactured a footer")
	}
	t.Logf("incomplete capture: %d bytes, err=%v, elapsed=%v", len(b), err, elapsed)
}

// A writer that fails on every record counts each failure, writes nothing, and
// says at close that the capture is defective.
func TestAFailingWriterCountsErrorsAndWritesNothing(t *testing.T) {
	f, err := os.Create(filepath.Join(t.TempDir(), "broken.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	f.Close() // every write now fails at the syscall
	sink := Start(f)
	for i := 0; i < 3; i++ {
		sink.Submit(map[string]interface{}{"record_kind": "x", "i": i})
	}
	if err := sink.Close(context.Background(), 5*time.Second); err == nil {
		t.Error("a capture whose footer could not be written closed without an error")
	}
	if got := sink.errors.Load(); got != 3 {
		t.Errorf("errors=%d, want 3", got)
	}
	if got := sink.written.Load(); got != 0 {
		t.Errorf("a failing writer reported %d records written", got)
	}
}
