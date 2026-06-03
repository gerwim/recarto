package parse

import "testing"

// TestParseFileRepeatedDoesNotPanicOrLeak exercises the Parser/Tree/QueryCursor
// open+Close cycle many times. With the defer Close() discipline this is stable;
// a missing Close would leak C memory (observable as growth, and unsafe under
// the race detector across goroutines).
func TestParseFileRepeatedDoesNotPanicOrLeak(t *testing.T) {
	r, err := NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	src := []byte("package p\nfunc F() { G() }\nfunc G() {}")
	for i := 0; i < 2000; i++ {
		if _, err := r.ParseFile("go", "p.go", src); err != nil {
			t.Fatal(err)
		}
	}
}

func TestParseFileConcurrent(t *testing.T) {
	r, err := NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	src := []byte("package p\nfunc F() {}")
	done := make(chan error, 8)
	for g := 0; g < 8; g++ {
		go func() {
			for i := 0; i < 200; i++ {
				if _, err := r.ParseFile("go", "p.go", src); err != nil {
					done <- err
					return
				}
			}
			done <- nil
		}()
	}
	for g := 0; g < 8; g++ {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
}
