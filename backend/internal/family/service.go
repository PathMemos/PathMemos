package family

import (
	"context"
	stderrors "errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"papafeiji/backend/internal/db"
	"papafeiji/backend/internal/db/sqlc"
	dbx "papafeiji/backend/pkg/db"
	"papafeiji/backend/pkg/util"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/redis/go-redis/v9"
)

type Service struct {
	pool          *db.Pool
	rdb           *redis.Client
	lock          db.Locker
	defaultAvatar string
}

func NewService(pool *db.Pool, rdb *redis.Client, lock db.Locker, defaultAvatarURL string) *Service {
	return &Service{pool: pool, rdb: rdb, lock: lock, defaultAvatar: defaultAvatarURL}
}

func (s *Service) invalidateFamilySummary(ctx context.Context, familyID string) {
	if s.rdb == nil || familyID == "" {
		return
	}
	//nolint:errcheck
	if err := s.rdb.Del(ctx, "ai:family_summary:"+familyID).Err(); err != nil {
		slog.WarnContext(ctx, "invalidate family summary cache failed", slog.String("family_id", familyID), slog.Any("error", err))
	}
}

type Member struct {
	UserID   string
	Avatar   interface{}
	Nickname interface{}
	Role     string
	JoinedAt time.Time
}

type FamilyInfo struct {
	FamilyID   string
	OwnerID    string
	IsPersonal bool
	Members    []Member
}

type AccountCleanupInfo struct {
	Paths            []string
	CoverFileIDs     []string
	MarkerPath       string
	MarkerStorage    string
	FamilyID         string
	PersonalFamilyID string
	AffectedUserIDs  []string
}

// DefaultAvatarMarkerPath 是系统默认头像 marker 的资源路径：全局共享资产，注销时不可删除。
const DefaultAvatarMarkerPath = "system-assets/default-marker.png"

// MarkerDeletable 判断注销清理时是否应删除用户头像 marker（B6a-14/C2）。
// 用精确相等替换原先的 strings.Contains("default-marker") 子串判断，避免误伤合法路径。
func (i *AccountCleanupInfo) MarkerDeletable() bool {
	return i.MarkerPath != "" && i.MarkerPath != DefaultAvatarMarkerPath
}

func (s *Service) GetFamily(ctx context.Context, userID string) (*FamilyInfo, error) {
	user, err := s.pool.Queries().GetUserByID(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("get user: %w", err)
	}

	currentFamilyID := util.ToString(user.CurrentFamilyID)
	if currentFamilyID == "" {
		// 无当前家庭（如 open 默认用户）返回空信息而非 500。
		return &FamilyInfo{}, nil
	}
	family, err := s.pool.Queries().GetFamilyByID(ctx, currentFamilyID)
	if err != nil {
		return nil, fmt.Errorf("get family: %w", err)
	}

	members, err := s.pool.Queries().ListFamilyMembers(ctx, util.ToString(user.CurrentFamilyID))
	if err != nil {
		return nil, fmt.Errorf("list members: %w", err)
	}

	info := &FamilyInfo{
		FamilyID:   family.ID,
		IsPersonal: family.IsPersonal,
		Members:    make([]Member, 0, len(members)),
	}

	for _, m := range members {
		member := Member{
			UserID:   m.UserID,
			Avatar:   s.avatarURL(m.Avatar, m.UserID),
			Nickname: util.ToInterface(m.Nickname),
			Role:     m.Role,
			JoinedAt: m.JoinedAt.Time,
		}
		info.Members = append(info.Members, member)
		if m.Role == "owner" {
			info.OwnerID = m.UserID
		}
	}

	if info.IsPersonal {
		info.OwnerID = userID
	}

	return info, nil
}

func (s *Service) CreateFamily(ctx context.Context, userID string) (string, error) {
	newFamilyID, err := util.NewUUID()
	if err != nil {
		return "", fmt.Errorf("generate family id: %w", err)
	}

	membershipID, err := util.NewUUID()
	if err != nil {
		return "", fmt.Errorf("generate membership id: %w", err)
	}

	if err := db.WithTxDeferrable(ctx, s.pool.Pool(), func(ctx context.Context, q *sqlc.Queries) error {

		user, err := q.GetUserByID(ctx, userID)
		if err != nil {
			return fmt.Errorf("get user: %w", err)
		}
		if util.ToString(user.CurrentFamilyID) != util.ToString(user.PersonalFamilyID) {
			return ErrAlreadyInFamily
		}

		if _, err := q.CreateFamily(ctx, sqlc.CreateFamilyParams{
			ID:         newFamilyID,
			IsPersonal: false,
		}); err != nil {
			return fmt.Errorf("create family: %w", err)
		}

		if err := q.DeleteFamilyMembership(ctx, sqlc.DeleteFamilyMembershipParams{
			FamilyID: util.ToString(user.PersonalFamilyID),
			UserID:   userID,
		}); err != nil {
			return fmt.Errorf("leave personal family: %w", err)
		}

		if _, err := q.UpsertFamilyMembership(ctx, sqlc.UpsertFamilyMembershipParams{
			ID:       membershipID,
			FamilyID: newFamilyID,
			UserID:   userID,
			Role:     "owner",
		}); err != nil {
			return fmt.Errorf("join new family: %w", err)
		}

		if err := q.UpdateUserCurrentFamily(ctx, sqlc.UpdateUserCurrentFamilyParams{
			ID:              userID,
			CurrentFamilyID: pgtype.Text{String: newFamilyID, Valid: true},
		}); err != nil {
			return fmt.Errorf("update current family: %w", err)
		}

		return nil
	}); err != nil {
		if dbx.IsUniqueViolation(err) {
			return "", ErrAlreadyInFamily
		}
		return "", err
	}

	return newFamilyID, nil
}

