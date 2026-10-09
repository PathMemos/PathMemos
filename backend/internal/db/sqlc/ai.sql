-- name: InsertAIDialogLog :exec
INSERT INTO ai_dialog_logs (id, user_id, role, content, created_at)
VALUES ($1, $2, $3, $4, $5);

-- name: ListRecentDialogLogs :many
SELECT * FROM ai_dialog_logs
WHERE user_id = $1
  AND created_at > now() - interval '7 days'
ORDER BY created_at DESC
LIMIT $2;

-- name: DeleteOldDialogLogs :execrows
DELETE FROM ai_dialog_logs
WHERE id IN (
    SELECT id FROM ai_dialog_logs
    WHERE created_at < now() - interval '90 days'
    -- 按创建时间最旧优先删除，而非随机 UUID 顺序。
    ORDER BY created_at ASC
    LIMIT $1::bigint
);

-- name: ListDiaryEntriesByDateRange :many
SELECT
    COALESCE(de.record_time, de.created_at) AS created_at,
    COALESCE(NULLIF(TRIM(de.text), ''), '')::text AS content,
    COALESCE(NULLIF(TRIM(de.address), ''), '')::text AS location
FROM diary_entries de
JOIN diaries d ON d.id = de.diary_id
WHERE d.user_id = $1
  AND d.record_date >= $2::date
  AND d.record_date <= $3::date
-- 排序与展示列同源：record_time 被编辑后结果顺序仍与展示值一致。
ORDER BY COALESCE(de.record_time, de.created_at) DESC
LIMIT $4;

-- name: ListAIBackgroundEntries :many
SELECT
    COALESCE(NULLIF(TRIM(u.nickname), ''), '家人')::text AS nickname,
    string_agg(
        to_char(sub.record_at AT TIME ZONE 'Asia/Shanghai', 'YYYYMMDD HH24"时"') ||
        CASE WHEN sub.detail_address <> '' THEN ' | ' || sub.detail_address ELSE '' END ||
        CASE WHEN sub.text <> '' THEN ' | ' || sub.text ELSE '' END,
        E'\n' ORDER BY sub.record_at DESC
    )::text AS content
FROM family_members fm
JOIN users u ON u.id = fm.user_id
JOIN LATERAL (
    SELECT
        COALESCE(de.record_time, de.created_at) AS record_at,
        COALESCE(NULLIF(TRIM(de.address), ''), NULLIF(TRIM(de.detail_address), ''), '')::text AS detail_address,
        COALESCE(NULLIF(TRIM(de.text), ''), '') AS text
    FROM diaries d
    JOIN diary_entries de ON de.diary_id = d.id
    WHERE d.user_id = fm.user_id
      AND de.created_at > now() - interval '90 days'
    ORDER BY de.created_at DESC
    LIMIT 500
) sub ON true
WHERE fm.family_id = $1
GROUP BY fm.user_id, u.nickname
ORDER BY MIN(sub.record_at) DESC
LIMIT $2;

-- name: IncrementAIDailyQuotaUsed :one
WITH upsert AS (
    INSERT INTO ai_daily_quota_usage (user_id, quota_date, used, created_at, updated_at)
    VALUES ($1, $2, 1, now(), now())
    ON CONFLICT (user_id, quota_date)
    DO UPDATE SET
        used = ai_daily_quota_usage.used + 1,
        updated_at = now()
    WHERE ai_daily_quota_usage.used < $3
    RETURNING used
)
SELECT u.used, true AS incremented
FROM upsert u
UNION ALL
SELECT d.used, false AS incremented
FROM ai_daily_quota_usage d
WHERE d.user_id = $1 AND d.quota_date = $2
  AND NOT EXISTS (SELECT 1 FROM upsert);

-- name: DecrementAIDailyQuotaUsed :one
UPDATE ai_daily_quota_usage
SET used = GREATEST(used - 1, 0),
    updated_at = now()
WHERE user_id = $1
  AND quota_date = $2
RETURNING used;

-- name: GetAIDailyQuotaUsed :one
SELECT used FROM ai_daily_quota_usage
WHERE user_id = $1 AND quota_date = $2;

-- name: DeleteAIDailyQuotaUsageByUserID :exec
DELETE FROM ai_daily_quota_usage
WHERE user_id = $1;
