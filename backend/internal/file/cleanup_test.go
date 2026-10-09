package file

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"papafeiji/backend/internal/db"
	"papafeiji/backend/internal/db/sqlc"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/pashagolub/pgxmock/v4"
)

// TestRefsAllowPhysicalDelete 表驱动覆盖删除判定谓词的全部真值组合：
// 零引用 / 仅他人 entry 引用 / 仅自己 entry 引用 / 自己+他人混合 / cover 引用 / avatar 引用，
// 分别在 file 路径语义（selfEntryID 空 = 严格零引用）与 diary 路径语义（selfEntryID 非空 =
// 「引用者全是自己」）下断言。
func TestRefsAllowPhysicalDelete(t *testing.T) {
	cases := []struct {
		name               string
		refs               sqlc.GetFileReferencesRow
		selfEntryID        string
		otherEntryRefCount int
		want               bool
	}{
		// file 路径语义：selfEntryID 为空，任何 entry 引用都阻止删除。
		{"file: zero refs allows", sqlc.GetFileReferencesRow{}, "", 0, true},
		{"file: only other entry ref blocks", sqlc.GetFileReferencesRow{UsedByEntry: true}, "", 0, false},
		{"file: cover ref blocks", sqlc.GetFileReferencesRow{UsedByCover: true}, "", 0, false},
		{"file: avatar ref blocks", sqlc.GetFileReferencesRow{AvatarUserCount: 1}, "", 0, false},
		{"file: cover+entry+avatar all block", sqlc.GetFileReferencesRow{UsedByCover: true, UsedByEntry: true, AvatarUserCount: 2}, "", 0, false},
		// diary 路径语义：selfEntryID 非空，仅自身条目的引用不阻止删除。
		{"diary: zero refs allows", sqlc.GetFileReferencesRow{}, "entry-1", 0, true},
		{"diary: only self entry ref allows", sqlc.GetFileReferencesRow{UsedByEntry: true}, "entry-1", 0, true},
		{"diary: only other entry ref blocks", sqlc.GetFileReferencesRow{UsedByEntry: true}, "entry-1", 1, false},
		{"diary: self+other mixed blocks", sqlc.GetFileReferencesRow{UsedByEntry: true}, "entry-1", 2, false},
		{"diary: cover ref blocks", sqlc.GetFileReferencesRow{UsedByCover: true}, "entry-1", 0, false},
		{"diary: avatar ref blocks", sqlc.GetFileReferencesRow{AvatarUserCount: 3}, "entry-1", 0, false},
		{"diary: cover ref blocks even with only self entry", sqlc.GetFileReferencesRow{UsedByCover: true, UsedByEntry: true}, "entry-1", 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := RefsAllowPhysicalDelete(tc.refs, tc.selfEntryID, tc.otherEntryRefCount); got != tc.want {
				t.Fatalf("RefsAllowPhysicalDelete(%+v, %q, %d) = %v, want %v",
					tc.refs, tc.selfEntryID, tc.otherEntryRefCount, got, tc.want)
			}
		})
	}
}

// cleanupFileRow 构造 files 行（GetFileByIDForUpdate 为 SELECT *，11 列）。
func cleanupFileRow(fileType string) *pgxmock.Rows {
	return pgxmock.NewRows([]string{
		"id", "created_by", "path", "name", "suffix", "size_bytes",
		"file_type", "metadata", "created_at", "updated_at", "storage_type",
	}).AddRow("f1", pgtype.Text{String: "u1", Valid: true}, "p/a.jpg", "a", "jpg",
		int64(123), fileType, nil, time.Now(), time.Now(), "local")
}

func cleanupRefsRow(cover, entry bool, avatarCount int32) *pgxmock.Rows {
	return pgxmock.NewRows([]string{"used_by_cover", "used_by_entry", "avatar_user_count"}).
		AddRow(cover, entry, avatarCount)
}

// TestDeletePhysicalIfUnreferenced 锁定 file 清理路径经共用判定谓词 RefsAllowPhysicalDelete
// 接入（selfEntryID 传空 = 严格零引用语义）。
func TestDeletePhysicalIfUnreferenced(t *testing.T) {
	setup := func(t *testing.T) (pgxmock.PgxPoolIface, string, string) {
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
		return mock, tmp, full
	}

	t.Run("unreferenced image deletes and decrements quota", func(t *testing.T) {
		mock, tmp, full := setup(t)
		mock.ExpectBegin()
		mock.ExpectQuery("FOR UPDATE").WithArgs("f1").WillReturnRows(cleanupFileRow("image"))
		mock.ExpectQuery("EXISTS").WithArgs(pgtype.Text{String: "f1", Valid: true}).
			WillReturnRows(cleanupRefsRow(false, false, 0))
		mock.ExpectExec("DELETE FROM files").WithArgs("f1").WillReturnResult(pgxmock.NewResult("DELETE", 1))
		mock.ExpectExec("image_storage_bytes").
			WithArgs("u1", int64(123)).
			WillReturnResult(pgxmock.NewResult("UPDATE", 1))
		mock.ExpectCommit()

		if err := DeletePhysicalIfUnreferenced(context.Background(), db.NewPoolWithDBTX(mock), NewStorage(tmp), "f1"); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if _, err := os.Stat(full); !os.IsNotExist(err) {
			t.Fatalf("physical file should be removed")
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("expectations: %v", err)
		}
	})

	t.Run("entry-referenced file is kept (strict zero-reference semantics)", func(t *testing.T) {
		mock, tmp, full := setup(t)
		mock.ExpectBegin()
		mock.ExpectQuery("FOR UPDATE").WithArgs("f1").WillReturnRows(cleanupFileRow("image"))
		mock.ExpectQuery("EXISTS").WithArgs(pgtype.Text{String: "f1", Valid: true}).
			WillReturnRows(cleanupRefsRow(false, true, 0))
		mock.ExpectCommit()

		if err := DeletePhysicalIfUnreferenced(context.Background(), db.NewPoolWithDBTX(mock), NewStorage(tmp), "f1"); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if _, err := os.Stat(full); err != nil {
			t.Fatalf("physical file should remain: %v", err)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("expectations: %v", err)
		}
	})

	t.Run("in-use file returns ErrFileInUse in DeleteFile", func(t *testing.T) {
		mock, tmp, _ := setup(t)
		mock.ExpectBegin()
		mock.ExpectQuery("FOR UPDATE").WithArgs("f1").WillReturnRows(cleanupFileRow("image"))
		mock.ExpectQuery("EXISTS").WithArgs(pgtype.Text{String: "f1", Valid: true}).
			WillReturnRows(cleanupRefsRow(false, true, 0))
		mock.ExpectRollback()

		err := DeleteFile(context.Background(), db.NewPoolWithDBTX(mock), NewStorage(tmp), "f1", "u1")
		if !errors.Is(err, ErrFileInUse) {
			t.Fatalf("err = %v, want ErrFileInUse", err)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("expectations: %v", err)
		}
	})
}
