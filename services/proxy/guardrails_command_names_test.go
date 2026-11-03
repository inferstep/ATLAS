package main

import (
	"encoding/json"
	"testing"
)

// Three common ways to run a program named its files without a plain token,
// so a green run of them bound no coverage, and a work request verified that
// way ended "verification demanded, unmet": a subshell, a glob, and a Java
// class run.

func TestCommandNamesPathSeesSubshellsGlobsAndJavaClasses(t *testing.T) {
	for _, c := range []struct {
		cmd, path string
		want      bool
	}{
		{"(cd app && python main.py)", "app/main.py", true},
		{"(cd app && python3 main.py)", "main.py", true},
		{"cd app && python main.py", "app/main.py", true},
		{"javac *.java", "Main.java", true},
		{"javac src/*.java", "src/Util.java", true},
		{"javac src/*.java", "/work/src/Util.java", true},
		{"javac /work/src/*.java", "/work/src/Util.java", true},
		{"javac *.java", "/work/Main.java", true},
		{"javac *.java && java Main", "Main.java", true},
		{"java -cp . Main", "Main.java", true},
		{"java com.example.Main", "com/example/Main.java", true},
		{"timeout 5 java Main", "Main.java", true},
		{"`python3 main.py`", "main.py", true},
		// Still not named:
		{"(cd app && python main.py.bak)", "app/main.py", false},
		{"python *.py", "notes.md", false},
		{"javac src/*.java", "Util.java", false},
		{"javac src/*.java", "/work/lib/Util.java", false},
		{"javac /work/src/*.java", "/work/lib/Util.java", false},
		{"java -version", "Main.java", false},
		{"java -cp . Other", "Main.java", false},
		{"echo Main", "Main.java", false},
		{"java Main", "Main.kt", false},
	} {
		if got := commandNamesPath(c.cmd, c.path); got != c.want {
			t.Errorf("commandNamesPath(%q, %q) = %v, want %v", c.cmd, c.path, got, c.want)
		}
	}
}

// End to end through the coverage builder: the subshell and the Java run each
// cover the file they ran.
func TestAGreenSubshellOrJavaRunCoversItsFiles(t *testing.T) {
	dir := t.TempDir()
	ctx := NewAgentContext(dir, Tier2Medium)
	ctx.PermissionMode = PermissionYolo
	write := func(rel, body string) {
		args, _ := json.Marshal(map[string]string{"path": rel, "content": body})
		if res := executeToolCall("write_file", args, ctx); res == nil || !res.Success {
			t.Fatalf("setup write of %s failed: %+v", rel, res)
		}
	}
	write("app/main.py", "print('ok')\n")
	write("Main.java", "public class Main {\n    public static void main(String[] a) {\n        System.out.println(\"ok\");\n    }\n}\n")
	write("src/Util.java", "public class Util {}\n")

	// The changed paths include the resolved absolute form, and the lookup
	// below uses it, so the glob case checks that a relative glob with a
	// directory matches the path's trailing components.
	for cmd, rel := range map[string]string{
		"(cd app && python3 main.py)": "app/main.py",
		"javac *.java && java Main":   "Main.java",
		"javac src/*.java":            "src/Util.java",
	} {
		covered := coverageForGreenCommand(ctx, cmd)
		if _, ok := covered[resolveAgentPath(ctx, rel)]; !ok {
			t.Errorf("%q did not cover %s; covered=%v", cmd, rel, covered)
		}
	}
}
