-- name: DeleteUserCommonAddresses :exec
DELETE FROM user_common_addresses WHERE user_id = $1;

-- name: InsertUserCommonAddresses :exec
INSERT INTO user_common_addresses (user_id, name, lat, lon, count, updated_at)
SELECT unnest(@user_ids::text[]),
       unnest(@names::text[]),
       unnest(@lats::numeric[]),
       unnest(@lons::numeric[]),
       unnest(@counts::int[]),
       now();

-- name: ListUserCommonAddresses :many
SELECT name, lat, lon, count
FROM user_common_addresses
WHERE user_id = $1
ORDER BY count DESC, updated_at DESC
LIMIT 10;

-- name: GetUserCommonAddress :one
SELECT name, lat, lon, count
FROM user_common_addresses
WHERE user_id = $1 AND name = $2;

-- name: GetUserCommonAddressForUpdate :one
SELECT name, lat, lon, count
FROM user_common_addresses
WHERE user_id = $1 AND name = $2
FOR UPDATE;

-- name: RenameUserCommonAddress :exec
UPDATE user_common_addresses
SET name = $3, updated_at = now()
WHERE user_id = $1 AND name = $2;

-- name: MergeUserCommonAddress :exec
-- upsert 目标行（源行坐标/计数随 INSERT 带入），目标行不存在时计数不静默丢失；
-- ON CONFLICT 保证并发合并计数不丢。源行删除由 DeleteUserCommonAddressByName 在调用方事务内完成
-- （H2：sqlc 会静默丢弃同一 named 块中的第二条语句，不可合并写在这里）。
INSERT INTO user_common_addresses (user_id, name, lat, lon, count, updated_at)
SELECT src.user_id, $3, src.lat, src.lon, src.count, now()
FROM user_common_addresses AS src
WHERE src.user_id = $1 AND src.name = $2
ON CONFLICT (user_id, name) DO UPDATE SET
    count = user_common_addresses.count + EXCLUDED.count,
    updated_at = now();

-- name: DeleteUserCommonAddressByName :exec
DELETE FROM user_common_addresses
WHERE user_id = $1 AND name = $2;

-- name: UpdateDiaryEntriesAddress :execrows
UPDATE diary_entries AS de
SET address = $3, updated_at = now()
WHERE de.created_by = $1 AND de.address = $2;

-- name: UpdateDiaryEntriesAddressBatch :execrows
UPDATE diary_entries AS de
SET address = $4, updated_at = now()
WHERE de.id IN (
    SELECT de_inner.id FROM diary_entries AS de_inner
    WHERE de_inner.created_by = $1 AND de_inner.address = $2
    ORDER BY de_inner.id ASC
    LIMIT $3
);

-- name: GetLatestCoordinateByAddress :one
SELECT de.lat, de.lon
FROM diary_entries AS de
WHERE de.created_by = $1 AND de.address = $2
ORDER BY de.created_at DESC
LIMIT 1;

-- name: ListLatestCoordinatesByAddresses :many
-- 按地址批量取最新坐标（DISTINCT ON），避免循环内逐条 N+1 查询。
SELECT DISTINCT ON (de.address)
    de.address AS name, de.lat, de.lon
FROM diary_entries AS de
WHERE de.created_by = $1 AND de.address = ANY($2::text[])
ORDER BY de.address, de.created_at DESC;

-- name: ListTopAddressesByUser :many
SELECT de.address AS name, COUNT(*) AS count
FROM diary_entries AS de
WHERE de.created_by = $1 AND de.address IS NOT NULL AND de.address <> ''
GROUP BY de.address
ORDER BY COUNT(*) DESC, MAX(de.created_at) DESC
LIMIT $2;

-- name: ListUsersWithDiaryChangesSince :many
-- keyset 游标分页：按 created_by 排序 + 游标 + LIMIT，避免一次性物化全部变更用户。
SELECT DISTINCT de.created_by
FROM diary_entries AS de
WHERE de.updated_at >= sqlc.arg('updated_at') AND de.updated_at < sqlc.arg('updated_at_2')
  AND de.created_by > sqlc.arg('cursor')
ORDER BY de.created_by
LIMIT sqlc.arg('limit');

-- name: NeedsCommonAddressRefresh :one
SELECT
  COALESCE(de_max.ts, '1970-01-01'::timestamptz) > COALESCE(ca_max.ts, '1970-01-01'::timestamptz)
  OR (ca_max.ts IS NOT NULL AND de_max.ts IS NULL) AS needs_refresh
FROM (SELECT MAX(updated_at) AS ts FROM user_common_addresses WHERE user_id = $1) ca_max
CROSS JOIN (SELECT MAX(updated_at) AS ts FROM diary_entries WHERE created_by = $1) de_max;
