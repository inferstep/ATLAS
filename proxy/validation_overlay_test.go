package main

import (
	"errors"
	"testing"
)

// The new-file route obtains a stronger validation observation than
// writeFileDirect's conservative default, so it overlays that observation
// afterwards. These helpers replace ONLY the validation fields; mutation
// facts and the error contract are untouched.

func TestOverlayValidationOnSuccess(t *testing.T) {
	for _, c := range []struct {
		name string
		in   checkOutcome
		want ValidationStatus
		kind ValidationKind
	}{
		{"passed", checkOutcome{Status: ValidationPassed}, ValidationPassed, ValidationKindSyntax},
		{"not_run", checkOutcome{Status: ValidationNotRun, Detail: "sandbox unreachable"},
			ValidationNotRun, ValidationKindSyntax},
		{"not_applicable", checkOutcome{Status: ValidationNotApplicable},
			ValidationNotApplicable, ValidationKindNone},
		{"failed", checkOutcome{Status: ValidationFailed, Detail: "SyntaxError"},
			ValidationFailed, ValidationKindSyntax},
	} {
		t.Run(c.name, func(t *testing.T) {
			res := &ToolResult{Success: true, MutationStatus: MutationApplied,
				ValidationKind: ValidationKindSyntax, ValidationStatus: ValidationNotRun}
			overlayValidation(res, c.in)
			if res.ValidationStatus != c.want {
				t.Errorf("ValidationStatus = %q, want %q", res.ValidationStatus, c.want)
			}
			if res.ValidationKind != c.kind {
				t.Errorf("ValidationKind = %q, want %q", res.ValidationKind, c.kind)
			}
			if res.ValidationDetail != c.in.Detail {
				t.Errorf("detail = %q, want %q", res.ValidationDetail, c.in.Detail)
			}
			// Mutation facts and Success are never touched by the overlay.
			if res.MutationStatus != MutationApplied || !res.Success {
				t.Error("overlay altered mutation or Success")
			}
		})
	}
}

// Unknown must fail closed: it stays Unknown so a sentinel can catch the
// producer defect, and never becomes passed or not_run.
func TestOverlayUnknownFailsClosed(t *testing.T) {
	res := &ToolResult{Success: true, MutationStatus: MutationApplied}
	overlayValidation(res, checkOutcome{Status: ValidationUnknown})
	if res.ValidationStatus != ValidationUnknown {
		t.Fatalf("ValidationStatus = %q, want unknown", res.ValidationStatus)
	}
	if res.ValidationStatus.Passed() {
		t.Fatal("Unknown must never read as passed")
	}
	if res.Classified() {
		t.Fatal("an Unknown validation must leave the result unclassified")
	}
}

// Mutation failure and validation success are orthogonal: bytes that were
// checked and passed can still fail to land.
func TestOverlayValidationOnClassifiedError(t *testing.T) {
	base := failedMutation("mod.py", errors.New("cannot rename temp file: boom"))
	out := overlayValidationOnError(base, checkOutcome{Status: ValidationPassed})

	if out == nil {
		t.Fatal("the non-nil error contract was broken")
	}
	if out.Error() != base.Error() {
		t.Errorf("wrapped error text changed: %q", out.Error())
	}
	var ce *classifiedError
	if !errors.As(out, &ce) {
		t.Fatal("classification carrier lost")
	}
	if ce.mutationStatus != MutationFailed {
		t.Errorf("mutationStatus = %q, want failed", ce.mutationStatus)
	}
	if ce.validationStatus != ValidationPassed || ce.validationKind != ValidationKindSyntax {
		t.Errorf("validation = %q/%q, want syntax/passed",
			ce.validationKind, ce.validationStatus)
	}
}

// An untyped error is returned unchanged: the overlay must not manufacture a
// classification where no producer made one.
func TestOverlayLeavesUntypedErrorsAlone(t *testing.T) {
	plain := errors.New("some unrelated failure")
	out := overlayValidationOnError(plain, checkOutcome{Status: ValidationPassed})
	var ce *classifiedError
	if errors.As(out, &ce) {
		t.Fatal("an untyped error must not gain a classification carrier")
	}
	if out != plain {
		t.Error("untyped error was not returned unchanged")
	}
}
