-- name: CreateFile :one
INSERT INTO files (id, created_by, path, name, suffix, size_bytes, file_type, storage_type, metadata, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, now(), now())
RETURNING *;

-- name: GetFileByID :one
SELECT * FROM files WHERE id = $1;

-- name: GetFileByIDForUpdate :one
SELECT * FROM files WHERE id = $1 FOR UPDATE;

-- name: GetFilesByIDs :many
SELECT * FROM files WHERE id = ANY($1::text[]);

-- name: GetFileByPath :one
SELECT * FROM files WHERE path = $1;

-- name: DeleteFile :exec
DELETE FROM files WHERE id = $1;

-- name: BatchDeleteFiles :many
DELETE FROM files
WHERE id = ANY($1::text[])
RETURNING id, path, storage_type;

-- name: ListFilesByCreator :many
SELECT f.id, f.path
FROM files f
WHERE f.created_by = $1
  AND f.id > $2
ORDER BY f.id ASC
LIMIT $3;

-- name: ListUserInviteQRCodeFiles :many
SELECT id, path, storage_type FROM files
WHERE created_by = $1 AND file_type = 'system'
  AND metadata ? 'family_id'
  AND NOT metadata ? 'record_date'
  AND metadata->>'raw' = $2
ORDER BY id ASC
LIMIT 100;

-- name: ListOrphanFilesByCreator :many
SELECT f.id, f.path
FROM files f
WHERE f.created_by = $1
  AND f.id > $2
  AND NOT EXISTS (SELECT 1 FROM family_daily_covers fdc WHERE fdc.cover_file_id = f.id)
  AND NOT EXISTS (SELECT 1 FROM family_daily_covers fdc2 WHERE fdc2.manual_cover_file_id = f.id)
  AND NOT EXISTS (SELECT 1 FROM diary_entry_images dei WHERE dei.file_id = f.id)
  AND NOT EXISTS (SELECT 1 FROM users u WHERE u.avatar_file_id = f.id)
ORDER BY f.id ASC
LIMIT $3;

-- name: ClearUserAvatarByFile :exec
UPDATE users SET avatar = NULL, avatar_file_id = NULL, updated_at = now()
WHERE avatar_file_id = $1 AND id = $2;

-- name: ClearAvatarReferencesToOrphanFiles :exec
UPDATE users u
SET avatar = NULL, avatar_file_id = NULL, updated_at = now()
WHERE u.avatar_file_id IS NOT NULL
  AND NOT EXISTS (SELECT 1 FROM files f WHERE f.id = u.avatar_file_id);

-- name: ScanOrphanFiles :many
-- 宽限窗 7 天：上传成功但尚未挂到任何引用（客户端崩溃/离线草稿）的图片不应被回收，
-- 与代码注释及 spec「7 天孤儿清理兜底」口径一致。
SELECT f.id, f.path
FROM files f
WHERE f.file_type = 'image'
  AND f.id > $1
  AND f.created_at < now() - interval '7 days'
  AND NOT EXISTS (SELECT 1 FROM diary_entry_images dei WHERE dei.file_id = f.id)
  AND NOT EXISTS (SELECT 1 FROM users u WHERE u.avatar_file_id = f.id)
  AND NOT EXISTS (SELECT 1 FROM family_daily_covers fdc WHERE fdc.cover_file_id = f.id)
  AND NOT EXISTS (SELECT 1 FROM family_daily_covers fdc2 WHERE fdc2.manual_cover_file_id = f.id)
ORDER BY f.id ASC
LIMIT 1000;

-- name: ScanOldSystemFiles :many
-- 邀请二维码/分享海报豁免：invite/qrcode.go generate 写入的海报记录 metadata 恒含 'raw' 键
--（轨迹图为 {family_id, record_date}、头像 marker 为 {}，均无该键，故 'raw' 是 invite 资产唯一识别特征）。
-- 已分发到微信会话的图片 URL 不能被 7 天窗口静默回收成 404；其回收由两条替代路径覆盖：
-- ①重生成替换（generate 事务内删旧记录、提交后删旧物理文件）②注销清理（DeleteAccount 按
-- created_by 收集全部 files 路径删物理文件）。COALESCE 兜底 metadata 为 NULL 的行维持可回收。
SELECT id, path, storage_type FROM files
WHERE file_type = 'system'
  AND id > $1
  AND created_at < now() - interval '7 days'
  AND updated_at < now() - interval '7 days'
  AND NOT COALESCE(metadata ? 'raw', false)
  AND NOT EXISTS (
    SELECT 1 FROM family_daily_covers fdc
    WHERE fdc.cover_file_id = files.id
  )
  AND NOT EXISTS (
    SELECT 1 FROM family_daily_covers fdc2
    WHERE fdc2.manual_cover_file_id = files.id
  )
  AND NOT EXISTS (
    SELECT 1 FROM user_avatar_markers uam
    WHERE uam.marker_path = files.path
  )
ORDER BY id ASC
LIMIT 1000;

-- name: GetFileReferences :one
SELECT
    EXISTS(SELECT 1 FROM family_daily_covers WHERE cover_file_id = $1 OR manual_cover_file_id = $1) AS used_by_cover,
    EXISTS(SELECT 1 FROM diary_entry_images WHERE file_id = $1) AS used_by_entry,
    (SELECT COUNT(*) FROM users WHERE avatar_file_id = $1)::int AS avatar_user_count;

-- name: ClearTrajectoryFamilyID :exec
UPDATE files
SET metadata = metadata - 'family_id',
    updated_at = now()
WHERE file_type = 'system'
  AND metadata->>'family_id' = sqlc.arg(family_id)::text;
