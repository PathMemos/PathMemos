package file

import (
	"context"
	"errors"
	"fmt"

	"papafeiji/backend/internal/db"
	"papafeiji/backend/internal/db/sqlc"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// RefsAllowPhysicalDelete 是文件物理删除的引用判定谓词（纯函数，无 IO），
// 由 file 包删除路径与 diary 包 deleteIfOnlySelfReferenced 共用，防止双实现漂移。
//
// refs 为 GetFileReferences 的结果（cover / entry / avatar 三类引用）。
// selfEntryID 参数化两种删除语义：
//   - 传空（file 包 DeleteFile / DeletePhysicalIfUnreferenced）：严格零引用语义，
//     cover / entry / avatar 任一引用存在即阻止删除；
//   - 传自身条目 ID（diary 包编辑/删除条目路径）：「引用者全是自己」语义，
//     自身条目对文件的 entry 引用不阻止删除；otherEntryRefCount 为排除自身条目后
//     仍引用该文件的条目数（调用方经 GetDiaryEntriesByImageID 查得，仅在
//     refs.UsedByEntry 为真时需要查询），大于 0 说明还有他人条目引用，阻止删除。
//
// selfEntryID 为空时 otherEntryRefCount 不参与判定，调用方固定传 0。
func RefsAllowPhysicalDelete(refs sqlc.GetFileReferencesRow, selfEntryID string, otherEntryRefCount int) bool {
	if refs.UsedByCover || refs.AvatarUserCount > 0 {
		return false
	}
	if !refs.UsedByEntry {
		return true
	}
	if selfEntryID == "" {
		return false
	}
	return otherEntryRefCount == 0
}

func DeleteFile(ctx context.Context, pool *db.Pool, storage *Storage, fileID, userID string) error {
	var physicalPath, physicalStorageType string
	err := db.WithTx(ctx, pool.Pool(), func(ctx context.Context, q *sqlc.Queries) error {
		file, err := q.GetFileByIDForUpdate(ctx, fileID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrFileNotFound
			}
			return fmt.Errorf("get file: %w", err)
		}

		if file.FileType == "system" {
			return ErrSystemFileDelete
		}

		if !file.CreatedBy.Valid || file.CreatedBy.String == "" {
			return ErrNotFileOwner
		}
		if file.CreatedBy.String != userID {
			return ErrNotFileOwner
		}

		refs, err := q.GetFileReferences(ctx, pgtype.Text{String: fileID, Valid: true})
		if err != nil {
			return fmt.Errorf("check file references: %w", err)
		}

		if !RefsAllowPhysicalDelete(refs, "", 0) {
			return ErrFileInUse
		}

		if err := q.DeleteFile(ctx, fileID); err != nil {
			return fmt.Errorf("delete file record: %w", err)
		}

		if file.FileType == "image" && file.CreatedBy.Valid && file.CreatedBy.String != "" {
			// 扣减失败须回滚整个事务：否则 files 记录已删、image_storage_bytes 未回退，
			// 幽灵字节永久占用用户配额，且孤儿清理任务无法补偿。
			if decErr := q.DecrementUserImageStorage(ctx, sqlc.DecrementUserImageStorageParams{
				ID:                file.CreatedBy.String,
				ImageStorageBytes: file.SizeBytes,
			}); decErr != nil {
				return fmt.Errorf("decrement user image storage: %w", decErr)
			}
		}

		physicalPath = file.Path
		physicalStorageType = file.StorageType
		return nil
	})
	if err != nil {
		return err
	}

	if physicalPath != "" {
		_ = storage.DeleteFile(physicalPath, physicalStorageType) //nolint:errcheck // physical cleanup is best-effort
	}

	return nil
}

func DeletePhysicalIfUnreferenced(ctx context.Context, pool *db.Pool, storage *Storage, fileID string) error {
	var physicalPath, physicalStorageType string
	err := db.WithTx(ctx, pool.Pool(), func(ctx context.Context, q *sqlc.Queries) error {
		file, err := q.GetFileByIDForUpdate(ctx, fileID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrFileNotFound
			}
			return fmt.Errorf("get file: %w", err)
		}

		refs, err := q.GetFileReferences(ctx, pgtype.Text{String: fileID, Valid: true})
		if err != nil {
			return fmt.Errorf("check references: %w", err)
		}

		if !RefsAllowPhysicalDelete(refs, "", 0) {
			return nil
		}

		if err := q.DeleteFile(ctx, fileID); err != nil {
			return fmt.Errorf("delete file record: %w", err)
		}

		if file.FileType == "image" && file.CreatedBy.Valid && file.CreatedBy.String != "" {
			// 扣减失败须回滚整个事务：否则 files 记录已删、image_storage_bytes 未回退，
			// 幽灵字节永久占用用户配额，且孤儿清理任务无法补偿。
			if decErr := q.DecrementUserImageStorage(ctx, sqlc.DecrementUserImageStorageParams{
				ID:                file.CreatedBy.String,
				ImageStorageBytes: file.SizeBytes,
			}); decErr != nil {
				return fmt.Errorf("decrement user image storage: %w", decErr)
			}
		}

		physicalPath = file.Path
		physicalStorageType = file.StorageType
		return nil
	})
	if err != nil {
		return err
	}

	if physicalPath != "" {
		_ = storage.DeleteFile(physicalPath, physicalStorageType) //nolint:errcheck // physical cleanup is best-effort
	}

	return nil
}
