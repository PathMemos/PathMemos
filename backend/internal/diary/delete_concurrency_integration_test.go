package diary

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"papafeiji/backend/internal/db/sqlc"
	"papafeiji/backend/internal/migration"
	"papafeiji/backend/migratefs"
)

// 该文件为 A1（docs/OBJECTIVES §3.1）要求的**真实 PG 并发回归用例**：
// 复现「删除空日记（锁头行 + 条件删除）× 并发新增条目」竞态，断言并发已提交的
// 条目存活、删除按条件生效。pgxmock 无法表达行锁等待语义，故仅在提供
// TEST_DATABASE_URL 时运行（CI 无 PG，自动跳过）。TEST_DATABASE_URL 必须指向
// **一次性空库**——setup 执行仓库内嵌迁移建立生产 schema（与 open 模式启动自动
// 迁移同一机制）：
//
//	TEST_DATABASE_URL=postgres://user:pass@127.0.0.1:15432/delete_race_test \
//	  go test ./backend/internal/diary/ -run TestDeleteEmptyDiary -v
//
// 语义背景（PG 15 实测）：READ COMMITTED 下 DELETE 的 NOT EXISTS
// 子查询在等锁后**不会**以新快照重评（EPQ 沿用语句快照）——纯条件删除不闭合竞态，
// 必须先对头行 FOR UPDATE（与并发 INSERT 的 FK KEY SHARE 冲突）再条件删除。

func newIntegrationDB(t *testing.T) (*pgx.Conn, func()) {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL 未设置：跳过真实 PG 并发回归（CI 无 PG）")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect pool: %v", err)
	}
	// go test 的 CWD 是包目录：按本文件位置定位仓库内 backend/migrations。
	_, thisFile, _, _ := runtime.Caller(0)
	migDir := filepath.Join(filepath.Dir(thisFile), "..", "..", "migrations")
	ms, err := migratefs.LoadMigrations(migDir)
	if err != nil {
		t.Fatalf("load migrations: %v", err)
	}
	if err := migration.Run(ctx, pool, ms); err != nil {
		t.Fatalf("run migrations: %v", err)
	}
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatalf("connect raw conn: %v", err)
	}
	cleanup := func() {
		_ = conn.Close(ctx)
		pool.Close()
	}
	return conn, cleanup
}