func (s *Service) JoinFamily(ctx context.Context, userID, targetFamilyID string) error {
	if targetFamilyID == "" {
		return ErrFamilyNotFound
	}

	targetFamily, err := s.pool.Queries().GetFamilyByID(ctx, targetFamilyID)
	if err != nil {
		return ErrFamilyNotFound
	}
	if targetFamily.IsPersonal {
		return ErrTargetIsPersonalFamily
	}

	user, err := s.pool.Queries().GetUserByID(ctx, userID)
	if err != nil {
		return fmt.Errorf("get user: %w", err)
	}
	if util.ToString(user.CurrentFamilyID) == targetFamilyID {
		return ErrAlreadyInTargetFamily
	}

	// 分布式锁保证跨请求串行；家庭维度已加锁，且 family_members 对 user_id 有唯一约束，
	// 无需在事务内重新读取用户状态来补偿并发。
	// 这里先根据加锁前的状态预估源家庭，仅用于决定需要上锁的家庭集合。
	// 注意：读取 user 与加锁/事务执行之间存在极窄窗口，若 CurrentFamilyID 在此期间被其他请求改变，
	// 锁定的源家庭可能与事务实际操作的源家庭不一致；该小概率并发由 family_members.user_id 唯一约束
	// 与 RepeatableRead 事务兜底，不引入锁后重读等复杂机制。
	possibleSourceFamilyID := util.ToString(user.CurrentFamilyID)
	if possibleSourceFamilyID == util.ToString(user.PersonalFamilyID) {
		possibleSourceFamilyID = ""
	}

	lockKeys := []string{"lock:family:" + targetFamilyID}
	if possibleSourceFamilyID != "" {
		lockKeys = append(lockKeys, "lock:family:"+possibleSourceFamilyID)
	}
	sort.Strings(lockKeys)

	ok, lockTokens, err := s.lock.TryLocks(ctx, lockKeys)
	if err != nil {
		return fmt.Errorf("acquire locks: %w", err)
	}
	if !ok {
		return ErrOperationInProgress
	}
	//nolint:errcheck
	defer s.lock.UnlockMany(context.WithoutCancel(ctx), lockTokens) //nolint:errcheck

	var sourceFamilyID string
	if err := db.WithTxDeferrable(ctx, s.pool.Pool(), func(ctx context.Context, q *sqlc.Queries) error {
		currentFamilyID := util.ToString(user.CurrentFamilyID)
		personalFamilyID := util.ToString(user.PersonalFamilyID)

		if currentFamilyID == targetFamilyID {
			return ErrAlreadyInTargetFamily
		}

		var isOwnerOfSource bool
		if currentFamilyID != "" && currentFamilyID != personalFamilyID {
			// sourceFamilyID 与 possibleSourceFamilyID 均来自加锁前快照；若期间状态变化，
			// 由 family_members.user_id 唯一约束与事务语义兜底，不引入锁后重读。
			sourceFamilyID = currentFamilyID
			owner, err := q.GetFamilyOwner(ctx, sourceFamilyID)
			if err != nil {
				return fmt.Errorf("get source family owner: %w", err)
			}
			isOwnerOfSource = owner == userID
		}

		targetCount, err := q.CountFamilyMembers(ctx, targetFamilyID)
		if err != nil {
			return fmt.Errorf("count target family members: %w", err)
		}

		if isOwnerOfSource {
			sourceCount, err := q.CountFamilyMembers(ctx, sourceFamilyID)
			if err != nil {
				return fmt.Errorf("count source family members: %w", err)
			}
			if targetCount+sourceCount > 6 {
				return ErrFamilyFull
			}
		} else {
			if targetCount >= 6 {
				return ErrFamilyFull
			}
		}

		return s.joinFamilyTx(ctx, q, user, targetFamilyID, isOwnerOfSource)
	}); err != nil {
		return err
	}

	s.invalidateFamilySummary(ctx, sourceFamilyID)
	s.invalidateFamilySummary(ctx, targetFamilyID)

	return nil
}

