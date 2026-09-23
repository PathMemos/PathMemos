// Package autorecord provides related functionality.
package autorecord

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"papafeiji/backend/internal/config"
	"papafeiji/backend/internal/db"
	"papafeiji/backend/internal/db/sqlc"
	"papafeiji/backend/internal/location"
	"papafeiji/backend/internal/pkg/safe"
	"papafeiji/backend/pkg/timeutil"
	"papafeiji/backend/pkg/util"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/redis/go-redis/v9"
)

const (
	stayPointMergeRadiusM = 300.0
	stayPointMergeWindow  = 30 * time.Minute
	maxPendingPerUser     = 100
	processWorkers        = 5
	maxGeocodeAttempts    = 10
	// R-01：候选 keyset 轮转游标与积压告警阈值。
	autoRecordCursorKey            = "job:cursor:auto_record"
	autoRecordBacklogWarnThreshold = 1000
)

type Service struct {
	pool  *db.Pool
	rdb   *redis.Client
	lock  db.Locker
	loc   *location.Client
	diary DiaryCoverRefresher
	push  PushService
}

type DiaryCoverRefresher interface {
	RefreshFamilyDailyCover(ctx context.Context, familyID, recordDate string) error
}

// PushService sends new-place alerts and tracks user activity for abnormal detection.
type PushService interface {
	SendNewPlaceAlert(ctx context.Context, userID, diaryID, entryID string) error
	TouchActiveAt(ctx context.Context, userID string) error
}

func NewService(pool *db.Pool, rdb *redis.Client, cfg *config.Config, diary DiaryCoverRefresher, push PushService) *Service {
	return &Service{
		pool:  pool,
		rdb:   rdb,
		lock:  db.NewAdvisoryLock(pool.PGX()),
		loc:   location.NewClient(cfg.TencentMapKeys),
		diary: diary,
		push:  push,
	}
}

// TouchActiveAt 仅更新 last_active_at，不重置 abnormal_alert_sent_at（AR-11.7：告警抑制由候选 SQL 的 last_active_at 条件实现）。
func (s *Service) TouchActiveAt(ctx context.Context, userID string) error {
	if s.push == nil {
		return nil
	}
	return s.push.TouchActiveAt(ctx, userID)
}

// HasActiveVIP 严格判定：expire_time > now，无宽限期（与 02c「严格 VIP」口径一致）。
func (s *Service) HasActiveVIP(ctx context.Context, userID string) bool {
	return s.vipInfo(ctx, userID)
}

func (s *Service) ProcessRound(ctx context.Context) error {
	// R-01：keyset 轮转游标；Redis 丢失只会从头再扫，不影响正确性（不变量 I2）。
	cursor := ""
	if s.rdb != nil {
		if v, rerr := s.rdb.Get(ctx, autoRecordCursorKey).Result(); rerr == nil {
			cursor = v
		}
	}
	userIDs, err := s.candidateBatch(ctx, cursor)
	if err != nil {
		return fmt.Errorf("list pending users: %w", err)
	}
	next, wrapped := nextCursor(userIDs, cursor)
	if wrapped {
		userIDs, err = s.candidateBatch(ctx, "")
		if err != nil {
			return fmt.Errorf("list pending users (wrap): %w", err)
		}
		next, _ = nextCursor(userIDs, "")
	}
	if s.rdb != nil && next != cursor {
		if werr := s.rdb.Set(ctx, autoRecordCursorKey, next, 0).Err(); werr != nil {
			slog.WarnContext(ctx, "auto record cursor write failed", slog.Any("error", werr))
		}
	}
	// 满批时统计候选总量，超过阈值告警（R-01）。
	if len(userIDs) == int(maxPendingPerUser) {
		if n, cerr := s.pool.Queries().CountAutoRecordCandidates(ctx, maxGeocodeAttempts); cerr == nil && n > autoRecordBacklogWarnThreshold {
			slog.WarnContext(ctx, "auto_record_backlog_warn", slog.Int64("candidates", n), slog.Int("batch", len(userIDs)))
		}
	}

	jobs := make(chan string, len(userIDs))
	for _, userID := range userIDs {
		jobs <- userID
	}
	close(jobs)

	var wg sync.WaitGroup
	var failed atomic.Int32
	for i := 0; i < processWorkers; i++ {
		wg.Add(1)
		safe.Go(ctx, nil, func() {
			defer wg.Done()
			for userID := range jobs {
				if err := s.processUser(ctx, userID); err != nil {
					slog.ErrorContext(ctx, "auto record process user failed", slog.String("user_id", userID), slog.Any("error", err))
					failed.Add(1)
				}
			}
		})
	}
	wg.Wait()

	if failed.Load() > 0 {
		// R-19：轮次存在用户级失败（轨迹已保留、下轮重试），输出告警关键字供 alert-watch 捕获。
		slog.ErrorContext(ctx, "alert=auto_record_failed", slog.Int("failed_users", int(failed.Load())))
		return fmt.Errorf("auto record round completed with %d failures", failed.Load())
	}
	return nil
}

