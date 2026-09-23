package diary

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"papafeiji/backend/internal/db"
	"papafeiji/backend/internal/db/sqlc"
	"papafeiji/backend/internal/file"
	"papafeiji/backend/internal/location"
	"papafeiji/backend/pkg/timeutil"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/pashagolub/pgxmock/v4"
)

func testRecordDate() time.Time {
	return time.Date(2024, 6, 1, 0, 0, 0, 0, timeutil.Shanghai)
}

func userRowDiary(id string) *pgxmock.Rows {
	now := time.Now()
	return pgxmock.NewRows([]string{
		"id", "open_id", "unionid", "phone_number", "avatar", "avatar_file_id",
		"nickname", "user_type", "phone_bind_time", "auto_record_enabled",
		"personal_family_id", "current_family_id", "invited_by", "lang",
		"created_at", "updated_at", "abnormal_subscribe_accepted", "last_active_at",
	}).AddRow(id, "openid-"+id, nil, nil, nil, nil,
		pgtype.Text{String: "用户", Valid: true}, "wechat", nil, false,
		pgtype.Text{String: "fam-1", Valid: true}, pgtype.Text{String: "fam-1", Valid: true},
		nil, "zh", now, now, false, now)
}

func diaryEntryRow(id string) *pgxmock.Rows {
	return pgxmock.NewRows([]string{
		"id", "diary_id", "created_by", "text", "lat", "lon", "address",
		"detail_address", "record_time", "sort", "color", "created_at", "updated_at",
	}).AddRow(id, "diary-1", "u1", pgtype.Text{String: "旧内容", Valid: true},
		nil, nil, nil, nil, nil, int32(0), nil, time.Now(), time.Now())
}

func diaryRow() *pgxmock.Rows {
	return pgxmock.NewRows([]string{
		"id", "user_id", "record_date", "created_at", "updated_at",
	}).AddRow("diary-1", "u1", pgtype.Date{Time: testRecordDate(), Valid: true}, time.Now(), time.Now())
}

func emptyMemberRows() *pgxmock.Rows {
	return pgxmock.NewRows([]string{"user_id", "role", "joined_at", "avatar", "avatar_file_id", "nickname"})
}

// 构造最小 Service：仅注入 pgxmock pool；rdb/lock/storage/sysCfg 均为 nil
// （mcp.InvalidateUserCache 与 RefreshFamilyDailyCover 已做 nil 保护）。
func newDiaryService(mock pgxmock.PgxPoolIface) *Service {
	return &Service{pool: db.NewPoolWithDBTX(mock)}
}

