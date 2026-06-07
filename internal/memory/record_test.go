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
	if len(AllKinds) != len(validKinds) {
		t.Fatalf("AllKinds has %d, validKinds has %d", len(AllKinds), len(validKinds))
	}
	for _, k := range AllKinds {
		if !k.Valid() {
			t.Errorf("AllKinds entry %q not in validKinds", k)
		}
	}
	if AllKinds[len(AllKinds)-1] != KindTodo {
		t.Errorf("expected todo last, got %q", AllKinds[len(AllKinds)-1])
	}
}