func (s *Service) joinFamilyTx(ctx context.Context, q *sqlc.Queries, user sqlc.GetUserByIDRow, targetFamilyID string, isOwnerOfSource bool) error {
	if isOwnerOfSource {
		// 设计决策：家庭 owner 加入别人家庭时，会把源家庭全体成员一起迁入目标家庭并解散源家庭
		//（家庭合并语义）。/family/invite-link/join 使用此实现，
		// owner 扫码加入即触发整家合并——这是预期行为，非越权/误删。

		sourceFamilyID := util.ToString(user.CurrentFamilyID)

		members, err := q.ListFamilyMembers(ctx, sourceFamilyID)
		if err != nil {
			return fmt.Errorf("list source members: %w", err)
		}

		memberIDs := make([]string, 0, len(members))
		membershipIDs := make([]string, 0, len(members))
		for _, m := range members {
			membershipID, err := util.NewUUID()
			if err != nil {
				return fmt.Errorf("generate membership id: %w", err)
			}
			membershipIDs = append(membershipIDs, membershipID)
			memberIDs = append(memberIDs, m.UserID)
		}

		if len(memberIDs) > 0 {
			if err := q.BatchUpsertFamilyMembership(ctx, sqlc.BatchUpsertFamilyMembershipParams{
				Ids:      membershipIDs,
				FamilyID: targetFamilyID,
				UserIds:  memberIDs,
			}); err != nil {
				return fmt.Errorf("move members to target: %w", err)
			}

			if err := q.UpdateUsersCurrentFamily(ctx, sqlc.UpdateUsersCurrentFamilyParams{
				Column1:         memberIDs,
				CurrentFamilyID: pgtype.Text{String: targetFamilyID, Valid: true},
			}); err != nil {
				return fmt.Errorf("update members current family: %w", err)
			}
		}

		if sourceFamilyID != "" {
			hasCovers, err := q.CountFamilyDailyCovers(ctx, sourceFamilyID)
			if err != nil {
				return fmt.Errorf("count source family daily covers: %w", err)
			}
			if hasCovers > 0 {
				if err := q.MigrateFamilyDailyCovers(ctx, sqlc.MigrateFamilyDailyCoversParams{
					FamilyID:   sourceFamilyID,
					FamilyID_2: targetFamilyID,
				}); err != nil {
					return fmt.Errorf("migrate family daily covers: %w", err)
				}
			}
			if err := q.DeleteFamilyDailyCovers(ctx, sourceFamilyID); err != nil {
				return fmt.Errorf("delete source family daily covers: %w", err)
			}
		}

		if err := q.DeleteFamilyMembers(ctx, sourceFamilyID); err != nil {
			return fmt.Errorf("delete source memberships: %w", err)
		}
		if err := q.DeleteFamily(ctx, sourceFamilyID); err != nil {
			return fmt.Errorf("delete source family: %w", err)
		}

		return nil
	}

	sourceFamilyID := util.ToString(user.CurrentFamilyID)

	if err := q.DeleteFamilyMembership(ctx, sqlc.DeleteFamilyMembershipParams{
		FamilyID: sourceFamilyID,
		UserID:   user.ID,
	}); err != nil {
		return fmt.Errorf("leave current family: %w", err)
	}

	if sourceFamilyID != "" {
		hasCovers, err := q.CountFamilyDailyCovers(ctx, sourceFamilyID)
		if err != nil {
			return fmt.Errorf("count source family daily covers: %w", err)
		}
		if hasCovers > 0 {
			if err := q.MigrateUserDailyCoversToFamily(ctx, sqlc.MigrateUserDailyCoversToFamilyParams{
				FamilyID:   sourceFamilyID,
				UserID:     user.ID,
				FamilyID_2: targetFamilyID,
			}); err != nil {
				return fmt.Errorf("migrate user daily covers to family: %w", err)
			}
		}
	}

	membershipID, err := util.NewUUID()
	if err != nil {
		return fmt.Errorf("generate membership id: %w", err)
	}
	if _, err := q.UpsertFamilyMembership(ctx, sqlc.UpsertFamilyMembershipParams{
		ID:       membershipID,
		FamilyID: targetFamilyID,
		UserID:   user.ID,
		Role:     "member",
	}); err != nil {
		return fmt.Errorf("join target family: %w", err)
	}

	if err := q.UpdateUserCurrentFamily(ctx, sqlc.UpdateUserCurrentFamilyParams{
		ID:              user.ID,
		CurrentFamilyID: pgtype.Text{String: targetFamilyID, Valid: true},
	}); err != nil {
		return fmt.Errorf("update current family: %w", err)
	}

	return nil
}

