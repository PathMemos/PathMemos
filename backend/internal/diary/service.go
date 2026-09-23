package diary

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/jpeg"
	_ "image/png"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"errors"
	"papafeiji/backend/internal/autorecord"
	"papafeiji/backend/internal/config"
	"papafeiji/backend/internal/db"
	"papafeiji/backend/internal/db/sqlc"
	"papafeiji/backend/internal/file"
	"papafeiji/backend/internal/location"
	"papafeiji/backend/internal/mcp"
	"papafeiji/backend/internal/pkg/safe"
	"papafeiji/backend/pkg/limiter"
	"papafeiji/backend/pkg/timeutil"
	"papafeiji/backend/pkg/util"
	"papafeiji/backend/pkg/validator"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/redis/go-redis/v9"
)

const dateFormat = "2006-01-02"

func normalizeDate(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", fmt.Errorf("empty date")
	}
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return "", fmt.Errorf("invalid date: %s", s)
	}
	return t.Format(dateFormat), nil
}

var (
	ErrFamilyMismatch            = errors.New("family mismatch")
	ErrMemberNotFound            = errors.New("member not found")
	ErrEntryNotFound             = errors.New("entry not found")
	ErrPermissionDenied          = errors.New("permission denied")
	ErrFileNotFound              = errors.New("file not found")
	ErrNotFileOwner              = errors.New("file does not belong to user")
	ErrFileNotImage              = errors.New("file is not an image")
	ErrMemoryNotFound            = errors.New("memory not found")
	ErrCoverImageNotFromDiary    = errors.New("cover image must be from today's diary")
	ErrRecordTimeCrossDay        = errors.New("record time cannot cross day")
	ErrCoverUpdateInProgress     = errors.New("cover update in progress")
	ErrDailyReverseQuotaExceeded = errors.New("daily reverse geocode quota exceeded")
	// ErrAutoEntryInProgress 表示该用户的自动记录用户锁被后台任务持有，HTTP 入口本次未获取到。
	ErrAutoEntryInProgress = errors.New("auto entry in progress")
)

type NewPlaceAlerter interface {
	SendNewPlaceAlert(ctx context.Context, userID, diaryID, entryID string) error
}

type Service struct {
	pool    *db.Pool
	rdb     *redis.Client
	lock    db.Locker
	storage *file.Storage
	sysCfg  *config.SysConfig

	defaultIcon     string
	loc             *location.Client
	newPlaceAlerter NewPlaceAlerter
}

func NewService(pool *db.Pool, rdb *redis.Client, lock db.Locker, storage *file.Storage, sysCfg *config.SysConfig, tencentMapKeys []string) *Service {
	return &Service{
		pool:        pool,
		rdb:         rdb,
		lock:        lock,
		storage:     storage,
		sysCfg:      sysCfg,
		defaultIcon: sysCfg.DefaultTrajectoryIcon,
		loc:         location.NewClient(tencentMapKeys),
	}
}

func (s *Service) SetNewPlaceAlerter(alerter NewPlaceAlerter) {
	s.newPlaceAlerter = alerter
}

func (s *Service) GetFamilyMembers(ctx context.Context, userID string) (string, []sqlc.ListFamilyMembersRow, error) {
	user, err := s.pool.Queries().GetUserByID(ctx, userID)
	if err != nil {
		return "", nil, fmt.Errorf("get user: %w", err)
	}

	familyID := util.ToString(user.CurrentFamilyID)
	members, err := s.pool.Queries().ListFamilyMembers(ctx, familyID)
	if err != nil {
		return "", nil, fmt.Errorf("list members: %w", err)
	}

	return familyID, members, nil
}

func (s *Service) GetFamilyMemberIDs(ctx context.Context, userID string) ([]string, error) {
	_, members, err := s.GetFamilyMembers(ctx, userID)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(members))
	for _, m := range members {
		ids = append(ids, m.UserID)
	}
	return ids, nil
}

func (s *Service) ValidateFamilyAccess(ctx context.Context, userID, familyID string) error {
	currentFamilyID, _, err := s.GetFamilyMembers(ctx, userID)
	if err != nil {
		return err
	}
	if currentFamilyID != familyID {
		return ErrFamilyMismatch
	}
	return nil
}

func (s *Service) ListInfoCards(ctx context.Context, userID, cursorDate string, size int) ([]map[string]interface{}, string, error) {
	familyID, members, err := s.GetFamilyMembers(ctx, userID)
	if err != nil {
		return nil, "", err
	}

	memberIDs := make([]string, 0, len(members))
	memberMap := make(map[string]sqlc.ListFamilyMembersRow)
	for _, m := range members {
		memberIDs = append(memberIDs, m.UserID)
		memberMap[m.UserID] = m
	}

	recordDateTime, err := parseDate(cursorDate)
	if err != nil {
		return nil, "", err
	}
	rows, err := s.pool.Queries().ListDiaryCards(ctx, sqlc.ListDiaryCardsParams{
		Column1: memberIDs,
		Column2: recordDateTime,
		Limit:   int32(size),
	})
	if err != nil {
		return nil, "", fmt.Errorf("list diary cards: %w", err)
	}

	grouped := groupCardsByDate(rows)
	var dates []string
	for d := range grouped {
		dates = append(dates, d)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(dates)))

	var targetDates []string
	var nextCursor string
	var lastIncludedDate string
	for _, d := range dates {
		targetDates = append(targetDates, d)
		lastIncludedDate = d
	}
	// R2-05：SQL LIMIT 作用在 distinct_dates CTE 上，dates 恒 ≤ size；
	// 取满一页即设置游标（下一页按 record_date < cursor 去重），否则更早记录不可达。
	if len(targetDates) >= size {
		nextCursor = lastIncludedDate
	}

	cards, err := s.buildCardsForDates(ctx, familyID, targetDates, grouped, memberMap, memberIDs, userID)
	if err != nil {
		return nil, "", err
	}

	return cards, nextCursor, nil
}

func (s *Service) buildCardsForDates(ctx context.Context, familyID string, dates []string, grouped map[string][]sqlc.ListDiaryCardsRow, memberMap map[string]sqlc.ListFamilyMembersRow, memberIDs []string, currentUserID string) ([]map[string]interface{}, error) {
	if len(dates) == 0 {
		return nil, nil
	}

	parsedDates := make([]pgtype.Date, 0, len(dates))
	for _, d := range dates {
		t, err := parseDate(d)
		if err != nil {
			return nil, err
		}
		parsedDates = append(parsedDates, t)
	}

	addressRows, err := s.pool.Queries().ListAddressEntriesByDates(ctx, sqlc.ListAddressEntriesByDatesParams{
		Column1: memberIDs,
		Column2: parsedDates,
	})
	if err != nil {
		return nil, fmt.Errorf("list address entries by dates: %w", err)
	}

	addressRecordsByDate := s.groupAddressRecordsByDate(addressRows, memberMap)

	coverRows, err := s.pool.Queries().ListFamilyDailyCovers(ctx, sqlc.ListFamilyDailyCoversParams{
		FamilyID:    familyID,
		RecordDates: parsedDates,
	})
	if err != nil {
		return nil, fmt.Errorf("list family daily covers: %w", err)
	}
	coverURLMap := make(map[string]string, len(coverRows))
	coverImageIDMap := make(map[string]string, len(coverRows))
	hasCoverRow := make(map[string]bool, len(coverRows))
	coverURLFailed := make(map[string]bool, len(coverRows))
	preloadedCovers := make(map[string]sqlc.ListFamilyDailyCoversRow, len(coverRows))
	for _, c := range coverRows {
		d := c.RecordDate.Time.Format(dateFormat)
		hasCoverRow[d] = true
		preloadedCovers[d] = c
		switch c.CoverType {
		case "manual", "image", "trajectory":
			if c.CoverPath.Valid && c.CoverPath.String != "" {
				storageType := c.CoverStorageType.String
				if storageType == "" {
					storageType = "local"
				}
				url, urlErr := s.storage.URL(c.CoverPath.String, storageType)
				if urlErr != nil {
					slog.ErrorContext(ctx, "get cover url failed", slog.String("path", c.CoverPath.String), slog.Any("error", urlErr))
					coverURLMap[d] = s.defaultCoverURL()
					coverURLFailed[d] = true
				} else {
					coverURLMap[d] = url
				}
			} else if c.CoverFileID.Valid && c.CoverFileID.String != "" {
				// 封面行指向一个文件，但文件 path 为空或无效；按 AGENTS.md §3.8 视为底层依赖失效，
				// 需要同步刷新以降级到可用的封面。
				coverURLFailed[d] = true
			}
			if c.CoverFileID.Valid && c.CoverFileID.String != "" {
				coverImageIDMap[d] = c.CoverFileID.String
			}
		case "default":
			coverURLMap[d] = s.defaultCoverURL()
		}
	}

	var cards []map[string]interface{}
	for _, d := range dates {
		if len(grouped[d]) == 0 {
			continue
		}
		coverURL := coverURLMap[d]

		// 读接口不阻塞在轨迹图生成，也不加锁。
		// 无封面行或封面 URL 失效时，先按 manual → image → default 做无锁评估；
		// 评估后异步触发完整刷新，让后端在锁保护下持久化正确的 cover_type（AGENTS.md §3.8）。
		if (coverURL == "" && !hasCoverRow[d]) || coverURLFailed[d] {
			var preloaded *sqlc.ListFamilyDailyCoversRow
			if c, ok := preloadedCovers[d]; ok {
				preloaded = &c
			}
			evalURL, evalFileID, _, evalErr := s.evaluateCoverForDate(ctx, familyID, d, memberIDs, preloaded)
			if evalErr != nil {
				slog.WarnContext(ctx, "evaluate cover for list cards failed", slog.String("family_id", familyID), slog.String("record_date", d), slog.Any("error", evalErr))
			} else {
				coverURL = evalURL
				if evalFileID != "" {
					coverImageIDMap[d] = evalFileID
				}
			}
			s.refreshCoverAsync(familyID, d)
		}
		if coverURL == "" {
			coverURL = s.defaultCoverURL()
		}
		card, err := s.buildCard(ctx, familyID, d, grouped[d], memberMap, currentUserID, addressRecordsByDate[d], coverURL, coverImageIDMap[d])
		if err != nil {
			return nil, err
		}
		cards = append(cards, card)
	}
	return cards, nil
}

// evaluateCoverForDate 在读路径对单日期做无锁的封面优先级评估。
// preloadedCover 为可选参数，当调用方已通过 ListFamilyDailyCovers 批量加载时传入，
// 避免重复 DB 查询。
// 返回值 needTrajectory 表示需要生成轨迹图；调用方应在锁外异步触发完整刷新。
func (s *Service) evaluateCoverForDate(ctx context.Context, familyID, recordDate string, memberIDs []string, preloadedCover *sqlc.ListFamilyDailyCoversRow) (coverURL, coverFileID string, needTrajectory bool, err error) {
	recordDateTime, err := parseDate(recordDate)
	if err != nil {
		return "", "", false, err
	}

	if preloadedCover != nil && preloadedCover.CoverType != "" {
		if url, fileID, ok, isManualStale, evalErr := s.evaluateFromPreloadedCover(ctx, *preloadedCover, memberIDs, recordDateTime); evalErr != nil {
			return "", "", false, evalErr
		} else if ok && !isManualStale {
			return url, fileID, false, nil
		}
		// preloaded 数据不可用（URL 解析失败或手动封面已失效），继续后续评估
	} else {
		cover, err := s.pool.Queries().GetFamilyDailyCover(ctx, sqlc.GetFamilyDailyCoverParams{
			FamilyID:   familyID,
			RecordDate: recordDateTime,
		})
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return "", "", false, fmt.Errorf("get family daily cover: %w", err)
		}
		if err == nil && cover.CoverType != "" {
			isManualStale := false
			if cover.CoverType == "manual" && cover.ManualCoverFileID.Valid && cover.ManualCoverFileID.String != "" {
				used, usedErr := s.pool.Queries().IsImageUsedByFamilyDate(ctx, sqlc.IsImageUsedByFamilyDateParams{
					FileID:  cover.ManualCoverFileID.String,
					Column2: memberIDs,
					Column3: recordDateTime,
				})
				if usedErr != nil {
					return "", "", false, fmt.Errorf("check manual cover usage: %w", usedErr)
				}
				if !used {
					isManualStale = true
				}
			}
			if !isManualStale {
				if url, ok, urlErr := s.coverURLFromRow(cover); ok {
					fileID := ""
					if cover.CoverFileID.Valid {
						fileID = cover.CoverFileID.String
					}
					return url, fileID, false, nil
				} else if urlErr != nil {
					slog.WarnContext(ctx, "manual cover url resolve failed", slog.String("family_id", familyID), slog.String("record_date", recordDate), slog.Any("error", urlErr))
				}
			}
		}
	}

	img, err := s.pool.Queries().GetLatestImageEntry(ctx, sqlc.GetLatestImageEntryParams{
		Column1: memberIDs,
		Column2: recordDateTime,
	})
	if err == nil && img.ImageID != "" {
		if url, urlErr := s.fileURL(img.Path, img.StorageType); urlErr == nil {
			return url, img.ImageID, false, nil
		}
	}

	locs, err := s.pool.Queries().ListLocationEntries(ctx, sqlc.ListLocationEntriesParams{
		Column1: memberIDs,
		Column2: recordDateTime,
	})
	if err == nil && len(locs) > 0 {
		return s.defaultCoverURL(), "", true, nil
	}

	return s.defaultCoverURL(), "", false, nil
}

