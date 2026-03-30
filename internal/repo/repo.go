package repo

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"tg-tasks-bot/internal/crypto"
)

type Repo struct {
	pool    *pgxpool.Pool
	cryptor *crypto.AESGCM
}

func New(pool *pgxpool.Pool, cryptor *crypto.AESGCM) *Repo {
	return &Repo{pool: pool, cryptor: cryptor}
}

type Task struct {
	ID        int64
	Text      string
	DueDate   *time.Time
	Status    string
	CreatedAt time.Time
	UpdatedAt time.Time
}

type ChatMessage struct {
	Role    string
	Content string
}

func (r *Repo) EnsureUser(ctx context.Context, userID int64) error {
	_, err := r.pool.Exec(ctx, `
INSERT INTO users (telegram_user_id) VALUES ($1)
ON CONFLICT (telegram_user_id) DO NOTHING
`, userID)
	return err
}

func (r *Repo) SetAwaitingKey(ctx context.Context, userID int64, awaiting bool) error {
	_, err := r.pool.Exec(ctx, `
UPDATE users SET awaiting_openai_key=$2, updated_at=now() WHERE telegram_user_id=$1
`, userID, awaiting)
	return err
}

func (r *Repo) IsAwaitingKey(ctx context.Context, userID int64) (bool, error) {
	var awaiting bool
	err := r.pool.QueryRow(ctx, `SELECT awaiting_openai_key FROM users WHERE telegram_user_id=$1`, userID).Scan(&awaiting)
	if err != nil {
		return false, err
	}
	return awaiting, nil
}

func (r *Repo) HasOpenAIKey(ctx context.Context, userID int64) (bool, error) {
	var v *string
	err := r.pool.QueryRow(ctx, `SELECT encrypted_openai_key FROM users WHERE telegram_user_id=$1`, userID).Scan(&v)
	if err != nil {
		return false, err
	}
	return v != nil && *v != "", nil
}

func (r *Repo) StoreOpenAIKey(ctx context.Context, userID int64, rawKey string) error {
	enc, err := r.cryptor.EncryptToBase64([]byte(rawKey))
	if err != nil {
		return err
	}
	_, err = r.pool.Exec(ctx, `
UPDATE users SET encrypted_openai_key=$2, awaiting_openai_key=FALSE, updated_at=now() WHERE telegram_user_id=$1
`, userID, enc)
	return err
}

