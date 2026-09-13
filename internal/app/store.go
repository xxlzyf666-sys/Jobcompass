package app

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schema string

var (
	ErrNotFound    = errors.New("not found")
	ErrRateLimit   = errors.New("rate limited")
	ErrQueueFull   = errors.New("queue full")
	ErrActiveLimit = errors.New("active task limit")
)

type Store struct {
	db   *sql.DB
	aead cipher.AEAD
	key  []byte
}
type Diagnosis struct {
	ID        string  `json:"id"`
	Status    string  `json:"status"`
	CreatedAt int64   `json:"created_at"`
	ExpiresAt int64   `json:"expires_at"`
	Attempts  int     `json:"attempts"`
	Error     string  `json:"error,omitempty"`
	Report    *Report `json:"report,omitempty"`
}
type Job struct {
	ID, WorkerToken string
	Input           Input
	Attempts        int
	ExpiresAt       int64
}

func OpenStore(directory string, key []byte) (*Store, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", filepath.Join(directory, "app.db"))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	s := &Store{db: db, aead: aead, key: key}
	if _, err = db.Exec("PRAGMA journal_mode=WAL; PRAGMA foreign_keys=ON; PRAGMA busy_timeout=5000; PRAGMA secure_delete=ON; PRAGMA cache_size=-4096; PRAGMA temp_store=MEMORY;"); err != nil {
		db.Close()
		return nil, err
	}
	if _, err = db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	var check []byte
	err = db.QueryRow("SELECT value FROM metadata WHERE name='encryption_check'").Scan(&check)
	if errors.Is(err, sql.ErrNoRows) {
		check, err = s.encrypt([]byte("jobcompass-encryption-v1"), "key-check")
		if err == nil {
			_, err = db.Exec("INSERT INTO metadata(name,value) VALUES('encryption_check',?)", check)
		}
	} else if err == nil {
		var text []byte
		text, err = s.decrypt(check, "key-check")
		if err == nil && string(text) != "jobcompass-encryption-v1" {
			err = errors.New("invalid encryption check")
		}
	}
	if err != nil {
		db.Close()
		return nil, errors.New("database encryption key does not match or database is invalid")
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }
func (s *Store) encrypt(value []byte, aad string) ([]byte, error) {
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return s.aead.Seal(nonce, nonce, value, []byte(aad)), nil
}
func (s *Store) decrypt(value []byte, aad string) ([]byte, error) {
	n := s.aead.NonceSize()
	if len(value) < n {
		return nil, errors.New("invalid encrypted data")
	}
	return s.aead.Open(nil, value[:n], value[n:], []byte(aad))
}
func randomHex(size int) (string, error) {
	value := make([]byte, size)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return hex.EncodeToString(value), nil
}
func digest(value string) string {
	hash := sha256.Sum256([]byte(value))
	return hex.EncodeToString(hash[:])
}
func (s *Store) privateHash(value string) string {
	h := hmac.New(sha256.New, s.key)
	h.Write([]byte(value))
	return hex.EncodeToString(h.Sum(nil))
}

func (s *Store) NewSession(now time.Time) (token, owner string, err error) {
	if token, err = randomHex(32); err != nil {
		return
	}
	if owner, err = randomHex(16); err != nil {
		return
	}
	_, err = s.db.Exec("INSERT INTO sessions(token_hash,owner_id,expires_at) VALUES(?,?,?)", digest(token), owner, now.Add(30*24*time.Hour).Unix())
	return
}
func (s *Store) Session(token string, now time.Time) (string, error) {
	if len(token) != 64 {
		return "", ErrNotFound
	}
	var owner string
	err := s.db.QueryRow("SELECT owner_id FROM sessions WHERE token_hash=? AND expires_at>?", digest(token), now.Unix()).Scan(&owner)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return owner, err
}

func takeQuota(ctx context.Context, tx *sql.Tx, key string, window int64, limit int) error {
	var count int
	err := tx.QueryRowContext(ctx, "SELECT count FROM rate_limits WHERE key=? AND window_start=?", key, window).Scan(&count)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if count >= limit {
		return ErrRateLimit
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO rate_limits(key,window_start,count) VALUES(?,?,1) ON CONFLICT(key,window_start) DO UPDATE SET count=count+1", key, window)
	return err
}
func (s *Store) TakeLimit(ctx context.Context, key string, now time.Time, window time.Duration, limit int) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = takeQuota(ctx, tx, key, now.Unix()/int64(window.Seconds())*int64(window.Seconds()), limit); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) Create(ctx context.Context, owner, ip string, input Input, config Config, now time.Time) (id, recovery string, duplicate bool, err error) {
	material, err := json.Marshal(input)
	if err != nil {
		return "", "", false, err
	}
	dedup := s.privateHash("submission:" + owner + ":" + string(material))
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", "", false, err
	}
	defer tx.Rollback()
	err = tx.QueryRowContext(ctx, "SELECT id FROM diagnoses WHERE dedup_hash=? AND expires_at>? AND status!='failed' ORDER BY created_at DESC LIMIT 1", dedup, now.Unix()).Scan(&id)
	if err == nil {
		return id, "", true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", "", false, err
	}
	var active int
	if err = tx.QueryRowContext(ctx, "SELECT (SELECT COUNT(*) FROM diagnoses WHERE status IN ('pending','running') AND expires_at>?) + (SELECT COUNT(*) FROM preparation_tasks t JOIN diagnoses d ON d.id=t.diagnosis_id WHERE t.status IN ('pending','running') AND d.expires_at>?)", now.Unix(), now.Unix()).Scan(&active); err != nil {
		return "", "", false, err
	}
	if active >= config.QueueLimit {
		return "", "", false, ErrQueueFull
	}
	if err = tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM diagnoses d JOIN diagnosis_access a ON a.diagnosis_id=d.id WHERE a.owner_id=? AND d.status IN ('pending','running') AND d.expires_at>?", owner, now.Unix()).Scan(&active); err != nil {
		return "", "", false, err
	}
	if active >= 2 {
		return "", "", false, ErrActiveLimit
	}
	if err = takeQuota(ctx, tx, s.privateHash("submit-ip:"+ip), now.Unix()/3600*3600, config.IPQuota); err != nil {
		return "", "", false, err
	}
	if err = takeQuota(ctx, tx, s.privateHash("submit-owner:"+owner), now.Unix()/86400*86400, config.SessionQuota); err != nil {
		return "", "", false, err
	}
	if id, err = randomHex(16); err != nil {
		return "", "", false, err
	}
	raw, err := randomHex(20)
	if err != nil {
		return "", "", false, err
	}
	raw = strings.ToUpper(raw)
	var groups []string
	for i := 0; i < len(raw); i += 8 {
		groups = append(groups, raw[i:i+8])
	}
	recovery = "JC-" + strings.Join(groups, "-")
	inputCipher, err := s.encrypt(material, "input:"+id)
	if err != nil {
		return "", "", false, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO diagnoses(id,dedup_hash,recovery_hash,status,input_cipher,created_at,updated_at,expires_at,next_attempt_at) VALUES(?,?,?,'pending',?,?,?,?,?)`, id, dedup, digest("JC"+raw), inputCipher, now.Unix(), now.Unix(), now.Add(config.Retention).Unix(), now.Unix())
	if err != nil {
		return "", "", false, err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO diagnosis_access(diagnosis_id,owner_id) VALUES(?,?)", id, owner); err != nil {
		return "", "", false, err
	}
	if err = s.reserveBillingCredit(ctx, tx, owner, "diagnosis:"+id, "diagnosis:"+id, id, "diagnosis", true, false, now); err != nil {
		return "", "", false, err
	}
	if err = tx.Commit(); err != nil {
		return "", "", false, err
	}
	return id, recovery, false, nil
}

func (s *Store) Get(ctx context.Context, owner, id string, now time.Time) (Diagnosis, error) {
	var d Diagnosis
	var encrypted []byte
	err := s.db.QueryRowContext(ctx, `SELECT d.id,d.status,d.created_at,d.expires_at,d.attempts,d.error_message,d.report_cipher FROM diagnoses d JOIN diagnosis_access a ON a.diagnosis_id=d.id WHERE d.id=? AND a.owner_id=? AND d.expires_at>?`, id, owner, now.Unix()).Scan(&d.ID, &d.Status, &d.CreatedAt, &d.ExpiresAt, &d.Attempts, &d.Error, &encrypted)
	if errors.Is(err, sql.ErrNoRows) {
		return d, ErrNotFound
	}
	if err != nil {
		return d, err
	}
	if d.Status == "done" {
		plain, err := s.decrypt(encrypted, "report:"+id)
		if err != nil {
			return d, errors.New("report decryption failed")
		}
		var r Report
		if err = json.Unmarshal(plain, &r); err != nil {
			return d, errors.New("report data invalid")
		}
		d.Report = &r
	}
	return d, nil
}

func (s *Store) List(ctx context.Context, owner string, now time.Time) ([]Diagnosis, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT d.id,d.status,d.created_at,d.expires_at,d.attempts,d.error_message FROM diagnoses d JOIN diagnosis_access a ON a.diagnosis_id=d.id WHERE a.owner_id=? AND d.expires_at>? ORDER BY d.created_at DESC,d.id DESC LIMIT 50`, owner, now.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	list := []Diagnosis{}
	for rows.Next() {
		var d Diagnosis
		if err = rows.Scan(&d.ID, &d.Status, &d.CreatedAt, &d.ExpiresAt, &d.Attempts, &d.Error); err != nil {
			return nil, err
		}
		list = append(list, d)
	}
	return list, rows.Err()
}