func (s *Service) groupAddressRecordsByDate(rows []sqlc.ListAddressEntriesByDatesRow, memberMap map[string]sqlc.ListFamilyMembersRow) map[string][]map[string]interface{} {
	grouped := make(map[string]map[string][]string)
	for _, e := range rows {
		d := e.RecordDate.Time.Format(dateFormat)
		if grouped[d] == nil {
			grouped[d] = make(map[string][]string)
		}
		grouped[d][e.CreatedBy] = append(grouped[d][e.CreatedBy], e.Address.String)
	}

	result := make(map[string][]map[string]interface{})
	for d, byUser := range grouped {
		var records []map[string]interface{}
		for userID, addresses := range byUser {
			m, ok := memberMap[userID]
			if !ok {
				continue
			}
			unique := make([]string, 0, len(addresses))
			for _, addr := range addresses {
				short := smartShortenAddress(addr)
				if len(unique) == 0 || unique[len(unique)-1] != short {
					unique = append(unique, short)
				}
			}

			if len(unique) > 10 {
				unique = unique[:10]
			}
			if len(unique) > 0 {
				records = append(records, map[string]interface{}{
					"familyMemberAvatar":        util.AvatarURLOrDefault(m.Avatar, userID, s.sysCfg.DefaultAvatarURL),
					"familyMemberAddressConcat": strings.Join(unique, "—"),
				})
			}
		}
		result[d] = records
	}
	return result
}

func (s *Service) ListInfoCardsByDates(ctx context.Context, userID string, dates []string) ([]map[string]interface{}, error) {
	familyID, members, err := s.GetFamilyMembers(ctx, userID)
	if err != nil {
		return nil, err
	}

	memberIDs := make([]string, 0, len(members))
	memberMap := make(map[string]sqlc.ListFamilyMembersRow)
	for _, m := range members {
		memberIDs = append(memberIDs, m.UserID)
		memberMap[m.UserID] = m
	}

	parsedDates := make([]pgtype.Date, 0, len(dates))
	for _, d := range dates {
		t, err := parseDate(d)
		if err != nil {
			return nil, err
		}
		parsedDates = append(parsedDates, t)
	}
	allRows, err := s.pool.Queries().ListDiaryCardsByDates(ctx, sqlc.ListDiaryCardsByDatesParams{
		Column1: memberIDs,
		Column2: parsedDates,
	})
	if err != nil {
		return nil, fmt.Errorf("list diary cards by dates: %w", err)
	}

	grouped := s.groupCardsByDateFromByDates(allRows)
	return s.buildCardsForDates(ctx, familyID, dates, grouped, memberMap, memberIDs, userID)
}

func (s *Service) GetStats(ctx context.Context, userID string) (map[string]interface{}, error) {
	_, members, err := s.GetFamilyMembers(ctx, userID)
	if err != nil {
		return nil, err
	}

	memberIDs := make([]string, 0, len(members))
	for _, m := range members {
		memberIDs = append(memberIDs, m.UserID)
	}

	now := timeutil.NowShanghai()
	weekday := int(now.Weekday())
	if weekday == 0 {
		weekday = 7
	}
	weekStart := now.AddDate(0, 0, -(weekday - 1)).Format(dateFormat)

	firstRecordDate, err := s.pool.Queries().GetDiaryFirstRecordDate(ctx, memberIDs)
	if err != nil {
		return nil, fmt.Errorf("get diary first record date: %w", err)
	}

	totalEntries, err := s.pool.Queries().CountDiaryEntriesByUsers(ctx, memberIDs)
	if err != nil {
		return nil, fmt.Errorf("count diary entries: %w", err)
	}

	weekStartTime, err := parseDate(weekStart)
	if err != nil {
		return nil, err
	}
	weeklyEntries, err := s.pool.Queries().CountWeeklyDiaryEntriesByUsers(ctx, sqlc.CountWeeklyDiaryEntriesByUsersParams{
		Column1: memberIDs,
		Column2: weekStartTime,
	})
	if err != nil {
		return nil, fmt.Errorf("count weekly diary entries: %w", err)
	}

	days := 0
	firstRecordDateStr := ""
	if !firstRecordDate.Time.IsZero() && firstRecordDate.Time.Year() > 1970 {
		firstRecordDateStr = firstRecordDate.Time.Format(dateFormat)
		days = int(now.Sub(firstRecordDate.Time).Hours()/24) + 1
	}

	return map[string]interface{}{
		"firstRecordDate": firstRecordDateStr,
		"recordDays":      days,
		"totalEntries":    totalEntries,
		"weeklyEntries":   weeklyEntries,
	}, nil
}

func (s *Service) GetDetails(ctx context.Context, userID, familyID, recordDate, memberUserID string, page, size int) (map[string]interface{}, error) {
	currentFamilyID, members, err := s.GetFamilyMembers(ctx, userID)
	if err != nil {
		return nil, err
	}
	if currentFamilyID != familyID {
		return nil, ErrFamilyMismatch
	}

	memberIDs := make([]string, 0, len(members))
	memberMap := make(map[string]sqlc.ListFamilyMembersRow)
	for _, m := range members {
		memberIDs = append(memberIDs, m.UserID)
		memberMap[m.UserID] = m
	}

	var targetUserIDs []string
	switch memberUserID {
	case "", "self":
		if memberUserID == "self" {
			targetUserIDs = []string{userID}
		} else {
			targetUserIDs = memberIDs
		}
	default:
		found := false
		for _, m := range members {
			if m.UserID == memberUserID {
				found = true
				break
			}
		}
		if !found {
			return nil, ErrMemberNotFound
		}
		targetUserIDs = []string{memberUserID}
	}

	// 记忆属于个人，查看指定成员日记时返回该成员的记忆。
	memoryUserID := userID
	if memberUserID != "" && memberUserID != "self" {
		memoryUserID = memberUserID
	}

	recordDateTime, err := parseDate(recordDate)
	if err != nil {
		return nil, err
	}
	entries, err := s.pool.Queries().ListDiaryEntries(ctx, sqlc.ListDiaryEntriesParams{
		Column1: targetUserIDs,
		Column2: recordDateTime,
		Offset:  int32((page - 1) * size),
		Limit:   int32(size),
	})
	if err != nil {
		return nil, fmt.Errorf("list entries: %w", err)
	}

	entryIDs := make([]string, 0, len(entries))
	for _, e := range entries {
		entryIDs = append(entryIDs, e.ID)
	}
	entryImageIDs := make(map[string][]string)
	entryImageRecords := make(map[string]map[string]sqlc.ListDiaryEntryImagePathsByEntryIDsRow)
	if len(entryIDs) > 0 {
		imageRows, err := s.pool.Queries().ListDiaryEntryImagePathsByEntryIDs(ctx, entryIDs)
		if err != nil {
			return nil, fmt.Errorf("list entry image paths: %w", err)
		}
		for _, r := range imageRows {
			entryImageIDs[r.DiaryEntryID] = append(entryImageIDs[r.DiaryEntryID], r.FileID)
			if entryImageRecords[r.DiaryEntryID] == nil {
				entryImageRecords[r.DiaryEntryID] = make(map[string]sqlc.ListDiaryEntryImagePathsByEntryIDsRow)
			}
			entryImageRecords[r.DiaryEntryID][r.FileID] = r
		}
	}

	var data []map[string]interface{}
	for _, e := range entries {
		data = append(data, entryToMap(e, s.storage, memberMap, s.sysCfg.DefaultAvatarURL, entryImageIDs[e.ID], entryImageRecords[e.ID]))
	}

	memRows, memErr := s.pool.Queries().ListMemoriesByUserAndDate(ctx, sqlc.ListMemoriesByUserAndDateParams{
		UserID:  memoryUserID,
		Column2: recordDateTime,
	})
	if memErr != nil {
		slog.Error("list memories for diary detail", slog.Any("error", memErr))
	}
	var memories []map[string]interface{}
	for _, m := range memRows {
		memories = append(memories, memoryToMap(m, memoryUserID, memberMap, s.sysCfg.DefaultAvatarURL))
	}

	coverURL, coverImageID, err := s.ResolveCoverImage(ctx, userID, familyID, recordDate)
	if err != nil {
		coverURL = s.defaultCoverURL()
		coverImageID = ""
	}

	totalCount, err := s.pool.Queries().CountDiaryEntries(ctx, sqlc.CountDiaryEntriesParams{
		Column1: targetUserIDs,
		Column2: recordDateTime,
	})
	if err != nil {
		return nil, fmt.Errorf("count diary entries: %w", err)
	}

	return map[string]interface{}{
		"data":  data,
		"count": totalCount,
		"extra": map[string]interface{}{
			"coverImg":   coverURL,
			"coverImage": coverImageID,
			"memories":   memories,
		},
	}, nil
}

func (s *Service) CreateMemory(ctx context.Context, userID, title, content string, recordTime time.Time) (string, error) {
	id, err := util.NewUUID()
	if err != nil {
		return "", fmt.Errorf("generate memory id: %w", err)
	}
	t := recordTime.In(timeutil.Shanghai)
	recordDate := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	recordDateTime := pgtype.Date{Time: recordDate, Valid: true}
	err = db.WithTx(ctx, s.pool.Pool(), func(ctx context.Context, q *sqlc.Queries) error {
		if _, err := q.CreateMemory(ctx, sqlc.CreateMemoryParams{
			ID:         id,
			UserID:     userID,
			RecordTime: pgtype.Timestamptz{Time: recordTime, Valid: true},
			RecordDate: recordDateTime,
			Title:      title,
			Content:    content,
		}); err != nil {
			return fmt.Errorf("create memory: %w", err)
		}
		if _, err := q.UpsertDiary(ctx, sqlc.UpsertDiaryParams{
			ID:         id,
			UserID:     userID,
			RecordDate: recordDateTime,
		}); err != nil {
			return fmt.Errorf("upsert diary for memory: %w", err)
		}
		return nil
	})
	if err != nil {
		return "", err
	}

	// 记忆新增后异步失效 MCP 查询缓存，失败由 30 分钟 TTL 兜底。
	safe.Go(context.WithoutCancel(ctx), nil, func() {
		mcp.InvalidateUserCache(context.WithoutCancel(ctx), s.rdb, userID)
	})
	return id, nil
}

func (s *Service) UpdateMemory(ctx context.Context, userID, memoryID, title, content string, recordTime time.Time) error {
	t := recordTime.In(timeutil.Shanghai)
	recordDate := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	newDate := pgtype.Date{Time: recordDate, Valid: true}

	// 读取旧记录日期：改期后需清理旧日期的空 diary 行，避免残留幽灵空卡片。
	oldMemory, err := s.pool.Queries().GetMemory(ctx, sqlc.GetMemoryParams{ID: memoryID, UserID: userID})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrMemoryNotFound
		}
		return fmt.Errorf("get memory: %w", err)
	}

	err = db.WithTx(ctx, s.pool.Pool(), func(ctx context.Context, q *sqlc.Queries) error {
		if _, err := q.UpdateMemory(ctx, sqlc.UpdateMemoryParams{
			Title:      title,
			Content:    content,
			RecordTime: pgtype.Timestamptz{Time: recordTime, Valid: true},
			RecordDate: newDate,
			ID:         memoryID,
			UserID:     userID,
		}); err != nil {
			return fmt.Errorf("update memory: %w", err)
		}

		// 确保新日期存在 diary 行（与 CreateMemory 一致），否则改期后该记忆在首页列表不出现卡片。
		if _, err := q.UpsertDiary(ctx, sqlc.UpsertDiaryParams{
			ID:         memoryID,
			UserID:     userID,
			RecordDate: newDate,
		}); err != nil {
			return fmt.Errorf("upsert diary for memory: %w", err)
		}

		// 日期未变则无需清理旧日记。
		if oldMemory.RecordDate.Time.Format("2006-01-02") == recordDate.Format("2006-01-02") {
			return nil
		}

		// 旧日期 diary 若无 entries 且无 memories 则删除，避免幽灵空卡片。
		diary, err := q.GetDiaryByUserAndDate(ctx, sqlc.GetDiaryByUserAndDateParams{
			UserID:     userID,
			RecordDate: oldMemory.RecordDate,
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			return fmt.Errorf("get old diary: %w", err)
		}
		entryCount, err := q.CountDiaryEntriesByDiaryID(ctx, diary.ID)
		if err != nil {
			return fmt.Errorf("count diary entries: %w", err)
		}
		if entryCount > 0 {
			return nil
		}
		memCount, err := q.CountMemoriesByUserAndDate(ctx, sqlc.CountMemoriesByUserAndDateParams{
			UserID:  userID,
			Column2: oldMemory.RecordDate,
		})
		if err != nil {
			return fmt.Errorf("count memories: %w", err)
		}
		if memCount > 0 {
			return nil
		}
		if err := q.DeleteDiaryByID(ctx, diary.ID); err != nil {
			return fmt.Errorf("delete empty old diary: %w", err)
		}
		return nil
	})
	if err != nil {
		return err
	}

	// 记忆更新后异步失效 MCP 查询缓存，失败由 30 分钟 TTL 兜底。
	safe.Go(context.WithoutCancel(ctx), nil, func() {
		mcp.InvalidateUserCache(context.WithoutCancel(ctx), s.rdb, userID)
	})
	return nil
}

