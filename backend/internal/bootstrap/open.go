// Package bootstrap provides startup seeding logic.
package bootstrap

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"

	"papafeiji/backend/internal/config"
	"papafeiji/backend/internal/db"
	"papafeiji/backend/internal/db/sqlc"
	"papafeiji/backend/internal/middleware"
	dbx "papafeiji/backend/pkg/db"
	"papafeiji/backend/pkg/util"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// SeedOpenBackend ensures the open-source backend has a default user and an
// API key record matching OPEN_API_KEY. Worker-routed requests use this key to
// authenticate, so the record must exist before the first request.
func SeedOpenBackend(ctx context.Context, cfg *config.Config, pool *db.Pool, sessions *middleware.SessionManager) error {
	if cfg.DeploymentMode != "open" {
		return nil
	}
	if cfg.OpenAPIKey == "" {
		slog.WarnContext(ctx, "OPEN_API_KEY is empty; Worker-routed requests will fail with 401")
		return nil
	}

	queries := pool.Queries()
	keyHash := hashAPIKey(cfg.OpenAPIKey)
	defaultOpenID := "open_default_user"

	// Ensure default open user exists.
	user, err := queries.GetUserByOpenID(ctx, defaultOpenID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("lookup default open user: %w", err)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		userID, err := util.NewUUID()
		if err != nil {
			return fmt.Errorf("generate default user id: %w", err)
		}

		created, err := queries.CreateUser(ctx, sqlc.CreateUserParams{
			ID:                userID,
			OpenID:            defaultOpenID,
			Unionid:           pgtype.Text{String: "", Valid: false},
			PhoneNumber:       pgtype.Text{String: "", Valid: false},
			Avatar:            pgtype.Text{String: "", Valid: false},
			AvatarFileID:      pgtype.Text{String: "", Valid: false},
			Nickname:          pgtype.Text{String: "Open User", Valid: true},
			UserType:          "wechat",
			PhoneBindTime:     pgtype.Timestamptz{Valid: false},
			AutoRecordEnabled: true,
			SessionKey:        pgtype.Text{String: "", Valid: false},
			PersonalFamilyID:  pgtype.Text{String: "", Valid: false},
			CurrentFamilyID:   pgtype.Text{String: "", Valid: false},
			InvitedBy:         pgtype.Text{String: "", Valid: false},
		})
		if err != nil {
			if dbx.IsUniqueViolation(err) {
				// 多副本并发启动竞态——另一副本已创建，重新读取即可。
				existing, gerr := queries.GetUserByOpenID(ctx, defaultOpenID)
				if gerr != nil {
					return fmt.Errorf("re-fetch default open user: %w", gerr)
				}
				user = existing
			} else {
				return fmt.Errorf("create default open user: %w", err)
			}
		} else {
			user.ID = created.ID
			slog.InfoContext(ctx, "created default open user", slog.String("user_id", user.ID))
		}
	} else {
		slog.InfoContext(ctx, "found existing default open user", slog.String("user_id", user.ID))
	}

	// Sync API key: if the configured OPEN_API_KEY differs from the one stored
	// for the default user, delete the old record and create a new one.
	existing, err := queries.GetAPIKeyByUser(ctx, user.ID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("lookup existing api key: %w", err)
	}
	if err == nil && existing.KeyHash == keyHash {
		slog.InfoContext(ctx, "open backend api key already synced", slog.String("user_id", user.ID))
		return nil
	}

	if err == nil {
		keyID, err := util.NewUUID()
		if err != nil {
			return fmt.Errorf("generate api key id: %w", err)
		}
		// 先提交替换事务，成功后再 flush 所有 session——
		// 避免事务失败时已产生“全部用户被登出但 key 未更换”的副作用。
		if txErr := db.WithTx(ctx, pool.Pool(), func(txCtx context.Context, tx *sqlc.Queries) error {
			if err := tx.DeleteAPIKeyByUser(txCtx, user.ID); err != nil {
				return fmt.Errorf("delete stale open api key: %w", err)
			}
			_, err := tx.CreateAPIKey(txCtx, sqlc.CreateAPIKeyParams{
				ID:      keyID,
				UserID:  user.ID,
				KeyHash: keyHash,
				ApiKey:  cfg.OpenAPIKey,
			})
			if err != nil {
				return fmt.Errorf("create open api key: %w", err)
			}
			return nil
		}); txErr != nil {
			return fmt.Errorf("sync open api key: %w", txErr)
		}

		// API Key 变更 → 强制清除所有 session，使所有用户重新登录，
		// 避免旧 API Key 持有者通过缓存的 session 继续访问。
		if sessions != nil {
			slog.WarnContext(ctx, "OPEN_API_KEY changed; flushing all sessions")
			if flushErr := sessions.FlushAllSessions(ctx); flushErr != nil {
				return fmt.Errorf("flush sessions after key change: %w", flushErr)
			}
		}
		slog.InfoContext(ctx, "open backend api key synced; all sessions flushed", slog.String("user_id", user.ID))
		return nil
	}

	keyID, err := util.NewUUID()
	if err != nil {
		return fmt.Errorf("generate api key id: %w", err)
	}
	_, err = queries.CreateAPIKey(ctx, sqlc.CreateAPIKeyParams{
		ID:      keyID,
		UserID:  user.ID,
		KeyHash: keyHash,
		ApiKey:  cfg.OpenAPIKey,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// ON CONFLICT (user_id) DO NOTHING 命中——并发副本已创建，视为成功。
			slog.InfoContext(ctx, "open backend api key created by concurrent replica", slog.String("user_id", user.ID))
			return nil
		}
		return fmt.Errorf("create default api key: %w", err)
	}
	slog.InfoContext(ctx, "open backend api key created", slog.String("user_id", user.ID))
	return nil
}

func hashAPIKey(raw string) string {
	h := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(h[:])
}
