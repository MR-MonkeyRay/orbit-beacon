package storage

import (
	"bytes"
	"context"
)

// DeleteBatch 先取调用者限定数量的主键并关闭 Rows，避免 MySQL 同表子查询删除。
func DeleteBatch(ctx context.Context, tx *Tx, selectSQL, deleteSQL string, args ...any) (int64, error) {
	rows, err := tx.QueryContext(ctx, selectSQL, args...)
	if err != nil {
		return 0, err
	}
	names, err := rows.Columns()
	if err != nil {
		_ = rows.Close()
		return 0, err
	}
	var keys [][]any
	pointers := make([]any, len(names))
	for rows.Next() {
		key := make([]any, len(names))
		for i := range key {
			pointers[i] = &key[i]
		}
		if err = rows.Scan(pointers...); err != nil {
			_ = rows.Close()
			return 0, err
		}
		for i, v := range key {
			if b, ok := v.([]byte); ok {
				key[i] = bytes.Clone(b)
			}
		}
		keys = append(keys, key)
	}
	if err = rows.Err(); err != nil {
		_ = rows.Close()
		return 0, err
	}
	if err = rows.Close(); err != nil {
		return 0, err
	}
	var count int64
	for _, key := range keys {
		result, err := tx.ExecContext(ctx, deleteSQL, key...)
		if err != nil {
			return count, err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return count, err
		}
		count += n
	}
	return count, nil
}