// candidateBatch 取一批候选用户（keyset，按 user_id 升序）。
func (s *Service) candidateBatch(ctx context.Context, cursor string) ([]string, error) {
	return s.pool.Queries().ListAutoRecordCandidates(ctx, sqlc.ListAutoRecordCandidatesParams{
		CursorID:           cursor,
		MaxGeocodeAttempts: maxGeocodeAttempts,
		MaxUsers:           maxPendingPerUser,
	})
}

// nextCursor 返回下一轮游标与是否回绕：满批取最后一个 id；空批且已有游标表示扫到尾部，回绕到起点。
func nextCursor(batch []string, prev string) (string, bool) {
	if len(batch) == 0 {
		return "", prev != ""
	}
	return batch[len(batch)-1], false
}

func (s *Service) processUser(ctx context.Context, userID string) (err error) {
	lockKey := "lock:auto_record:" + userID
	ok, lockToken, err := s.lock.TryLock(ctx, lockKey)
	if err != nil {
		slog.ErrorContext(ctx, "auto record process user lock error", slog.String("user_id", userID), slog.Any("error", err))
		return err
	}
	if !ok {
		return fmt.Errorf("auto record already in progress for user %s", userID)
	}

	// 封面刷新与缓存失效必须在分布式锁释放后再异步执行，避免异步任务开始时锁仍被占用
	//（AGENTS.md §3.8/§4.1）。
	var asyncFamilyID string
	var asyncDates []string
	defer func() {
		if uerr := s.lock.Unlock(context.WithoutCancel(ctx), lockKey, lockToken); uerr != nil {
			slog.ErrorContext(ctx, "auto record unlock failed", slog.String("user_id", userID), slog.Any("error", uerr))
		}
		if err == nil && asyncFamilyID != "" && len(asyncDates) > 0 {
			safe.Go(ctx, nil, func() {
				bgCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				for _, recordDate := range asyncDates {
					func(date string) {
						ctx, cancel := context.WithTimeout(bgCtx, 30*time.Second)
						refreshErr := s.diary.RefreshFamilyDailyCover(ctx, asyncFamilyID, date)
						if refreshErr != nil {
							slog.ErrorContext(ctx, "auto record refresh family daily cover failed",
								slog.String("family_id", asyncFamilyID),
								slog.String("record_date", date),
								slog.Any("error", refreshErr))
						}
						cancel()
					}(recordDate)
				}
				// 封面刷新完成后再删除汇总缓存，避免刷新期间其他读请求用旧封面重建缓存。
				if s.rdb != nil {
					if err := s.rdb.Del(context.WithoutCancel(ctx), "ai:family_summary:"+asyncFamilyID).Err(); err != nil {
						slog.WarnContext(ctx, "invalidate family summary cache failed", slog.String("family_id", asyncFamilyID), slog.Any("error", err))
					}
				}
			})
		}
	}()

	user, err := s.pool.Queries().GetUserByID(ctx, userID)
	if err != nil {
		return fmt.Errorf("get user: %w", err)
	}
	if !user.AutoRecordEnabled {
		return nil
	}
	if !s.vipInfo(ctx, userID) {
		return nil
	}

	rows, err := s.pool.Queries().ListTrajectoriesByUser(ctx, sqlc.ListTrajectoriesByUserParams{
		UserID:             userID,
		MaxGeocodeAttempts: maxGeocodeAttempts,
		MaxRows:            maxPendingPerUser,
	})
	if err != nil {
		return fmt.Errorf("list trajectories: %w", err)
	}
	if len(rows) == 0 {
		return nil
	}

	// 一次性查询该用户的常用地址，在内存中供所有 cluster 复用（性能最低方案）。
	commonAddrs, commonAddrErr := s.pool.Queries().ListUserCommonAddresses(ctx, userID)
	if commonAddrErr != nil {
		slog.WarnContext(ctx, "auto record list common addresses failed, skip matching",
			slog.String("user_id", userID),
			slog.Any("error", commonAddrErr))
	}

	clusters, invalidIDs := s.mergeStayPoints(rows)
	if len(clusters) == 0 {
		// OPS-LOG：轨迹未形成停留点即被清理（用于数据问题复盘）。
		slog.InfoContext(ctx, "auto record round no clusters",
			slog.String("user_id", userID),
			slog.Int("trajectories", len(rows)),
		)
		return s.pool.Queries().DeleteTrajectories(ctx, collectTrajectoryIDs(rows))
	}

	// R-20（同日去重集合化）：按日期缓存当天全部自动条目的地址集合，候选与集合比较——
	// 旧逻辑只与当天最后一条比较，同日折返旧地点（家→公司→家）会重复成文。
	autoAddrSets := make(map[string]map[string]struct{})
	autoAddrSetFor := func(recordDate string, date pgtype.Date) (map[string]struct{}, error) {
		if set, ok := autoAddrSets[recordDate]; ok {
			return set, nil
		}
		rows, err := s.pool.Queries().ListAutoEntryAddressesByDate(ctx, sqlc.ListAutoEntryAddressesByDateParams{
			CreatedBy:  userID,
			RecordDate: date,
		})
		if err != nil {
			return nil, err
		}
		autoAddrSets[recordDate] = autoAddressSet(rows)
		return autoAddrSets[recordDate], nil
	}

	toDelete := invalidIDs
	var createdDates []string
	parsedDates := make(map[string]pgtype.Date)
	for idx, cluster := range clusters {
		sp := cluster.representative
		if !sp.Lat.Valid || !sp.Lon.Valid {
			slog.ErrorContext(ctx, "auto record invalid averaged coordinates, delete trajectories",
				slog.String("user_id", userID),
				slog.Int("index", idx),
				slog.String("traj_id", sp.ID))
			toDelete = append(toDelete, cluster.ids...)
			continue
		}
		lat, errLat := sp.Lat.Float64Value()
		lon, errLon := sp.Lon.Float64Value()
		if errLat != nil || errLon != nil {
			slog.ErrorContext(ctx, "auto record invalid coordinates, delete trajectories",
				slog.String("user_id", userID),
				slog.Int("index", idx),
				slog.String("traj_id", sp.ID),
				slog.Any("lat_error", errLat),
				slog.Any("lon_error", errLon))
			toDelete = append(toDelete, cluster.ids...)
			continue
		}

		// 先匹配常用地址：命中时直接用其名称生成条目，跳过腾讯逆地理编码
		// （省配额与延迟），且地图服务暂时不可用时常用地址仍可生成条目。
		landmark := ""
		address := ""
		if addr := s.matchCommonAddress(commonAddrs, lat.Float64, lon.Float64); addr != nil {
			landmark = addr.Name
		} else if !location.CheckReverseQuota(ctx, s.rdb, userID) {
			// 每用户日配额已用尽（或 Redis 不可用，fail-closed）：跳过逆地理编码，
			// cluster 留待下轮重试，避免耗尽腾讯地图 key 影响全站交互接口。
			slog.WarnContext(ctx, "auto record reverse geocode quota exhausted, skip cluster",
				slog.String("user_id", userID),
				slog.Int("index", idx),
				slog.String("traj_id", sp.ID))
			continue
		} else {
			var geocodeErr error
			landmark, address, geocodeErr = s.reverseGeocode(ctx, lat.Float64, lon.Float64)
			if geocodeErr != nil {
				s.handleGeocodeRetry(ctx, rows, cluster.ids, userID)
				slog.ErrorContext(ctx, "auto record reverse geocode failed, skip cluster",
					slog.String("user_id", userID),
					slog.Int("index", idx),
					slog.Float64("lat", lat.Float64),
					slog.Float64("lon", lon.Float64),
					slog.String("traj_id", sp.ID),
					slog.Any("error", geocodeErr))
				continue
			}
			// 空地址但返回了 landmark（POI）时降级用 landmark 生成条目，
			// 避免偏远地区/海外坐标在 10 轮重试后被静默删轨迹。
			if address == "" && landmark == "" {
				s.handleGeocodeRetry(ctx, rows, cluster.ids, userID)
				slog.WarnContext(ctx, "auto record reverse geocode returned empty address, skip cluster",
					slog.String("user_id", userID),
					slog.Int("index", idx),
					slog.Float64("lat", lat.Float64),
					slog.Float64("lon", lon.Float64),
					slog.String("traj_id", sp.ID))
				continue
			}
		}

		recordTime := sp.RecordedAt
		recordDate := recordTime.Time.In(timeutil.Shanghai).Format("2006-01-02")

		recordDateTime, ok := parsedDates[recordDate]
		if !ok {
			parsed, err := parseDate(recordDate)
			if err != nil {
				slog.ErrorContext(ctx, "auto record invalid record date", slog.String("user_id", userID), slog.Int("index", idx), slog.String("record_date", recordDate), slog.Any("error", err))
				return fmt.Errorf("parse record date %q: %w", recordDate, err)
			}
			recordDateTime = pgtype.Date{Time: parsed, Valid: true}
			parsedDates[recordDate] = recordDateTime
		}

		addrSet, setErr := autoAddrSetFor(recordDate, recordDateTime)
		if setErr != nil {
			slog.ErrorContext(ctx, "auto record list auto entry addresses failed, skip cluster",
				slog.String("user_id", userID),
				slog.Int("index", idx),
				slog.String("record_date", recordDate),
				slog.Any("error", setErr))
			continue
		}
		if isDuplicateAutoAddress(addrSet, landmark, address) {
			toDelete = append(toDelete, cluster.ids...)
			continue
		}

		var entryID, createdDiaryID string
		err = db.WithTx(ctx, s.pool.Pool(), func(ctx context.Context, q *sqlc.Queries) error {
			diaryID, err := util.NewUUID()
			if err != nil {
				return fmt.Errorf("generate diary id: %w", err)
			}
			diaryID, err = q.UpsertDiary(ctx, sqlc.UpsertDiaryParams{
				ID:         diaryID,
				UserID:     userID,
				RecordDate: recordDateTime,
			})
			if err != nil {
				return fmt.Errorf("upsert diary: %w", err)
			}
			createdDiaryID = diaryID

			newEntryID, err := util.NewUUID()
			if err != nil {
				return fmt.Errorf("generate auto record entry id: %w", err)
			}
			if err := q.CreateAutoRecordEntry(ctx, sqlc.CreateAutoRecordEntryParams{
				ID:            newEntryID,
				DiaryID:       diaryID,
				CreatedBy:     userID,
				Text:          pgtype.Text{String: "（自动记录）", Valid: true},
				Lat:           sp.Lat,
				Lon:           sp.Lon,
				Address:       pgtype.Text{String: landmark, Valid: true},
				DetailAddress: pgtype.Text{String: address, Valid: address != ""},
				RecordTime:    recordTime,
			}); err != nil {
				return fmt.Errorf("create auto record entry: %w", err)
			}
			entryID = newEntryID
			return nil
		})
		if err != nil {
			slog.ErrorContext(ctx, "auto record create entry failed",
				slog.String("user_id", userID),
				slog.Int("index", idx),
				slog.Any("error", err))

			continue
		}

		// 创建成功后把新条目地址并入集合，同日后续 cluster 直接与集合比较
		if landmark != "" {
			addrSet[landmark] = struct{}{}
		}
		if address != "" {
			addrSet[address] = struct{}{}
		}

		createdDates = append(createdDates, recordDate)
		toDelete = append(toDelete, cluster.ids...)

		// 触发新地点提醒（内部按天去重，失败不阻塞主流程）
		if s.push != nil && createdDiaryID != "" && entryID != "" {
			diaryID := createdDiaryID
			eID := entryID
			safe.Go(ctx, nil, func() {
				bgCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				if pushErr := s.push.SendNewPlaceAlert(bgCtx, userID, diaryID, eID); pushErr != nil {
					slog.ErrorContext(bgCtx, "auto record send new place alert failed",
						slog.String("user_id", userID),
						slog.String("diary_id", diaryID),
						slog.String("entry_id", eID),
						slog.Any("error", pushErr))
				}
			})
		}
	}

	if len(createdDates) > 0 && user.CurrentFamilyID.Valid && user.CurrentFamilyID.String != "" {
		asyncFamilyID = user.CurrentFamilyID.String
		asyncDates = uniqueStrings(createdDates)
	}

	// 批量删除已处理/无效/重复的 trajectory，减少 DB 写入次数。
	// 封面刷新参数先赋值；即使 DeleteTrajectories 失败也要触发封面刷新，避免 stale UI，
	// 剩余轨迹留待下一轮处理（AGENTS.md 5.7 AR13）。
	if len(toDelete) > 0 {
		if derr := s.pool.Queries().DeleteTrajectories(ctx, toDelete); derr != nil {
			slog.ErrorContext(ctx, "auto record batch delete trajectories failed",
				slog.String("user_id", userID),
				slog.Int("count", len(toDelete)),
				slog.Any("error", derr))
		}
	}

	// OPS-LOG：自动记录轮次处理结果（用于数据问题复盘）。
	slog.InfoContext(ctx, "auto record round processed",
		slog.String("user_id", userID),
		slog.Int("trajectories", len(rows)),
		slog.Int("clusters", len(clusters)),
		slog.Int("entries_created", len(createdDates)),
		slog.Int("deleted", len(toDelete)),
	)

	return nil
}