func (s *Service) LeaveFamily(ctx context.Context, userID string) error {
	user, err := s.pool.Queries().GetUserByID(ctx, userID)
	if err != nil {
		return fmt.Errorf("get user: %w", err)
	}

	if util.ToString(user.CurrentFamilyID) == util.ToString(user.PersonalFamilyID) {
		return ErrNotInNormalFamily
	}

	ownerID, err := s.pool.Queries().GetFamilyOwner(ctx, util.ToString(user.CurrentFamilyID))
	if err == nil && ownerID == userID {
		return ErrOwnerCannotLeave
	}
	if err != nil && !stderrors.Is(err, pgx.ErrNoRows) {
		// 无 owner 记录属异常数据（告警并放行），其余 DB 错误不得静默吞掉。
		return fmt.Errorf("get family owner: %w", err)
	}
	if stderrors.Is(err, pgx.ErrNoRows) {
		slog.WarnContext(ctx, "family has no owner record", slog.String("user_id", userID))
	}

	familyID := util.ToString(user.CurrentFamilyID)
	lockKeys := []string{"lock:family:" + familyID}
	ok, lockTokens, err := s.lock.TryLocks(ctx, lockKeys)
	if err != nil {
		return fmt.Errorf("acquire locks: %w", err)
	}
	if !ok {
		return ErrOperationInProgress
	}
	//nolint:errcheck
	defer s.lock.UnlockMany(context.WithoutCancel(ctx), lockTokens) //nolint:errcheck

	if err := db.WithTxDeferrable(ctx, s.pool.Pool(), func(ctx context.Context, q *sqlc.Queries) error {
		return s.leaveToPersonalTx(ctx, q, user)
	}); err != nil {
		return err
	}

	s.invalidateFamilySummary(ctx, familyID)
	return nil
}

func (s *Service) RemoveMember(ctx context.Context, ownerID, targetUserID string) error {
	if ownerID == targetUserID {
		return ErrCannotRemoveSelf
	}

	owner, err := s.pool.Queries().GetUserByID(ctx, ownerID)
	if err != nil {
		return fmt.Errorf("get owner: %w", err)
	}

	if owner.CurrentFamilyID == owner.PersonalFamilyID {
		return ErrNotInNormalFamily
	}

	familyID := util.ToString(owner.CurrentFamilyID)
	familyOwner, err := s.pool.Queries().GetFamilyOwner(ctx, familyID)
	if err != nil || familyOwner != ownerID {
		return ErrNotOwner
	}

	lockKeys := []string{"lock:family:" + familyID}
	ok, lockTokens, err := s.lock.TryLocks(ctx, lockKeys)
	if err != nil {
		return fmt.Errorf("acquire locks: %w", err)
	}
	if !ok {
		return ErrOperationInProgress
	}
	//nolint:errcheck
	defer s.lock.UnlockMany(context.WithoutCancel(ctx), lockTokens) //nolint:errcheck

	if err := db.WithTxDeferrable(ctx, s.pool.Pool(), func(ctx context.Context, q *sqlc.Queries) error {
		target, err := q.GetUserByID(ctx, targetUserID)
		if err != nil {
			return fmt.Errorf("get target user: %w", err)
		}

		if target.CurrentFamilyID != owner.CurrentFamilyID {
			return ErrTargetNotInFamily
		}

		if familyOwner == targetUserID {
			return ErrCannotRemoveOwner
		}

		return s.leaveToPersonalTx(ctx, q, target)
	}); err != nil {
		return err
	}

	s.invalidateFamilySummary(ctx, familyID)
	return nil
}

func (s *Service) DissolveFamily(ctx context.Context, ownerID string) error {
	owner, err := s.pool.Queries().GetUserByID(ctx, ownerID)
	if err != nil {
		return fmt.Errorf("get owner: %w", err)
	}

	if owner.CurrentFamilyID == owner.PersonalFamilyID {
		return ErrCannotDissolvePersonal
	}

	familyID := util.ToString(owner.CurrentFamilyID)
	familyOwner, err := s.pool.Queries().GetFamilyOwner(ctx, familyID)
	if err != nil || familyOwner != ownerID {
		return ErrNotOwner
	}

	lockKeys := []string{"lock:family:" + familyID}
	ok, lockTokens, err := s.lock.TryLocks(ctx, lockKeys)
	if err != nil {
		return fmt.Errorf("acquire locks: %w", err)
	}
	if !ok {
		return ErrOperationInProgress
	}
	defer s.lock.UnlockMany(context.WithoutCancel(ctx), lockTokens) //nolint:errcheck

	if err := db.WithTxDeferrable(ctx, s.pool.Pool(), func(ctx context.Context, q *sqlc.Queries) error {
		return s.dissolveFamilyTx(ctx, q, familyID)
	}); err != nil {
		return err
	}

	s.invalidateFamilySummary(ctx, familyID)
	return nil
}