func (r *Repo) LoadOpenAIKey(ctx context.Context, userID int64) (string, error) {
	var v *string
	err := r.pool.QueryRow(ctx, `SELECT encrypted_openai_key FROM users WHERE telegram_user_id=$1`, userID).Scan(&v)
	if err != nil {
		return "", err
	}
	if v == nil || *v == "" {
		return "", errors.New("no OpenAI key")
	}
	raw, err := r.cryptor.DecryptFromBase64(*v)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

func (r *Repo) CreateTask(ctx context.Context, userID int64, text string, due *time.Time) (int64, error) {
	var id int64
	err := r.pool.QueryRow(ctx, `
INSERT INTO tasks (telegram_user_id, text, due_date) VALUES ($1,$2,$3)
RETURNING id
`, userID, text, due).Scan(&id)
	return id, err
}

func (r *Repo) UpdateTaskText(ctx context.Context, userID, taskID int64, text string) error {
	ct, err := r.pool.Exec(ctx, `
UPDATE tasks SET text=$3, updated_at=now() WHERE telegram_user_id=$1 AND id=$2
`, userID, taskID, text)
	if err != nil {
		return err
	}
	if ct.RowsAffected() == 0 {
		return errors.New("task not found")
	}
	return nil
}

func (r *Repo) UpdateTaskDue(ctx context.Context, userID, taskID int64, due *time.Time) error {
	ct, err := r.pool.Exec(ctx, `
UPDATE tasks SET due_date=$3, updated_at=now() WHERE telegram_user_id=$1 AND id=$2
`, userID, taskID, due)
	if err != nil {
		return err
	}
	if ct.RowsAffected() == 0 {
		return errors.New("task not found")
	}
	return nil
}

func (r *Repo) UpdateTaskStatus(ctx context.Context, userID, taskID int64, status string) error {
	ct, err := r.pool.Exec(ctx, `
UPDATE tasks SET status=$3, updated_at=now() WHERE telegram_user_id=$1 AND id=$2
`, userID, taskID, status)
	if err != nil {
		return err
	}
	if ct.RowsAffected() == 0 {
		return errors.New("task not found")
	}
	return nil
}

func (r *Repo) DeleteTask(ctx context.Context, userID, taskID int64) error {
	ct, err := r.pool.Exec(ctx, `
DELETE FROM tasks WHERE telegram_user_id=$1 AND id=$2
`, userID, taskID)
	if err != nil {
		return err
	}
	if ct.RowsAffected() == 0 {
		return errors.New("task not found")
	}
	return nil
}

type ListFilter struct {
	Status *string
	From   *time.Time
	To     *time.Time
}

func (r *Repo) ListTasks(ctx context.Context, userID int64, f ListFilter) ([]Task, error) {
	q := `
SELECT id, text, due_date, status, created_at, updated_at
FROM tasks
WHERE telegram_user_id=$1
`
	args := []any{userID}
	n := 2

	if f.Status != nil {
		q += " AND status=$" + itoa(n)
		args = append(args, *f.Status)
		n++
	}
	if f.From != nil {
		q += " AND (due_date IS NULL OR due_date >= $" + itoa(n) + ")"
		args = append(args, *f.From)
		n++
	}
	if f.To != nil {
		q += " AND (due_date IS NULL OR due_date <= $" + itoa(n) + ")"
		args = append(args, *f.To)
		n++
	}

	q += " ORDER BY COALESCE(due_date, created_at) ASC, id ASC"

	rows, err := r.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Task
	for rows.Next() {
		var t Task
		var due *time.Time
		if err := rows.Scan(&t.ID, &t.Text, &due, &t.Status, &t.CreatedAt, &t.UpdatedAt); err != nil {
			return nil, err
		}
		t.DueDate = due
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func (r *Repo) AppendChatMessage(ctx context.Context, userID int64, role, content string) error {
	_, err := r.pool.Exec(ctx, `
INSERT INTO chat_messages (telegram_user_id, role, content) VALUES ($1,$2,$3)
`, userID, role, content)
	return err
}

func (r *Repo) LoadRecentChatMessages(ctx context.Context, userID int64, limit int) ([]ChatMessage, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := r.pool.Query(ctx, `
SELECT role, content
FROM chat_messages
WHERE telegram_user_id=$1
ORDER BY id DESC
LIMIT $2
`, userID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var rev []ChatMessage
	for rows.Next() {
		var m ChatMessage
		if err := rows.Scan(&m.Role, &m.Content); err != nil {
			return nil, err
		}
		rev = append(rev, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	for i, j := 0, len(rev)-1; i < j; i, j = i+1, j-1 {
		rev[i], rev[j] = rev[j], rev[i]
	}
	return rev, nil
}

func (r *Repo) SetPendingDelete(ctx context.Context, userID, taskID int64) error {
	_, err := r.pool.Exec(ctx, `
INSERT INTO pending_deletes (telegram_user_id, task_id) VALUES ($1,$2)
ON CONFLICT (telegram_user_id) DO UPDATE SET task_id=EXCLUDED.task_id, created_at=now()
`, userID, taskID)
	return err
}

func (r *Repo) GetPendingDelete(ctx context.Context, userID int64) (int64, bool, error) {
	var taskID int64
	err := r.pool.QueryRow(ctx, `SELECT task_id FROM pending_deletes WHERE telegram_user_id=$1`, userID).Scan(&taskID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, false, nil
		}
		return 0, false, err
	}
	return taskID, true, nil
}

func (r *Repo) ClearPendingDelete(ctx context.Context, userID int64) error {
	_, err := r.pool.Exec(ctx, `DELETE FROM pending_deletes WHERE telegram_user_id=$1`, userID)
	return err
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var buf [32]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + (i % 10))
		i /= 10
	}
	return string(buf[pos:])
}
