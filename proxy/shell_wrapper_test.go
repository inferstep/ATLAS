package main

import (
	"strings"
	"testing"
)

func TestValidateShellCommandRefusesUninspectableWrappers(t *testing.T) {
	for _, cmd := range []string{
		`bash -c 'unterminated`,
		`/bin/sh -c "unterminated`,
		`env ATLAS_FLAG=x b\ash -lc 'unterminated`,
		`sudo -u root bash -c 'unterminated`,
		`doas -u root eval 'unterminated`,
		`env -u HOME eval "unterminated`,
		`env -S 'unterminated`,
		`env '-ivSbash -c "unterminated`,
		`env '--split-str=bash -c "unterminated`,
		`env -S "bash -c 'unterminated"`,
		`env '--split-string=bash -c "unterminated'`,
		`env -S '-S "unterminated'`,
		`env -S 'env -S "unterminated'`,
		`eval "'unterminated"`,
		`bash -c "echo 'unterminated"`,
		`bash -c echo\`,
		`eval echo\`,
		`env -S \`,
		`bash '-c`,
		`eval 'unterminated`,
		`bash -c 'echo safe' 'unterminated`,
		`bash -o 'unterminated`,
		`bash --rcfile $'a\'b' -c 'rm -rf /'`,
		`bash --rcfile $'a\'b' -c 'rm -rf /' 'x y' z`,
		`env -u $'a\'b' -S 'bash -c "rm -rf /"'`,
		`sudo -u $'a\'b' bash -c 'rm -rf /'`,
		`bash -c $'rm -rf /'`,
		`eval $'rm -rf /'`,
		`$'bash' -c 'rm -rf /'`,
		`$"bash" -c 'rm -rf /'`,
		`env ATLAS_FLAG=x 'unterminated`,
		`env ATLAS_FLAG='unterminated`,
		"eval " + quoteWrapperTestCommand(`sudo -u root bash -c 'unterminated`),
	} {
		t.Run(cmd, func(t *testing.T) {
			const want = "refused: the command's quoting could not be checked; simplify it."
			if got := validateShellCommand(cmd); got != want {
				t.Errorf("validateShellCommand(%q) = %q, want %q", cmd, got, want)
			}
		})
	}
}

func TestValidateShellCommandRefusesIncompleteWrappers(t *testing.T) {
	for _, cmd := range []string{
		`bash -c`,
		`bash -lc`,
		`bash -c -o`,
		`env -S`,
		`env --split-string`,
		`env --s`,
		`env -iS`,
		`env -S -S`,
		`env -S 'bash -c'`,
		`eval 'env -S'`,
		`sudo -u root env -S`,
		`bash -c 'env --split-string'`,
	} {
		t.Run(cmd, func(t *testing.T) {
			const want = "refused: the execution wrapper is incomplete; simplify it."
			if got := validateShellCommand(cmd); got != want {
				t.Errorf("validateShellCommand(%q) = %q, want %q", cmd, got, want)
			}
		})
	}
}

func TestValidateShellCommandKeepsNonExecutingWrapperTextAllowed(t *testing.T) {
	for _, cmd := range []string{
		`eval`,
		`eval --`,
		`eval ''`,
		`bash -c ''`,
		`env -S ''`,
		`bash -o`,
		`bash script.sh 'unterminated`,
		`bash script.sh $'ordinary\'argument'`,
		`echo $'bash -c rm -rf /'`,
		`sudo -u root echo $'eval rm -rf /'`,
		`python3 -c "open('stats.py', 'a').write('# checked by hand\n')"`,
		`echo bash -c 'unterminated`,
		`echo env -S 'unterminated`,
		`python app.py --description 'eval`,
		`env LABEL=eval python app.py --label 'env -S`,
		`sudo -u env echo -S 'unterminated`,
		`bash -- -c 'unterminated`,
	} {
		t.Run(cmd, func(t *testing.T) {
			if got := validateShellCommand(cmd); got != "" {
				t.Errorf("validateShellCommand(%q) rejected: %s", cmd, got)
			}
		})
	}
}

func TestValidateShellCommandLimitsWrapperDepth(t *testing.T) {
	for name, wrap := range map[string]func(int) string{
		"eval": func(depth int) string {
			return strings.Repeat("eval ", depth) + "python app.py"
		},
		"env commands": func(depth int) string {
			return strings.Repeat("env -S ", depth) + "python app.py"
		},
		"env options": func(depth int) string {
			return "env -S " + quoteWrapperTestCommand(strings.Repeat("-S ", depth-1)+"python") + " app.py"
		},
		"mixed": func(depth int) string {
			return strings.Repeat("eval ", depth-2) + `env -S bash -c 'python app.py'`
		},
	} {
		t.Run(name, func(t *testing.T) {
			if got := validateShellCommand(wrap(16)); got != "" {
				t.Errorf("16 execution layers rejected: %s", got)
			}
			const want = "refused: the command exceeds 16 execution wrapper layers; simplify it."
			if got := validateShellCommand(wrap(17)); got != want {
				t.Errorf("17 execution layers = %q, want %q", got, want)
			}
		})
	}
	if got := validateShellCommand(strings.Repeat("eval ", 16) + "rm -rf /"); got == "" {
		t.Error("catastrophic command at the depth limit was allowed")
	}
}

func TestValidateShellCommandTracksDepthPerExecutionPath(t *testing.T) {
	ordinary := strings.Repeat("eval ", 16) + "python app.py"
	if got := validateShellCommand(ordinary + "; " + ordinary); got != "" {
		t.Errorf("independent command segments rejected: %s", got)
	}
	inner := quoteWrapperTestCommand("eval python app.py; python app.py")
	cmd := strings.Repeat("eval ", 15) + "bash -c " + inner
	const want = "refused: the command exceeds 16 execution wrapper layers; simplify it."
	if got := validateShellCommand(cmd); got != want {
		t.Errorf("nested segments lost their inherited depth: %q, want %q", got, want)
	}
}

func quoteWrapperTestCommand(cmd string) string {
	return "'" + strings.ReplaceAll(cmd, "'", "'\\''") + "'"
}
