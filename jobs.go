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
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
)

type job struct {
	ID        string `json:"id"`
	Number    string `json:"number"`
	Message   string `json:"message"`
	Status    string `json:"status"`
	MessageID string `json:"message_id,omitempty"`
	Error     string `json:"error,omitempty"`
}

func initJobs(db *sql.DB) error {
	_, err := db.Exec(`
CREATE TABLE IF NOT EXISTS jobs (
 id TEXT PRIMARY KEY, number TEXT NOT NULL, message TEXT NOT NULL,
 status TEXT NOT NULL, message_id TEXT NOT NULL DEFAULT '', error TEXT NOT NULL DEFAULT '',
 created_at INTEGER NOT NULL);
CREATE INDEX IF NOT EXISTS jobs_queue ON jobs(status, created_at);
CREATE TABLE IF NOT EXISTS send_attempts (recipient TEXT NOT NULL, content_hash TEXT NOT NULL, at INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS settings (key TEXT PRIMARY KEY, value INTEGER NOT NULL);
UPDATE jobs SET status='unknown', error='Server stopped during send; delivery is unknown. Check WhatsApp before resending.' WHERE status='sending';`)
	return err
}

// Only one worker sends, so concurrent HTTP requests cannot overshoot the
// limiter between GetDelay and Record. SQLite keeps jobs and limits over restarts.
func (a *app) work(ctx context.Context) {
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
	for ctx.Err() == nil {
		if !a.client.IsConnected() || !a.client.IsLoggedIn() {
			if !wait(ctx, time.Second) {
				return
			}
			continue
		}
		var j job
		err := a.db.QueryRow(`SELECT id,number,message FROM jobs WHERE status='queued' ORDER BY created_at,rowid LIMIT 1`).Scan(&j.ID, &j.Number, &j.Message)
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
		sendCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		response, err := a.client.SendMessage(sendCtx, recipient, &waE2E.Message{Conversation: proto.String(j.Message)})
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
	if _, err := a.db.Exec(`UPDATE jobs SET status=?, message_id=?, error=? WHERE id=?`, status, messageID, detail, id); err != nil {
		log.Printf("Save job %s: %v", id, err)
	}
}