func (s *Service) leaveToPersonalTx(ctx context.Context, q *sqlc.Queries, user sqlc.GetUserByIDRow) error {
	if err := q.DeleteFamilyMembership(ctx, sqlc.DeleteFamilyMembershipParams{
		FamilyID: util.ToString(user.CurrentFamilyID),
		UserID:   user.ID,
	}); err != nil {
		return fmt.Errorf("delete membership: %w", err)
	}

	personalFamilyID := util.ToString(user.PersonalFamilyID)
	// PersonalFamilyID 为空（open 模式默认用户/异常数据）时跳过重入个人家庭，
	// 否则空 family_id 命中 family_members 外键导致退出/被移除/注销永久 500。
	if personalFamilyID != "" {
		membershipID, err := util.NewUUID()
		if err != nil {
			return fmt.Errorf("generate membership id: %w", err)
		}
		if _, err := q.UpsertFamilyMembership(ctx, sqlc.UpsertFamilyMembershipParams{
			ID:       membershipID,
			FamilyID: personalFamilyID,
			UserID:   user.ID,
			Role:     "owner",
		}); err != nil {
			return fmt.Errorf("rejoin personal family: %w", err)
		}
	}

	sourceFamilyID := util.ToString(user.CurrentFamilyID)
	targetFamilyID := personalFamilyID
	if sourceFamilyID != "" && targetFamilyID != "" && sourceFamilyID != targetFamilyID {
		hasCovers, err := q.CountFamilyDailyCovers(ctx, sourceFamilyID)
		if err != nil {
			return fmt.Errorf("count source family daily covers: %w", err)
		}
		if hasCovers > 0 {
			if err := q.MigrateUserDailyCoversToFamily(ctx, sqlc.MigrateUserDailyCoversToFamilyParams{
				FamilyID:   sourceFamilyID,
				UserID:     user.ID,
				FamilyID_2: targetFamilyID,
			}); err != nil {
				return fmt.Errorf("migrate user daily covers to personal family: %w", err)
			}
		}
	}

	// B-1：无个人家庭（open 模式默认用户/异常数据）时 current_family_id 置 NULL——
	// 空串 Valid=true 会命中 families 外键返回 500（与下方 dissolveFamilyTx 的 NULL 写法一致）。
	currentFamilyID := pgtype.Text{}
	if user.PersonalFamilyID.Valid && user.PersonalFamilyID.String != "" {
		currentFamilyID = pgtype.Text{String: user.PersonalFamilyID.String, Valid: true}
	}
	if err := q.UpdateUserCurrentFamily(ctx, sqlc.UpdateUserCurrentFamilyParams{
		ID:              user.ID,
		CurrentFamilyID: currentFamilyID,
	}); err != nil {
		return fmt.Errorf("update current family: %w", err)
	}

	return nil
}