func (s *Store) Recover(ctx context.Context, owner, code string, now time.Time) (string, error) {
	var id string
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	err = tx.QueryRowContext(ctx, "SELECT id FROM diagnoses WHERE recovery_hash=? AND expires_at>?", digest(code), now.Unix()).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	if _, err = tx.ExecContext(ctx, "INSERT OR IGNORE INTO diagnosis_access(diagnosis_id,owner_id) VALUES(?,?)", id, owner); err != nil {
		return "", err
	}
	return id, tx.Commit()
}

func (s *Store) Delete(ctx context.Context, owner, id string, now time.Time) error {
	result, err := s.db.ExecContext(ctx, `DELETE FROM diagnoses WHERE id=? AND expires_at>? AND EXISTS(SELECT 1 FROM diagnosis_access WHERE diagnosis_id=? AND owner_id=?)`, id, now.Unix(), id, owner)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) Claim(ctx context.Context, now time.Time, lease time.Duration) (*Job, error) {
	token, err := randomHex(16)
	if err != nil {
		return nil, err
	}
	job := &Job{WorkerToken: token}
	var encrypted []byte
	err = s.db.QueryRowContext(ctx, `UPDATE diagnoses SET status='running',worker_token=?,attempts=attempts+1,updated_at=?,lease_until=? WHERE id=(SELECT id FROM diagnoses WHERE status='pending' AND next_attempt_at<=? AND expires_at>? AND attempts<3 ORDER BY created_at,id LIMIT 1) RETURNING id,input_cipher,attempts,expires_at`, token, now.Unix(), now.Add(lease).Unix(), now.Unix(), now.Unix()).Scan(&job.ID, &encrypted, &job.Attempts, &job.ExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	plain, err := s.decrypt(encrypted, "input:"+job.ID)
	if err != nil {
		return job, errors.New("input decryption failed")
	}
	if err = json.Unmarshal(plain, &job.Input); err != nil {
		return job, errors.New("input data invalid")
	}
	return job, nil
}

func (s *Store) Complete(ctx context.Context, job *Job, report Report, usage Usage, now time.Time) error {
	plain, err := json.Marshal(report)
	if err != nil {
		return err
	}
	encrypted, err := s.encrypt(plain, "report:"+job.ID)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `UPDATE diagnoses SET status='done',report_cipher=?,updated_at=?,worker_token='',lease_until=0,error_message='',tokens_in=tokens_in+?,tokens_out=tokens_out+? WHERE id=? AND status='running' AND worker_token=? AND expires_at>?`, encrypted, now.Unix(), usage.Input, usage.Output, job.ID, job.WorkerToken, now.Unix())
	return err
}

