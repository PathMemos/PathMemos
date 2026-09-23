package diary

import (
	"testing"
	"time"
)

// 空串 color 必须与「未设置」一致映射为 NULL，避免写入 color=” 触发 DB CHECK。
func TestBuildCreateEntryParamsColor(t *testing.T) {
	now := time.Now()

	valid := "#aabbcc"
	params, err := buildCreateEntryParams("e1", "d1", "u1", &entryRequest{Color: &valid}, now)
	if err != nil {
		t.Fatalf("valid color: unexpected error: %v", err)
	}
	if !params.Color.Valid || params.Color.String != valid {
		t.Fatalf("valid color: got valid=%v value=%q", params.Color.Valid, params.Color.String)
	}

	empty := ""
	params, err = buildCreateEntryParams("e1", "d1", "u1", &entryRequest{Color: &empty}, now)
	if err != nil {
		t.Fatalf("empty color: unexpected error: %v", err)
	}
	if params.Color.Valid {
		t.Fatalf("empty color should map to NULL, got %q", params.Color.String)
	}

	params, err = buildCreateEntryParams("e1", "d1", "u1", &entryRequest{}, now)
	if err != nil {
		t.Fatalf("nil color: unexpected error: %v", err)
	}
	if params.Color.Valid {
		t.Fatalf("nil color should map to NULL, got %q", params.Color.String)
	}
}
