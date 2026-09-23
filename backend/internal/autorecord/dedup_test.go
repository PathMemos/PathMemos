package autorecord

import (
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"papafeiji/backend/internal/db/sqlc"
)

func TestAutoAddressSet(t *testing.T) {
	rows := []sqlc.ListAutoEntryAddressesByDateRow{
		{Address: pgtype.Text{String: "家", Valid: true}, DetailAddress: pgtype.Text{String: "XX路1号", Valid: true}},
		{Address: pgtype.Text{String: "公司", Valid: true}, DetailAddress: pgtype.Text{String: "", Valid: false}},
		{Address: pgtype.Text{String: "", Valid: false}, DetailAddress: pgtype.Text{String: "XX路1号", Valid: true}},
	}
	set := autoAddressSet(rows)
	if len(set) != 3 {
		t.Fatalf("expected 3 distinct addresses, got %d: %v", len(set), set)
	}
	for _, want := range []string{"家", "公司", "XX路1号"} {
		if _, ok := set[want]; !ok {
			t.Errorf("set missing %q", want)
		}
	}
	if n := len(autoAddressSet(nil)); n != 0 {
		t.Errorf("empty rows should produce empty set, got %d", n)
	}
}

func TestIsDuplicateAutoAddress(t *testing.T) {
	set := map[string]struct{}{"家": {}, "XX路1号": {}}
	cases := []struct {
		name     string
		landmark string
		address  string
		want     bool
	}{
		{"landmark 命中", "家", "", true},
		{"详细地址命中", "", "XX路1号", true},
		{"都不命中", "公司", "YY路2号", false},
		{"空值不误判", "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isDuplicateAutoAddress(set, tc.landmark, tc.address); got != tc.want {
				t.Errorf("isDuplicateAutoAddress(%q, %q) = %v, want %v", tc.landmark, tc.address, got, tc.want)
			}
		})
	}
}

func TestFindDuplicateAutoEntry(t *testing.T) {
	// rows 按 record_time DESC（ListAutoEntryAddressesByDate 保证）：首条命中即最新条目。
	rows := []sqlc.ListAutoEntryAddressesByDateRow{
		{ID: "entry-latest", Address: pgtype.Text{String: "公司", Valid: true}, DetailAddress: pgtype.Text{String: "YY路2号", Valid: true}},
		{ID: "entry-old", Address: pgtype.Text{String: "家", Valid: true}, DetailAddress: pgtype.Text{String: "XX路1号", Valid: true}},
	}
	cases := []struct {
		name     string
		landmark string
		address  string
		wantID   string
		wantDup  bool
	}{
		{"折返旧地点按 landmark 命中（返回旧行 id，契约不变）", "家", "", "entry-old", true},
		{"按 detail_address 命中", "", "XX路1号", "entry-old", true},
		{"最新条目命中", "公司", "", "entry-latest", true},
		{"landmark 命中他条的 detail_address（并集语义）", "XX路1号", "", "entry-old", true},
		{"新地点不重复", "公园", "ZZ路3号", "", false},
		{"空值不误判", "", "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id, dup := FindDuplicateAutoEntry(rows, tc.landmark, tc.address)
			if dup != tc.wantDup || id != tc.wantID {
				t.Errorf("FindDuplicateAutoEntry(%q, %q) = (%q, %v), want (%q, %v)",
					tc.landmark, tc.address, id, dup, tc.wantID, tc.wantDup)
			}
		})
	}
	if id, dup := FindDuplicateAutoEntry(nil, "家", ""); dup || id != "" {
		t.Errorf("empty rows should not dup, got (%q, %v)", id, dup)
	}
}
