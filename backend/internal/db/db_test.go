package db

import "testing"

func TestIntEnv(t *testing.T) {
	const key = "DB_TEST_INT_ENV"

	t.Setenv(key, "")
	if got := intEnv(key, 10, 1); got != 10 {
		t.Fatalf("missing env: got %d, want default 10", got)
	}
	t.Setenv(key, "25")
	if got := intEnv(key, 10, 1); got != 25 {
		t.Fatalf("valid env: got %d, want 25", got)
	}
	t.Setenv(key, "0")
	if got := intEnv(key, 10, 5); got != 10 {
		t.Fatalf("below min: got %d, want default 10", got)
	}
	t.Setenv(key, "abc")
	if got := intEnv(key, 10, 1); got != 10 {
		t.Fatalf("non-numeric: got %d, want default 10", got)
	}
}
