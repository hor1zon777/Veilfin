package db

import (
	"context"
	"database/sql"
	"encoding/json"
)

// TagStat 是某个标签（类型 / 标签 / 演员）的名称和使用它的条目数量。
type TagStat struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

// tagField 把标签种类对应到 Meta 中可锁定的字段名。
var tagField = map[string]string{"genre": "genres", "tag": "tags", "actor": "actors"}

// TagStats 列出若干媒体库中某种标签的全部取值及条目数量（按数量从多到少）。
func (d *DB) TagStats(ctx context.Context, libs []int64, kind string) ([]TagStat, error) {
	out := []TagStat{}
	if len(libs) == 0 || tagField[kind] == "" {
		return out, nil
	}
	args := append(int64Args(libs), kind)
	rows, err := d.R.QueryContext(ctx, `SELECT t.name, COUNT(*) FROM item_tags t JOIN items i ON i.id=t.item_id
		WHERE i.library_id IN (`+placeholders(len(libs))+`) AND t.kind=? GROUP BY t.name ORDER BY COUNT(*) DESC, t.name`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var t TagStat
		if err := rows.Scan(&t.Name, &t.Count); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ReplaceTags 把若干媒体库中名为 from 的标签统一改成 to（to 已存在时即为合并）；to 为空表示删除这些标签。
// 受影响的条目会锁定对应字段，之后重新刮削不会把修改覆盖回去。返回受影响的条目数量。
func (d *DB) ReplaceTags(ctx context.Context, libs []int64, kind string, from []string, to string) (int, error) {
	field := tagField[kind]
	from = dedupeExact(from)
	if len(libs) == 0 || field == "" || len(from) == 0 {
		return 0, nil
	}
	inLibs := `item_id IN (SELECT id FROM items WHERE library_id IN (` + placeholders(len(libs)) + `))`
	libArgs := int64Args(libs)
	var n int
	err := d.Tx(ctx, func(tx *sql.Tx) error {
		// 先记下受影响的条目，改名之后就查不到了
		args := append(append([]any{kind}, strArgs(from)...), libArgs...)
		rows, err := tx.QueryContext(ctx, `SELECT DISTINCT item_id FROM item_tags WHERE kind=? AND name IN (`+placeholders(len(from))+`) AND `+inLibs, args...)
		if err != nil {
			return err
		}
		var ids []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		n = len(ids)
		if n == 0 {
			return nil
		}
		for _, name := range from {
			if name == to {
				continue
			}
			if to != "" {
				// 原地改名保留标签在条目中的顺序；已经有 to 的条目会因主键冲突被跳过，下面直接删掉旧名
				if _, err := tx.ExecContext(ctx, `UPDATE OR IGNORE item_tags SET name=? WHERE kind=? AND name=? AND `+inLibs,
					append([]any{to, kind, name}, libArgs...)...); err != nil {
					return err
				}
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM item_tags WHERE kind=? AND name=? AND `+inLibs,
				append([]any{kind, name}, libArgs...)...); err != nil {
				return err
			}
		}
		return lockField(ctx, tx, ids, field)
	})
	return n, err
}

// lockField 给条目加上字段锁，并更新修改时间。
func lockField(ctx context.Context, tx *sql.Tx, ids []int64, field string) error {
	t := now()
	for _, id := range ids {
		var s string
		if err := tx.QueryRowContext(ctx, "SELECT locked FROM items WHERE id=?", id).Scan(&s); err != nil {
			return err
		}
		var locked []string
		json.Unmarshal([]byte(s), &locked)
		if !contains(locked, field) {
			locked = append(locked, field)
		}
		b, _ := json.Marshal(locked)
		if _, err := tx.ExecContext(ctx, "UPDATE items SET locked=?, updated_at=? WHERE id=?", string(b), t, id); err != nil {
			return err
		}
	}
	return nil
}

func strArgs(s []string) []any {
	out := make([]any, len(s))
	for i, v := range s {
		out[i] = v
	}
	return out
}

// dedupeExact 去掉空串和完全相同的重复项（区分大小写，和 item_tags 的主键一致）。
func dedupeExact(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}
