package db

import (
	"context"
	"database/sql"
	"errors"
)

type User struct {
	ID             int64  `json:"id"`
	Username       string `json:"username"`
	PwHash         string `json:"-"`
	IsAdmin        bool   `json:"isAdmin"`
	Disabled       bool   `json:"disabled"`
	PinHash        string `json:"-"`
	BlurRestricted bool   `json:"blurRestricted"`
	FailedLogins   int    `json:"-"`
	LockedUntil    int64  `json:"-"`
	CreatedAt      int64  `json:"createdAt"`
	LastLoginAt    int64  `json:"lastLoginAt"`
	PinFails       int    `json:"-"`
	PinLockedUntil int64  `json:"-"`
	TokenGen       uint32 `json:"-"`
}

const userCols = "id,username,pw_hash,is_admin,disabled,pin_hash,blur_restricted,failed_logins,locked_until,created_at,last_login_at,pin_fails,pin_locked_until,token_gen"

func scanUser(row interface{ Scan(...any) error }) (*User, error) {
	var u User
	err := row.Scan(&u.ID, &u.Username, &u.PwHash, &u.IsAdmin, &u.Disabled, &u.PinHash, &u.BlurRestricted,
		&u.FailedLogins, &u.LockedUntil, &u.CreatedAt, &u.LastLoginAt, &u.PinFails, &u.PinLockedUntil, &u.TokenGen)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &u, err
}

func (d *DB) CountUsers(ctx context.Context) (int, error) {
	var n int
	err := d.R.QueryRowContext(ctx, "SELECT COUNT(*) FROM users").Scan(&n)
	return n, err
}

func (d *DB) CreateUser(ctx context.Context, username, pwHash string, isAdmin bool) (int64, error) {
	res, err := d.W.ExecContext(ctx, "INSERT INTO users(username,pw_hash,is_admin,created_at) VALUES(?,?,?,?)",
		username, pwHash, isAdmin, now())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (d *DB) UserByID(ctx context.Context, id int64) (*User, error) {
	return scanUser(d.R.QueryRowContext(ctx, "SELECT "+userCols+" FROM users WHERE id=?", id))
}

func (d *DB) UserByName(ctx context.Context, name string) (*User, error) {
	return scanUser(d.R.QueryRowContext(ctx, "SELECT "+userCols+" FROM users WHERE username=?", name))
}

func (d *DB) ListUsers(ctx context.Context) ([]*User, error) {
	rows, err := d.R.QueryContext(ctx, "SELECT "+userCols+" FROM users ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

func (d *DB) UpdateUserFlags(ctx context.Context, id int64, isAdmin, disabled bool) error {
	_, err := d.W.ExecContext(ctx, "UPDATE users SET is_admin=?, disabled=?, token_gen=token_gen+1 WHERE id=?", isAdmin, disabled, id)
	return err
}

func (d *DB) SetPassword(ctx context.Context, id int64, pwHash string) error {
	return d.Tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "UPDATE users SET pw_hash=?, failed_logins=0, locked_until=0, token_gen=token_gen+1 WHERE id=?", pwHash, id); err != nil {
			return err
		}
		// 改密码后吊销所有“记住登录”令牌
		_, err := tx.ExecContext(ctx, "DELETE FROM login_tokens WHERE user_id=?", id)
		return err
	})
}

func (d *DB) SetPin(ctx context.Context, id int64, pinHash string) error {
	_, err := d.W.ExecContext(ctx, "UPDATE users SET pin_hash=?, pin_fails=0, pin_locked_until=0, token_gen=token_gen+1 WHERE id=?", pinHash, id)
	return err
}

// BumpTokenGen 让该用户已签发的对象访问令牌全部失效（锁定、改密码、禁用时调用）。
func (d *DB) BumpTokenGen(ctx context.Context, id int64) error {
	_, err := d.W.ExecContext(ctx, "UPDATE users SET token_gen=token_gen+1 WHERE id=?", id)
	return err
}

// AttemptPolicy 是失败锁定的参数：Window 内累计 Threshold 次失败就锁定 Window 这么久；
// 距离上次失败超过 Window 的话计数从头开始（所以锁定到期后不会一次失败就立刻再锁）。
type AttemptPolicy struct {
	Threshold int
	Window    int64 // 秒
}