func (s *Service) DeleteMemory(ctx context.Context, userID, memoryID string) error {
	memory, err := s.pool.Queries().GetMemory(ctx, sqlc.GetMemoryParams{
		ID:     memoryID,
		UserID: userID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrMemoryNotFound
		}
		return fmt.Errorf("get memory: %w", err)
	}

	err = db.WithTx(ctx, s.pool.Pool(), func(ctx context.Context, q *sqlc.Queries) error {
		rows, err := q.DeleteMemory(ctx, sqlc.DeleteMemoryParams{
			ID:     memoryID,
			UserID: userID,
		})
		if err != nil {
			return fmt.Errorf("delete memory: %w", err)
		}
		if rows == 0 {
			return ErrMemoryNotFound
		}

		diary, err := q.GetDiaryByUserAndDate(ctx, sqlc.GetDiaryByUserAndDateParams{
			UserID:     userID,
			RecordDate: memory.RecordDate,
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			return fmt.Errorf("get diary: %w", err)
		}

		entryCount, err := q.CountDiaryEntriesByDiaryID(ctx, diary.ID)
		if err != nil {
			return fmt.Errorf("count diary entries: %w", err)
		}
		if entryCount > 0 {
			return nil
		}

		memCount, err := q.CountMemoriesByUserAndDate(ctx, sqlc.CountMemoriesByUserAndDateParams{
			UserID:  userID,
			Column2: memory.RecordDate,
		})
		if err != nil {
			return fmt.Errorf("count memories: %w", err)
		}
		if memCount > 0 {
			return nil
		}

		err = q.DeleteDiaryByID(ctx, diary.ID)
		if err != nil {
			return fmt.Errorf("delete empty diary: %w", err)
		}
		return nil
	})
	if err != nil {
		return err
	}

	// 记忆删除后异步失效 MCP 查询缓存，失败由 30 分钟 TTL 兜底。
	safe.Go(context.WithoutCancel(ctx), nil, func() {
		mcp.InvalidateUserCache(context.WithoutCancel(ctx), s.rdb, userID)
	})
	return nil
}

func (s *Service) UpdateCover(ctx context.Context, userID, familyID, recordDate string, coverImage pgtype.Text) (err error) {
	if err := s.ValidateFamilyAccess(ctx, userID, familyID); err != nil {
		return err
	}

	coverLockKey := fmt.Sprintf("lock:covers:%s:%s", familyID, recordDate)
	ok, coverLockToken, err := s.lock.TryLock(ctx, coverLockKey)
	if err != nil {
		return fmt.Errorf("acquire cover lock: %w", err)
	}
	if !ok {
		return ErrCoverUpdateInProgress
	}
	// 封面锁临界区仅为纯 DB 操作（评估降级 + upsert 封面记录），亚秒级完成；轨迹图生成等
	// 外部 IO 已移出锁在释放后异步执行。advisory lock 无 TTL、连接断开自动释放，
	// 不引入续期机制。
	// 必须在锁释放后再启动异步封面刷新，否则 goroutine 会因锁仍被占用而直接失败
	//（02b D4「轨迹图生成等外部 IO 必须在锁释放后执行」）。
	needAsyncRefresh := false
	var trajectoryFilesToDelete []sqlc.FindTrajectoryCoversRow
	defer func() {
		if uerr := s.lock.Unlock(context.WithoutCancel(ctx), coverLockKey, coverLockToken); uerr != nil {
			slog.ErrorContext(ctx, "unlock cover lock failed", slog.String("family_id", familyID), slog.String("record_date", recordDate), slog.Any("error", uerr))
		}
		// 锁释放后再删除物理文件，避免在锁内做外部 IO。
		if len(trajectoryFilesToDelete) > 0 {
			s.deleteTrajectoryCoverFiles(context.WithoutCancel(ctx), trajectoryFilesToDelete, "")
		}
		if needAsyncRefresh && err == nil {
			safe.Go(ctx, nil, func() {
				bgCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
				defer cancel()
				if refreshErr := s.RefreshFamilyDailyCover(bgCtx, familyID, recordDate); refreshErr != nil {
					slog.WarnContext(bgCtx, "clear manual cover: refresh family daily cover failed", slog.String("family_id", familyID), slog.String("record_date", recordDate), slog.Any("error", refreshErr))
				}
			})
		}
	}()

	recordDateTime, err := parseDate(recordDate)
	if err != nil {
		return err
	}
	if coverImage.Valid && coverImage.String != "" {
		f, err := s.pool.Queries().GetFileByID(ctx, coverImage.String)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrFileNotFound
			}
			return fmt.Errorf("get cover file: %w", err)
		}
		if f.FileType != "image" {
			return ErrFileNotImage
		}
		// 校验手动封面图片 URL 可访问，避免设置 stale 封面（AGENTS.md §3.8）。
		if _, urlErr := s.fileURL(f.Path, f.StorageType); urlErr != nil {
			return fmt.Errorf("manual cover url not accessible: %w", urlErr)
		}
		memberIDs, err := s.GetFamilyMemberIDs(ctx, userID)
		if err != nil {
			return err
		}
		ok, err := s.pool.Queries().IsImageUsedByFamilyDate(ctx, sqlc.IsImageUsedByFamilyDateParams{
			FileID:  coverImage.String,
			Column2: memberIDs,
			Column3: recordDateTime,
		})
		if err != nil {
			return fmt.Errorf("check cover image usage: %w", err)
		}
		if !ok {
			return ErrCoverImageNotFromDiary
		}
		if err := s.pool.Queries().SetFamilyDailyManualCover(ctx, sqlc.SetFamilyDailyManualCoverParams{
			FamilyID:    familyID,
			RecordDate:  recordDateTime,
			CoverFileID: coverImage,
		}); err != nil {
			return fmt.Errorf("set manual cover: %w", err)
		}
		// 手动封面已是最高优先级，无需再走完整降级刷新；在锁内只删除 DB 记录并收集待删文件，物理删除在锁释放后执行（AGENTS.md §3.8）。
		if rows, err := s.deleteOldTrajectoryCovers(ctx, familyID, recordDate, ""); err != nil {
			slog.WarnContext(ctx, "set manual cover delete old trajectory failed", slog.String("family_id", familyID), slog.String("record_date", recordDate), slog.Any("error", err))
		} else {
			trajectoryFilesToDelete = append(trajectoryFilesToDelete, rows...)
		}
		s.InvalidateFamilySummary(ctx, familyID)
		return nil
	}

	if err := s.pool.Queries().ClearFamilyDailyManualCover(ctx, sqlc.ClearFamilyDailyManualCoverParams{
		FamilyID:   familyID,
		RecordDate: recordDateTime,
	}); err != nil {
		return fmt.Errorf("clear family daily manual cover: %w", err)
	}

	// 在锁内同步完成轻量封面降级评估，避免用户看到 default 闪烁。
	memberIDs, err := s.GetFamilyMemberIDs(ctx, userID)
	if err != nil {
		return err
	}
	img, err := s.pool.Queries().GetLatestImageEntry(ctx, sqlc.GetLatestImageEntryParams{
		Column1: memberIDs,
		Column2: recordDateTime,
	})
	if err == nil && img.ImageID != "" {
		// 校验图片 URL 可访问，避免降级到 stale 封面（AGENTS.md §3.8）。
		_, urlErr := s.fileURL(img.Path, img.StorageType)
		if urlErr == nil {
			if err := s.pool.Queries().UpsertFamilyDailyCover(ctx, sqlc.UpsertFamilyDailyCoverParams{
				FamilyID:          familyID,
				RecordDate:        recordDateTime,
				CoverFileID:       pgtype.Text{String: img.ImageID, Valid: true},
				CoverType:         "image",
				ManualCoverFileID: pgtype.Text{Valid: false},
			}); err != nil {
				return fmt.Errorf("upsert image cover after clear manual: %w", err)
			}
			// 降级到图片封面后，在锁内只删除 DB 记录并收集待删文件，物理删除在锁释放后执行。
			if rows, err := s.deleteOldTrajectoryCovers(ctx, familyID, recordDate, ""); err != nil {
				slog.WarnContext(ctx, "clear manual cover delete old trajectory failed", slog.String("family_id", familyID), slog.String("record_date", recordDate), slog.Any("error", err))
			} else {
				trajectoryFilesToDelete = append(trajectoryFilesToDelete, rows...)
			}
			s.InvalidateFamilySummary(ctx, familyID)
			return nil
		}
		slog.WarnContext(ctx, "latest image cover url not accessible, will downgrade",
			slog.String("family_id", familyID),
			slog.String("record_date", recordDate),
			slog.String("cover_file_id", img.ImageID),
			slog.Any("error", urlErr))
	}
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("get latest image entry after clear manual: %w", err)
	}

	// 没有图片封面时，检查是否存在定位点，完成轻量降级评估（image → trajectory → default）。
	// 若存在定位点则先设为 default 保证首页不白屏，并在锁释放后异步生成轨迹图；
	// 否则直接设为 default。此过程中用户可能短暂看到 default 封面，符合 AGENTS.md §3.8
	// 「轨迹图异步/非阻塞」及「接受极短暂降级窗口」的容忍声明。
	locs, locErr := s.pool.Queries().ListLocationEntries(ctx, sqlc.ListLocationEntriesParams{
		Column1: memberIDs,
		Column2: recordDateTime,
	})
	if locErr != nil && !errors.Is(locErr, pgx.ErrNoRows) {
		return fmt.Errorf("list location entries after clear manual: %w", locErr)
	}
	if err := s.pool.Queries().UpsertFamilyDailyCover(ctx, sqlc.UpsertFamilyDailyCoverParams{
		FamilyID:          familyID,
		RecordDate:        recordDateTime,
		CoverFileID:       pgtype.Text{Valid: false},
		CoverType:         "default",
		ManualCoverFileID: pgtype.Text{Valid: false},
	}); err != nil {
		return fmt.Errorf("upsert default cover after clear manual: %w", err)
	}
	s.InvalidateFamilySummary(ctx, familyID)

	// 存在定位点时，在锁释放后异步触发完整刷新以生成轨迹图。
	if len(locs) > 0 {
		needAsyncRefresh = true
	}
	return nil
}