type stayPointCluster struct {
	representative sqlc.AutoRecordTrajectory
	ids            []string
}

type clusterBuilder struct {
	representative sqlc.AutoRecordTrajectory
	ids            []string
	latSum         float64
	lonSum         float64
	count          float64
	lastLat        float64
	lastLon        float64
}

func (b *clusterBuilder) finalize() (stayPointCluster, bool) {
	avgLat := b.latSum / b.count
	avgLon := b.lonSum / b.count
	latNum := pgtype.Numeric{}
	if err := latNum.Scan(fmt.Sprintf("%.7f", avgLat)); err != nil {
		return stayPointCluster{ids: b.ids}, false
	}
	lonNum := pgtype.Numeric{}
	if err := lonNum.Scan(fmt.Sprintf("%.7f", avgLon)); err != nil {
		return stayPointCluster{ids: b.ids}, false
	}
	b.representative.Lat = latNum
	b.representative.Lon = lonNum
	return stayPointCluster{representative: b.representative, ids: b.ids}, true
}

func (s *Service) mergeStayPoints(rows []sqlc.AutoRecordTrajectory) ([]stayPointCluster, []string) {
	if len(rows) == 0 {
		return nil, nil
	}

	current := &clusterBuilder{}
	var started bool
	var result []stayPointCluster
	var invalidIDs []string
	for i := range rows {
		lat := trajectoryCoord(rows[i].Lat)
		lon := trajectoryCoord(rows[i].Lon)
		if math.IsNaN(lat) || math.IsNaN(lon) {
			invalidIDs = append(invalidIDs, rows[i].ID)
			continue
		}
		if !started {
			current = &clusterBuilder{
				representative: rows[i],
				ids:            []string{rows[i].ID},
				latSum:         lat,
				lonSum:         lon,
				lastLat:        lat,
				lastLon:        lon,
				count:          1,
			}
			started = true
			continue
		}

		distSq := flatDistanceMetersSq(current.lastLat, current.lastLon, lat, lon)
		gap := rows[i].RecordedAt.Time.Sub(current.representative.RecordedAt.Time)

		if distSq <= stayPointMergeRadiusM*stayPointMergeRadiusM && gap <= stayPointMergeWindow {
			current.ids = append(current.ids, rows[i].ID)
			current.latSum += lat
			current.lonSum += lon
			current.lastLat = lat
			current.lastLon = lon
			current.count++
			if rows[i].RecordedAt.Time.After(current.representative.RecordedAt.Time) {
				current.representative.RecordedAt = rows[i].RecordedAt
			}
			continue
		}

		cluster, ok := current.finalize()
		if !ok {
			invalidIDs = append(invalidIDs, cluster.ids...)
		} else {
			result = append(result, cluster)
		}
		current = &clusterBuilder{
			representative: rows[i],
			ids:            []string{rows[i].ID},
			latSum:         lat,
			lonSum:         lon,
			lastLat:        lat,
			lastLon:        lon,
			count:          1,
		}
	}
	if started {
		cluster, ok := current.finalize()
		if !ok {
			invalidIDs = append(invalidIDs, cluster.ids...)
		} else {
			result = append(result, cluster)
		}
	}
	return result, invalidIDs
}