// TestCreateEntry 新建日记：GetFamilyMembers → tx 内 UpsertDiary + CreateDiaryEntry + TouchDiaryUpdatedAt → 返回 entryID/familyID/recordDate。
func TestCreateEntry(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("new mock pool: %v", err)
	}
	defer mock.Close()

	// GetFamilyMembers
	mock.ExpectQuery("FROM users WHERE id = \\$1").
		WithArgs("u1").
		WillReturnRows(userRowDiary("u1"))
	mock.ExpectQuery("FROM family_members fm").
		WithArgs("fam-1").
		WillReturnRows(emptyMemberRows())

	// 事务内：UpsertDiary（返回 diary-1）→ CreateDiaryEntry → TouchDiaryUpdatedAt
	mock.ExpectBegin()
	mock.ExpectQuery("INSERT INTO diaries").
		WithArgs(pgxmock.AnyArg(), "u1", pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow("diary-1"))
	mock.ExpectQuery("INSERT INTO diary_entries").
		WithArgs(pgxmock.AnyArg(), "diary-1", "u1", pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(diaryEntryRow("entry-new"))
	mock.ExpectExec("UPDATE diaries SET updated_at").
		WithArgs("diary-1").
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectCommit()

	// 封面刷新：s.lock == nil → 记录日志后继续；card 兜底路径（ListInfoCardsByDates 内第二次 GetUserByID 显式失败）
	mock.ExpectQuery("FROM users WHERE id = \\$1").
		WithArgs("u1").
		WillReturnError(errors.New("list cards failed"))
	mock.ExpectQuery("FROM family_daily_covers").
		WithArgs("fam-1", pgxmock.AnyArg()).
		WillReturnError(pgx.ErrNoRows)

	text := "今天很开心"
	recordTime := "2024-06-01T10:00:00+08:00"
	req := &entryRequest{Text: &text, RecordTime: &recordTime}

	s := newDiaryService(mock)
	entryID, familyID, recordDate, _, err := s.CreateEntry(context.Background(), "u1", req)
	if err != nil {
		t.Fatalf("CreateEntry failed: %v", err)
	}
	if entryID == "" {
		t.Fatal("entryID should not be empty")
	}
	if familyID != "fam-1" {
		t.Fatalf("familyID = %q, want fam-1", familyID)
	}
	if recordDate != "2024-06-01" {
		t.Fatalf("recordDate = %q, want 2024-06-01", recordDate)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestUpdateEntry_Success 修改日记：存在 → tx 内 UpdateDiaryEntry 1 行 → 更新成功。
func TestUpdateEntry_Success(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("new mock pool: %v", err)
	}
	defer mock.Close()

	// 前置读取
	mock.ExpectQuery("FROM diary_entries WHERE id = \\$1").
		WithArgs("entry-1").
		WillReturnRows(diaryEntryRow("entry-1"))
	mock.ExpectQuery("FROM diaries WHERE id = \\$1").
		WithArgs("diary-1").
		WillReturnRows(diaryRow())
	// GetFamilyMembers
	mock.ExpectQuery("FROM users WHERE id = \\$1").
		WithArgs("u1").
		WillReturnRows(userRowDiary("u1"))
	mock.ExpectQuery("FROM family_members fm").
		WithArgs("fam-1").
		WillReturnRows(emptyMemberRows())
	// 现有图片为空
	mock.ExpectQuery("FROM diary_entry_images WHERE diary_entry_id = \\$1").
		WithArgs("entry-1").
		WillReturnRows(pgxmock.NewRows([]string{"file_id"}))

	// 事务内：UpdateDiaryEntry 1 行 → DeleteDiaryEntryImages → TouchDiaryUpdatedAt
	mock.ExpectBegin()
	mock.ExpectExec("UPDATE diary_entries SET").
		WithArgs("entry-1", pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectExec("DELETE FROM diary_entry_images").
		WithArgs("entry-1").
		WillReturnResult(pgxmock.NewResult("DELETE", 0))
	mock.ExpectExec("UPDATE diaries SET updated_at").
		WithArgs("diary-1").
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectCommit()

	// 封面刷新（s.lock nil → 日志）与 card 兜底路径
	mock.ExpectQuery("FROM users WHERE id = \\$1").
		WithArgs("u1").
		WillReturnError(errors.New("list cards failed"))
	mock.ExpectQuery("FROM family_daily_covers").
		WithArgs("fam-1", pgxmock.AnyArg()).
		WillReturnError(pgx.ErrNoRows)

	text := "更新后的内容"
	req := &entryRequest{ID: "entry-1", Text: &text}

	s := newDiaryService(mock)
	familyID, recordDate, _, err := s.UpdateEntry(context.Background(), "u1", req)
	if err != nil {
		t.Fatalf("UpdateEntry failed: %v", err)
	}
	if familyID != "fam-1" {
		t.Fatalf("familyID = %q, want fam-1", familyID)
	}
	if recordDate != "2024-06-01" {
		t.Fatalf("recordDate = %q, want 2024-06-01", recordDate)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestUpdateEntry_NotFound 修改不存在的日记 → ErrEntryNotFound。
func TestUpdateEntry_NotFound(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("new mock pool: %v", err)
	}
	defer mock.Close()

	mock.ExpectQuery("FROM diary_entries WHERE id = \\$1").
		WithArgs("missing-entry").
		WillReturnError(pgx.ErrNoRows)

	text := "x"
	req := &entryRequest{ID: "missing-entry", Text: &text}

	s := newDiaryService(mock)
	_, _, _, err = s.UpdateEntry(context.Background(), "u1", req)
	if !errors.Is(err, ErrEntryNotFound) {
		t.Fatalf("want ErrEntryNotFound, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestCoverURLFromRow_DefaultTypeIgnoresFile 验证 M2：cover_type='default' 时不展示任何文件图。
func TestCoverURLFromRow_DefaultTypeIgnoresFile(t *testing.T) {
	s := &Service{} // storage 为 nil：default 类型提前返回，不应触碰 storage
	url, ok, err := s.coverURLFromRow(sqlc.GetFamilyDailyCoverRow{
		CoverType:        "default",
		CoverFileID:      pgtype.Text{String: "file-1", Valid: true},
		CoverPath:        pgtype.Text{String: "uploads/x.png", Valid: true},
		CoverStorageType: pgtype.Text{String: "local", Valid: true},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok || url != "" {
		t.Fatalf("default type must ignore file, got ok=%v url=%q", ok, url)
	}
}

// TestCoverURLFromRow_ImageTypeUsesFile 验证 image 类型正常返回文件 URL。
func TestCoverURLFromRow_ImageTypeUsesFile(t *testing.T) {
	s := &Service{storage: file.NewStorage("").WithBaseURL("https://cdn.example.com")}
	url, ok, err := s.coverURLFromRow(sqlc.GetFamilyDailyCoverRow{
		CoverType:        "image",
		CoverFileID:      pgtype.Text{String: "file-1", Valid: true},
		CoverPath:        pgtype.Text{String: "uploads/x.png", Valid: true},
		CoverStorageType: pgtype.Text{String: "local", Valid: true},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok || url == "" {
		t.Fatalf("image type should use file, got ok=%v url=%q", ok, url)
	}
}

// TestPickGeocodeResult DA-P2-03：地址空但 POI 非空仍可成文；两者都空才失败。
func TestPickGeocodeResult(t *testing.T) {
	cases := []struct {
		name         string
		res          *location.ReverseResult
		wantLandmark string
		wantAddress  string
		wantOK       bool
	}{
		{"poi only", &location.ReverseResult{Landmark: "某小区"}, "某小区", "", true},
		{"address only", &location.ReverseResult{Address: "北京路"}, "北京路", "北京路", true},
		{"both empty", &location.ReverseResult{}, "", "", false},
		{"poi wins", &location.ReverseResult{Landmark: "POI", Address: "北京路"}, "POI", "北京路", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lm, addr, ok := pickGeocodeResult(tc.res)
			if lm != tc.wantLandmark || addr != tc.wantAddress || ok != tc.wantOK {
				t.Fatalf("got (landmark=%q address=%q ok=%v), want (%q %q %v)", lm, addr, ok, tc.wantLandmark, tc.wantAddress, tc.wantOK)
			}
		})
	}
}

// fileRow 构造 files 行（GetFileByID 为 SELECT *，11 列）。
func fileRow(fileType string) *pgxmock.Rows {
	return pgxmock.NewRows([]string{
		"id", "created_by", "path", "name", "suffix", "size_bytes",
		"file_type", "metadata", "created_at", "updated_at", "storage_type",
	}).AddRow("f1", pgtype.Text{String: "u1", Valid: true}, "p/a.jpg", "a", "jpg",
		int64(123), fileType, nil, time.Now(), time.Now(), "local")
}

func fileRefRow() *pgxmock.Rows {
	return pgxmock.NewRows([]string{"used_by_cover", "used_by_entry", "avatar_user_count"}).
		AddRow(false, false, int32(0))
}

// TestDeleteIfOnlySelfReferenced R-29：编辑/删除条目路径的物理删除与配额回退同一事务——
// image 文件删除必须调 DecrementUserImageStorage 且失败整体回滚；system 文件不扣配额。
func TestDeleteIfOnlySelfReferenced(t *testing.T) {
	setup := func(t *testing.T) (pgxmock.PgxPoolIface, *Service, string) {
		t.Helper()
		mock, err := pgxmock.NewPool()
		if err != nil {
			t.Fatalf("new mock pool: %v", err)
		}
		t.Cleanup(mock.Close)
		tmp := t.TempDir()
		full := filepath.Join(tmp, "p", "a.jpg")
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(full, []byte("x"), 0o644); err != nil {
			t.Fatalf("write file: %v", err)
		}
		svc := &Service{pool: db.NewPoolWithDBTX(mock), storage: file.NewStorage(tmp)}
		return mock, svc, full
	}

	t.Run("image decrements quota in same tx", func(t *testing.T) {
		mock, svc, full := setup(t)
		mock.ExpectBegin()
		mock.ExpectQuery("EXISTS").WithArgs(pgtype.Text{String: "f1", Valid: true}).WillReturnRows(fileRefRow())
		mock.ExpectQuery("FROM files WHERE id").WithArgs("f1").WillReturnRows(fileRow("image"))
		mock.ExpectExec("DELETE FROM files").WithArgs("f1").WillReturnResult(pgxmock.NewResult("DELETE", 1))
		mock.ExpectExec("image_storage_bytes").
			WithArgs("u1", int64(123)).
			WillReturnResult(pgxmock.NewResult("UPDATE", 1))
		mock.ExpectCommit()

		if err := svc.deleteIfOnlySelfReferenced(context.Background(), "f1", "entry-1"); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if _, err := os.Stat(full); !os.IsNotExist(err) {
			t.Fatalf("physical file should be removed")
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("expectations: %v", err)
		}
	})

	t.Run("decrement failure rolls back", func(t *testing.T) {
		mock, svc, full := setup(t)
		mock.ExpectBegin()
		mock.ExpectQuery("EXISTS").WithArgs(pgtype.Text{String: "f1", Valid: true}).WillReturnRows(fileRefRow())
		mock.ExpectQuery("FROM files WHERE id").WithArgs("f1").WillReturnRows(fileRow("image"))
		mock.ExpectExec("DELETE FROM files").WithArgs("f1").WillReturnResult(pgxmock.NewResult("DELETE", 1))
		mock.ExpectExec("image_storage_bytes").WithArgs("u1", int64(123)).WillReturnError(errors.New("dec fail"))
		mock.ExpectRollback()

		if err := svc.deleteIfOnlySelfReferenced(context.Background(), "f1", "entry-1"); err == nil {
			t.Fatalf("expected error on decrement failure")
		}
		if _, err := os.Stat(full); err != nil {
			t.Fatalf("physical file should remain after rollback")
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("expectations: %v", err)
		}
	})

	t.Run("system file skips decrement", func(t *testing.T) {
		mock, svc, _ := setup(t)
		mock.ExpectBegin()
		mock.ExpectQuery("EXISTS").WithArgs(pgtype.Text{String: "f1", Valid: true}).WillReturnRows(fileRefRow())
		mock.ExpectQuery("FROM files WHERE id").WithArgs("f1").WillReturnRows(fileRow("system"))
		mock.ExpectExec("DELETE FROM files").WithArgs("f1").WillReturnResult(pgxmock.NewResult("DELETE", 1))
		mock.ExpectCommit()

		if err := svc.deleteIfOnlySelfReferenced(context.Background(), "f1", "entry-1"); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("expectations: %v", err)
		}
	})
}