func (s *Service) DeleteDiary(ctx context.Context, userID, familyID, recordDate string) error {
	if err := s.ValidateFamilyAccess(ctx, userID, familyID); err != nil {
		return err
	}

	recordDateTime, err := parseDate(recordDate)
	if err != nil {
		return err
	}
	diaryID, err := s.pool.Queries().GetDiaryByUserAndDate(ctx, sqlc.GetDiaryByUserAndDateParams{
		UserID:     userID,
		RecordDate: recordDateTime,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return fmt.Errorf("get diary by user and date: %w", err)
	}

	var fileIDs []string
	if err := db.WithTx(ctx, s.pool.Pool(), func(ctx context.Context, q *sqlc.Queries) error {
		var err error
		// 将当日 memories 一并清除，与 entries 保持一致：日记卡片删除即该日所有数据删除。
		if _, err := q.DeleteMemoriesByUserAndDate(ctx, sqlc.DeleteMemoriesByUserAndDateParams{
			UserID:  userID,
			Column2: recordDateTime,
		}); err != nil {
			return fmt.Errorf("delete memories: %w", err)
		}
		// 在事务内删除图片关联并返回 file_id，再删除日记。
		// 注意：当前 SQL 未加 SELECT ... FOR UPDATE 行锁，并发新增的图片条目可能在删除后成为孤儿；
		// 孤儿文件由后台孤儿清理任务兜底（DA-P2-01：原注释误称有行锁，已按实现更正）。
		fileIDs, err = q.DeleteDiaryImagesReturningFileIDs(ctx, diaryID.ID)
		if err != nil {
			return fmt.Errorf("delete diary images: %w", err)
		}
		if len(fileIDs) > 0 {
			if err := q.ClearFamilyDailyManualCoverByFileIDs(ctx, sqlc.ClearFamilyDailyManualCoverByFileIDsParams{
				FamilyID:   familyID,
				RecordDate: recordDateTime,
				Column3:    fileIDs,
			}); err != nil {
				return fmt.Errorf("clear manual covers by file ids: %w", err)
			}
			if err := q.ClearFamilyDailyImageCoverByFileIDs(ctx, sqlc.ClearFamilyDailyImageCoverByFileIDsParams{
				FamilyID:   familyID,
				RecordDate: recordDateTime,
				Column3:    fileIDs,
			}); err != nil {
				return fmt.Errorf("clear image covers by file ids: %w", err)
			}
		}
		if err := q.DeleteDiaryByID(ctx, diaryID.ID); err != nil {
			return fmt.Errorf("delete diary: %w", err)
		}
		return nil
	}); err != nil {
		return err
	}

	// 日记删除后异步失效 MCP 查询缓存，失败由 30 分钟 TTL 兜底。
	safe.Go(context.WithoutCancel(ctx), nil, func() {
		mcp.InvalidateUserCache(context.WithoutCancel(ctx), s.rdb, userID)
	})

	// 物理文件删除在 DB 提交后异步执行，避免条目较多时拖慢接口响应；
	// 删除失败仅记录日志，由后台孤儿文件回收任务兜底（AGENTS.md §4.4）。
	if len(fileIDs) > 0 {
		safe.Go(ctx, nil, func() {
			bgCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
			defer cancel()
			for _, fid := range fileIDs {
				if err := file.DeletePhysicalIfUnreferenced(bgCtx, s.pool, s.storage, fid); err != nil {
					slog.ErrorContext(bgCtx, "delete orphan file failed", slog.String("file_id", fid), slog.Any("error", err))
				}
			}
		})
	}

	if familyID != "" {
		safe.Go(ctx, nil, func() {
			bgCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			defer cancel()
			if err := s.RefreshFamilyDailyCover(bgCtx, familyID, recordDate); err != nil {
				slog.WarnContext(bgCtx, "delete diary refresh cover failed", slog.String("family_id", familyID), slog.String("record_date", recordDate), slog.Any("error", err))
			}
		})
	}

	// OPS-LOG：整日删除审计日志（用于数据问题复盘）。
	slog.InfoContext(ctx, "diary deleted",
		slog.String("user_id", userID),
		slog.String("family_id", familyID),
		slog.String("record_date", recordDate),
	)
	return nil
}

func (s *Service) CreateEntry(ctx context.Context, userID string, req *entryRequest) (entryID, familyID, recordDate string, card map[string]interface{}, err error) {
	familyID, _, err = s.GetFamilyMembers(ctx, userID)
	if err != nil {
		return "", "", "", nil, err
	}

	recordDate, recordTime, err := resolveRecordDateAndTime(req)
	if err != nil {
		return "", "", "", nil, err
	}

	imageIDs, err := s.validateImageFiles(ctx, req.ImageIDs, userID)
	if err != nil {
		return "", "", "", nil, err
	}

	recordDateTime, err := parseDate(recordDate)
	if err != nil {
		return "", "", "", nil, err
	}

	diaryID, err := util.NewUUID()
	if err != nil {
		return "", "", "", nil, fmt.Errorf("generate diary id: %w", err)
	}
	newEntryID, err := util.NewUUID()
	if err != nil {
		return "", "", "", nil, fmt.Errorf("generate entry id: %w", err)
	}
	err = db.WithTx(ctx, s.pool.Pool(), func(ctx context.Context, q *sqlc.Queries) error {

		diaryID, err = q.UpsertDiary(ctx, sqlc.UpsertDiaryParams{
			ID:         diaryID,
			UserID:     userID,
			RecordDate: recordDateTime,
		})
		if err != nil {
			return fmt.Errorf("upsert diary: %w", err)
		}

		createParams, err := buildCreateEntryParams(newEntryID, diaryID, userID, req, recordTime)
		if err != nil {
			return fmt.Errorf("build create entry params: %w", err)
		}
		_, err = q.CreateDiaryEntry(ctx, createParams)
		if err != nil {
			return fmt.Errorf("create entry: %w", err)
		}

		if len(imageIDs) > 0 {
			if err := s.createDiaryEntryImages(ctx, q, newEntryID, imageIDs); err != nil {
				return fmt.Errorf("create diary entry images: %w", err)
			}
		}

		if err := q.TouchDiaryUpdatedAt(ctx, diaryID); err != nil {
			return fmt.Errorf("touch diary updated at: %w", err)
		}
		return nil
	})
	if err != nil {
		return "", "", "", nil, err
	}

	// OPS-LOG：手动创建条目审计日志（用于数据问题复盘）。
	slog.InfoContext(ctx, "diary entry created",
		slog.String("user_id", userID),
		slog.String("entry_id", newEntryID),
		slog.String("record_date", recordDate),
		slog.String("source", "manual"),
	)

	// 日记新增后异步失效 MCP 查询缓存，失败由 30 分钟 TTL 兜底。
	safe.Go(context.WithoutCancel(ctx), nil, func() {
		mcp.InvalidateUserCache(context.WithoutCancel(ctx), s.rdb, userID)
	})

	// 创建接口需要返回带最新封面的 card；同步完成封面评估（纯 DB，～50ms），
	// 轨迹图生成在锁释放后异步执行，不阻塞响应。
	if familyID != "" {
		if refreshErr := s.RefreshFamilyDailyCover(ctx, familyID, recordDate); refreshErr != nil {
			slog.WarnContext(ctx, "refresh cover after create entry failed",
				slog.String("family_id", familyID),
				slog.String("record_date", recordDate),
				slog.Any("error", refreshErr))
		}
	}
	cards, cardErr := s.ListInfoCardsByDates(ctx, userID, []string{recordDate})
	if cardErr != nil {
		slog.WarnContext(ctx, "list info card after create entry failed",
			slog.String("family_id", familyID),
			slog.String("record_date", recordDate),
			slog.Any("error", cardErr))
		coverURL := s.defaultCoverURL()
		if resolvedURL, _, resolveErr := s.resolveFamilyDailyCover(ctx, familyID, recordDate); resolveErr == nil && resolvedURL != "" {
			coverURL = resolvedURL
		}
		card = map[string]interface{}{
			"id":         makeVirtualID(familyID, recordDate),
			"recordDate": recordDate,
			"coverImg":   coverURL,
		}
		return newEntryID, familyID, recordDate, card, nil
	}
	if len(cards) > 0 {
		card = cards[0]
	}
	return newEntryID, familyID, recordDate, card, nil
}

func (s *Service) UpdateEntry(ctx context.Context, userID string, req *entryRequest) (familyID, recordDate string, card map[string]interface{}, err error) {
	entry, err := s.pool.Queries().GetDiaryEntry(ctx, req.ID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", "", nil, ErrEntryNotFound
		}
		return "", "", nil, fmt.Errorf("get diary entry: %w", err)
	}

	diary, err := s.pool.Queries().GetDiaryByID(ctx, entry.DiaryID)
	if err != nil {
		return "", "", nil, fmt.Errorf("get diary: %w", err)
	}
	if diary.UserID != userID {
		return "", "", nil, ErrPermissionDenied
	}
	recordDate = diary.RecordDate.Time.Format(dateFormat)

	familyID, _, err = s.GetFamilyMembers(ctx, userID)
	if err != nil {
		return "", "", nil, err
	}

	imageIDs, err := s.validateImageFiles(ctx, req.ImageIDs, userID)
	if err != nil {
		return "", "", nil, err
	}

	oldImageFileIDs, err := s.pool.Queries().ListDiaryEntryImages(ctx, entry.ID)
	if err != nil {
		return "", "", nil, fmt.Errorf("list entry images: %w", err)
	}
	updateParams, err := mergeUpdateEntryParams(entry, req)
	if err != nil {
		return "", "", nil, err
	}
	removedImageIDs := make([]string, 0, len(oldImageFileIDs))
	newIDSet := make(map[string]struct{}, len(imageIDs))
	for _, id := range imageIDs {
		newIDSet[id.String] = struct{}{}
	}
	for _, fid := range oldImageFileIDs {
		if _, kept := newIDSet[fid]; !kept {
			removedImageIDs = append(removedImageIDs, fid)
		}
	}
	if req.RecordTime != nil && *req.RecordTime != "" && updateParams.RecordTime.Valid {
		newDate := updateParams.RecordTime.Time.In(timeutil.Shanghai).Format(dateFormat)
		if newDate != diary.RecordDate.Time.Format(dateFormat) {
			return "", "", nil, ErrRecordTimeCrossDay
		}
	}
	err = db.WithTx(ctx, s.pool.Pool(), func(ctx context.Context, q *sqlc.Queries) error {
		rowsAffected, err := q.UpdateDiaryEntry(ctx, updateParams)
		if err != nil {
			return fmt.Errorf("update diary entry: %w", err)
		}
		if rowsAffected == 0 {
			return ErrEntryNotFound
		}
		if err := q.DeleteDiaryEntryImages(ctx, entry.ID); err != nil {
			return fmt.Errorf("delete old entry images: %w", err)
		}
		if len(imageIDs) > 0 {
			if err := s.createDiaryEntryImages(ctx, q, entry.ID, imageIDs); err != nil {
				return fmt.Errorf("create new entry images: %w", err)
			}
		}
		if len(removedImageIDs) > 0 {
			if err := q.ClearFamilyDailyManualCoverByFileIDs(ctx, sqlc.ClearFamilyDailyManualCoverByFileIDsParams{
				FamilyID:   familyID,
				RecordDate: diary.RecordDate,
				Column3:    removedImageIDs,
			}); err != nil {
				return fmt.Errorf("clear manual covers by file ids: %w", err)
			}
			if err := q.ClearFamilyDailyImageCoverByFileIDs(ctx, sqlc.ClearFamilyDailyImageCoverByFileIDsParams{
				FamilyID:   familyID,
				RecordDate: diary.RecordDate,
				Column3:    removedImageIDs,
			}); err != nil {
				return fmt.Errorf("clear image covers by file ids: %w", err)
			}
		}
		if err := q.TouchDiaryUpdatedAt(ctx, entry.DiaryID); err != nil {
			return fmt.Errorf("touch diary updated at: %w", err)
		}
		return nil
	})
	if err != nil {
		return "", "", nil, err
	}

	// 日记更新后异步失效 MCP 查询缓存，失败由 30 分钟 TTL 兜底。
	safe.Go(context.WithoutCancel(ctx), nil, func() {
		mcp.InvalidateUserCache(context.WithoutCancel(ctx), s.rdb, userID)
	})

	// 更新接口需要返回带最新封面的 card；同步完成封面评估（纯 DB，～50ms），
	// 轨迹图生成在锁释放后异步执行，不阻塞响应。
	if familyID != "" {
		if refreshErr := s.RefreshFamilyDailyCover(ctx, familyID, recordDate); refreshErr != nil {
			slog.WarnContext(ctx, "refresh cover after update entry failed",
				slog.String("family_id", familyID),
				slog.String("record_date", recordDate),
				slog.Any("error", refreshErr))
		}
	}
	// 旧图片文件解引用与物理删除在 DB 提交后异步执行，避免拖慢接口响应；
	// 删除失败仅记录日志，由后台孤儿文件回收任务兜底（AGENTS.md §4.4）。
	if len(removedImageIDs) > 0 {
		safe.Go(ctx, nil, func() {
			bgCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
			defer cancel()
			for _, fid := range removedImageIDs {
				if err := s.deleteIfOnlySelfReferenced(bgCtx, fid, req.ID); err != nil {
					slog.ErrorContext(bgCtx, "delete old image failed", slog.String("file_id", fid), slog.Any("error", err))
				}
			}
		})
	}
	// OPS-LOG：更新条目审计日志（用于数据问题复盘）。
	slog.InfoContext(ctx, "diary entry updated",
		slog.String("user_id", userID),
		slog.String("entry_id", req.ID),
		slog.String("record_date", recordDate),
	)

	cards, cardErr := s.ListInfoCardsByDates(ctx, userID, []string{recordDate})
	if cardErr != nil {
		slog.WarnContext(ctx, "list info card after update entry failed",
			slog.String("family_id", familyID),
			slog.String("record_date", recordDate),
			slog.Any("error", cardErr))
		coverURL := s.defaultCoverURL()
		if resolvedURL, _, resolveErr := s.resolveFamilyDailyCover(ctx, familyID, recordDate); resolveErr == nil && resolvedURL != "" {
			coverURL = resolvedURL
		}
		card = map[string]interface{}{
			"id":         makeVirtualID(familyID, recordDate),
			"recordDate": recordDate,
			"coverImg":   coverURL,
		}
		return familyID, recordDate, card, nil
	}
	if len(cards) > 0 {
		card = cards[0]
	}
	return familyID, recordDate, card, nil
}

func (s *Service) DeleteEntry(ctx context.Context, userID, entryID string) (familyID, recordDate string, err error) {
	entry, err := s.pool.Queries().GetDiaryEntry(ctx, entryID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", "", ErrEntryNotFound
		}
		return "", "", fmt.Errorf("get diary entry: %w", err)
	}

	diary, err := s.pool.Queries().GetDiaryByID(ctx, entry.DiaryID)
	if err != nil {
		return "", "", fmt.Errorf("get diary: %w", err)
	}
	if diary.UserID != userID {
		return "", "", ErrPermissionDenied
	}
	recordDate = diary.RecordDate.Time.Format(dateFormat)

	familyID, _, err = s.GetFamilyMembers(ctx, userID)
	if err != nil {
		return "", "", err
	}

	var oldImageFileIDs []string
	err = db.WithTx(ctx, s.pool.Pool(), func(ctx context.Context, q *sqlc.Queries) error {
		var err error
		// 在事务内删除图片关联并返回 file_id，再删除条目。
		// 注意：当前 SQL 未加 SELECT ... FOR UPDATE 行锁，并发新增的图片可能在删除后成为孤儿；
		// 孤儿文件由后台孤儿清理任务兜底（DA-P2-01：原注释误称有行锁，已按实现更正）。
		oldImageFileIDs, err = q.DeleteDiaryEntryImagesReturningFileIDs(ctx, entryID)
		if err != nil {
			return fmt.Errorf("delete diary entry images: %w", err)
		}
		if len(oldImageFileIDs) > 0 {
			if err := q.ClearFamilyDailyManualCoverByFileIDs(ctx, sqlc.ClearFamilyDailyManualCoverByFileIDsParams{
				FamilyID:   familyID,
				RecordDate: diary.RecordDate,
				Column3:    oldImageFileIDs,
			}); err != nil {
				return fmt.Errorf("clear manual covers by file ids: %w", err)
			}
			if err := q.ClearFamilyDailyImageCoverByFileIDs(ctx, sqlc.ClearFamilyDailyImageCoverByFileIDsParams{
				FamilyID:   familyID,
				RecordDate: diary.RecordDate,
				Column3:    oldImageFileIDs,
			}); err != nil {
				return fmt.Errorf("clear image covers by file ids: %w", err)
			}
		}
		if err := q.DeleteDiaryEntryByID(ctx, entryID); err != nil {
			return fmt.Errorf("delete diary entry: %w", err)
		}
		if err := q.TouchDiaryUpdatedAt(ctx, entry.DiaryID); err != nil {
			return fmt.Errorf("touch diary updated at: %w", err)
		}
		entryCount, err := q.CountDiaryEntriesByDiaryID(ctx, entry.DiaryID)
		if err != nil {
			return fmt.Errorf("count diary entries: %w", err)
		}
		if entryCount > 0 {
			return nil
		}
		memCount, err := q.CountMemoriesByUserAndDate(ctx, sqlc.CountMemoriesByUserAndDateParams{
			UserID:  diary.UserID,
			Column2: diary.RecordDate,
		})
		if err != nil {
			return fmt.Errorf("count memories: %w", err)
		}
		if memCount > 0 {
			return nil
		}
		err = q.DeleteDiaryByID(ctx, entry.DiaryID)
		if err != nil {
			return fmt.Errorf("delete empty diary: %w", err)
		}
		return nil
	})
	if err != nil {
		return "", "", err
	}

	// MCP 查询缓存含日记条目与回忆，删除后必须失效，避免 30 分钟 TTL 内 MCP 端仍能读到已删条目。
	mcp.InvalidateUserCache(context.WithoutCancel(ctx), s.rdb, userID)

	safe.Go(ctx, nil, func() {
		bgCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		if err := s.RefreshFamilyDailyCover(bgCtx, familyID, recordDate); err != nil {
			slog.WarnContext(bgCtx, "delete entry refresh cover failed", slog.String("family_id", familyID), slog.String("record_date", recordDate), slog.Any("error", err))
		}
	})
	// 物理文件删除在 DB 提交后异步执行，避免拖慢接口响应；
	// 删除失败仅记录日志，由后台孤儿文件回收任务兜底（AGENTS.md §4.4）。
	if len(oldImageFileIDs) > 0 {
		safe.Go(ctx, nil, func() {
			bgCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
			defer cancel()
			for _, fid := range oldImageFileIDs {
				if err := s.deleteIfOnlySelfReferenced(bgCtx, fid, entryID); err != nil {
					slog.ErrorContext(bgCtx, "delete old image failed", slog.String("file_id", fid), slog.Any("error", err))
				}
			}
		})
	}

	// OPS-LOG：删除条目审计日志（用于数据问题复盘）。
	slog.InfoContext(ctx, "diary entry deleted",
		slog.String("user_id", userID),
		slog.String("entry_id", entryID),
		slog.String("record_date", recordDate),
	)
	return familyID, recordDate, nil
}

