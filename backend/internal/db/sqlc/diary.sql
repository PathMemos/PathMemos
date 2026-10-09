-- name: UpsertDiary :one
WITH ins AS (
    INSERT INTO diaries (id, user_id, record_date)
    VALUES ($1, $2, $3)
    ON CONFLICT (user_id, record_date) DO NOTHING
    RETURNING id
)
SELECT id FROM ins
UNION ALL
SELECT id FROM diaries WHERE user_id = $2 AND record_date = $3
LIMIT 1;

-- name: GetDiaryByUserAndDate :one
SELECT * FROM diaries WHERE user_id = $1 AND record_date = $2;

-- name: GetDiaryByID :one
SELECT * FROM diaries WHERE id = $1;

-- name: TouchDiaryUpdatedAt :exec
UPDATE diaries SET updated_at = now() WHERE id = $1;

-- name: ListDiaryCards :many
WITH distinct_dates AS MATERIALIZED (
    SELECT DISTINCT record_date
    FROM diaries
    WHERE user_id = ANY($1::text[]) AND record_date < $2::date
    ORDER BY record_date DESC
    LIMIT $3
)
SELECT d.id, d.record_date, d.user_id, COUNT(de.id) AS entry_count
FROM diaries d
JOIN distinct_dates dd ON d.record_date = dd.record_date
LEFT JOIN diary_entries de ON de.diary_id = d.id
WHERE d.user_id = ANY($1::text[])
GROUP BY d.id, d.record_date, d.user_id
ORDER BY d.record_date DESC, d.user_id ASC;

-- name: ListDiaryCardsByDates :many
SELECT d.id, d.record_date, d.user_id, COUNT(de.id) AS entry_count
FROM diaries d
LEFT JOIN diary_entries de ON de.diary_id = d.id
WHERE d.user_id = ANY($1::text[])
  AND d.record_date = ANY($2::date[])
GROUP BY d.id, d.record_date, d.user_id
ORDER BY d.record_date DESC, d.user_id ASC
LIMIT 1000;

-- name: ListAddressEntriesByDates :many
SELECT de.created_by, de.address, de.record_time, d.record_date
FROM diary_entries de
JOIN diaries d ON d.id = de.diary_id
WHERE d.user_id = ANY($1::text[])
  AND d.record_date = ANY($2::date[])
  AND de.address IS NOT NULL
  AND de.address != ''
ORDER BY d.record_date, de.record_time ASC NULLS LAST, de.created_at ASC
LIMIT 1000;

-- name: ListDiaryEntries :many
SELECT de.*, u.avatar AS user_avatar, u.nickname AS user_nickname
FROM diary_entries de
JOIN diaries d ON d.id = de.diary_id
JOIN users u ON u.id = de.created_by
WHERE d.user_id = ANY($1::text[])
  AND d.record_date = $2::date
ORDER BY de.sort ASC, de.record_time ASC NULLS LAST, de.created_at ASC
LIMIT $4 OFFSET $3;

-- name: GetLatestImageEntry :one
-- 选择当日最新图片日记作为 image 优先级封面。
-- 排序语义：用户手动排序（sort_order 越小越靠前） > 日记记录时间 > 图片上传时间。
SELECT dei.file_id AS image_id, f.path, f.storage_type
FROM diary_entry_images dei
JOIN diary_entries de ON de.id = dei.diary_entry_id
JOIN diaries d ON d.id = de.diary_id
JOIN files f ON f.id = dei.file_id
WHERE d.user_id = ANY($1::text[])
  AND d.record_date = $2::date
ORDER BY dei.sort_order ASC, de.record_time DESC NULLS LAST, dei.created_at DESC
LIMIT 1;

-- name: ListLocationEntries :many
SELECT de.created_by, de.lat, de.lon, de.record_time, de.created_at
FROM diary_entries de
JOIN diaries d ON d.id = de.diary_id
WHERE d.user_id = ANY($1::text[])
  AND d.record_date = $2::date
  AND de.lat IS NOT NULL
  AND de.lon IS NOT NULL