func (s *Service) dissolveFamilyTx(ctx context.Context, q *sqlc.Queries, familyID string) error {
	members, err := q.ListFamilyMemberPersonalFamilies(ctx, familyID)
	if err != nil {
		return fmt.Errorf("list members: %w", err)
	}

	membershipIDs := make([]string, 0, len(members))
	memberFamilyIDs := make([]string, 0, len(members))
	memberUserIDs := make([]string, 0, len(members))
	migrateUserIDs := make([]string, 0, len(members))
	migrateFamilyIDs := make([]string, 0, len(members))
	noPersonalFamilyUserIDs := make([]string, 0, len(members))
	for _, m := range members {
		personalFamilyID := util.ToString(m.PersonalFamilyID)
		if personalFamilyID == "" {
			// open 模式默认用户可能无个人家庭：跳过其个人家庭重入，仅清空 current_family_id，
			// 避免 dissolveFamilyTx 硬失败导致家庭永久无法解散/owner 注销失败（与 leaveToPersonalTx 一致）。
			noPersonalFamilyUserIDs = append(noPersonalFamilyUserIDs, m.ID)
			continue
		}
		membershipID, err := util.NewUUID()
		if err != nil {
			return fmt.Errorf("generate membership id: %w", err)
		}
		membershipIDs = append(membershipIDs, membershipID)
		memberFamilyIDs = append(memberFamilyIDs, personalFamilyID)
		memberUserIDs = append(memberUserIDs, m.ID)
		if personalFamilyID != familyID {
			migrateUserIDs = append(migrateUserIDs, m.ID)
			migrateFamilyIDs = append(migrateFamilyIDs, personalFamilyID)
		}
	}

	if len(memberUserIDs) > 0 {
		// 成员批量回到各自个人家庭（owner 角色），避免逐用户 UpsertFamilyMembership（N+1）。
		if err := q.BatchUpsertFamilyMembershipOwner(ctx, sqlc.BatchUpsertFamilyMembershipOwnerParams{
			Ids:       membershipIDs,
			FamilyIds: memberFamilyIDs,
			UserIds:   memberUserIDs,
		}); err != nil {
			return fmt.Errorf("rejoin members to personal families: %w", err)
		}

		// 封面迁移必须在 DeleteFamilyDailyCovers 之前执行（源数据仍在 familyID 下）。
		if len(migrateUserIDs) > 0 {
			if err := q.BatchMigrateUsersDailyCoversToPersonal(ctx, sqlc.BatchMigrateUsersDailyCoversToPersonalParams{
				SourceFamilyID:    familyID,
				UserIds:           migrateUserIDs,
				PersonalFamilyIds: migrateFamilyIDs,
			}); err != nil {
				return fmt.Errorf("migrate members daily covers to personal families: %w", err)
			}
		}

		if err := q.BatchUpdateUsersCurrentFamilyToPersonal(ctx, sqlc.BatchUpdateUsersCurrentFamilyToPersonalParams{
			UserIds:           memberUserIDs,
			PersonalFamilyIds: memberFamilyIDs,
		}); err != nil {
			return fmt.Errorf("update members current family: %w", err)
		}
	}

	// 在删除家庭前清空无个人家庭成员的 current_family_id，避免 ON DELETE RESTRICT 外键失败。
	if len(noPersonalFamilyUserIDs) > 0 {
		if err := q.UpdateUsersCurrentFamily(ctx, sqlc.UpdateUsersCurrentFamilyParams{
			Column1:         noPersonalFamilyUserIDs,
			CurrentFamilyID: pgtype.Text{},
		}); err != nil {
			return fmt.Errorf("clear current family for members without personal family: %w", err)
		}
	}

	if err := q.DeleteFamilyDailyCovers(ctx, familyID); err != nil {
		return fmt.Errorf("delete family daily covers: %w", err)
	}

	if err := q.ClearTrajectoryFamilyID(ctx, familyID); err != nil {
		return fmt.Errorf("clear trajectory family id: %w", err)
	}
	if err := q.DeleteFamilyMembers(ctx, familyID); err != nil {
		return fmt.Errorf("delete memberships: %w", err)
	}
	if err := q.DeleteFamily(ctx, familyID); err != nil {
		return fmt.Errorf("delete family: %w", err)
	}

	return nil
}

