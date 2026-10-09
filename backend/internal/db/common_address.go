package db

import (
	"context"
	"fmt"

	"papafeiji/backend/internal/db/sqlc"

	"github.com/jackc/pgx/v5/pgtype"
)

const commonAddressMaxCount = 10

func SummarizeUserCommonAddresses(ctx context.Context, pool *Pool, userID string) error {
	needsRefresh, err := pool.Queries().NeedsCommonAddressRefresh(ctx, userID)
	if err != nil {
		return fmt.Errorf("check refresh needed: %w", err)
	}
	if !needsRefresh.Bool {
		return nil
	}
	return summarizeUserCommonAddresses(ctx, pool, userID)
}

func summarizeUserCommonAddresses(ctx context.Context, pool *Pool, userID string) error {
	top, err := pool.Queries().ListTopAddressesByUser(ctx, sqlc.ListTopAddressesByUserParams{
		CreatedBy: userID,
		Limit:     commonAddressMaxCount,
	})
	if err != nil {
		return fmt.Errorf("list top addresses: %w", err)
	}

	if len(top) == 0 {
		if err := pool.Queries().DeleteUserCommonAddresses(ctx, userID); err != nil {
			return fmt.Errorf("delete empty common addresses: %w", err)
		}
		return nil
	}

	userIDs := make([]string, 0, len(top))
	names := make([]string, 0, len(top))
	lats := make([]pgtype.Numeric, 0, len(top))
	lons := make([]pgtype.Numeric, 0, len(top))
	counts := make([]int32, 0, len(top))

	// 一次批量查询全部地址的最新坐标，替代循环内 N+1。
	coordNames := make([]string, 0, len(top))
	for _, row := range top {
		if row.Name.Valid && row.Name.String != "" {
			coordNames = append(coordNames, row.Name.String)
		}
	}
	coords, err := pool.Queries().ListLatestCoordinatesByAddresses(ctx, sqlc.ListLatestCoordinatesByAddressesParams{
		CreatedBy: userID,
		Column2:   coordNames,
	})
	if err != nil {
		return fmt.Errorf("list latest coordinates: %w", err)
	}
	coordByAddress := make(map[string]sqlc.ListLatestCoordinatesByAddressesRow, len(coords))
	for _, c := range coords {
		coordByAddress[c.Name.String] = c
	}

	for _, row := range top {
		if !row.Name.Valid || row.Name.String == "" {
			continue
		}
		coord, ok := coordByAddress[row.Name.String]
		if !ok {
			// 单条脏数据（无坐标记录）跳过而非整批失败，避免连坐其它地址。
			continue
		}
		userIDs = append(userIDs, userID)
		names = append(names, row.Name.String)
		lats = append(lats, coord.Lat)
		lons = append(lons, coord.Lon)
		counts = append(counts, int32(row.Count))
	}

	return WithTx(ctx, pool.Pool(), func(ctx context.Context, q *sqlc.Queries) error {
		if err := q.DeleteUserCommonAddresses(ctx, userID); err != nil {
			return fmt.Errorf("delete old common addresses: %w", err)
		}
		if len(names) == 0 {
			return nil
		}
		if err := q.InsertUserCommonAddresses(ctx, sqlc.InsertUserCommonAddressesParams{
			UserIds: userIDs,
			Names:   names,
			Lats:    lats,
			Lons:    lons,
			Counts:  counts,
		}); err != nil {
			return fmt.Errorf("insert common addresses: %w", err)
		}
		return nil
	})
}