func (s *Store) Fail(ctx context.Context, job *Job, message string, retryable bool, usage Usage, now time.Time) error {
	status := "failed"
	next := now.Add(time.Duration(1<<job.Attempts) * time.Second).Unix()
	if retryable && job.Attempts < 3 && next < job.ExpiresAt {
		status = "pending"
	}
	_, err := s.db.ExecContext(ctx, `UPDATE diagnoses SET status=?,error_message=?,updated_at=?,next_attempt_at=?,worker_token='',lease_until=0,tokens_in=tokens_in+?,tokens_out=tokens_out+? WHERE id=? AND status='running' AND worker_token=? AND expires_at>?`, status, message, now.Unix(), next, usage.Input, usage.Output, job.ID, job.WorkerToken, now.Unix())
	return err
}

func (s *Store) RecoverLeases(ctx context.Context, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE diagnoses SET status=CASE WHEN attempts<3 THEN 'pending' ELSE 'failed' END,error_message=CASE WHEN attempts<3 THEN '' ELSE '分析多次中断，请重新提交。' END,worker_token='',lease_until=0,next_attempt_at=?,updated_at=? WHERE status='running' AND lease_until<=? AND expires_at>?`, now.Unix(), now.Unix(), now.Unix(), now.Unix())
	if err == nil {
		_, err = s.db.ExecContext(ctx, `UPDATE preparation_tasks SET status=CASE WHEN attempts<3 THEN 'pending' ELSE 'failed' END,error_message=CASE WHEN attempts<3 THEN '' ELSE '生成多次中断，可以重试；已保存的内容仍然保留。' END,worker_token='',lease_until=0,next_attempt_at=?,updated_at=? WHERE status='running' AND lease_until<=?`, now.Unix(), now.Unix(), now.Unix())
	}
	return err
}

func (s *Store) Cleanup(ctx context.Context, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, statement := range []struct {
		query string
		value int64
	}{
		{"DELETE FROM diagnoses WHERE expires_at<=?", now.Unix()},
		{"DELETE FROM sessions WHERE expires_at<=?", now.Unix()},
		{"DELETE FROM rate_limits WHERE window_start<?", now.Add(-48 * time.Hour).Unix()},
		{"DELETE FROM billing_admin_sessions WHERE expires_at<=?", now.Unix()},
		{"UPDATE billing_orders SET status='expired',revision=revision+1,updated_at=unixepoch() WHERE status='awaiting_payment' AND expires_at<=?", now.Unix()},
	} {
		if _, err = tx.ExecContext(ctx, statement.query, statement.value); err != nil {
			return err
		}
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)")
	return err
}

func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }
func validID(id string) bool {
	if len(id) != 32 {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil
}
func createdLabel(unix int64) string { return time.Unix(unix, 0).UTC().Format("2006-01-02 15:04 UTC") }
func (d Diagnosis) String() string   { return fmt.Sprintf("diagnosis(%s,%s)", d.ID, d.Status) }