// DeleteAccount 注销指定用户账号。所有 DB 提交后的清理（session 失效、物理文件删除）由调用方负责。
// 风险接受：本函数持有家庭锁但不持有封面锁；极端情况下，并发封面刷新可能为即将被删除的家庭
// 生成新的轨迹图文件，随后家庭被删，留下未引用的孤儿文件。该概率极小，由后台
// runCleanupOrphanTrajMaps/runCleanupOrphanFiles 任务兜底回收（AGENTS.md §4.4）。
func (s *Service) DeleteAccount(ctx context.Context, userID string) (*AccountCleanupInfo, error) {
	// 用户级锁防止同一用户并发注销产生竞态；必须在读取用户状态前获取。
	deleteAccountLockKey := "lock:delete_account:" + userID
	ok, deleteAccountLockToken, err := s.lock.TryLock(ctx, deleteAccountLockKey)
	if err != nil {
		return nil, fmt.Errorf("acquire delete account lock: %w", err)
	}
	if !ok {
		return nil, ErrOperationInProgress
	}
	defer func() {
		if err := s.lock.Unlock(context.WithoutCancel(ctx), deleteAccountLockKey, deleteAccountLockToken); err != nil {
			slog.ErrorContext(ctx, "unlock delete_account failed", slog.String("user_id", userID), slog.Any("error", err))
		}
	}()

	// 在用户级锁保护下读取用户状态，用于确定需要获取哪些家庭锁以及驱动事务决策。
	// 家庭级锁随后按此快照获取；JoinFamily/LeaveFamily 等操作不持有用户级注销锁，
	// 因此存在极窄窗口使家庭关系在读取后发生变化。该窗口由 DB 唯一约束与
	// WithTxDeferrable 重试兜底，不引入锁后重读等复杂机制（AGENTS.md §二「简单优先」）。
	user, err := s.pool.Queries().GetUserByID(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("get user: %w", err)
	}

	familyID := util.ToString(user.CurrentFamilyID)
	personalFamilyID := util.ToString(user.PersonalFamilyID)
	inNormalFamily := familyID != personalFamilyID

	// 家庭锁保证家庭变更串行；TryLocks 内部按字典序获取，避免死锁。
	var familyLockKeys []string
	if familyID != "" {
		familyLockKeys = append(familyLockKeys, "lock:family:"+familyID)
	}
	if inNormalFamily && personalFamilyID != "" && personalFamilyID != familyID {
		familyLockKeys = append(familyLockKeys, "lock:family:"+personalFamilyID)
	}
	if len(familyLockKeys) > 0 {
		familyLocksOk, familyLockTokens, err := s.lock.TryLocks(ctx, familyLockKeys)
		if err != nil {
			return nil, fmt.Errorf("acquire family lock: %w", err)
		}
		if !familyLocksOk {
			return nil, ErrOperationInProgress
		}
		defer func() {
			if uerr := s.lock.UnlockMany(context.WithoutCancel(ctx), familyLockTokens); uerr != nil {
				slog.ErrorContext(ctx, "unlock family locks failed", slog.String("user_id", userID), slog.Any("error", uerr))
			}
		}()
	}

	// 注销事务（含家庭解散）为纯 DB 操作，亚秒级完成；物理文件删除、session 清理等外部 IO
	// 均在事务提交后由调用方执行，锁内无外部 IO。advisory lock 无 TTL、连接断开自动释放，
	// 不引入续期机制。

	var txPaths []string
	var txAffectedUserIDs []string
	var coverFileIDs []string
	var markerPath, markerStorage string

	if err := db.WithTxDeferrable(ctx, s.pool.Pool(), func(ctx context.Context, q *sqlc.Queries) error {
		var localPaths []string
		var localAffectedUserIDs []string

		// 不加 FOR UPDATE：用户级 advisory 锁与家庭级 advisory 锁已保证加锁后没有其他并发请求能改变该用户的
		// 家庭归属（02d §6.1「锁后不重读」）。并发 DB 行更新由 WithTxDeferrable 重试兜底。

		// 以用户级锁下读取的家庭关系为准；家庭级锁按该快照获取。
		var familyIDsToClean []string
		if personalFamilyID != "" {
			familyIDsToClean = append(familyIDsToClean, personalFamilyID)
		}
		isOwner := false
		if inNormalFamily {
			familyOwner, err := q.GetFamilyOwner(ctx, familyID)
			if err == nil && familyOwner == userID {
				isOwner = true
				familyIDsToClean = append(familyIDsToClean, familyID)
			}
		}
		var err error
		coverFileIDs, err = s.listCoverFileIDs(ctx, q, familyIDsToClean)
		if err != nil {
			return fmt.Errorf("list cover file ids: %w", err)
		}
		if scanErr := q.QueryRowRaw(ctx,
			`SELECT marker_path, storage_type FROM user_avatar_markers WHERE user_id = $1`,
			userID,
		).Scan(&markerPath, &markerStorage); scanErr != nil && !stderrors.Is(scanErr, pgx.ErrNoRows) {
			return fmt.Errorf("get avatar marker: %w", scanErr)
		}

		if inNormalFamily {
			if isOwner {
				members, err := q.ListFamilyMembers(ctx, familyID)
				if err != nil {
					return fmt.Errorf("list members: %w", err)
				}
				for _, m := range members {
					localAffectedUserIDs = append(localAffectedUserIDs, m.UserID)
				}
			} else {
				if err := s.leaveToPersonalTx(ctx, q, user); err != nil {
					return fmt.Errorf("leave to personal: %w", err)
				}
			}
		}

		if inNormalFamily && isOwner {
			if err := s.dissolveFamilyTx(ctx, q, familyID); err != nil {
				return fmt.Errorf("dissolve family: %w", err)
			}
		}

		if err := q.UpdateUserCurrentFamily(ctx, sqlc.UpdateUserCurrentFamilyParams{
			ID:              userID,
			CurrentFamilyID: pgtype.Text{},
		}); err != nil {
			return fmt.Errorf("clear current family: %w", err)
		}
		if err := q.UpdateUserPersonalFamily(ctx, sqlc.UpdateUserPersonalFamilyParams{
			ID:               userID,
			PersonalFamilyID: pgtype.Text{},
		}); err != nil {
			return fmt.Errorf("clear personal family: %w", err)
		}

		// 无个人家庭的用户（如 open 默认用户）跳过删除，避免以空主键误删。
		if personalFamilyID != "" {
			if err := q.DeleteFamilyDailyCovers(ctx, personalFamilyID); err != nil {
				return fmt.Errorf("delete personal family daily covers: %w", err)
			}
			if err := q.DeleteFamily(ctx, personalFamilyID); err != nil {
				return fmt.Errorf("delete personal family: %w", err)
			}
		}

		if err := q.NullifyOrdersByUser(ctx, pgtype.Text{String: userID, Valid: true}); err != nil {
			return fmt.Errorf("nullify orders: %w", err)
		}

		if err := q.DeleteAPIKeyByUser(ctx, userID); err != nil {
			return fmt.Errorf("delete api keys: %w", err)
		}

		if err := q.DeleteUserInviteCodeByUserID(ctx, userID); err != nil {
			return fmt.Errorf("delete invite code: %w", err)
		}

		if err := q.DeleteUserCommonAddresses(ctx, userID); err != nil {
			return fmt.Errorf("delete user common addresses: %w", err)
		}

		if err := q.DeleteAIDailyQuotaUsageByUserID(ctx, userID); err != nil {
			return fmt.Errorf("delete ai daily quota usage: %w", err)
		}

		lastID := ""
		for {
			rows, err := q.ListFilesByCreator(ctx, sqlc.ListFilesByCreatorParams{
				CreatedBy: pgtype.Text{String: userID, Valid: true},
				ID:        lastID,
				Limit:     1000,
			})
			if err != nil {
				return fmt.Errorf("list files by creator: %w", err)
			}
			if len(rows) == 0 {
				break
			}
			for _, f := range rows {
				localPaths = append(localPaths, f.Path)
				lastID = f.ID
			}
			if len(rows) < 1000 {
				break
			}
		}

		if err := q.DeleteUser(ctx, userID); err != nil {
			return fmt.Errorf("delete user: %w", err)
		}

		txPaths = localPaths
		txAffectedUserIDs = localAffectedUserIDs
		return nil
	}); err != nil {
		return nil, err
	}

	// 去重，避免 owner 注销时当前用户被重复清理 session。
	affectedUserIDSet := make(map[string]struct{}, len(txAffectedUserIDs)+1)
	for _, id := range txAffectedUserIDs {
		if id != "" {
			affectedUserIDSet[id] = struct{}{}
		}
	}
	affectedUserIDSet[userID] = struct{}{}
	affectedUserIDs := make([]string, 0, len(affectedUserIDSet))
	for id := range affectedUserIDSet {
		affectedUserIDs = append(affectedUserIDs, id)
	}
	s.invalidateFamilySummary(ctx, familyID)
	s.invalidateFamilySummary(ctx, personalFamilyID)
	// 群主注销后，被迁回个人家庭的成员其个人家庭汇总缓存也可能失效，尽量逐一清理。
	for _, affectedUserID := range affectedUserIDs {
		if affectedUserID == userID {
			continue
		}
		affectedUser, err := s.pool.Queries().GetUserByID(ctx, affectedUserID)
		if err != nil {
			slog.WarnContext(ctx, "delete account: get affected user for summary invalidation failed", slog.String("user_id", affectedUserID), slog.Any("error", err))
			continue
		}
		s.invalidateFamilySummary(ctx, util.ToString(affectedUser.CurrentFamilyID))
		s.invalidateFamilySummary(ctx, util.ToString(affectedUser.PersonalFamilyID))
	}

	return &AccountCleanupInfo{
		Paths:            txPaths,
		CoverFileIDs:     coverFileIDs,
		MarkerPath:       markerPath,
		MarkerStorage:    markerStorage,
		FamilyID:         familyID,
		PersonalFamilyID: personalFamilyID,
		AffectedUserIDs:  affectedUserIDs,
	}, nil
}

