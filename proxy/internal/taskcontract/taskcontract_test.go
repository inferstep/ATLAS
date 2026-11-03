package taskcontract

import (
	"testing"

	"github.com/itigges22/atlas-proxy/internal/wire"
)

func TestAnUnsatisfiableObligationKindHasNoFloor(t *testing.T) {
	for _, kind := range obligationKinds {
		if !obligationUnsatisfiableKinds[kind] {
			continue
		}
		if _, ok := obligationRequiredStrength(kind, ""); ok {
			t.Errorf("%s is unsatisfiable and must have no strength", kind)
		}
	}
}

func TestADynamicObligationKindTakesItsFloorFromTheBaseline(t *testing.T) {
	for _, kind := range obligationKinds {
		if obligationUnsatisfiableKinds[kind] || !obligationDynamicStrengthKinds[kind] {
			continue
		}
		if _, ok := obligationRequiredStrength(kind, ""); ok {
			t.Errorf("%s must refuse to invent a baseline strength", kind)
		}
		s, ok := obligationRequiredStrength(kind, "behavioral")
		if !ok || s != "behavioral" {
			t.Errorf("%s took %q/%v from a behavioral baseline", kind, s, ok)
		}
	}
}

func TestEveryOtherObligationKindHasAFixedFloor(t *testing.T) {
	for _, kind := range obligationKinds {
		if obligationUnsatisfiableKinds[kind] || obligationDynamicStrengthKinds[kind] {
			continue
		}
		s, ok := obligationRequiredStrength(kind, "")
		if !ok || wire.StrengthRank(s) < 0 {
			t.Errorf("%s has no fixed strength: %q/%v", kind, s, ok)
		}
		// A fixed floor cannot be raised by handing it a baseline.
		if _, ok := obligationRequiredStrength(kind, "oracle"); ok {
			t.Errorf("%s let a baseline raise its fixed floor", kind)
		}
	}
}

func TestADeclaredCommandIsBehavioralAndNotAnOracle(t *testing.T) {
	// Exit zero says the command the client asked for ran and succeeded
	// against these bytes. It does not say an answer was checked against a
	// reference, so no declaration reaches oracle -- and the kind carries no
	// fixed floor at all, because how strongly a command counts is a statement
	// only the client can make about that command.
	if _, fixed := obligationKindRequiredStrength[ObligationDeclaredCommand]; fixed {
		t.Error("the declared-command kind carries a fixed floor")
	}
	if !obligationDynamicStrengthKinds[ObligationDeclaredCommand] {
		t.Error("the declared-command floor is not taken from the obligation")
	}
	for _, kind := range []string{VerificationKindSyntax, VerificationKindRuntime,
		VerificationKindBehavioral} {
		got, ok := obligationRequiredStrength(ObligationDeclaredCommand, kind)
		if !ok || got != kind {
			t.Errorf("declared %q resolved to %q (ok=%v)", kind, got, ok)
		}
	}
	if _, ok := obligationRequiredStrength(ObligationDeclaredCommand, "oracle"); ok {
		t.Error("a declared command reached oracle")
	}
	if got := obligationKindRequiredStrength[ObligationDeclaredExample]; got != "oracle" {
		t.Errorf("declared example floor is %q, want oracle", got)
	}
}

func TestEveryKindHasExactlyOneRole(t *testing.T) {
	for _, kind := range obligationKinds {
		role, ok := ObligationRole(kind)
		if !ok {
			t.Errorf("%s has no role; nothing knows when to ask it", kind)
			continue
		}
		switch role {
		case ObligationRoleTargetIdentity,
			ObligationRoleAuthorizationPrerequisite,
			ObligationRolePostDeliverySettlement:
		default:
			t.Errorf("%s carries the unknown role %q", kind, role)
		}
	}
	if _, ok := ObligationRole("something_new"); ok {
		t.Error("an unknown kind was given a role")
	}
}
