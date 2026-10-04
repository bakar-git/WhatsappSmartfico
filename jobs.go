package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"time"
	"unicode/utf8"

	antiban "github.com/TA-rathnayaka/whatsmeow-antiban"
	"go.mau.fi/whatsmeow/types"
)

type job struct {
	ID          string `json:"id"`
	Number      string `json:"number"`
	Message     string `json:"message,omitempty"`
	Type        string `json:"type"`
	Filename    string `json:"filename,omitempty"`
	MimeType    string `json:"mime_type,omitempty"`
	Size        int64  `json:"size,omitempty"`
	ContentHash string `json:"-"`
	Status      string `json:"status"`
	MessageID   string `json:"message_id,omitempty"`
	Error       string `json:"error,omitempty"`
}

func initJobs(db *sql.DB) error {
	_, err := db.Exec(`
CREATE TABLE IF NOT EXISTS jobs (
 id TEXT PRIMARY KEY, number TEXT NOT NULL, message TEXT NOT NULL,
 status TEXT NOT NULL, message_id TEXT NOT NULL DEFAULT '', error TEXT NOT NULL DEFAULT '',
 created_at INTEGER NOT NULL);
CREATE INDEX IF NOT EXISTS jobs_queue ON jobs(status, created_at);
CREATE TABLE IF NOT EXISTS job_media (
 job_id TEXT PRIMARY KEY REFERENCES jobs(id) ON DELETE CASCADE,
 type TEXT NOT NULL, filename TEXT NOT NULL, mime_type TEXT NOT NULL,
 file_size INTEGER NOT NULL, data BLOB NOT NULL, content_hash TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS send_attempts (recipient TEXT NOT NULL, content_hash TEXT NOT NULL, at INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS settings (key TEXT PRIMARY KEY, value INTEGER NOT NULL);
UPDATE jobs SET status='unknown', error='Server stopped during send; delivery is unknown. Check WhatsApp before resending.' WHERE status='sending';
UPDATE job_media SET data=x'' WHERE job_id IN (SELECT id FROM jobs WHERE status NOT IN ('queued','sending'));`)
	return err
}

const jobSelect = `SELECT j.id,j.number,j.message,j.status,j.message_id,j.error,
COALESCE(m.type,'text'),COALESCE(m.filename,''),COALESCE(m.mime_type,''),COALESCE(m.file_size,0),COALESCE(m.content_hash,'')
FROM jobs j LEFT JOIN job_media m ON m.job_id=j.id`

func readJob(row *sql.Row) (job, error) {
	var j job
	err := row.Scan(&j.ID, &j.Number, &j.Message, &j.Status, &j.MessageID, &j.Error,
		&j.Type, &j.Filename, &j.MimeType, &j.Size, &j.ContentHash)
	return j, err
}

