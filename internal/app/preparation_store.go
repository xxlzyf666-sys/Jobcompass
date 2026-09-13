package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

type PreparationTask struct {
	ID        string           `json:"id"`
	Kind      string           `json:"kind"`
	Status    string           `json:"status"`
	Revision  int64            `json:"revision"`
	Attempts  int              `json:"attempts"`
	Error     string           `json:"error,omitempty"`
	Parent    string           `json:"-"`
	Token     string           `json:"-"`
	ExpiresAt int64            `json:"-"`
	Input     PreparationInput `json:"-"`
}

func (s *Store) GetPreparation(ctx context.Context, owner, id string, now time.Time) (Preparation, *PreparationTask, int64, error) {
	var p Preparation
	var data, source []byte
	var created, expires, revision int64
	var status string
	err := s.db.QueryRowContext(ctx, `SELECT d.input_cipher,d.created_at,d.expires_at,d.status,COALESCE(p.revision,0),p.data_cipher FROM diagnoses d JOIN diagnosis_access a ON a.diagnosis_id=d.id LEFT JOIN preparations p ON p.diagnosis_id=d.id WHERE d.id=? AND a.owner_id=? AND d.expires_at>?`, id, owner, now.Unix()).Scan(&source, &created, &expires, &status, &revision, &data)
	if errors.Is(err, sql.ErrNoRows) {
		return p, nil, 0, ErrNotFound
	}
	if err != nil {
		return p, nil, 0, err
	}
	if status != "done" {
		return p, nil, 0, ErrConflict
	}
	if revision == 0 {
		plain, err := s.decrypt(source, "input:"+id)
		if err != nil {
			return p, nil, 0, err
		}
		var input Input
		if err = json.Unmarshal(plain, &input); err != nil {
			return p, nil, 0, err
		}
		p = defaultPreparation(input, created)
	} else {
		plain, err := s.decrypt(data, "preparation:"+id)
		if err != nil {
			return p, nil, 0, err
		}
		if err = json.Unmarshal(plain, &p); err != nil {
			return p, nil, 0, err
		}
	}
	p.Revision = revision
	t := &PreparationTask{}
	err = s.db.QueryRowContext(ctx, `SELECT id,kind,status,revision,attempts,error_message FROM preparation_tasks WHERE diagnosis_id=? ORDER BY rowid DESC LIMIT 1`, id).Scan(&t.ID, &t.Kind, &t.Status, &t.Revision, &t.Attempts, &t.Error)
	if errors.Is(err, sql.ErrNoRows) {
		return p, nil, expires, nil
	}
	return p, t, expires, err
}

// Save and enqueue in one transaction: a lost HTTP response cannot append an answer twice.
func (s *Store) SavePreparation(ctx context.Context, owner, ip, id string, p Preparation, input *PreparationInput, c Config, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var revision int64
	err = tx.QueryRowContext(ctx, `SELECT COALESCE(p.revision,0) FROM diagnoses d JOIN diagnosis_access a ON a.diagnosis_id=d.id LEFT JOIN preparations p ON p.diagnosis_id=d.id WHERE d.id=? AND a.owner_id=? AND d.status='done' AND d.expires_at>?`, id, owner, now.Unix()).Scan(&revision)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if revision != p.Revision {
		return ErrConflict
	}
	var active int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM preparation_tasks WHERE diagnosis_id=? AND status IN ('pending','running')`, id).Scan(&active); err != nil {
		return err
	}
	if active != 0 {
		return ErrConflict
	}
	if input != nil {
		if err = s.allowAccountAI(ctx, tx, owner); err != nil {
			return err
		}
		if err = tx.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM diagnoses WHERE status IN ('pending','running') AND expires_at>?) + (SELECT COUNT(*) FROM preparation_tasks t JOIN diagnoses d ON d.id=t.diagnosis_id WHERE t.status IN ('pending','running') AND d.expires_at>?)`, now.Unix(), now.Unix()).Scan(&active); err != nil {
			return err
		}
		if active >= c.QueueLimit {
			return ErrQueueFull
		}
		ipLimit, ownerLimit := c.PreparationIPQuota, c.PreparationSessionQuota
		if ipLimit == 0 {
			ipLimit = 60
		}
		if ownerLimit == 0 {
			ownerLimit = 120
		}
		if err = takeQuota(ctx, tx, s.privateHash("prepare-ip:"+ip), now.Unix()/3600*3600, ipLimit); err != nil {
			return err
		}
		if err = takeQuota(ctx, tx, s.privateHash("prepare-owner:"+owner), now.Unix()/86400*86400, ownerLimit); err != nil {
			return err
		}
	}
	p.Revision++
	plain, err := json.Marshal(p)
	if err != nil {
		return err
	}
	data, err := s.encrypt(plain, "preparation:"+id)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO preparations(diagnosis_id,revision,data_cipher) VALUES(?,?,?) ON CONFLICT(diagnosis_id) DO UPDATE SET revision=excluded.revision,data_cipher=excluded.data_cipher`, id, p.Revision, data); err != nil {
		return err
	}
	if input != nil {
		taskID, err := randomHex(16)
		if err != nil {
			return err
		}
		plain, err := json.Marshal(input)
		if err != nil {
			return err
		}
		data, err := s.encrypt(plain, "preparation-task:"+taskID)
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO preparation_tasks(id,diagnosis_id,kind,revision,status,input_cipher,created_at,updated_at,next_attempt_at) VALUES(?,?,?,?,'pending',?,?,?,?)`, taskID, id, input.Kind, p.Revision, data, now.Unix(), now.Unix(), now.Unix()); err != nil {
			return err
		}
		if err = s.reservePreparationCredit(ctx, tx, owner, id, taskID, *input, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) RetryPreparation(ctx context.Context, parent, taskID string, revision int64) (*PreparationInput, error) {
	var data []byte
	err := s.db.QueryRowContext(ctx, `SELECT input_cipher FROM preparation_tasks WHERE id=? AND diagnosis_id=? AND revision=? AND status='failed'`, taskID, parent, revision).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrConflict
	}
	if err != nil {
		return nil, err
	}
	plain, err := s.decrypt(data, "preparation-task:"+taskID)
	if err != nil {
		return nil, err
	}
	var input PreparationInput
	if err = json.Unmarshal(plain, &input); err != nil {
		return nil, err
	}
	return &input, nil
}

