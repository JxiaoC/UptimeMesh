package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
)

// jsonBytes 把 Go 值序列化成 JSON 文本;空值统一存 NULL。
func jsonBytes(v any) (any, error) {
	switch typed := v.(type) {
	case nil:
		return nil, nil
	case string:
		if typed == "" {
			return nil, nil
		}
		return typed, nil
	case []byte:
		if len(typed) == 0 {
			return nil, nil
		}
		return string(typed), nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	if string(b) == "null" {
		return nil, nil
	}
	return string(b), nil
}

// mustJSON 调用方的写入辅助:序列化失败时以 NULL 落库(调用方按业务忽略)。
func mustJSON(v any) any {
	out, err := jsonBytes(v)
	if err != nil {
		return nil
	}
	return out
}

// parseJSON 把列里的 JSON 文本还原到 out;NULL/空串保持 out 的零值。
func parseJSON(raw sql.NullString, out any) error {
	if !raw.Valid || strings.TrimSpace(raw.String) == "" {
		return nil
	}
	return json.Unmarshal([]byte(raw.String), out)
}

// normalizeErr 把驱动错误归一到 store 的哨兵错误。
// modernc.org/sqlite 的错误文本形如 "constraint failed: UNIQUE constraint failed: ..."。
func normalizeErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	msg := err.Error()
	if strings.Contains(msg, "constraint failed") || strings.Contains(msg, "UNIQUE constraint") {
		return ErrDuplicate
	}
	if strings.Contains(msg, "database is closed") {
		return ErrClosed
	}
	return err
}

// inPlaceholders 拼出 n 个 "?" 占位符,供动态 IN 查询使用。
func inPlaceholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// trimNull 把 sql.NullString 收敛成 string。
func trimNull(v sql.NullString) string {
	if v.Valid {
		return v.String
	}
	return ""
}

// nullBool 把可空整型列还原成三态布尔:NULL ⇒ nil(未上报),0 ⇒ false,其余 ⇒ true。
func nullBool(v sql.NullInt64) *bool {
	if !v.Valid {
		return nil
	}
	b := v.Int64 != 0
	return &b
}

// boolArg 把三态布尔写成 SQL 参数:nil 写 NULL(未上报),否则写 1/0。
func boolArg(v *bool) any {
	if v == nil {
		return nil
	}
	if *v {
		return 1
	}
	return 0
}

// withTx 在事务里执行 fn;fn 返回错误即回滚。
func (s *Store) withTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return normalizeErr(err)
	}
	if err = fn(tx); err != nil {
		_ = tx.Rollback()
		return normalizeErr(err)
	}
	if err = tx.Commit(); err != nil {
		return normalizeErr(err)
	}
	return nil
}