// acquireAutoEntryLock 以有限重试获取自动记录用户锁（与后台 autorecord.processUser 同键）。
// 返回空 token 表示锁被占用（后台正在处理该用户）；有限重试约 2s，避免用户请求长时间挂起。
func (s *Service) acquireAutoEntryLock(ctx context.Context, key string) (string, error) {
	for i := 0; i < 10; i++ {
		ok, token, err := s.lock.TryLock(ctx, key)
		if err != nil {
			return "", err
		}
		if ok {
			return token, nil
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
	return "", nil
}

func (s *Service) CreateAutoEntry(ctx context.Context, userID string, lat, lon float64) (entryID, familyID, recordDate string, err error) {
	if err := validator.ValidateCoordinates(lat, lon); err != nil {
		return "", "", "", fmt.Errorf("invalid coordinates: %w", err)
	}

	// R2-06：按用户日配额限制逆地理编码调用，防止恶意坐标耗尽腾讯地图配额影响正常用户。
	if !location.CheckReverseQuota(ctx, s.rdb, userID) {
		return "", "", "", ErrDailyReverseQuotaExceeded
	}

	landmark, address, err := s.reverseGeocode(ctx, lat, lon)
	if err != nil {
		return "", "", "", fmt.Errorf("reverse geocode failed: %w", err)
	}

	recordDate = todayShanghai()
	recordDateTime, err := parseDate(recordDate)
	if err != nil {
		return "", "", "", err
	}
	now := timeutil.NowShanghai()

	familyID, _, err = s.GetFamilyMembers(ctx, userID)
	if err != nil {
		return "", "", "", err
	}

	// DA-P1-04：与后台 autorecord.processUser 共用 lock:auto_record:{userID}，
	// 避免 HTTP 入口与后台任务并发对同一驻留点各生成一条自动条目。
	if s.lock != nil {
		lockKey := "lock:auto_record:" + userID
		lockToken, lockErr := s.acquireAutoEntryLock(ctx, lockKey)
		if lockErr != nil {
			return "", "", "", fmt.Errorf("acquire auto entry lock: %w", lockErr)
		}
		if lockToken == "" {
			return "", "", "", ErrAutoEntryInProgress
		}
		defer func() {
			if uerr := s.lock.Unlock(context.WithoutCancel(ctx), lockKey, lockToken); uerr != nil {
				slog.ErrorContext(ctx, "auto entry unlock failed", slog.String("user_id", userID), slog.Any("error", uerr))
			}
		}()
	}

	// R-22：与后台 autorecord 共用同日地址并集判重（可识别同日折返旧地点），
	// 命中时直接返回已存在条目 id（响应契约不变）。
	addrRows, err := s.pool.Queries().ListAutoEntryAddressesByDate(ctx, sqlc.ListAutoEntryAddressesByDateParams{
		CreatedBy:  userID,
		RecordDate: recordDateTime,
	})
	if err != nil {
		return "", "", "", fmt.Errorf("list auto entry addresses: %w", err)
	}
	lastID, dup := autorecord.FindDuplicateAutoEntry(addrRows, landmark, address)
	if dup {
		slog.DebugContext(ctx, "auto entry same as last auto entry, skip create", slog.String("user_id", userID), slog.String("landmark", landmark))
		return lastID, familyID, recordDate, nil
	}

	diaryID, err := util.NewUUID()
	if err != nil {
		return "", "", "", fmt.Errorf("generate diary id: %w", err)
	}
	newEntryID, err := util.NewUUID()
	if err != nil {
		return "", "", "", fmt.Errorf("generate entry id: %w", err)
	}
	err = db.WithTx(ctx, s.pool.Pool(), func(ctx context.Context, q *sqlc.Queries) error {

		diaryID, err = q.UpsertDiary(ctx, sqlc.UpsertDiaryParams{
			ID:         diaryID,
			UserID:     userID,
			RecordDate: recordDateTime,
		})
		if err != nil {
			return fmt.Errorf("upsert diary: %w", err)
		}

		latNum := pgtype.Numeric{}
		if err := latNum.Scan(fmt.Sprintf("%.4f", lat)); err != nil {
			return fmt.Errorf("invalid lat: %w", err)
		}
		lonNum := pgtype.Numeric{}
		if err := lonNum.Scan(fmt.Sprintf("%.4f", lon)); err != nil {
			return fmt.Errorf("invalid lon: %w", err)
		}

		if _, err := q.CreateDiaryEntry(ctx, sqlc.CreateDiaryEntryParams{
			ID:            newEntryID,
			DiaryID:       diaryID,
			CreatedBy:     userID,
			Text:          pgtype.Text{String: "（自动记录）", Valid: true},
			Lat:           latNum,
			Lon:           lonNum,
			Address:       pgtype.Text{String: landmark, Valid: true},
			DetailAddress: pgtype.Text{String: address, Valid: address != ""},
			RecordTime:    pgtype.Timestamptz{Time: now, Valid: true},
		}); err != nil {
			return fmt.Errorf("create diary entry: %w", err)
		}
		return nil
	})
	if err != nil {
		return "", "", "", err
	}

	// 自动成文同样影响 MCP 日记/回忆查询，失效缓存避免 MCP 端看不到刚生成的记录。
	mcp.InvalidateUserCache(context.WithoutCancel(ctx), s.rdb, userID)

	// OPS-LOG：自动记录条目创建审计日志（用于数据问题复盘）。
	slog.InfoContext(ctx, "diary auto entry created",
		slog.String("user_id", userID),
		slog.String("entry_id", newEntryID),
		slog.String("record_date", recordDate),
		slog.Float64("lat", lat),
		slog.Float64("lon", lon),
	)

	s.refreshCoverAsync(familyID, recordDate)
	if s.newPlaceAlerter != nil {
		safe.Go(ctx, nil, func() {
			bgCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			defer cancel()
			if pushErr := s.newPlaceAlerter.SendNewPlaceAlert(bgCtx, userID, diaryID, newEntryID); pushErr != nil {
				slog.ErrorContext(bgCtx, "auto entry send new place alert failed",
					slog.String("user_id", userID),
					slog.String("diary_id", diaryID),
					slog.String("entry_id", newEntryID),
					slog.Any("error", pushErr))
			}
		})
	}
	return newEntryID, familyID, recordDate, nil
}

func (s *Service) reverseGeocode(ctx context.Context, lat, lon float64) (landmark, address string, err error) {
	if s.loc == nil {
		return "", "", fmt.Errorf("location client not initialized")
	}
	res, err := s.loc.Reverse(ctx, lat, lon, true)
	if err != nil {
		return "", "", err
	}
	landmark, address, ok := pickGeocodeResult(res)
	if !ok {
		// DA-P2-03：只有 POI 与地址都为空才是真正的上游空结果；地址空但 POI 非空时
		// 仍按 landmark 成文（与后台 autorecord 路径一致），避免首次即时成文 500。
		return "", "", fmt.Errorf("reverse geocode returned empty landmark and address")
	}
	return landmark, address, nil
}

// pickGeocodeResult 从逆地理结果中挑出用于成文的 landmark 与 address：
// landmark 优先 POI，回退地址；两者都空返回 ok=false。纯函数便于单测。
func pickGeocodeResult(res *location.ReverseResult) (landmark, address string, ok bool) {
	landmark = res.Landmark
	if landmark == "" {
		landmark = res.Address
	}
	return landmark, res.Address, landmark != ""
}

func (s *Service) validateImageFiles(ctx context.Context, fileIDs []string, userID string) ([]pgtype.Text, error) {
	if len(fileIDs) == 0 {
		return nil, nil
	}

	files, err := s.pool.Queries().GetFilesByIDs(ctx, fileIDs)
	if err != nil {
		return nil, fmt.Errorf("get files: %w", err)
	}
	fileMap := make(map[string]sqlc.File, len(files))
	for _, f := range files {
		fileMap[f.ID] = f
	}

	seen := make(map[string]struct{}, len(fileIDs))
	result := make([]pgtype.Text, 0, len(fileIDs))
	for _, fid := range fileIDs {
		if fid == "" {
			continue
		}
		if _, ok := seen[fid]; ok {
			continue
		}
		seen[fid] = struct{}{}

		f, ok := fileMap[fid]
		if !ok {
			return nil, ErrFileNotFound
		}
		if f.FileType != "image" {
			return nil, ErrFileNotImage
		}
		if !f.CreatedBy.Valid || f.CreatedBy.String == "" || f.CreatedBy.String != userID {
			return nil, ErrNotFileOwner
		}
		result = append(result, pgtype.Text{String: fid, Valid: true})
	}
	return result, nil
}

func (s *Service) createDiaryEntryImages(ctx context.Context, q *sqlc.Queries, entryID string, imageIDs []pgtype.Text) error {
	if len(imageIDs) == 0 {
		return nil
	}
	ids := make([]string, 0, len(imageIDs))
	fileIDs := make([]string, 0, len(imageIDs))
	sortOrders := make([]int32, 0, len(imageIDs))
	for i, id := range imageIDs {
		imageID, err := util.NewUUID()
		if err != nil {
			return fmt.Errorf("generate entry image id: %w", err)
		}
		ids = append(ids, imageID)
		fileIDs = append(fileIDs, id.String)
		sortOrders = append(sortOrders, int32(i))
	}
	if err := q.BatchCreateDiaryEntryImages(ctx, sqlc.BatchCreateDiaryEntryImagesParams{
		Ids:          ids,
		DiaryEntryID: entryID,
		FileIds:      fileIDs,
		SortOrders:   sortOrders,
	}); err != nil {
		return fmt.Errorf("batch create entry images: %w", err)
	}
	return nil
}

func (s *Service) deleteIfOnlySelfReferenced(ctx context.Context, fileID, selfEntryID string) error {
	var filePath, storageType string
	err := db.WithTx(ctx, s.pool.Pool(), func(ctx context.Context, q *sqlc.Queries) error {
		refs, err := q.GetFileReferences(ctx, pgtype.Text{String: fileID, Valid: true})
		if err != nil {
			return fmt.Errorf("get file references: %w", err)
		}
		if refs.UsedByCover || refs.AvatarUserCount > 0 {
			return nil
		}
		if refs.UsedByEntry {

			rows, err := q.GetDiaryEntriesByImageID(ctx, sqlc.GetDiaryEntriesByImageIDParams{
				FileID:       fileID,
				DiaryEntryID: selfEntryID,
			})
			if err != nil {
				return fmt.Errorf("check entry reference: %w", err)
			}
			if len(rows) > 0 {
				return nil
			}
		}
		// FOR UPDATE 在 files 行上串行化「引用检查→删除→扣减」，防止同一文件被两个并发
		// 删除事务先后通过引用检查而双扣配额（与 file.DeletePhysicalIfUnreferenced 对齐）。
		file, err := q.GetFileByIDForUpdate(ctx, fileID)
		if err != nil {
			return fmt.Errorf("get file: %w", err)
		}
		if err := q.DeleteFile(ctx, fileID); err != nil {
			return fmt.Errorf("delete file record: %w", err)
		}
		// 与 file.DeletePhysicalIfUnreferenced 同约束：扣减失败须回滚，否则 files 行已删、
		// image_storage_bytes 未回退，幽灵字节永久占用配额且无法补偿。
		if file.FileType == "image" && file.CreatedBy.Valid && file.CreatedBy.String != "" {
			if decErr := q.DecrementUserImageStorage(ctx, sqlc.DecrementUserImageStorageParams{
				ID:                file.CreatedBy.String,
				ImageStorageBytes: file.SizeBytes,
			}); decErr != nil {
				return fmt.Errorf("decrement user image storage: %w", decErr)
			}
		}
		filePath = file.Path
		storageType = file.StorageType
		return nil
	})
	if err != nil {
		return err
	}
	if filePath == "" {
		return nil
	}
	return s.storage.DeleteFile(filePath, storageType)
}

func (s *Service) RefreshFamilyDailyCover(ctx context.Context, familyID, recordDate string) error {
	// 未配置 Redis 锁（如单元测试构造的最小 Service）时直接报错，
	// 调用方对封面刷新失败仅记录日志，不影响主流程（正常路径行为不变）。
	if s.lock == nil {
		return fmt.Errorf("cover lock not configured")
	}
	_, err := s.refreshFamilyDailyCover(ctx, familyID, recordDate, true)
	return err
}

// refreshFamilyDailyCover 是 RefreshFamilyDailyCover 的内部实现。
// withTrajectory=true 时在锁释放后同步生成轨迹图（避免锁内持有外部 HTTP 调用）；withTrajectory=false 时若需要轨迹图则设置 default 并触发异步完整刷新，返回 needTrajectory=true。
func (s *Service) refreshFamilyDailyCover(ctx context.Context, familyID, recordDate string, withTrajectory bool) (needTrajectory bool, err error) {
	if familyID == "" {
		return false, fmt.Errorf("family_id is required")
	}

	coverLockKey := fmt.Sprintf("lock:covers:%s:%s", familyID, recordDate)
	ok, coverLockToken, err := s.lock.TryLock(ctx, coverLockKey)
	if err != nil {
		return false, fmt.Errorf("acquire cover lock: %w", err)
	}
	if !ok {
		return false, ErrCoverUpdateInProgress
	}
	// 封面锁临界区仅为纯 DB 操作（评估降级 + 写 default/upsert 封面记录），亚秒级完成；
	// 轨迹图生成（外部 HTTP + 文件落盘）已移出锁在释放后异步执行。advisory lock 无 TTL、
	// 连接断开自动释放，不引入续期机制。
	// 轨迹图生成涉及外部 HTTP 调用 + 文件落盘，不可在锁内执行。
	// 锁内完成评估并写入 default，锁释放后再生成轨迹图并 upsert。
	needPostLockTrajectory := false
	var postLockLocs []sqlc.ListLocationEntriesRow
	var trajectoryFilesToDelete []sqlc.FindTrajectoryCoversRow
	defer func() {
		if uerr := s.lock.Unlock(context.WithoutCancel(ctx), coverLockKey, coverLockToken); uerr != nil {
			slog.ErrorContext(ctx, "unlock cover lock failed", slog.String("family_id", familyID), slog.String("record_date", recordDate), slog.Any("error", uerr))
		}
		// 锁释放后再删除物理文件，避免在锁内做外部 IO。
		if len(trajectoryFilesToDelete) > 0 {
			s.deleteTrajectoryCoverFiles(context.WithoutCancel(ctx), trajectoryFilesToDelete, "")
		}
		if needPostLockTrajectory && err == nil && len(postLockLocs) > 0 {
			locs := postLockLocs
			fid := familyID
			rd := recordDate
			// 整个异步流程限时 30s（含腾讯静态地图限流等待与 HTTP 调用），
			// 与同文件其它异步任务保持一致，避免 goroutine 无限滞留。
			bctx, bcancel := context.WithTimeout(context.Background(), 30*time.Second)
			safe.Go(bctx, nil, func() {
				defer bcancel()
				t, parseErr := time.Parse(dateFormat, rd)
				if parseErr != nil {
					slog.Error("invalid record date format for trajectory cover", slog.String("record_date", rd), slog.Any("error", parseErr))
					return
				}
				recordDateTime := pgtype.Date{Time: t, Valid: true}
				url, fileID, genErr := s.ensureTrajectoryCover(bctx, fid, rd, locs)
				if genErr == nil && fileID != "" {
					// 轨迹图生成耗时可达 30s，期间用户可能已通过 UpdateCover 设置手动封面；
					// 先查当前封面，若已是 manual 则跳过 trajectory upsert，避免覆盖用户手动选择。
					cur, curErr := s.pool.Queries().GetFamilyDailyCover(bctx, sqlc.GetFamilyDailyCoverParams{
						FamilyID:   fid,
						RecordDate: recordDateTime,
					})
					if curErr == nil && cur.CoverType == "manual" && cur.ManualCoverFileID.Valid && cur.ManualCoverFileID.String != "" {
						slog.Info("skip trajectory cover upsert, manual cover set during generation", slog.String("family_id", fid), slog.String("record_date", rd))
						return
					}
					upsertErr := s.pool.Queries().UpsertFamilyDailyCover(bctx, sqlc.UpsertFamilyDailyCoverParams{
						FamilyID:          fid,
						RecordDate:        recordDateTime,
						CoverFileID:       pgtype.Text{String: fileID, Valid: true},
						CoverType:         "trajectory",
						ManualCoverFileID: pgtype.Text{Valid: false},
					})
					if upsertErr != nil {
						slog.Error("post-lock upsert trajectory cover failed", slog.String("family_id", fid), slog.String("record_date", rd), slog.Any("error", upsertErr))
					} else {
						if rows, cleanupErr := s.deleteOldTrajectoryCovers(bctx, fid, rd, fileID); cleanupErr != nil {
							slog.Warn("post-lock delete old trajectory failed", slog.String("family_id", fid), slog.String("record_date", rd), slog.Any("error", cleanupErr))
						} else {
							s.deleteTrajectoryCoverFiles(context.WithoutCancel(bctx), rows, fileID)
						}
						s.InvalidateFamilySummary(bctx, fid)
						slog.Debug("refresh family daily cover generate trajectory (post-lock)", slog.String("family_id", fid), slog.String("record_date", rd), slog.String("cover_file_id", fileID), slog.String("url", url))
					}
				} else if genErr != nil {
					slog.Warn("post-lock generate trajectory failed", slog.String("family_id", fid), slog.String("record_date", rd), slog.Any("error", genErr))
				}
			})
		}
	}()

	memberIDs, err := s.listFamilyMemberIDs(ctx, familyID)
	if err != nil {
		return false, fmt.Errorf("list family members: %w", err)
	}
	recordDateTime, err := parseDate(recordDate)
	if err != nil {
		return false, err
	}

	cover, err := s.pool.Queries().GetFamilyDailyCover(ctx, sqlc.GetFamilyDailyCoverParams{
		FamilyID:   familyID,
		RecordDate: recordDateTime,
	})
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return false, fmt.Errorf("get family daily cover: %w", err)
	}
	if cover.ManualCoverFileID.Valid && cover.ManualCoverFileID.String != "" {
		// 手动封面必须同时满足：日记条目仍引用该图片、文件记录仍存在、且 URL 可访问。
		// 任一条件不满足即按 manual → image → trajectory → default 降级（AGENTS.md §3.8）。
		ok, err := s.pool.Queries().IsImageUsedByFamilyDate(ctx, sqlc.IsImageUsedByFamilyDateParams{
			FileID:  cover.ManualCoverFileID.String,
			Column2: memberIDs,
			Column3: recordDateTime,
		})
		if err != nil {
			return false, fmt.Errorf("check manual cover image usage: %w", err)
		}
		if ok {
			manualFile, err := s.pool.Queries().GetFileByID(ctx, cover.ManualCoverFileID.String)
			if err == nil {
				// 增加 URL 可访问性校验；失败则降级，避免 cover_type 与实际生效封面不一致。
				_, urlErr := s.storage.URL(manualFile.Path, manualFile.StorageType)
				if urlErr == nil {
					if err := s.pool.Queries().UpsertFamilyDailyCover(ctx, sqlc.UpsertFamilyDailyCoverParams{
						FamilyID:          familyID,
						RecordDate:        recordDateTime,
						CoverFileID:       cover.ManualCoverFileID,
						CoverType:         "manual",
						ManualCoverFileID: cover.ManualCoverFileID,
					}); err != nil {
						return false, fmt.Errorf("upsert manual cover: %w", err)
					}
					s.InvalidateFamilySummary(ctx, familyID)
					slog.DebugContext(ctx, "refresh family daily cover use manual", slog.String("family_id", familyID), slog.String("record_date", recordDate), slog.String("cover_file_id", cover.ManualCoverFileID.String))
					return false, nil
				}
				slog.WarnContext(ctx, "manual cover url not accessible, will downgrade",
					slog.String("family_id", familyID),
					slog.String("record_date", recordDate),
					slog.String("cover_file_id", cover.ManualCoverFileID.String),
					slog.Any("error", urlErr))
			} else if errors.Is(err, pgx.ErrNoRows) {
				slog.WarnContext(ctx, "manual cover file record missing, will downgrade",
					slog.String("family_id", familyID),
					slog.String("record_date", recordDate),
					slog.String("cover_file_id", cover.ManualCoverFileID.String))
			} else {
				return false, fmt.Errorf("get manual cover file: %w", err)
			}
		}

		if err := s.pool.Queries().ClearFamilyDailyManualCover(ctx, sqlc.ClearFamilyDailyManualCoverParams{
			FamilyID:   familyID,
			RecordDate: recordDateTime,
		}); err != nil {
			return false, fmt.Errorf("clear invalid manual cover: %w", err)
		}
	}

	img, err := s.pool.Queries().GetLatestImageEntry(ctx, sqlc.GetLatestImageEntryParams{
		Column1: memberIDs,
		Column2: recordDateTime,
	})
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return false, fmt.Errorf("get latest image entry: %w", err)
	}
	if err == nil && img.ImageID != "" {
		// 增加 URL 可访问性校验；失败则降级，避免 cover_type 与实际生效封面不一致（AGENTS.md §3.8）。
		_, urlErr := s.fileURL(img.Path, img.StorageType)
		if urlErr == nil {
			if err := s.pool.Queries().UpsertFamilyDailyCover(ctx, sqlc.UpsertFamilyDailyCoverParams{
				FamilyID:          familyID,
				RecordDate:        recordDateTime,
				CoverFileID:       pgtype.Text{String: img.ImageID, Valid: true},
				CoverType:         "image",
				ManualCoverFileID: pgtype.Text{Valid: false},
			}); err != nil {
				return false, fmt.Errorf("upsert image cover: %w", err)
			}
			if rows, err := s.deleteOldTrajectoryCovers(ctx, familyID, recordDate, ""); err != nil {
				slog.WarnContext(ctx, "refresh family daily cover delete old trajectory failed", slog.String("family_id", familyID), slog.String("record_date", recordDate), slog.Any("error", err))
			} else {
				trajectoryFilesToDelete = append(trajectoryFilesToDelete, rows...)
			}
			s.InvalidateFamilySummary(ctx, familyID)
			slog.DebugContext(ctx, "refresh family daily cover use image", slog.String("family_id", familyID), slog.String("record_date", recordDate), slog.String("cover_file_id", img.ImageID))
			return false, nil
		}
		slog.WarnContext(ctx, "image cover url not accessible, will downgrade",
			slog.String("family_id", familyID),
			slog.String("record_date", recordDate),
			slog.String("cover_file_id", img.ImageID),
			slog.Any("error", urlErr))
	}

	locs, err := s.pool.Queries().ListLocationEntries(ctx, sqlc.ListLocationEntriesParams{
		Column1: memberIDs,
		Column2: recordDateTime,
	})
	if err != nil {
		return false, fmt.Errorf("list location entries: %w", err)
	}
	if len(locs) == 0 {
		if err := s.pool.Queries().UpsertFamilyDailyCover(ctx, sqlc.UpsertFamilyDailyCoverParams{
			FamilyID:          familyID,
			RecordDate:        recordDateTime,
			CoverFileID:       pgtype.Text{Valid: false},
			CoverType:         "default",
			ManualCoverFileID: pgtype.Text{Valid: false},
		}); err != nil {
			return false, fmt.Errorf("upsert default cover: %w", err)
		}
		if rows, err := s.deleteOldTrajectoryCovers(ctx, familyID, recordDate, ""); err != nil {
			slog.WarnContext(ctx, "refresh family daily cover delete old trajectory failed", slog.String("family_id", familyID), slog.String("record_date", recordDate), slog.Any("error", err))
		} else {
			trajectoryFilesToDelete = append(trajectoryFilesToDelete, rows...)
		}
		s.InvalidateFamilySummary(ctx, familyID)
		slog.DebugContext(ctx, "refresh family daily cover fallback to default", slog.String("family_id", familyID), slog.String("record_date", recordDate))
		return false, nil
	}

	if !withTrajectory {
		// 调用方要求不阻塞在轨迹图生成；先返回 default，并通知调用方在锁释放后触发异步完整刷新。
		// 不在锁内直接启动 goroutine，避免异步任务开始抢锁时父函数 defer 尚未释放锁（AGENTS.md §4.1）。
		if err := s.pool.Queries().UpsertFamilyDailyCover(ctx, sqlc.UpsertFamilyDailyCoverParams{
			FamilyID:          familyID,
			RecordDate:        recordDateTime,
			CoverFileID:       pgtype.Text{Valid: false},
			CoverType:         "default",
			ManualCoverFileID: pgtype.Text{Valid: false},
		}); err != nil {
			return false, fmt.Errorf("upsert default cover before trajectory: %w", err)
		}
		s.InvalidateFamilySummary(ctx, familyID)
		slog.DebugContext(ctx, "refresh family daily cover need async trajectory", slog.String("family_id", familyID), slog.String("record_date", recordDate))
		return true, nil
	}

	// withTrajectory=true：锁内先写入 default 保证不白屏，锁释放后在 defer 中生成轨迹图。
	// 轨迹图生成涉及外部 HTTP 调用（腾讯地图）和文件落盘，不可在分布式锁内执行。
	if err := s.pool.Queries().UpsertFamilyDailyCover(ctx, sqlc.UpsertFamilyDailyCoverParams{
		FamilyID:          familyID,
		RecordDate:        recordDateTime,
		CoverFileID:       pgtype.Text{Valid: false},
		CoverType:         "default",
		ManualCoverFileID: pgtype.Text{Valid: false},
	}); err != nil {
		return false, fmt.Errorf("upsert default cover before post-lock trajectory: %w", err)
	}
	s.InvalidateFamilySummary(ctx, familyID)
	needPostLockTrajectory = true
	postLockLocs = locs
	slog.DebugContext(ctx, "refresh family daily cover deferred trajectory generation", slog.String("family_id", familyID), slog.String("record_date", recordDate), slog.Int("locs", len(locs)))
	return false, nil
}