func (s *Service) listCoverFileIDs(ctx context.Context, q *sqlc.Queries, familyIDs []string) ([]string, error) {
	if len(familyIDs) == 0 {
		return nil, nil
	}
	placeholders := make([]string, len(familyIDs))
	args := make([]interface{}, len(familyIDs))
	for i, fid := range familyIDs {
		placeholders[i] = fmt.Sprintf("$%d", i+1)
		args[i] = fid
	}
	query := fmt.Sprintf(
		`SELECT cover_file_id, manual_cover_file_id FROM family_daily_covers WHERE family_id IN (%s)`,
		strings.Join(placeholders, ","),
	)
	rows, err := q.QueryRaw(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query cover file ids: %w", err)
	}
	defer rows.Close()

	fileIDSet := make(map[string]struct{})
	for rows.Next() {
		var coverFileID, manualCoverFileID *string
		if err := rows.Scan(&coverFileID, &manualCoverFileID); err != nil {
			return nil, fmt.Errorf("scan cover file id: %w", err)
		}
		if coverFileID != nil && *coverFileID != "" {
			fileIDSet[*coverFileID] = struct{}{}
		}
		if manualCoverFileID != nil && *manualCoverFileID != "" {
			fileIDSet[*manualCoverFileID] = struct{}{}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate cover file id rows: %w", err)
	}

	fileIDs := make([]string, 0, len(fileIDSet))
	for id := range fileIDSet {
		fileIDs = append(fileIDs, id)
	}
	return fileIDs, nil
}

func (s *Service) avatarURL(avatar pgtype.Text, userID string) interface{} {
	return util.AvatarURLOrDefault(avatar, userID, s.defaultAvatar)
}
