package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jag2011/strings/internal/arb"
)

// What an AI run translates.
const (
	AIInputMissing = "missing" // strings without a translation
	AIInputAll     = "all"     // also translated ones, which get suggestions
)

// States of an AI run. Queued and running runs are active; the others are
// finished. Interrupted runs stopped when the server did and can be started
// again, like failed and cancelled ones.
const (
	AIQueued      = "queued"
	AIRunning     = "running"
	AIDone        = "done"
	AIFailed      = "failed"
	AICancelled   = "cancelled"
	AIInterrupted = "interrupted"
)

type AIRun struct {
	ID            int64
	Lang          string
	UserID        int64 // the AI user
	UserName      string
	StartedBy     int64
	StartedByName string
	Model         string
	Input         string
	Suggest       bool
	Notes         string
	MaxCost       float64
	Status        string
	Translated    int
	Suggested     int
	Failed        int
	Steps         int
	InputTokens   int64
	CachedTokens  int64
	OutputTokens  int64
	Cost          float64
	Summary       string
	Error         string
	CreatedAt     string
	StartedAt     string
	FinishedAt    string
}

// Active says whether the run is queued or running.
func (r *AIRun) Active() bool { return r.Status == AIQueued || r.Status == AIRunning }

// CanRestart says whether the run can be started again.
func (r *AIRun) CanRestart() bool {
	return r.Status == AIInterrupted || r.Status == AIFailed || r.Status == AICancelled
}

const aiRunSelect = `
	SELECT r.id, r.lang, r.user_id, u.name, coalesce(r.started_by, 0), coalesce(b.name, ''), r.model, r.input, r.suggest, r.notes,
		r.max_cost, r.status, r.translated, r.suggested, r.failed, r.steps, r.input_tokens, r.cached_tokens, r.output_tokens,
		r.cost, r.summary, r.error, r.created_at, coalesce(r.started_at, ''), coalesce(r.finished_at, '')
	FROM ai_runs r JOIN users u ON u.id = r.user_id LEFT JOIN users b ON b.id = r.started_by`

