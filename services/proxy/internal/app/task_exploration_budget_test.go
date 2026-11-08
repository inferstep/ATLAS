package app

import (
	"strings"
	"testing"

	"github.com/itigges22/atlas-proxy/internal/taskcontract"
)

// A request that demands no change is answered by reading, and its writes are
// refused, so the note after a run of reads asks for the answer. A request
// that demands a change keeps the write notes.
func TestTheExplorationNoteFollowsTheRequest(t *testing.T) {
	for _, reads := range []int{4, 5} {
		n := reads
		note := explorationNudge(&n, 1, false)
		if note != explorationAnswer || strings.Contains(note, "write_file") || strings.Contains(note, "Do not read") {
			t.Errorf("no change demanded, %d reads: %q", reads, note)
		}
	}
	n := 3
	if got := explorationNudge(&n, 1, true); got != "" {
		t.Errorf("3 reads: %q", got)
	}
	n = 4
	if got := explorationNudge(&n, 1, true); got != explorationWarning || n != 4 {
		t.Errorf("4 reads: %q (count %d)", got, n)
	}
	n = 5
	if got := explorationNudge(&n, 1, true); got != explorationEscalated || n != 2 {
		t.Errorf("5 reads: %q (count %d)", got, n)
	}
}

// A question, or an explicit "change nothing", wants an answer; declared
// work, or no contract and no prohibition, keeps the write notes.
func TestAnAnswerOnlyRequestIsAQuestionOrAProhibition(t *testing.T) {
	cases := []struct {
		ctx  *AgentContext
		want bool
	}{
		{&AgentContext{runSetup: runSetup{TaskContract: &taskcontract.TaskContract{TaskMode: taskcontract.TaskModeQuestion}}}, true},
		{&AgentContext{runSetup: runSetup{HumanTask: "Just explain the parser. Do not change any code."}}, true},
		{&AgentContext{runSetup: runSetup{TaskContract: &taskcontract.TaskContract{TaskMode: taskcontract.TaskModeWork}}}, false},
		{&AgentContext{runSetup: runSetup{HumanTask: "Fix the tie-break in the planner."}}, false},
	}
	for i, c := range cases {
		if got := answerOnlyRequest(c.ctx); got != c.want {
			t.Errorf("case %d: answerOnlyRequest = %v, want %v", i, got, c.want)
		}
	}
}