func (s *Service) refreshCoverAsync(familyID, recordDate string) {
	if familyID == "" {
		return
	}
	// 去重节流：同一 family+date 的封面刷新带 30s TTL 去重，避免列表请求对每页最多 20 个
	// 日期各起一个 goroutine 重复评估/抢锁（封面行未持久化期间每次请求都会触发）。
	if s.rdb != nil {
		dedupKey := fmt.Sprintf("lock:covers:refresh:%s:%s", familyID, recordDate)
		ok, err := s.rdb.SetNX(context.Background(), dedupKey, "1", 30*time.Second).Result()
		if err != nil {
			slog.WarnContext(context.Background(), "cover refresh dedup check failed", slog.String("family_id", familyID), slog.String("record_date", recordDate), slog.Any("error", err))
		} else if !ok {
			return
		}
	}
	safe.Go(context.Background(), nil, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := s.RefreshFamilyDailyCover(ctx, familyID, recordDate); err != nil {
			slog.WarnContext(ctx, "refresh cover async failed", slog.String("family_id", familyID), slog.String("record_date", recordDate), slog.Any("error", err))
		}
	})
}

func (s *Service) ResolveCoverImage(ctx context.Context, userID, familyID, recordDate string) (coverURL string, coverFileID string, err error) {
	if familyID == "" {
		return s.defaultCoverURL(), "", nil
	}
	return s.resolveFamilyDailyCover(ctx, familyID, recordDate)
}

