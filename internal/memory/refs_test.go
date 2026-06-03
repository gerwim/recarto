package memory

import "testing"

func TestRelFileRef(t *testing.T) {
	cases := []struct{ worktree, path, want string }{
		{"/repo", "/repo/internal/foo.go", "internal/foo.go"}, // abs under worktree → relative
		{"/repo", "internal/foo.go", "internal/foo.go"},       // already relative → unchanged
		{"/repo", "/other/x.go", "/other/x.go"},               // abs outside worktree → unchanged
		{"", "internal/foo.go", "internal/foo.go"},            // no worktree → unchanged
	}
	for _, c := range cases {
		if got := RelFileRef(c.worktree, c.path); got != c.want {
			t.Errorf("RelFileRef(%q,%q)=%q, want %q", c.worktree, c.path, got, c.want)
		}
	}
}