func trajectoryCoord(n pgtype.Numeric) float64 {
	v, err := n.Float64Value()
	if err != nil || !v.Valid {
		return math.NaN()
	}
	return v.Float64
}

func collectTrajectoryIDs(rows []sqlc.AutoRecordTrajectory) []string {
	ids := make([]string, len(rows))
	for i, r := range rows {
		ids[i] = r.ID
	}
	return ids
}

func uniqueStrings(ss []string) []string {
	seen := make(map[string]struct{}, len(ss))
	var result []string
	for _, s := range ss {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		result = append(result, s)
	}
	return result
}

// FindDuplicateAutoEntry 在当天自动条目地址索引中查找同址条目（R-22：后台与手动即时成文共用同一判重语义）。
// rows 须按 record_time DESC 排序（ListAutoEntryAddressesByDate 保证），首条命中即最新条目；
// 返回已存在条目 id 供手动成文的响应契约使用（命中时直接返回既有条目）。
func FindDuplicateAutoEntry(rows []sqlc.ListAutoEntryAddressesByDateRow, landmark, address string) (entryID string, dup bool) {
	ids := make(map[string]string, len(rows)*2)
	set := make(map[string]struct{}, len(rows)*2)
	for _, r := range rows {
		if r.Address.Valid && r.Address.String != "" {
			if _, ok := set[r.Address.String]; !ok {
				ids[r.Address.String] = r.ID
			}
			set[r.Address.String] = struct{}{}
		}
		if r.DetailAddress.Valid && r.DetailAddress.String != "" {
			if _, ok := set[r.DetailAddress.String]; !ok {
				ids[r.DetailAddress.String] = r.ID
			}
			set[r.DetailAddress.String] = struct{}{}
		}
	}
	if landmark != "" {
		if id, ok := ids[landmark]; ok {
			return id, true
		}
	}
	if address != "" {
		if id, ok := ids[address]; ok {
			return id, true
		}
	}
	return "", false
}