func (s *Service) resolveFamilyDailyCover(ctx context.Context, familyID, recordDate string) (coverURL string, coverFileID string, err error) {
	recordDateTime, err := parseDate(recordDate)
	if err != nil {
		return "", "", err
	}

	cover, err := s.pool.Queries().GetFamilyDailyCover(ctx, sqlc.GetFamilyDailyCoverParams{
		FamilyID:   familyID,
		RecordDate: recordDateTime,
	})
	if err == nil && cover.CoverType != "" {
		url, ok, urlErr := s.coverURLFromRow(cover)
		if urlErr != nil {
			// D9：URL 不可构造即视为封面失效，降级到下方无锁评估，不作为错误上抛。
			slog.WarnContext(ctx, "build cover url failed", slog.String("family_id", familyID), slog.String("record_date", recordDate), slog.Any("error", urlErr))
		} else if ok {
			fileID := ""
			if cover.CoverFileID.Valid {
				fileID = cover.CoverFileID.String
			}
			return url, fileID, nil
		}
		if cover.CoverType == "default" {
			return s.defaultCoverURL(), "", nil
		}
	}

	// D6：读路径（详情）封面行缺失或 URL 失效时，与列表路径一致先做无锁评估
	// 即时返回可用封面，再异步触发完整刷新（30s 去重节流）由后端持久化纠正。
	members, memberErr := s.pool.Queries().ListFamilyMembers(ctx, familyID)
	if memberErr != nil {
		slog.WarnContext(ctx, "list family members for cover resolve failed", slog.String("family_id", familyID), slog.String("record_date", recordDate), slog.Any("error", memberErr))
	} else if len(members) > 0 {
		memberIDs := make([]string, 0, len(members))
		for _, m := range members {
			memberIDs = append(memberIDs, m.UserID)
		}
		if evalURL, evalFileID, _, evalErr := s.evaluateCoverForDate(ctx, familyID, recordDate, memberIDs, nil); evalErr != nil {
			slog.WarnContext(ctx, "evaluate cover for detail failed", slog.String("family_id", familyID), slog.String("record_date", recordDate), slog.Any("error", evalErr))
		} else if evalURL != "" {
			s.refreshCoverAsync(familyID, recordDate)
			return evalURL, evalFileID, nil
		}
	}
	s.refreshCoverAsync(familyID, recordDate)
	return s.defaultCoverURL(), "", nil
}

func (s *Service) ensureTrajectoryCover(ctx context.Context, familyID, recordDate string, locs []sqlc.ListLocationEntriesRow) (string, string, error) {
	fileID, path, storageType, err := s.generateTrajectoryMap(ctx, familyID, recordDate, locs)
	if err != nil {
		return "", "", err
	}
	url, urlErr := s.fileURL(path, storageType)
	if urlErr != nil {
		return "", "", urlErr
	}
	return url, fileID, nil
}