func (s *Store) ClaimPreparation(ctx context.Context, now time.Time, lease time.Duration) (*PreparationTask, error) {
	token, err := randomHex(16)
	if err != nil {
		return nil, err
	}
	t := &PreparationTask{Token: token}
	var data []byte
	err = s.db.QueryRowContext(ctx, `UPDATE preparation_tasks SET status='running',worker_token=?,attempts=attempts+1,updated_at=?,lease_until=? WHERE id=(SELECT t.id FROM preparation_tasks t JOIN diagnoses d ON d.id=t.diagnosis_id WHERE t.status='pending' AND t.next_attempt_at<=? AND t.attempts<3 AND d.expires_at>? ORDER BY t.created_at,t.rowid LIMIT 1) RETURNING id,diagnosis_id,kind,revision,input_cipher,attempts,(SELECT expires_at FROM diagnoses WHERE id=diagnosis_id)`, token, now.Unix(), now.Add(lease).Unix(), now.Unix(), now.Unix()).Scan(&t.ID, &t.Parent, &t.Kind, &t.Revision, &data, &t.Attempts, &t.ExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	plain, err := s.decrypt(data, "preparation-task:"+t.ID)
	if err != nil {
		return t, err
	}
	err = json.Unmarshal(plain, &t.Input)
	return t, err
}

func (s *Store) CompletePreparation(ctx context.Context, t *PreparationTask, output PreparationOutput, usage Usage, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var data []byte
	var revision int64
	err = tx.QueryRowContext(ctx, `SELECT p.data_cipher,p.revision FROM preparations p JOIN diagnoses d ON d.id=p.diagnosis_id JOIN preparation_tasks t ON t.diagnosis_id=p.diagnosis_id WHERE t.id=? AND t.status='running' AND t.worker_token=? AND d.expires_at>?`, t.ID, t.Token, now.Unix()).Scan(&data, &revision)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if revision != t.Revision {
		return ErrConflict
	}
	plain, err := s.decrypt(data, "preparation:"+t.Parent)
	if err != nil {
		return err
	}
	var p Preparation
	if err = json.Unmarshal(plain, &p); err != nil {
		return err
	}
	if err = applyPreparationOutput(&p, t.Input, output, now.Unix()); err != nil {
		return err
	}
	p.Revision = revision + 1
	plain, err = json.Marshal(p)
	if err != nil {
		return err
	}
	data, err = s.encrypt(plain, "preparation:"+t.Parent)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE preparations SET data_cipher=?,revision=? WHERE diagnosis_id=? AND revision=?`, data, p.Revision, t.Parent, revision); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE preparation_tasks SET status='done',input_cipher=NULL,worker_token='',lease_until=0,error_message='',updated_at=?,tokens_in=tokens_in+?,tokens_out=tokens_out+? WHERE id=? AND worker_token=?`, now.Unix(), usage.Input, usage.Output, t.ID, t.Token)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) FailPreparation(ctx context.Context, t *PreparationTask, message string, retry bool, usage Usage, now time.Time) error {
	status := "failed"
	next := now.Add(time.Duration(1<<t.Attempts) * time.Second).Unix()
	if retry && t.Attempts < 3 && next < t.ExpiresAt {
		status = "pending"
	}
	_, err := s.db.ExecContext(ctx, `UPDATE preparation_tasks SET status=?,error_message=?,worker_token='',lease_until=0,next_attempt_at=?,updated_at=?,tokens_in=tokens_in+?,tokens_out=tokens_out+? WHERE id=? AND status='running' AND worker_token=?`, status, message, next, now.Unix(), usage.Input, usage.Output, t.ID, t.Token)
	return err
}
