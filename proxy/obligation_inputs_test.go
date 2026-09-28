package main

import (
	"strings"
	"testing"
)

// A file the task says to read is an input, not something the run owes.
//
// The legacy heuristic took any filename within 70 characters after a write
// verb as an output, so "Write solve.py that reads input.txt" owed input.txt.
// The run never writes an input, so the ledger never owns it and it can never
// be demonstrated. Measured in stabilization cycle 2 (stabilization2/C): 12 aoc
// sessions, in both arms, wrote only solve.py, ran it, printed the correct
// answer and ended incomplete / deliverables_not_demonstrated.

func TestAFileTheTaskReadsIsNotAnOutputObligation(t *testing.T) {
	for prompt, want := range map[string]string{
		"Write solve.py that reads input.txt and prints how many fish exist after 256 days.":  "solve.py",
		"read the file input.txt and write the result to output.txt":                          "output.txt",
		"Write a script that loads the file config.yaml and saves the summary to report.json": "report.json",
		"Create parser.py that parses log.txt and writes errors.txt":                          "parser.py|errors.txt",
		"Write the parse results to out.json":                                                 "out.json",
	} {
		if got := strings.Join(expectedOutputPaths(prompt), "|"); got != want {
			t.Errorf("%q: outputs %q, want %q", prompt, got, want)
		}
	}
}

// The captured session shape, end to end: the run reads input.txt, writes
// solve.py, runs it and it prints the answer, then says done. Nothing the task
// asked for is outstanding, so the run completes.
func TestAnAocRunThatWritesAndRunsItsSolverCompletes(t *testing.T) {
	const prompt = "input.txt holds one command per line: 'forward N', 'down N' or 'up N'. Track horizontal " +
		"position, depth and aim, all starting at 0. 'down N' increases aim by N, 'up N' decreases aim by N, " +
		"and 'forward N' increases horizontal position by N AND increases depth by aim multiplied by N. " +
		"Write solve.py that reads input.txt and prints the final horizontal position multiplied by the final " +
		"depth. Then run it and confirm the answer."
	solve := "def main():\n    x = d = aim = 0\n    with open('input.txt') as f:\n        for line in f:\n" +
		"            op, n = line.split()\n            n = int(n)\n            if op == 'down':\n                aim += n\n" +
		"            elif op == 'up':\n                aim -= n\n            else:\n                x += n\n" +
		"                d += aim * n\n    print(x * d)\n\n\nif __name__ == '__main__':\n    main()\n"
	input := map[string]string{"input.txt": "forward 5\ndown 5\nforward 8\nup 3\ndown 8\nforward 2\n"}
	// The reliability runner sends {"task_mode":"work"} and nothing else.
	work := func(ctx *AgentContext) {
		tc, err := validateTaskContract(&TaskContract{TaskMode: TaskModeWork}, ctx.WorkingDir)
		if err != nil {
			t.Fatal(err)
		}
		ctx.TaskContract = tc
	}
	run := integrityLoopWith(t, prompt, input, []string{
		toolCall("read_file", map[string]interface{}{"path": "input.txt"}),
		toolCall("write_file", map[string]interface{}{"path": "solve.py", "content": solve}),
		toolCall("run_command", map[string]interface{}{"command": "python3 solve.py", "timeout": 30}),
		`{"type":"done","summary":"solve.py prints 900."}`,
	}, nil, work)
	if got, _ := run.disk(t, "solve.py"); got != solve {
		t.Fatalf("solve.py on disk differs: %q", got)
	}
	if run.terminal["status"] != "completed" {
		t.Errorf("terminal %s / %s, want completed\n%s", run.terminal["status"], run.terminal["reason"], run.feedback())
	}

	// What the task does owe is still owed: solve.py never run is not done.
	unrun := integrityLoopWith(t, prompt, input, []string{
		toolCall("write_file", map[string]interface{}{"path": "solve.py", "content": solve}),
		`{"type":"done","summary":"solve.py prints 900."}`,
		`{"type":"done","summary":"solve.py prints 900."}`,
		`{"type":"done","summary":"solve.py prints 900."}`,
		`{"type":"done","summary":"solve.py prints 900."}`,
	}, nil, work)
	if unrun.terminal["status"] == "completed" {
		t.Errorf("a solver that was never run completed (%s)", unrun.terminal["reason"])
	}
}