func scanAIRun(row interface{ Scan(...any) error }) (*AIRun, error) {
	r := &AIRun{}
	err := row.Scan(&r.ID, &r.Lang, &r.UserID, &r.UserName, &r.StartedBy, &r.StartedByName, &r.Model, &r.Input, &r.Suggest, &r.Notes,
		&r.MaxCost, &r.Status, &r.Translated, &r.Suggested, &r.Failed, &r.Steps, &r.InputTokens, &r.CachedTokens, &r.OutputTokens,
		&r.Cost, &r.Summary, &r.Error, &r.CreatedAt, &r.StartedAt, &r.FinishedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return r, err
}

// CreateAIRun queues a run of an AI user's model. Only Lang, UserID,
// StartedBy, Input, Suggest, Notes and MaxCost are used.
func (s *Store) CreateAIRun(ctx context.Context, r AIRun) (*AIRun, error) {
	if r.Input != AIInputMissing && r.Input != AIInputAll {
		return nil, fmt.Errorf("unknown input %q", r.Input)
	}
	if r.MaxCost <= 0 {
		return nil, fmt.Errorf("the cost limit must be more than 0")
	}
	var id int64
	err := s.w.QueryRowContext(ctx, `
		INSERT INTO ai_runs (lang, user_id, started_by, model, input, suggest, notes, max_cost)
		SELECT ?, id, ?, ai_model, ?, ?, ?, ? FROM users WHERE id = ? AND ai_model IS NOT NULL AND disabled = 0
		RETURNING id`,
		r.Lang, sql.NullInt64{Int64: r.StartedBy, Valid: r.StartedBy != 0}, r.Input, r.Suggest, strings.TrimSpace(r.Notes), r.MaxCost, r.UserID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	} else if err != nil {
		return nil, err
	}
	return s.AIRun(ctx, id)
}

func (s *Store) AIRun(ctx context.Context, id int64) (*AIRun, error) {
	return scanAIRun(s.r.QueryRowContext(ctx, aiRunSelect+" WHERE r.id = ?", id))
}

// AIRuns returns the latest runs, newest first.
func (s *Store) AIRuns(ctx context.Context, limit int) ([]*AIRun, error) {
	rows, err := s.r.QueryContext(ctx, aiRunSelect+" ORDER BY r.id DESC LIMIT ?", limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*AIRun
	for rows.Next() {
		r, err := scanAIRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// StartNextAIRun marks the oldest queued run as running and returns it, or
// ErrNotFound if none is queued.
func (s *Store) StartNextAIRun(ctx context.Context) (*AIRun, error) {
	var id int64
	err := s.w.QueryRowContext(ctx, `
		UPDATE ai_runs SET status = 'running', started_at = coalesce(started_at, datetime('now')), finished_at = NULL, error = ''
		WHERE id = (SELECT min(id) FROM ai_runs WHERE status = 'queued') RETURNING id`).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	} else if err != nil {
		return nil, err
	}
	return s.AIRun(ctx, id)
}

// FinishAIRun ends a running run with one of the finished states.
func (s *Store) FinishAIRun(ctx context.Context, id int64, status, summary, errMsg string) error {
	_, err := s.w.ExecContext(ctx, `
		UPDATE ai_runs SET status = ?, summary = ?, error = ?, finished_at = datetime('now') WHERE id = ? AND status = 'running'`,
		status, summary, errMsg, id)
	return err
}

// InterruptAIRuns marks running runs as interrupted, for when the server
// starts: whatever ran before was stopped with it.
func (s *Store) InterruptAIRuns(ctx context.Context) (int, error) {
	res, err := s.w.ExecContext(ctx, `
		UPDATE ai_runs SET status = 'interrupted', finished_at = datetime('now') WHERE status = 'running'`)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// RestartAIRun queues an interrupted, failed or cancelled run again. Its
// counters and cost keep adding up, so the cost limit covers the whole run.
func (s *Store) RestartAIRun(ctx context.Context, id int64) error {
	res, err := s.w.ExecContext(ctx, `
		UPDATE ai_runs SET status = 'queued', error = '', finished_at = NULL
		WHERE id = ? AND status IN ('interrupted', 'failed', 'cancelled')`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// CancelQueuedAIRun cancels a run that hasn't started. A running run is
// cancelled by the worker instead.
func (s *Store) CancelQueuedAIRun(ctx context.Context, id int64) (bool, error) {
	res, err := s.w.ExecContext(ctx, `
		UPDATE ai_runs SET status = 'cancelled', finished_at = datetime('now') WHERE id = ? AND status = 'queued'`, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// AIUsage is what one or more model calls used.
type AIUsage struct {
	Steps                                   int
	InputTokens, CachedTokens, OutputTokens int64
	Cost                                    float64
}

func (s *Store) AddAIRunUsage(ctx context.Context, id int64, u AIUsage) error {
	_, err := s.w.ExecContext(ctx, `
		UPDATE ai_runs SET steps = steps + ?, input_tokens = input_tokens + ?, cached_tokens = cached_tokens + ?,
			output_tokens = output_tokens + ?, cost = cost + ? WHERE id = ?`,
		u.Steps, u.InputTokens, u.CachedTokens, u.OutputTokens, u.Cost, id)
	return err
}

// AddAIRunCounts adds saved strings to a run and sets the number of strings
// that failed so far.
func (s *Store) AddAIRunCounts(ctx context.Context, id int64, translated, suggested, failed int) error {
	_, err := s.w.ExecContext(ctx, `
		UPDATE ai_runs SET translated = translated + ?, suggested = suggested + ?, failed = ? WHERE id = ?`,
		translated, suggested, failed, id)
	return err
}

type AIRunEvent struct {
	Text      string
	CreatedAt string
}

func (s *Store) AddAIRunEvent(ctx context.Context, id int64, text string) error {
	_, err := s.w.ExecContext(ctx, "INSERT INTO ai_run_events (run_id, text) VALUES (?, ?)", id, text)
	return err
}

// AIRunEvents returns a run's log, oldest first.
func (s *Store) AIRunEvents(ctx context.Context, id int64) ([]AIRunEvent, error) {
	rows, err := s.r.QueryContext(ctx, "SELECT text, created_at FROM ai_run_events WHERE run_id = ? ORDER BY id", id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AIRunEvent
	for rows.Next() {
		var e AIRunEvent
		if err := rows.Scan(&e.Text, &e.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// AIString is a string an AI run still has to do.
type AIString struct {
	Key, Source string
	Meta        json.RawMessage // the "@key" object, nil if absent
	Current     string          // the current translation, if any
	HasCurrent  bool
	Outdated    bool
}

// AIPending returns the strings an AI user still has to do for a language,
// in source order: those without a translation, and with input "all" also
// unapproved translations by others. Strings the AI user already translated
// or suggested something for, for the current source text, are done.
// Approved strings are never pending.
func (s *Store) AIPending(ctx context.Context, lang string, aiUserID int64, input string) ([]AIString, error) {
	rows, err := s.r.QueryContext(ctx, `
		SELECT s.key, s.text, s.meta, coalesce(t.text, ''), t.key IS NOT NULL, coalesce(t.source_hash != s.hash, 0)
		FROM source_strings s
		LEFT JOIN translations t ON t.lang = ?1 AND t.key = s.key
		WHERE s.removed = 0 AND s.draft = 0
			AND (t.key IS NULL OR (?3 = 'all' AND t.approved_by IS NULL
				AND NOT (t.user_id IS ?2 AND t.source_hash = s.hash)))
			AND NOT EXISTS (SELECT 1 FROM suggestions g
				WHERE g.lang = ?1 AND g.key = s.key AND g.user_id = ?2 AND g.source_hash = s.hash)
		ORDER BY s.position`, lang, aiUserID, input)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AIString
	for rows.Next() {
		var a AIString
		var meta sql.NullString
		if err := rows.Scan(&a.Key, &a.Source, &meta, &a.Current, &a.HasCurrent, &a.Outdated); err != nil {
			return nil, err
		}
		if meta.Valid {
			a.Meta = json.RawMessage(meta.String)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// AIMatch is an existing translation found by AILookup.
type AIMatch struct {
	Key, Source, Text string
	Approved          bool
	Human             bool // written or imported by a person
	Outdated          bool
}

// AILookup searches a language's translations by key, source or translated
// text, approved ones first, so an agent can keep terms consistent.
func (s *Store) AILookup(ctx context.Context, lang, q string, limit int) ([]AIMatch, error) {
	search, args := searchSQL(strings.TrimSpace(q))
	rows, err := s.r.QueryContext(ctx, `
		SELECT s.key, s.text, t.text, t.approved_by IS NOT NULL, t.origin != 'machine', t.source_hash != s.hash
		FROM source_strings s JOIN translations t ON t.key = s.key AND t.lang = ?
		WHERE s.removed = 0 AND s.draft = 0`+search+`
		ORDER BY t.approved_by IS NULL, t.source_hash != s.hash, s.position LIMIT ?`,
		append(append([]any{lang}, args...), limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AIMatch
	for rows.Next() {
		var m AIMatch
		if err := rows.Scan(&m.Key, &m.Source, &m.Text, &m.Approved, &m.Human, &m.Outdated); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// What AISave did with a text.
const (
	AITranslated = "translated" // saved as the translation
	AISuggested  = "suggested"
	AISame       = "same"  // same as the current translation; nothing to review
	AIError      = "error" // not saved, see Errors
)

type AIText struct{ Key, Text string }

type AISaved struct {
	Key      string
	Result   string
	Errors   []string
	Warnings []string // saved anyway, like in the editor
}

// AISaveJob says for whom and how AISave saves.
type AISaveJob struct {
	Lang     string
	AIUserID int64
	Input    string // AIInputMissing or AIInputAll
	Suggest  bool   // suggestions only
}

// AISave saves an AI user's texts by these rules, whatever the agent asks:
// texts with errors from arb.Check are not saved; strings without a
// translation get one, or a suggestion if the job says so; existing
// translations only ever get suggestions, and only with input "all";
// approved strings are never touched. Everything is credited to the AI user.
func (s *Store) AISave(ctx context.Context, job AISaveJob, texts []AIText) ([]AISaved, error) {
	var out []AISaved
	err := s.tx(ctx, func(tx *sql.Tx) error {
		out = out[:0]
		var model sql.NullString
		var disabled bool
		err := tx.QueryRowContext(ctx, "SELECT ai_model, disabled FROM users WHERE id = ?", job.AIUserID).Scan(&model, &disabled)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && (!model.Valid || disabled)) {
			return fmt.Errorf("user %d is not an active AI user", job.AIUserID)
		} else if err != nil {
			return err
		}
		for _, t := range texts {
			r, err := aiSave(ctx, tx, job, t.Key, t.Text)
			if err != nil {
				return err
			}
			out = append(out, r)
		}
		return nil
	})
	return out, err
}

func aiSave(ctx context.Context, tx *sql.Tx, job AISaveJob, key, text string) (AISaved, error) {
	r := AISaved{Key: key}
	fail := func(msg ...string) (AISaved, error) {
		r.Result, r.Errors = AIError, msg
		return r, nil
	}
	var src, hash string
	err := tx.QueryRowContext(ctx, "SELECT text, hash FROM source_strings WHERE key = ? AND removed = 0 AND draft = 0", key).Scan(&src, &hash)
	if errors.Is(err, sql.ErrNoRows) {
		return fail("Unknown key.")
	} else if err != nil {
		return r, err
	}
	errs, warns := arb.Check(src, text)
	if len(errs) > 0 {
		return fail(errs...)
	}
	r.Warnings = warns

	var curText, curHash string
	var approved bool
	err = tx.QueryRowContext(ctx, "SELECT text, source_hash, approved_by IS NOT NULL FROM translations WHERE lang = ? AND key = ?",
		job.Lang, key).Scan(&curText, &curHash, &approved)
	found := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return r, err
	}
	switch {
	case approved:
		return fail("This string is approved and stays as it is.")
	case found && job.Input != AIInputAll:
		return fail("This string is already translated and not part of this job.")
	case !found && !job.Suggest:
		if _, err := save(ctx, tx, job.Lang, key, text, "machine", job.AIUserID, job.AIUserID, 0, KeepApproval); err != nil {
			return r, err
		}
		r.Result = AITranslated
		return r, nil
	}
	// A suggestion equal to the current translation is stored too, though not
	// shown, so the string counts as done for this AI user.
	if err := suggest(ctx, tx, job.Lang, key, text, job.AIUserID); err != nil {
		return r, err
	}
	r.Result = AISuggested
	if found && curText == text && curHash == hash {
		r.Result = AISame
	}
	return r, nil
}