// Only one worker sends, so concurrent HTTP requests cannot overshoot the
// limiter between GetDelay and Record. SQLite keeps jobs and limits over restarts.
func (a *app) work(ctx context.Context) {
	defer a.setWorkerRunning(false)
	preset := antiban.Resolve("conservative")
	limiter := antiban.NewRateLimiter(preset, time.Now().UnixNano())
	if _, err := a.db.Exec(`INSERT OR IGNORE INTO settings(key,value) VALUES('warmup_start',?)`, time.Now().UnixMilli()); err != nil {
		log.Print(err)
		return
	}
	var started int64
	if err := a.db.QueryRow(`SELECT value FROM settings WHERE key='warmup_start'`).Scan(&started); err != nil {
		log.Print(err)
		return
	}
	warmup := antiban.NewWarmup(preset, time.UnixMilli(started))
	known := map[string]bool{}
	rows, err := a.db.Query(`SELECT recipient, content_hash, at FROM send_attempts ORDER BY at`)
	if err != nil {
		log.Print(err)
		return
	}
	for rows.Next() {
		var recipient, hash string
		var at int64
		if err = rows.Scan(&recipient, &hash, &at); err != nil {
			break
		}
		when := time.UnixMilli(at)
		warmup.MaybeReset(when)
		warmup.Record(when)
		if when.After(time.Now().Add(-24 * time.Hour)) {
			limiter.Record(recipient, hash, when)
		}
		known[recipient] = true
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		log.Print(err)
		return
	}
	a.setWorkerRunning(true)
	for ctx.Err() == nil {
		if !a.client.IsConnected() || !a.client.IsLoggedIn() {
			if !wait(ctx, time.Second) {
				return
			}
			continue
		}
		j, err := readJob(a.db.QueryRow(jobSelect + ` WHERE j.status='queued' ORDER BY j.created_at,j.rowid LIMIT 1`))
		if errors.Is(err, sql.ErrNoRows) {
			if !wait(ctx, 250*time.Millisecond) {
				return
			}
			continue
		}
		if err != nil {
			log.Print(err)
			if !wait(ctx, time.Second) {
				return
			}
			continue
		}
		recipient := types.NewJID(j.Number[1:], types.DefaultUserServer)
		hash := fmt.Sprintf("%x", sha256.Sum256([]byte(j.Message)))
		if j.ContentHash != "" {
			hash = j.ContentHash
		}
		now := time.Now()
		warmup.MaybeReset(now)
		delay := limiter.GetDelay(recipient.String(), hash, !known[recipient.String()], utf8.RuneCountInString(j.Message), now)
		if !warmup.Allowed(now) || delay < 0 {
			a.finish(j.ID, "blocked", "", "Antiban rate or warm-up limit reached; message was not sent.")
			continue
		}
		if !wait(ctx, time.Duration(delay)*time.Millisecond) {
			return
		}
		if !a.client.IsConnected() || !a.client.IsLoggedIn() {
			continue
		}
		// Commit the attempt before touching the network. In-flight sends become
		// unknown after a crash rather than being retried and duplicating receipts.
		tx, err := a.db.Begin()
		if err != nil {
			log.Print(err)
			return
		}
		_, err = tx.Exec(`UPDATE jobs SET status='sending' WHERE id=?`, j.ID)
		attemptTime := time.Now()
		if err == nil {
			_, err = tx.Exec(`INSERT INTO send_attempts VALUES(?,?,?)`, recipient.String(), hash, attemptTime.UnixMilli())
		}
		if err != nil {
			_ = tx.Rollback()
			log.Print(err)
			return
		}
		if err = tx.Commit(); err != nil {
			log.Print(err)
			return
		}
		limiter.Record(recipient.String(), hash, attemptTime)
		warmup.Record(attemptTime)
		known[recipient.String()] = true
		timeout := 30 * time.Second
		if j.Type != "text" {
			timeout = 90 * time.Second
		}
		sendCtx, cancel := context.WithTimeout(ctx, timeout)
		message, err := a.prepareMessage(sendCtx, j)
		if err != nil {
			cancel()
			log.Printf("Prepare media job %s: %v", j.ID, err)
			a.finish(j.ID, "failed", "", "Attachment could not be prepared or uploaded; no message was sent.")
			continue
		}
		response, err := a.client.SendMessage(sendCtx, recipient, message)
		cancel()
		if err != nil {
			log.Printf("Send job %s: %v", j.ID, err)
			a.finish(j.ID, "unknown", "", "Send failed or timed out; delivery is uncertain. Check WhatsApp before resending.")
		} else {
			a.finish(j.ID, "sent", response.ID, "")
		}
	}
}

func (a *app) finish(id, status, messageID, detail string) {
	tx, err := a.db.Begin()
	if err != nil {
		log.Printf("Save job %s: %v", id, err)
		return
	}
	defer tx.Rollback()
	_, err = tx.Exec(`UPDATE jobs SET status=?, message_id=?, error=? WHERE id=?`, status, messageID, detail, id)
	if err == nil {
		_, err = tx.Exec(`UPDATE job_media SET data=x'' WHERE job_id=?`, id)
	}
	if err == nil {
		err = tx.Commit()
	}
	if err != nil {
		log.Printf("Save job %s: %v", id, err)
	}
}