// autoAddressSet 把当天自动条目的地址查询结果折叠成去重集合（address 与 detail_address 的并集，去空）。
func autoAddressSet(rows []sqlc.ListAutoEntryAddressesByDateRow) map[string]struct{} {
	set := make(map[string]struct{}, len(rows)*2)
	for _, r := range rows {
		if r.Address.Valid && r.Address.String != "" {
			set[r.Address.String] = struct{}{}
		}
		if r.DetailAddress.Valid && r.DetailAddress.String != "" {
			set[r.DetailAddress.String] = struct{}{}
		}
	}
	return set
}

// isDuplicateAutoAddress 判断候选驻点是否与当天已有自动条目同址。
// 匹配规则与原「比最后一条」一致（landmark 对 address、详细地址对 detail_address），
// 差别仅在比较范围扩为全天集合（R-20）。
func isDuplicateAutoAddress(set map[string]struct{}, landmark, address string) bool {
	if landmark != "" {
		if _, ok := set[landmark]; ok {
			return true
		}
	}
	if address != "" {
		if _, ok := set[address]; ok {
			return true
		}
	}
	return false
}

func (s *Service) vipInfo(ctx context.Context, userID string) bool {
	row, err := s.pool.Queries().GetUserVIP(ctx, userID)
	if err != nil {
		return false
	}
	return row.ExpireTime.Time.After(time.Now().UTC())
}