// TestDeleteEmptyDiaryConcurrency 并发时序：T2 插入条目（FK KEY SHARE 持锁）未提交 →
// T1 删除最后条目后走 LockDiaryByIDForUpdate（阻塞至 T2 提交）+ DeleteDiaryIfEmpty
// （生产 SQL，经 sqlc 生成方法执行）→ 断言 T2 已提交条目存活、diaries 行存活。
func TestDeleteEmptyDiaryConcurrency(t *testing.T) {
	conn, closeConn := newIntegrationDB(t)
	defer closeConn()
	ctx := context.Background()

	must := func(err error, what string) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}
	mustExec := func(sql string, args ...any) {
		t.Helper()
		if _, err := conn.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("exec %s: %v", sql, err)
		}
	}

	mustExec(`TRUNCATE diary_entries, memories, diaries CASCADE`)
	mustExec(`INSERT INTO families (id, is_personal) VALUES ('F1', true) ON CONFLICT DO NOTHING`)
	mustExec(`INSERT INTO users (id, open_id, personal_family_id, current_family_id) VALUES ('u1', 'openid-race-test', 'F1', 'F1') ON CONFLICT DO NOTHING`)
	// family_members.user_id 为 DEFERRABLE 唯一约束，不支持 ON CONFLICT 仲裁——用 NOT EXISTS 幂等
	mustExec(`INSERT INTO family_members (id, family_id, user_id, role) SELECT 'FM1', 'F1', 'u1', 'owner' WHERE NOT EXISTS (SELECT 1 FROM family_members WHERE user_id = 'u1')`)
	mustExec(`INSERT INTO diaries (id, user_id, record_date) VALUES ('D1', 'u1', '2026-10-01')`)
	mustExec(`INSERT INTO diary_entries (id, diary_id, created_by) VALUES ('E1', 'D1', 'u1')`)

	// T2：并发保存——UpsertDiary（已存在则 DO NOTHING）+ 新增条目；条目 INSERT
	// 对日记头行取 FK KEY SHARE 锁（不显式 FOR UPDATE，与生产保存路径一致），暂不提交。
	tx2, err := conn.Begin(ctx)
	must(err, "begin tx2")
	if _, err := tx2.Exec(ctx, `INSERT INTO diaries (id, user_id, record_date) VALUES ('D1','u1','2026-10-01') ON CONFLICT DO NOTHING`); err != nil {
		t.Fatalf("tx2 upsert diary: %v", err)
	}
	if _, err := tx2.Exec(ctx, `INSERT INTO diary_entries (id, diary_id, created_by) VALUES ('E2','D1','u1')`); err != nil {
		t.Fatalf("tx2 insert entry: %v", err)
	}

	// T1：删除最后条目 → 锁头行（阻塞至 T2 提交）→ 条件删除。
	t1Done := make(chan error, 1)
	go func() {
		tx1, err := conn.Begin(ctx)
		if err != nil {
			t1Done <- err
			return
		}
		defer func() { _ = tx1.Rollback(ctx) }()
		q1 := sqlc.New(tx1)
		if _, err := tx1.Exec(ctx, `DELETE FROM diary_entries WHERE id = 'E1'`); err != nil {
			t1Done <- err
			return
		}
		if _, err := q1.LockDiaryByIDForUpdate(ctx, "D1"); err != nil {
			t1Done <- err
			return
		}
		if _, err := q1.DeleteDiaryIfEmpty(ctx, "D1"); err != nil {
			t1Done <- err
			return
		}
		t1Done <- tx1.Commit(ctx)
	}()

	// 给 T1 足够时间走到锁等待点（其 DELETE 条目语句不与 T2 冲突）。
	time.Sleep(500 * time.Millisecond)
	must(tx2.Commit(ctx), "commit tx2")

	select {
	case err := <-t1Done:
		must(err, "tx1")
	case <-time.After(10 * time.Second):
		t.Fatal("tx1 未在 T2 提交后完成（锁未按预期释放）")
	}

	var diaries, entries int
	must(conn.QueryRow(ctx, `SELECT COUNT(*) FROM diaries`).Scan(&diaries), "count diaries")
	must(conn.QueryRow(ctx, `SELECT COUNT(*) FROM diary_entries`).Scan(&entries), "count entries")
	if diaries != 1 {
		t.Fatalf("diaries = %d, want 1：并发已提交条目所在的日记行被删除（竞态未闭合）", diaries)
	}
	if entries != 1 {
		t.Fatalf("entries = %d, want 1：T2 已提交的条目 E2 丢失（级联删除竞态复现）", entries)
	}
}

// TestDeleteEmptyDiaryNoContention 无并发基线：确无条目/记忆时条件删除生效，
// 「防幽灵空卡片」的原有语义不回归。
func TestDeleteEmptyDiaryNoContention(t *testing.T) {
	conn, closeConn := newIntegrationDB(t)
	defer closeConn()
	ctx := context.Background()
	q := sqlc.New(conn)

	if _, err := conn.Exec(ctx, `TRUNCATE diary_entries, memories, diaries CASCADE`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `INSERT INTO families (id, is_personal) VALUES ('F1', true) ON CONFLICT DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `INSERT INTO users (id, open_id, personal_family_id, current_family_id) VALUES ('u1', 'openid-race-test', 'F1', 'F1') ON CONFLICT DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `INSERT INTO family_members (id, family_id, user_id, role) VALUES ('FM1', 'F1', 'u1', 'owner') ON CONFLICT DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `INSERT INTO diaries (id, user_id, record_date) VALUES ('D2', 'u1', '2026-10-02')`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `INSERT INTO diary_entries (id, diary_id, created_by) VALUES ('E3', 'D2', 'u1')`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `DELETE FROM diary_entries WHERE id = 'E3'`); err != nil {
		t.Fatal(err)
	}

	n, err := q.DeleteDiaryIfEmpty(ctx, "D2")
	if err != nil {
		t.Fatalf("DeleteDiaryIfEmpty: %v", err)
	}
	if n != 1 {
		t.Fatalf("deleted rows = %d, want 1（空日记应被条件删除）", n)
	}
	var diaries int
	if err := conn.QueryRow(ctx, `SELECT COUNT(*) FROM diaries`).Scan(&diaries); err != nil {
		t.Fatal(err)
	}
	if diaries != 0 {
		t.Fatalf("diaries = %d, want 0", diaries)
	}
}
