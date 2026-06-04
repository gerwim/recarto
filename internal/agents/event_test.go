package agents

import "testing"

func TestKindFromString(t *testing.T) {
	cases := map[string]EventKind{
		"session-start": KindSessionStart,
		"post-read":     KindPostRead,
		"post-edit":     KindPostEdit,
		"augment":       KindAugment,
		"stop":          KindStop,
	}
	for s, want := range cases {
		got, err := KindFromString(s)
		if err != nil || got != want {
			t.Errorf("KindFromString(%q) = %v, %v", s, got, err)
		}
	}
	if _, err := KindFromString("nope"); err == nil {
		t.Error("expected error for unknown event")
	}
}