func (s *Service) generateTrajectoryMap(ctx context.Context, familyID, recordDate string, locs []sqlc.ListLocationEntriesRow) (string, string, string, error) {
	if len(locs) == 0 || s.loc == nil {
		return "", "", "", fmt.Errorf("missing locations or tencent map key")
	}

	markerList, bounds, err := s.buildTrajectoryMapParams(ctx, locs)
	if err != nil {
		return "", "", "", fmt.Errorf("build staticmap params: %w", err)
	}

	params := url.Values{}
	params.Set("key", s.loc.NextKey())

	params.Set("size", "640*640")
	params.Set("maptype", "roadmap")
	params.Set("scale", "2")
	for _, m := range markerList {
		params.Add("markers", m)
	}

	if len(locs) == 1 || bounds == "" {
		firstLat, latErr := locs[0].Lat.Float64Value()
		if latErr != nil {
			slog.Warn("first point lat Float64Value overflow", slog.Any("error", latErr))
		}
		firstLon, lonErr := locs[0].Lon.Float64Value()
		if lonErr != nil {
			slog.Warn("first point lon Float64Value overflow", slog.Any("error", lonErr))
		}
		params.Set("center", fmt.Sprintf("%.4f,%.4f", firstLat.Float64, firstLon.Float64))
		params.Set("zoom", "15")
	} else {
		params.Set("bounds", bounds)
	}

	const maxTencentMapURLLen = 8000
	for {
		estimated := len(params.Get("key")) + 200
		for _, m := range params["markers"] {
			estimated += len(m)
		}
		if estimated <= maxTencentMapURLLen {
			break
		}
		if len(params["markers"]) <= 2 {
			break
		}

		keep := []string{params["markers"][0]}
		step := max(1, len(params["markers"])/2)
		for i := step; i < len(params["markers"])-1; i += step {
			keep = append(keep, params["markers"][i])
		}
		if params["markers"][len(params["markers"])-1] != keep[len(keep)-1] {
			keep = append(keep, params["markers"][len(params["markers"])-1])
		}
		params["markers"] = keep
	}

	staticURL := "https://apis.map.qq.com/ws/staticmap/v2/?" + params.Encode()

	if err := limiter.WaitStaticMap(ctx); err != nil {
		return "", "", "", fmt.Errorf("wait staticmap quota: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, staticURL, nil)
	if err != nil {
		return "", "", "", fmt.Errorf("build staticmap request: %w", err)
	}
	req.Header.Set("Accept", "image/*")

	resp, err := config.HTTPClient().Do(req)
	if err != nil {
		return "", "", "", fmt.Errorf("staticmap request: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		body, readErr := io.ReadAll(resp.Body)
		if readErr != nil {
			return "", "", "", fmt.Errorf("staticmap status %d (read body: %w)", resp.StatusCode, readErr)
		}
		bodyStr := string(body)
		if bodyStr == "" {
			bodyStr = "<empty body>"
		}
		return "", "", "", fmt.Errorf("staticmap status %d: %s", resp.StatusCode, bodyStr)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, 10*1024*1024))
	if err != nil {
		return "", "", "", fmt.Errorf("read staticmap image: %w", err)
	}
	if len(data) == 0 {
		return "", "", "", fmt.Errorf("empty staticmap image")
	}

	contentType := strings.ToLower(resp.Header.Get("Content-Type"))
	ext := ".png"
	if strings.Contains(contentType, "jpeg") || strings.Contains(contentType, "jpg") {
		ext = ".jpg"
	}

	if ext == ".png" {
		img, _, decodeErr := image.Decode(bytes.NewReader(data))
		if decodeErr == nil {
			buf := new(bytes.Buffer)
			if encodeErr := jpeg.Encode(buf, img, &jpeg.Options{Quality: 90}); encodeErr == nil {
				data = buf.Bytes()
				ext = ".jpg"
			}
		}
	}

	relPath, storageType, _, err := s.storage.SaveSystem(bytes.NewReader(data), ext)
	if err != nil {
		return "", "", "", fmt.Errorf("save trajectory image: %w", err)
	}

	metadata, err := json.Marshal(map[string]string{
		"family_id":   familyID,
		"record_date": recordDate,
	})
	if err != nil {
		return "", "", "", fmt.Errorf("marshal metadata: %w", err)
	}

	fileID, err := util.NewUUID()
	if err != nil {
		return "", "", "", fmt.Errorf("generate trajectory file id: %w", err)
	}
	_, err = s.pool.Queries().CreateFile(ctx, sqlc.CreateFileParams{
		ID:          fileID,
		Path:        relPath,
		Name:        "trajectory",
		Suffix:      strings.TrimPrefix(ext, "."),
		SizeBytes:   int64(len(data)),
		FileType:    "system",
		StorageType: storageType,
		Metadata:    metadata,
	})
	if err != nil {
		return "", "", "", fmt.Errorf("record trajectory file: %w", err)
	}

	return fileID, relPath, storageType, nil
}

func (s *Service) buildTrajectoryMapParams(ctx context.Context, locs []sqlc.ListLocationEntriesRow) (markerList []string, bounds string, err error) {
	if len(locs) == 0 {
		return nil, "", nil
	}

	markerList, err = s.buildTrajectoryMarkers(ctx, locs)
	if err != nil {
		return nil, "", err
	}

	if len(locs) == 1 {
		return markerList, "", nil
	}

	points := make([][2]float64, 0, len(locs))
	for _, loc := range locs {
		latVal, err := loc.Lat.Float64Value()
		if err != nil || !latVal.Valid {
			continue
		}
		lonVal, err := loc.Lon.Float64Value()
		if err != nil || !lonVal.Valid {
			continue
		}
		points = append(points, [2]float64{latVal.Float64, lonVal.Float64})
	}
	if len(points) < 2 {
		return markerList, "", nil
	}

	minLat, maxLat := points[0][0], points[0][0]
	minLon, maxLon := points[0][1], points[0][1]
	for _, p := range points[1:] {
		if p[0] < minLat {
			minLat = p[0]
		}
		if p[0] > maxLat {
			maxLat = p[0]
		}
		if p[1] < minLon {
			minLon = p[1]
		}
		if p[1] > maxLon {
			maxLon = p[1]
		}
	}
	latSpan := maxLat - minLat
	lonSpan := maxLon - minLon

	centerLat := (minLat + maxLat) / 2
	centerLon := (minLon + maxLon) / 2
	halfLatSpan := math.Max(latSpan/2*1.4, 0.002)
	halfLonSpan := math.Max(lonSpan/2*1.4, 0.002)
	swLat := math.Max(centerLat-halfLatSpan, -90)
	neLat := math.Min(centerLat+halfLatSpan, 90)
	swLon := math.Max(centerLon-halfLonSpan, -180)
	neLon := math.Min(centerLon+halfLonSpan, 180)
	bounds = fmt.Sprintf("%.4f,%.4f;%.4f,%.4f", swLat, swLon, neLat, neLon)

	return markerList, bounds, nil
}

func (s *Service) deleteOldTrajectoryCovers(ctx context.Context, familyID, recordDate, keepID string) ([]sqlc.FindTrajectoryCoversRow, error) {
	rows, err := s.pool.Queries().FindTrajectoryCovers(ctx, sqlc.FindTrajectoryCoversParams{
		Column1: familyID,
		Column2: recordDate,
	})
	if err != nil {
		return nil, fmt.Errorf("find trajectory covers: %w", err)
	}

	if err := s.pool.Queries().DeleteTrajectoryCovers(ctx, sqlc.DeleteTrajectoryCoversParams{
		Column1: familyID,
		Column2: recordDate,
		Column3: keepID,
	}); err != nil {
		return nil, fmt.Errorf("delete old trajectory db records: %w", err)
	}
	return rows, nil
}

func (s *Service) deleteTrajectoryCoverFiles(ctx context.Context, rows []sqlc.FindTrajectoryCoversRow, keepID string) {
	for _, r := range rows {
		if r.ID == "" || r.ID == keepID {
			continue
		}
		if err := s.storage.DeleteFile(r.Path, r.StorageType); err != nil {
			slog.ErrorContext(ctx, "delete old trajectory physical file", slog.Any("error", err))
		}
	}
}

func (s *Service) listFamilyMemberIDs(ctx context.Context, familyID string) ([]string, error) {
	members, err := s.pool.Queries().ListFamilyMembers(ctx, familyID)
	if err != nil {
		return nil, fmt.Errorf("list family members: %w", err)
	}
	var ids []string
	for _, m := range members {
		ids = append(ids, m.UserID)
	}
	return ids, nil
}

func (s *Service) buildCard(ctx context.Context, familyID, recordDate string, rows []sqlc.ListDiaryCardsRow, memberMap map[string]sqlc.ListFamilyMembersRow, currentUserID string, addressRecords []map[string]interface{}, coverURL, coverImageID string) (map[string]interface{}, error) {
	countMap := make(map[string]int64)
	for _, r := range rows {
		countMap[r.UserID] = r.EntryCount
	}

	var detailCount int64
	var memberBriefs []map[string]interface{}
	var isMyDiary bool
	var myDiaryID interface{}

	for _, row := range rows {
		detailCount += row.EntryCount
		if row.UserID == currentUserID {
			isMyDiary = true
			myDiaryID = row.ID
		}
		m, ok := memberMap[row.UserID]
		if !ok {
			continue
		}
		memberBriefs = append(memberBriefs, map[string]interface{}{
			"userId":     m.UserID,
			"avatarUrl":  util.AvatarURLOrDefault(m.Avatar, m.UserID, s.sysCfg.DefaultAvatarURL),
			"nickName":   util.ToInterface(m.Nickname),
			"entryCount": countMap[m.UserID],
		})
	}

	return map[string]interface{}{
		"id":                               makeVirtualID(familyID, recordDate),
		"recordDate":                       recordDate,
		"coverImg":                         coverURL,
		"coverImage":                       coverImageID,
		"detailCount":                      detailCount,
		"isMyDiaryInfo":                    isMyDiary,
		"myDiaryInfoId":                    myDiaryID,
		"memberBriefs":                     memberBriefs,
		"familyMemberAddressConcatRecords": addressRecords,
	}, nil
}

var parenthesisRegex = regexp.MustCompile(`[（(].*?[）)]`)

func smartShortenAddress(addr string) string {
	if addr == "" {
		return addr
	}
	addr = strings.TrimSpace(addr)
	addr = parenthesisRegex.ReplaceAllString(addr, "")
	addr = strings.TrimSpace(addr)
	provinces := []string{
		"北京市", "天津市", "上海市", "重庆市",
		"河北省", "山西省", "辽宁省", "吉林省", "黑龙江省",
		"江苏省", "浙江省", "安徽省", "福建省", "江西省", "山东省",
		"河南省", "湖北省", "湖南省", "广东省", "海南省",
		"四川省", "贵州省", "云南省", "陕西省", "甘肃省", "青海省", "台湾省",
		"内蒙古自治区", "广西壮族自治区", "西藏自治区", "宁夏回族自治区", "新疆维吾尔自治区",
		"香港特别行政区", "澳门特别行政区",
	}
	for _, p := range provinces {
		if strings.HasPrefix(addr, p) {
			addr = strings.TrimPrefix(addr, p)
			break
		}
	}
	return strings.TrimSpace(addr)
}

func (s *Service) groupCardsByDateFromByDates(rows []sqlc.ListDiaryCardsByDatesRow) map[string][]sqlc.ListDiaryCardsRow {
	m := make(map[string][]sqlc.ListDiaryCardsRow)
	for _, r := range rows {
		d := r.RecordDate.Time.Format(dateFormat)
		m[d] = append(m[d], sqlc.ListDiaryCardsRow(r))
	}
	return m
}

func groupCardsByDate(rows []sqlc.ListDiaryCardsRow) map[string][]sqlc.ListDiaryCardsRow {
	m := make(map[string][]sqlc.ListDiaryCardsRow)
	for _, r := range rows {
		d := r.RecordDate.Time.Format(dateFormat)
		m[d] = append(m[d], r)
	}
	return m
}

func entryToMap(e sqlc.ListDiaryEntriesRow, storage *file.Storage, memberMap map[string]sqlc.ListFamilyMembersRow, defaultAvatarURL string, imageIDs []string, imageRecords map[string]sqlc.ListDiaryEntryImagePathsByEntryIDsRow) map[string]interface{} {
	var recordImages []map[string]interface{}
	for _, fid := range imageIDs {
		filePath := ""
		if rec, ok := imageRecords[fid]; ok {
			url, urlErr := storage.URL(rec.Path, rec.StorageType)
			if urlErr != nil {
				slog.Error("get diary image url failed", slog.String("file_id", fid), slog.Any("error", urlErr))
			} else {
				filePath = url
			}
		}
		recordImages = append(recordImages, map[string]interface{}{
			"id":       fid,
			"filePath": filePath,
		})
	}

	var lat, lon interface{}
	if e.Lat.Valid {
		v, latErr := e.Lat.Float64Value()
		if latErr != nil {
			slog.Warn("mcp entry lat Float64Value overflow", slog.Any("error", latErr))
		} else {
			lat = v.Float64
		}
	}
	if e.Lon.Valid {
		v, lonErr := e.Lon.Float64Value()
		if lonErr != nil {
			slog.Warn("mcp entry lon Float64Value overflow", slog.Any("error", lonErr))
		} else {
			lon = v.Float64
		}
	}

	var recordTime interface{}
	if e.RecordTime.Valid {
		recordTime = e.RecordTime.Time.In(timeutil.Shanghai).Format("2006-01-02 15:04:05")
	}

	var creatorAvatar, creatorNickname interface{}
	if m, ok := memberMap[e.CreatedBy]; ok {
		creatorAvatar = util.AvatarURLOrDefault(m.Avatar, e.CreatedBy, defaultAvatarURL)
		creatorNickname = util.ToInterface(m.Nickname)
	}

	return map[string]interface{}{
		"id":                    e.ID,
		"recordText":            util.ToInterface(e.Text),
		"recordImages":          recordImages,
		"diaryLat":              lat,
		"diaryLon":              lon,
		"diaryAddress":          util.ToInterface(e.Address),
		"detailAddr":            util.ToInterface(e.DetailAddress),
		"recordTime":            recordTime,
		"sort":                  e.Sort,
		"color":                 util.ToInterface(e.Color),
		"familyMemberUserId":    e.CreatedBy,
		"familyMemberAvatarUrl": creatorAvatar,
		"familyMemberNickName":  creatorNickname,
		"createdAt":             e.CreatedAt.Time.Format(time.RFC3339),
		"updatedAt":             e.UpdatedAt.Time.Format(time.RFC3339),
	}
}

func memoryToMap(m sqlc.ListMemoriesByUserAndDateRow, userID string, memberMap map[string]sqlc.ListFamilyMembersRow, defaultAvatarURL string) map[string]interface{} {
	var creatorAvatar, creatorNickname interface{}
	if mem, ok := memberMap[userID]; ok {
		creatorAvatar = util.AvatarURLOrDefault(mem.Avatar, userID, defaultAvatarURL)
		creatorNickname = util.ToInterface(mem.Nickname)
	}

	return map[string]interface{}{
		"id":                    m.ID,
		"recordText":            m.Content,
		"recordImages":          []interface{}{},
		"diaryLat":              nil,
		"diaryLon":              nil,
		"diaryAddress":          m.Title,
		"detailAddr":            nil,
		"recordTime":            m.RecordTime.Time.In(timeutil.Shanghai).Format("2006-01-02 15:04:05"),
		"sort":                  0,
		"color":                 "#543116",
		"familyMemberUserId":    userID,
		"familyMemberAvatarUrl": creatorAvatar,
		"familyMemberNickName":  creatorNickname,
		"createdAt":             m.CreatedAt.Time.Format(time.RFC3339),
		"updatedAt":             m.CreatedAt.Time.Format(time.RFC3339),
		"source":                "memory",
	}
}

func resolveRecordDateAndTime(req *entryRequest) (recordDate string, recordTime time.Time, err error) {
	if req.RecordTime != nil && *req.RecordTime != "" {
		t, err := time.Parse(time.RFC3339Nano, *req.RecordTime)
		if err != nil {
			return "", time.Time{}, fmt.Errorf("invalid recordTime: %w", err)
		}
		recordTime = t
	} else {
		recordTime = timeutil.NowShanghai()
	}

	recordDate = recordTime.In(timeutil.Shanghai).Format(dateFormat)
	return recordDate, recordTime, nil
}

func buildCreateEntryParams(entryID, diaryID, userID string, req *entryRequest, recordTime time.Time) (sqlc.CreateDiaryEntryParams, error) {
	params := sqlc.CreateDiaryEntryParams{
		ID:         entryID,
		DiaryID:    diaryID,
		CreatedBy:  userID,
		RecordTime: pgtype.Timestamptz{Time: recordTime, Valid: true},
	}

	if req.Text != nil {
		params.Text = pgtype.Text{String: *req.Text, Valid: true}
	}
	if req.Lat != nil {
		lat := pgtype.Numeric{}
		if err := lat.Scan(fmt.Sprintf("%.4f", *req.Lat)); err != nil {
			return params, fmt.Errorf("invalid lat: %w", err)
		}
		params.Lat = lat
	}
	if req.Lon != nil {
		lon := pgtype.Numeric{}
		if err := lon.Scan(fmt.Sprintf("%.4f", *req.Lon)); err != nil {
			return params, fmt.Errorf("invalid lon: %w", err)
		}
		params.Lon = lon
	}
	if req.Address != nil {
		params.Address = pgtype.Text{String: *req.Address, Valid: true}
	}
	if req.DetailAddress != nil {
		params.DetailAddress = pgtype.Text{String: *req.DetailAddress, Valid: true}
	}
	if req.Sort != nil {
		params.Sort = *req.Sort
	}
	if req.Color != nil && *req.Color != "" {
		params.Color = pgtype.Text{String: *req.Color, Valid: true}
	}

	return params, nil
}

func mergeUpdateEntryParams(existing sqlc.DiaryEntry, req *entryRequest) (sqlc.UpdateDiaryEntryParams, error) {
	params := sqlc.UpdateDiaryEntryParams{
		ID:            existing.ID,
		Text:          existing.Text,
		Lat:           existing.Lat,
		Lon:           existing.Lon,
		Address:       existing.Address,
		DetailAddress: existing.DetailAddress,
		RecordTime:    existing.RecordTime,
		Sort:          existing.Sort,
		Color:         existing.Color,
	}

	if req.Text != nil {
		params.Text = pgtype.Text{String: *req.Text, Valid: true}
	}
	if req.Lat != nil {
		lat := pgtype.Numeric{}
		if err := lat.Scan(fmt.Sprintf("%.4f", *req.Lat)); err != nil {
			return params, fmt.Errorf("invalid lat: %w", err)
		}
		params.Lat = lat
	}
	if req.Lon != nil {
		lon := pgtype.Numeric{}
		if err := lon.Scan(fmt.Sprintf("%.4f", *req.Lon)); err != nil {
			return params, fmt.Errorf("invalid lon: %w", err)
		}
		params.Lon = lon
	}
	if req.Address != nil {
		params.Address = pgtype.Text{String: *req.Address, Valid: true}
	}
	if req.DetailAddress != nil {
		params.DetailAddress = pgtype.Text{String: *req.DetailAddress, Valid: true}
	}
	if req.RecordTime != nil {
		if *req.RecordTime == "" {
			params.RecordTime = pgtype.Timestamptz{}
		} else {
			t, err := time.Parse(time.RFC3339Nano, *req.RecordTime)
			if err != nil {
				return params, fmt.Errorf("invalid recordTime: %w", err)
			}
			params.RecordTime = pgtype.Timestamptz{Time: t, Valid: true}
		}
	}
	if req.Sort != nil {
		params.Sort = *req.Sort
	}
	if req.Color != nil {
		if *req.Color == "" {
			params.Color = pgtype.Text{}
		} else {
			params.Color = pgtype.Text{String: *req.Color, Valid: true}
		}
	}

	return params, nil
}

func (s *Service) fileURL(path, storageType string) (string, error) {
	return s.storage.URL(path, storageType)
}

func (s *Service) coverURLFromParts(fileID, path, storageType string) (string, bool, error) {
	if fileID == "" || path == "" {
		return "", false, nil
	}
	if storageType == "" {
		storageType = "local"
	}
	url, err := s.storage.URL(path, storageType)
	if err != nil {
		return "", false, err
	}
	return url, true, nil
}

func (s *Service) coverURLFromRow(cover sqlc.GetFamilyDailyCoverRow) (string, bool, error) {
	// M2：cover_type='default' 时不使用任何文件图，防止触发器保留的 cover_file_id（B3-08）或历史脏数据被误展示。
	if cover.CoverType == "default" {
		return "", false, nil
	}
	fileID := ""
	if cover.CoverFileID.Valid {
		fileID = cover.CoverFileID.String
	}
	path := ""
	if cover.CoverPath.Valid {
		path = cover.CoverPath.String
	}
	storageType := ""
	if cover.CoverStorageType.Valid {
		storageType = cover.CoverStorageType.String
	}
	return s.coverURLFromParts(fileID, path, storageType)
}

func (s *Service) coverURLFromListRow(c sqlc.ListFamilyDailyCoversRow) (string, bool, error) {
	// M2：cover_type='default' 时不使用任何文件图，防止触发器保留的 cover_file_id（B3-08）或历史脏数据被误展示。
	if c.CoverType == "default" {
		return "", false, nil
	}
	fileID := ""
	if c.CoverFileID.Valid {
		fileID = c.CoverFileID.String
	}
	path := ""
	if c.CoverPath.Valid {
		path = c.CoverPath.String
	}
	storageType := ""
	if c.CoverStorageType.Valid {
		storageType = c.CoverStorageType.String
	}
	return s.coverURLFromParts(fileID, path, storageType)
}

// evaluateFromPreloadedCover 使用批量加载的封面行做无锁评估，避免重复 DB 查询 GetFamilyDailyCover。
// 返回值 ok=false 表示 preloaded 数据不可直接使用（URL 解析失败），isManualStale=true 表示手动封面已失效。
func (s *Service) evaluateFromPreloadedCover(ctx context.Context, c sqlc.ListFamilyDailyCoversRow, memberIDs []string, recordDateTime pgtype.Date) (coverURL, coverFileID string, ok, isManualStale bool, err error) {
	if c.CoverType == "manual" && c.ManualCoverFileID.Valid && c.ManualCoverFileID.String != "" {
		used, usedErr := s.pool.Queries().IsImageUsedByFamilyDate(ctx, sqlc.IsImageUsedByFamilyDateParams{
			FileID:  c.ManualCoverFileID.String,
			Column2: memberIDs,
			Column3: recordDateTime,
		})
		if usedErr != nil {
			return "", "", false, false, fmt.Errorf("check manual cover usage: %w", usedErr)
		}
		if !used {
			return "", "", false, true, nil
		}
	}
	url, ok, err := s.coverURLFromListRow(c)
	if err != nil || !ok {
		return "", "", false, false, err
	}
	fileID := ""
	if c.CoverFileID.Valid {
		fileID = c.CoverFileID.String
	}
	return url, fileID, true, false, nil
}

func (s *Service) defaultCoverURL() string {
	return ""
}

func (s *Service) defaultTrajectoryIconURL() string {
	if s.defaultIcon != "" {
		return s.defaultIcon
	}
	if s.sysCfg != nil {
		return s.sysCfg.DefaultTrajectoryIcon
	}
	return ""
}

func (s *Service) GetCoverURL(ctx context.Context, familyID, recordDate string) (string, error) {
	recordDateTime, err := parseDate(recordDate)
	if err != nil {
		return "", err
	}
	cover, err := s.pool.Queries().GetFamilyDailyCover(ctx, sqlc.GetFamilyDailyCoverParams{
		FamilyID:   familyID,
		RecordDate: recordDateTime,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return s.defaultCoverURL(), nil
		}
		return "", err
	}
	// M2：cover_type='default' 时返回默认占位图，防止触发器保留的 cover_file_id（B3-08）或历史脏数据
	// 被轮询端点误展示为旧图（与 coverURLFromRow/coverURLFromListRow 的 M2 防护保持一致）。
	if cover.CoverType == "default" {
		return s.defaultCoverURL(), nil
	}
	url, urlErr := s.fileURL(cover.CoverPath.String, cover.CoverStorageType.String)
	if urlErr != nil || url == "" {
		return s.defaultCoverURL(), nil
	}
	return url, nil
}

func (s *Service) InvalidateFamilySummary(ctx context.Context, familyID string) {
	if s.rdb == nil {
		return
	}
	if err := s.rdb.Del(ctx, familySummaryKey(familyID)).Err(); err != nil {
		slog.WarnContext(ctx, "invalidate family summary cache failed", slog.String("family_id", familyID), slog.Any("error", err))
	}
}

func familySummaryKey(familyID string) string {
	return "ai:family_summary:" + familyID
}

func parseDate(s string) (pgtype.Date, error) {
	t, err := time.Parse(dateFormat, s)
	if err != nil {
		return pgtype.Date{}, fmt.Errorf("invalid date %q: %w", s, err)
	}
	return pgtype.Date{Time: t, Valid: true}, nil
}
