package shellcmd

import (
	"strings"
)

// makeTargetKind classifies `make TARGET` by the target's name. Lint and
// format targets check; test, check and run targets execute; setup targets
// prove nothing. Any other target, and a bare `make`, builds.
func makeTargetKind(args []string) CommandEvidenceKind {
	for i := 0; i < len(args); i++ {
		if nextMakeArgument(args, i) {
			// An option and its value: -j and -l take one only when the next word is a number.
			i++
			continue
		}
		if makeOption(args[i]) {
			continue
		}
		return makeTargetWord(strings.ToLower(args[i]))
	}
	return EvidenceStatic // the default target, which conventionally builds
}

// nextMakeArgument reports whether the word at i is an option whose value is
// the word after it, so that neither of them names a target.
func nextMakeArgument(args []string, i int) bool {
	switch a := args[i]; {
	case a == "-C" || a == "-f" || a == "--file" || a == "--directory" || a == "-I" || a == "-o" || a == "-W":
		return true
	case a == "-j" || a == "-l":
		return i+1 < len(args) && strings.Trim(args[i+1], "0123456789") == ""
	}
	return false
}

// makeOption reports whether a word names no target: it is a flag, or an
// assignment to the make environment.
func makeOption(word string) bool {
	return strings.HasPrefix(word, "-") || strings.Contains(word, "=")
}

// makeTargetWord classifies one target word, which the caller has lowercased.
// The three rules are asked in this order, so a word that matches more than
// one of them is read as the first.
func makeTargetWord(word string) CommandEvidenceKind {
	switch {
	case makeCheckTarget(word):
		return EvidenceStatic
	case makeRunsTarget(word):
		return EvidenceExecution
	case makeSetupTarget(word):
		return EvidenceNone
	}
	return EvidenceStatic
}

// makeCheckTarget reports whether a target word checks the code rather than
// running it.
func makeCheckTarget(word string) bool {
	return strings.Contains(word, "lint") || strings.Contains(word, "fmt") || strings.Contains(word, "format") ||
		strings.Contains(word, "typecheck") || strings.Contains(word, "type-check") ||
		word == "vet" || word == "tidy"
}

// makeRunsTarget reports whether a target word runs the code.
func makeRunsTarget(word string) bool {
	return strings.Contains(word, "test") || strings.Contains(word, "check") || strings.Contains(word, "spec") ||
		strings.Contains(word, "e2e") || strings.Contains(word, "smoke") || strings.Contains(word, "verify") ||
		word == "run" || word == "start" || word == "serve" || word == "demo" || word == "dev" ||
		strings.HasPrefix(word, "run-") || strings.HasPrefix(word, "run_")
}

// makeSetupTarget reports whether a target word prepares the tree and so
// proves nothing about the code.
func makeSetupTarget(word string) bool {
	return word == "install" || word == "uninstall" || word == "clean" || word == "distclean" ||
		word == "deps" || word == "setup" || word == "bootstrap" || word == "venv" || word == "init"
}