ORDER BY de.record_time ASC NULLS LAST, de.created_at ASC
LIMIT 200;

-- name: CountDiaryEntries :one
SELECT COUNT(*)
FROM diary_entries de
JOIN diaries d ON d.id = de.diary_id
WHERE d.user_id = ANY($1::text[])
  AND d.record_date = $2::date;

-- name: GetDiaryFirstRecordDate :one
SELECT COALESCE(MIN(record_date), '1970-01-01')::date AS first_record_date
FROM diaries
WHERE user_id = ANY($1::text[]);

-- name: CountDiaryEntriesByUsers :one
SELECT COUNT(*)
FROM diary_entries
WHERE diary_id IN (SELECT id FROM diaries WHERE user_id = ANY($1::text[]));

-- name: CountWeeklyDiaryEntriesByUsers :one
SELECT COUNT(*)
FROM diary_entries de
JOIN diaries d ON d.id = de.diary_id
WHERE d.user_id = ANY($1::text[])
  AND d.record_date >= $2::date;

-- name: DeleteDiaryImagesReturningFileIDs :many
-- 先删图片关联并返回 file_id，再由调用方在同一事务内删除日记（级联删除条目）。
-- 拆成两条语句：PostgreSQL 不保证同一 WITH 内多个数据修改 CTE 的执行顺序，
-- 若级联（删日记）先于显式 DELETE...RETURNING 执行，file_id 会为空导致文件漏清理。
DELETE FROM diary_entry_images
WHERE diary_entry_id IN (SELECT id FROM diary_entries WHERE diary_id = $1)
RETURNING file_id;

-- name: LockDiaryByIDForUpdate :one
SELECT id FROM diaries WHERE id = $1 FOR UPDATE;

-- name: DeleteDiaryIfEmpty :execrows
-- 条件删除：头行 FOR UPDATE（LockDiaryByIDForUpdate）与本语句配合，
-- 与并发新增条目（FK KEY SHARE）串行化，关闭「计数后删行」窗口内
-- 并发已提交条目被级联删除的丢失路径（docs/OBJECTIVES A1）。
DELETE FROM diaries AS d
WHERE d.id = $1
  AND NOT EXISTS (SELECT 1 FROM diary_entries e WHERE e.diary_id = d.id)
  AND NOT EXISTS (SELECT 1 FROM memories m WHERE m.user_id = d.user_id AND m.record_date = d.record_date);

-- name: DeleteDiaryEntriesByDiaryID :execrows
DELETE FROM diary_entries WHERE diary_id = $1;

-- name: FindTrajectoryCovers :many
SELECT id, path, storage_type FROM files
WHERE file_type = 'system'
  AND metadata->>'family_id' = $1::text
  AND metadata->>'record_date' = $2::text;

-- name: DeleteTrajectoryCovers :exec
DELETE FROM files
WHERE file_type = 'system'
  AND metadata->>'family_id' = $1::text
  AND metadata->>'record_date' = $2::text
  AND id <> $3::text;

-- name: GetFamilyDailyCover :one
SELECT fdc.cover_file_id, fdc.cover_type, fdc.manual_cover_file_id, f.path AS cover_path, f.storage_type AS cover_storage_type
FROM family_daily_covers fdc
LEFT JOIN files f ON f.id = fdc.cover_file_id
WHERE fdc.family_id = $1 AND fdc.record_date = $2;

-- name: ListFamilyDailyCovers :many
SELECT fdc.record_date, fdc.cover_type, fdc.cover_file_id, fdc.manual_cover_file_id, f.path AS cover_path, f.storage_type AS cover_storage_type
FROM family_daily_covers fdc
LEFT JOIN files f ON f.id = fdc.cover_file_id
WHERE fdc.family_id = @family_id AND fdc.record_date = ANY(@record_dates::date[]);