func (s *Service) reverseGeocode(ctx context.Context, lat, lon float64) (landmark, address string, err error) {
	res, err := s.loc.Reverse(ctx, lat, lon, true)
	if err != nil {
		return "", "", err
	}
	landmark = res.Landmark
	if landmark == "" {
		landmark = res.Address
	}
	return landmark, res.Address, nil
}

const commonAddressMatchRadiusM = 300.0

func (s *Service) matchCommonAddress(addrs []sqlc.ListUserCommonAddressesRow, lat, lon float64) *sqlc.ListUserCommonAddressesRow {
	for i := range addrs {
		aLat, err1 := addrs[i].Lat.Float64Value()
		aLon, err2 := addrs[i].Lon.Float64Value()
		if err1 != nil || err2 != nil || !aLat.Valid || !aLon.Valid {
			continue
		}
		if flatDistanceMetersSq(lat, lon, aLat.Float64, aLon.Float64) <= commonAddressMatchRadiusM*commonAddressMatchRadiusM {
			return &addrs[i]
		}
	}
	return nil
}

// flatDistanceMetersSq 返回平面距离的平方（米²）。
// 在 300m 聚类半径（stayPointMergeRadiusM）内，平面近似误差远小于 1m，且避免 haversine 的 sin/cos/sqrt 开销。
func flatDistanceMetersSq(lat1, lon1, lat2, lon2 float64) float64 {
	const metersPerDegLat = 111320.0
	avgLat := (lat1 + lat2) * 0.5 * math.Pi / 180
	dLat := lat2 - lat1
	dLon := lon2 - lon1
	dy := dLat * metersPerDegLat
	dx := dLon * metersPerDegLat * math.Cos(avgLat)
	return dx*dx + dy*dy
}