// reserveAttempt 在校验之前先原子地记一次失败（“占坑”），校验成功后再清零。
// 这样并发的请求不能同时通过“是否已锁定”的检查，客户端中途断开也不会漏记。
// 返回 false 表示正处于锁定期（或用户不存在），不应再校验。
func (d *DB) reserveAttempt(ctx context.Context, failsCol, lastCol, lockCol string, id int64, p AttemptPolicy, now int64) (bool, error) {
	fails := "CASE WHEN " + lastCol + " <= :cut THEN 1 ELSE " + failsCol + " + 1 END"
	res, err := d.W.ExecContext(ctx, "UPDATE users SET "+
		failsCol+" = "+fails+", "+
		lastCol+" = :now, "+
		lockCol+" = CASE WHEN "+fails+" >= :th THEN :until ELSE "+lockCol+" END "+
		"WHERE id = :id AND "+lockCol+" <= :now",
		sql.Named("cut", now-p.Window), sql.Named("now", now), sql.Named("th", p.Threshold),
		sql.Named("until", now+p.Window), sql.Named("id", id))
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// ReserveLoginAttempt 在校验密码前记一次失败，见 reserveAttempt。
func (d *DB) ReserveLoginAttempt(ctx context.Context, id int64, p AttemptPolicy, now int64) (bool, error) {
	return d.reserveAttempt(ctx, "failed_logins", "last_failed_at", "locked_until", id, p, now)
}

// ReservePinAttempt 在校验 PIN 前记一次失败（持久化，换会话也绕不过），见 reserveAttempt。
func (d *DB) ReservePinAttempt(ctx context.Context, id int64, p AttemptPolicy, now int64) (bool, error) {
	return d.reserveAttempt(ctx, "pin_fails", "pin_last_failed_at", "pin_locked_until", id, p, now)
}

// ResetLoginFailures 在密码校验通过后清掉失败计数。
func (d *DB) ResetLoginFailures(ctx context.Context, id int64) error {
	_, err := d.W.ExecContext(ctx, "UPDATE users SET failed_logins=0, locked_until=0 WHERE id=?", id)
	return err
}

func (d *DB) ResetPinFailures(ctx context.Context, id int64) error {
	_, err := d.W.ExecContext(ctx, "UPDATE users SET pin_fails=0, pin_locked_until=0 WHERE id=?", id)
	return err
}

func (d *DB) SetBlur(ctx context.Context, id int64, blur bool) error {
	_, err := d.W.ExecContext(ctx, "UPDATE users SET blur_restricted=? WHERE id=?", blur, id)
	return err
}

func (d *DB) DeleteUser(ctx context.Context, id int64) error {
	_, err := d.W.ExecContext(ctx, "DELETE FROM users WHERE id=?", id)
	return err
}

func (d *DB) RecordLoginSuccess(ctx context.Context, id int64) error {
	_, err := d.W.ExecContext(ctx, "UPDATE users SET failed_logins=0, locked_until=0, last_login_at=? WHERE id=?", now(), id)
	return err
}

// ---- 登录令牌（只存哈希） ----

func (d *DB) AddLoginToken(ctx context.Context, hash []byte, uid, expires int64) error {
	t := now()
	_, err := d.W.ExecContext(ctx, "INSERT INTO login_tokens(hash,user_id,created_at,expires_at,last_used_at) VALUES(?,?,?,?,?)", hash, uid, t, expires, t)
	return err
}

// UseLoginToken 校验令牌并滑动续期，返回用户 ID。
func (d *DB) UseLoginToken(ctx context.Context, hash []byte, newExpires int64) (int64, error) {
	var uid, exp int64
	err := d.R.QueryRowContext(ctx, "SELECT user_id, expires_at FROM login_tokens WHERE hash=?", hash).Scan(&uid, &exp)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, err
	}
	if exp < now() {
		d.W.ExecContext(ctx, "DELETE FROM login_tokens WHERE hash=?", hash)
		return 0, ErrNotFound
	}
	_, err = d.W.ExecContext(ctx, "UPDATE login_tokens SET expires_at=?, last_used_at=? WHERE hash=?", newExpires, now(), hash)
	return uid, err
}

func (d *DB) DeleteLoginToken(ctx context.Context, hash []byte) error {
	_, err := d.W.ExecContext(ctx, "DELETE FROM login_tokens WHERE hash=?", hash)
	return err
}

func (d *DB) PurgeExpiredTokens(ctx context.Context) error {
	_, err := d.W.ExecContext(ctx, "DELETE FROM login_tokens WHERE expires_at < ?", now())
	return err
}