-- name: UpsertFamilyDailyCover :exec
INSERT INTO family_daily_covers (family_id, record_date, cover_file_id, cover_type, manual_cover_file_id, updated_at)
VALUES ($1, $2, $3, $4, $5, now())
ON CONFLICT (family_id, record_date)
DO UPDATE SET
    cover_file_id = EXCLUDED.cover_file_id,
    cover_type = EXCLUDED.cover_type,
    manual_cover_file_id = EXCLUDED.manual_cover_file_id,
    updated_at = now();

-- name: SetFamilyDailyManualCover :exec
INSERT INTO family_daily_covers (family_id, record_date, cover_file_id, cover_type, manual_cover_file_id, updated_at)
VALUES ($1, $2, $3, 'manual', $3, now())
ON CONFLICT (family_id, record_date)
DO UPDATE SET
    cover_file_id = EXCLUDED.cover_file_id,
    cover_type = 'manual',
    manual_cover_file_id = EXCLUDED.manual_cover_file_id,
    updated_at = now();

-- name: ClearFamilyDailyManualCover :exec
UPDATE family_daily_covers
SET manual_cover_file_id = NULL,
    cover_file_id = CASE WHEN cover_type = 'manual' THEN NULL ELSE cover_file_id END,
    cover_type = CASE WHEN cover_type = 'manual' THEN 'default' ELSE cover_type END,
    updated_at = now()
WHERE family_id = $1
  AND record_date = $2
  AND manual_cover_file_id IS NOT NULL;

-- name: ClearFamilyDailyManualCoverByFileID :exec
UPDATE family_daily_covers
SET manual_cover_file_id = NULL,
    cover_file_id = CASE WHEN cover_type = 'manual' THEN NULL ELSE cover_file_id END,
    cover_type = CASE WHEN cover_type = 'manual' THEN 'default' ELSE cover_type END,
    updated_at = now()
WHERE family_id = $1
  AND record_date = $2
  AND manual_cover_file_id = $3;

-- name: ClearFamilyDailyManualCoverByFileIDs :exec
UPDATE family_daily_covers
SET manual_cover_file_id = NULL,
    cover_file_id = CASE WHEN cover_type = 'manual' THEN NULL ELSE cover_file_id END,
    cover_type = CASE WHEN cover_type = 'manual' THEN 'default' ELSE cover_type END,
    updated_at = now()
WHERE family_id = $1
  AND record_date = $2
  AND manual_cover_file_id = ANY($3::text[]);

-- name: ClearFamilyDailyImageCoverByFileID :exec
UPDATE family_daily_covers
SET cover_file_id = NULL,
    cover_type = 'default',
    updated_at = now()
WHERE family_id = $1
  AND record_date = $2
  AND cover_type = 'image'
  AND cover_file_id = $3;

-- name: ClearFamilyDailyImageCoverByFileIDs :exec
UPDATE family_daily_covers
SET cover_file_id = NULL,
    cover_type = 'default',
    updated_at = now()
WHERE family_id = $1
  AND record_date = $2
  AND cover_type = 'image'
  AND cover_file_id = ANY($3::text[]);

-- name: GetDiaryEntriesByImageID :many
SELECT 1 AS ref
FROM diary_entries de
JOIN diary_entry_images dei ON dei.diary_entry_id = de.id
WHERE dei.file_id = @file_id
  AND de.id <> @diary_entry_id::text
LIMIT 1;

-- name: IsImageUsedByFamilyDate :one
SELECT EXISTS (
    SELECT 1
    FROM diary_entry_images dei
    JOIN diary_entries de ON de.id = dei.diary_entry_id
    JOIN diaries d ON d.id = de.diary_id
    WHERE dei.file_id = $1
      AND d.user_id = ANY($2::text[])
      AND d.record_date = $3::date
);