func parseDate(s string) (time.Time, error) {
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid date %q: %w", s, err)
	}
	return t, nil
}

// handleGeocodeRetry 累计逆地理失败次数并从不删除轨迹（PPJ-C03）。R-02：达到 maxGeocodeAttempts
// 的轨迹成为终态，由 SQL（ListTrajectoriesByUser / 候选 EXISTS）排除，不再参与聚类与告警；
// 仅在跨过上限的那一次记录 geocode_discarded，轨迹保留至 7 天清理窗口回收。
func (s *Service) handleGeocodeRetry(ctx context.Context, rows []sqlc.AutoRecordTrajectory, clusterIDs []string, userID string) {
	clusterSet := make(map[string]struct{}, len(clusterIDs))
	for _, id := range clusterIDs {
		clusterSet[id] = struct{}{}
	}
	for _, row := range rows {
		if _, ok := clusterSet[row.ID]; !ok {
			continue
		}
		if row.GeocodeAttempts+1 >= maxGeocodeAttempts {
			slog.WarnContext(ctx, "geocode_discarded",
				slog.String("user_id", userID),
				slog.String("traj_id", row.ID),
				slog.Int("attempts", int(row.GeocodeAttempts+1)))
		}
	}
	if incErr := s.pool.Queries().IncrementTrajectoryGeocodeAttempts(ctx, clusterIDs); incErr != nil {
		slog.WarnContext(ctx, "auto record increment geocode attempts failed", slog.String("user_id", userID), slog.Any("error", incErr))
	}
}
