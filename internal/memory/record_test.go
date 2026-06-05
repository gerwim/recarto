package memory

import "testing"

func TestKindValid(t *testing.T) {
	for _, k := range []Kind{KindPreference, KindLearning, KindDecision,
		KindDoNotRepeat, KindBug, KindAction} {
		if !k.Valid() {
			t.Errorf("%q should be valid", k)
		}
	}
	if Kind("nonsense").Valid() {
		t.Error(`"nonsense" should be invalid`)
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
