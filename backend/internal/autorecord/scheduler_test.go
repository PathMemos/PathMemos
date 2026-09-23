package autorecord

import "testing"

// R-01：候选游标推进与回绕（纯逻辑，避免 DB 依赖）。
func TestNextCursor(t *testing.T) {
	cases := []struct {
		name     string
		batch    []string
		prev     string
		wantCur  string
		wantWrap bool
	}{
		{"full batch advances to last id", []string{"a", "b", "c"}, "x", "c", false},
		{"empty batch with prev wraps", nil, "z", "", true},
		{"empty batch without prev stays", nil, "", "", false},
	}
	for _, tc := range cases {
		cur, wrap := nextCursor(tc.batch, tc.prev)
		if cur != tc.wantCur || wrap != tc.wantWrap {
			t.Fatalf("%s: got (%q,%v), want (%q,%v)", tc.name, cur, wrap, tc.wantCur, tc.wantWrap)
		}
	}
}
