package app

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"errors"
	"strings"
	"time"
	"unicode/utf8"
)

func accountCredentials(username, password string) (string, error) {
	username = strings.ToLower(strings.TrimSpace(username))
	if !usernamePattern.MatchString(username) {
		return "", &billingValidationError{"账号需为 4—32 位小写字母、数字或下划线，并以字母或数字开头。"}
	}
	if utf8.RuneCountInString(password) < 10 || utf8.RuneCountInString(password) > 128 {
		return "", &billingValidationError{"密码需为 10—128 个字符，建议使用不与其他网站重复的长密码。"}
	}
	return username, nil
}

func accountPassword(password string, salt []byte) ([]byte, error) {
	return pbkdf2.Key(sha256.New, password, salt, accountPasswordIterations, 32)
}

func accountRecoveryCode() (raw, hashed string, err error) {
	raw, err = randomHex(24)
	if err != nil {
		return
	}
	raw = "JCA-" + strings.ToUpper(raw)
	hashed = digest(raw)
	return
}

func (s *Store) accountSession(ctx context.Context, tx *sql.Tx, oldToken, owner string, claimReports bool, now time.Time) (string, error) {
	var previousOwner string
	err := tx.QueryRowContext(ctx, "SELECT owner_id FROM sessions WHERE token_hash=? AND expires_at>?", digest(oldToken), now.Unix()).Scan(&previousOwner)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	if claimReports && previousOwner != "" && previousOwner != owner {
		var registered int
		if err = tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM billing_accounts WHERE owner_id=?", previousOwner).Scan(&registered); err != nil {
			return "", err
		}
		if registered == 0 {
			if _, err = tx.ExecContext(ctx, "INSERT OR IGNORE INTO diagnosis_access(diagnosis_id,owner_id) SELECT diagnosis_id,? FROM diagnosis_access WHERE owner_id=?", owner, previousOwner); err != nil {
				return "", err
			}
		}
	}
	token, err := randomHex(32)
	if err != nil {
		return "", err
	}
	if _, err = tx.ExecContext(ctx, "DELETE FROM sessions WHERE token_hash=?", digest(oldToken)); err != nil {
		return "", err
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO sessions(token_hash,owner_id,expires_at) VALUES(?,?,?)", digest(token), owner, now.Add(30*24*time.Hour).Unix())
	return token, err
}

func (s *Store) RegisterAccount(ctx context.Context, oldToken, username, password string, claimReports bool, now time.Time) (token, recovery string, err error) {
	username, err = accountCredentials(username, password)
	if err != nil {
		return
	}
	salt := make([]byte, 16)
	if _, err = rand.Read(salt); err != nil {
		return
	}
	hash, err := accountPassword(password, salt)
	if err != nil {
		return
	}
	owner, err := randomHex(16)
	if err != nil {
		return
	}
	recovery, recoveryHash, err := accountRecoveryCode()
	if err != nil {
		return
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return
	}
	defer tx.Rollback()
	var exists int
	if err = tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM billing_accounts WHERE username=?", username).Scan(&exists); err != nil {
		return
	}
	if exists != 0 {
		return "", "", ErrUsernameTaken
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO billing_accounts(owner_id,username,password_salt,password_hash,recovery_hash,created_at) VALUES(?,?,?,?,?,?)", owner, username, salt, hash, recoveryHash, now.Unix())
	if err != nil {
		return
	}
	if err = s.grantWelcomeCredits(ctx, tx, owner, now); err != nil {
		return
	}
	token, err = s.accountSession(ctx, tx, oldToken, owner, claimReports, now)
	if err != nil {
		return
	}
	err = tx.Commit()
	return
}

func (s *Store) LoginAccount(ctx context.Context, oldToken, username, password string, claimReports bool, now time.Time) (string, error) {
	username = strings.ToLower(strings.TrimSpace(username))
	var owner string
	var salt, expected []byte
	err := s.db.QueryRowContext(ctx, "SELECT owner_id,password_salt,password_hash FROM billing_accounts WHERE username=?", username).Scan(&owner, &salt, &expected)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	if len(salt) != 16 {
		salt = bytes.Repeat([]byte{0}, 16)
	}
	actual, err := accountPassword(password, salt)
	if err != nil {
		return "", err
	}
	if owner == "" || !hmac.Equal(expected, actual) {
		return "", ErrBadCredentials
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	var unchanged int
	if err = tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM billing_accounts WHERE owner_id=? AND password_hash=?", owner, expected).Scan(&unchanged); err != nil {
		return "", err
	}
	if unchanged != 1 {
		return "", ErrBadCredentials
	}
	token, err := s.accountSession(ctx, tx, oldToken, owner, claimReports, now)
	if err != nil {
		return "", err
	}
	return token, tx.Commit()
}

func (s *Store) ResetAccount(ctx context.Context, oldToken, username, password, code string, now time.Time) (token, recovery string, err error) {
	username, err = accountCredentials(username, password)
	if err != nil {
		return
	}
	code = strings.ToUpper(strings.TrimSpace(code))
	salt := make([]byte, 16)
	if _, err = rand.Read(salt); err != nil {
		return
	}
	hash, err := accountPassword(password, salt)
	if err != nil {
		return
	}
	recovery, recoveryHash, err := accountRecoveryCode()
	if err != nil {
		return
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return
	}
	defer tx.Rollback()
	var owner string
	err = tx.QueryRowContext(ctx, "SELECT owner_id FROM billing_accounts WHERE username=? AND recovery_hash=?", username, digest(code)).Scan(&owner)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", ErrBadCredentials
	}
	if err != nil {
		return
	}
	if _, err = tx.ExecContext(ctx, "UPDATE billing_accounts SET password_salt=?,password_hash=?,recovery_hash=? WHERE owner_id=?", salt, hash, recoveryHash, owner); err != nil {
		return
	}
	if _, err = tx.ExecContext(ctx, "DELETE FROM sessions WHERE owner_id=?", owner); err != nil {
		return
	}
	token, err = s.accountSession(ctx, tx, oldToken, owner, false, now)
	if err != nil {
		return
	}
	err = tx.Commit()
	return
}
