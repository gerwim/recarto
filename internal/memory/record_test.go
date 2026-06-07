package memory

import "testing"

func TestKindValid(t *testing.T) {
	for _, k := range []Kind{KindPreference, KindLearning, KindDecision,
		KindDoNotRepeat, KindBug, KindTodo} {
		if !k.Valid() {
			t.Errorf("%q should be valid", k)
		}
	}
	if Kind("nonsense").Valid() {
		t.Error(`"nonsense" should be invalid`)
	}
}

func TestActionKindRemoved(t *testing.T) {
	// The `action` activity-log kind was never instructed and is unused in practice;
	// it has been removed so it can't be recorded by mistake. (The DB CHECK still
	// permits it as a harmless superset; enforcement is at the validation layer.)
	if Kind("action").Valid() {
		t.Error(`"action" kind should be removed (no longer a valid kind)`)
	}
	for _, k := range AllKinds {
		if k == "action" {
			t.Error("AllKinds must not contain the removed action kind")
		}
	}
}

func TestKindTodoIsValid(t *testing.T) {
	if !KindTodo.Valid() {
		t.Fatal("KindTodo should be a valid kind")
	}
	if KindTodo != "todo" {
		t.Fatalf("KindTodo = %q, want \"todo\"", KindTodo)
	}
}

func TestAllKindsCoversValidKinds(t *testing.T) {
	// AllKinds is the user-facing enum (record_memory MCP tool); validKinds is a
	// superset that also includes internal kinds (e.g. KindFeedback) that must be
	// accepted by Engine.Record but must NOT appear in the MCP enum.
	if len(validKinds) < len(AllKinds) {
		t.Fatalf("validKinds has %d, AllKinds has %d — validKinds must be a superset", len(validKinds), len(AllKinds))
	}
	for _, k := range AllKinds {
		if !k.Valid() {
			t.Errorf("AllKinds entry %q not in validKinds", k)
		}
	}
	if AllKinds[len(AllKinds)-1] != KindTodo {
		t.Errorf("expected todo last, got %q", AllKinds[len(AllKinds)-1])
	}
	// Internal kinds must be valid but absent from the user-facing AllKinds list.
	for _, k := range []Kind{KindFeedback} {
		if !k.Valid() {
			t.Errorf("internal kind %q should be valid", k)
		}
		for _, pub := range AllKinds {
			if pub == k {
				t.Errorf("internal kind %q must not appear in AllKinds (MCP enum)", k)
			}
		}
	}
}
