package main

import "testing"

// Inline code that touches no file the session changed ran nothing the work is
// made of. `python -c "print(1)"` must not pass the fix-intent gate or clear
// an earlier red run of the real program.

func TestInlineCodeThatTouchesNothingDoesNotClearAFailure(t *testing.T) {
	ctx, dir := sepCtx(t, sepContract([]string{"fix.py"}, nil))
	sepWrite(t, ctx, dir, "fix.py", "def f():\n    return 1 / 0\n\nprint(f())\n")
	st := &runState{userWantsVerification: true}

	st.observeVerification(ctx, "", 1, "python3 fix.py", ranRed("ZeroDivisionError"))
	if !st.sawFailedVerification {
		t.Fatal("the red run of fix.py did not latch")
	}
	for _, cmd := range []string{
		`python -c "print(1)"`,
		`node -e "console.log(1)"`,
		`cd /tmp && python3 -c "print(1)"`,
	} {
		st.observeVerification(ctx, "", 2, cmd, ranClean("1\n"))
		if st.verifiedThisLoop || !st.sawFailedVerification {
			t.Fatalf("%s verified the run or cleared the failure: verified=%v latch=%v",
				cmd, st.verifiedThisLoop, st.sawFailedVerification)
		}
		if st.uncountedCheck == "" {
			t.Errorf("%s was not named back as uncounted", cmd)
		}
	}
}

func TestInlineCodeThatUsesTheChangedFileStillCounts(t *testing.T) {
	for _, cmd := range []string{
		`python3 -c "import fix; print(fix.f())"`,
		`python3 -c "x = 1; import fix; print(fix.f())"`,
		"python3 -c \"x = 1\nimport fix\nprint(fix.f())\"",
		`python3 -c "exec(open('fix.py').read())"`,
		`python3 -c "import sys; exec(open(sys.argv[1]).read())" fix.py`,
	} {
		ctx, dir := sepCtx(t, sepContract([]string{"fix.py"}, nil))
		sepWrite(t, ctx, dir, "fix.py", "def f():\n    return 1\n")
		st := &runState{userWantsVerification: true}
		st.observeVerification(ctx, "", 1, cmd, ranClean("1\n"))
		if !st.verifiedThisLoop {
			t.Errorf("%s uses the changed file and did not verify it", cmd)
		}
	}
}

func TestInlineCode(t *testing.T) {
	for _, c := range []struct {
		seg, code string
		ok        bool
	}{
		{`python3 -c "import stats; print(1)"`, "import stats; print(1)", true},
		{`timeout 5 node -e "console.log(1)"`, "console.log(1)", true},
		{"python3 -c \"import a\nprint(a.x)\"", "import a\nprint(a.x)", true},
		{`node --eval 'require("./lib")'`, `require("./lib")`, true},
		{`python3 fix.py`, "", false},
		{`python3 -m pytest -q`, "", false},
		{`node app.js -e`, "", false},
		{`python3 -m pytest -c setup.cfg`, "", false},
		{`python3 -X dev -c "print(1)"`, "print(1)", true},
	} {
		code, ok := inlineCode(c.seg)
		if ok != c.ok || code != c.code {
			t.Errorf("inlineCode(%q) = %q, %v; want %q, %v", c.seg, code, ok, c.code, c.ok)
		}
	}
}
