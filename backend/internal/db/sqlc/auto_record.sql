-- name: InsertTrajectories :exec
-- PPJ-C04：重复上报（同一 user+recorded_at+lat+lon）静默忽略，避免重试产生重复轨迹。
INSERT INTO auto_record_trajectories (id, user_id, lat, lon, recorded_at, geocode_attempts, created_at)
SELECT unnest(@ids::text[]), unnest(@user_ids::text[]), unnest(@lats::text[])::numeric, unnest(@lons::text[])::numeric, unnest(@recorded_ats::timestamptz[]), 0, now()
ON CONFLICT DO NOTHING;

-- name: ListTrajectoriesByUser :many
-- R-02：排除达到逆地理重试上限的终态轨迹，避免其每轮重复聚类/告警（保留至 7 天清理）。
SELECT id, user_id, lat, lon, recorded_at, geocode_attempts, created_at FROM auto_record_trajectories
WHERE user_id = @user_id
  AND geocode_attempts < @max_geocode_attempts
ORDER BY recorded_at ASC
LIMIT @max_rows;

-- name: DeleteTrajectories :exec
DELETE FROM auto_record_trajectories WHERE id = ANY($1::text[]);

-- name: DeleteStaleTrajectories :execrows
DELETE FROM auto_record_trajectories
WHERE id IN (
    SELECT id FROM auto_record_trajectories
    WHERE created_at < now() - interval '7 days'
    ORDER BY id ASC
    LIMIT $1::bigint
);

-- name: ListAutoRecordCandidates :many
-- R-01：公平轮转——按 user_id keyset 分页，替代「按积压量 ORDER BY cnt DESC」避免低频用户饥饿；
-- 仅取仍有未达重试上限轨迹的用户（R-02）。
SELECT u.id AS user_id
FROM users u
JOIN user_vips v ON v.user_id = u.id
WHERE u.auto_record_enabled = true
  AND v.expire_time > now()
  AND u.id > @cursor_id
  AND EXISTS (
      SELECT 1 FROM auto_record_trajectories t
      WHERE t.user_id = u.id AND t.geocode_attempts < @max_geocode_attempts
  )
ORDER BY u.id ASC
LIMIT @max_users;

-- name: CountAutoRecordCandidates :one
-- R-01：候选积压量（满批时才统计），超过阈值输出 auto_record_backlog_warn。
SELECT count(*)::bigint
FROM users u
JOIN user_vips v ON v.user_id = u.id
WHERE u.auto_record_enabled = true
  AND v.expire_time > now()
  AND EXISTS (
      SELECT 1 FROM auto_record_trajectories t
      WHERE t.user_id = u.id AND t.geocode_attempts < @max_geocode_attempts
  );

-- name: ListAbnormalAlertCandidates :many
SELECT u.id, u.unionid, u.abnormal_subscribe_accepted
FROM users u
JOIN user_vips v ON v.user_id = u.id
LEFT JOIN LATERAL (
    SELECT MAX(t.recorded_at) AS last_recorded_at
    FROM auto_record_trajectories t
    WHERE t.user_id = u.id
) lt ON true
WHERE u.auto_record_enabled = true
  -- PPJ-C01：告警发送侧用严格 VIP（无宽限），候选侧也须严格，否则过期用户每轮入选又被跳过。
  AND v.expire_time > now()
  AND (
      u.abnormal_alert_sent_at IS NULL
      OR u.abnormal_alert_sent_at < now() - interval '1 hour'
  )
  AND (
      u.last_active_at IS NULL
      OR u.last_active_at < $1::timestamptz
  )
  AND COALESCE(lt.last_recorded_at, '1970-01-01'::timestamptz) < $1::timestamptz
  AND (
      u.abnormal_subscribe_accepted = true
      OR EXISTS (SELECT 1 FROM wx_mp_accounts mp WHERE mp.user_id = u.id AND mp.subscribed = true)
  )
LIMIT $2::bigint;

-- name: IncrementTrajectoryGeocodeAttempts :exec
UPDATE auto_record_trajectories
SET geocode_attempts = geocode_attempts + 1
WHERE id = ANY($1::text[]);
