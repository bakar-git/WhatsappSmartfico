package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"mime"
	"net/http"
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"google.golang.org/protobuf/proto"
)

const maxFileSize = 16 << 20 // 16 MiB

type attachment struct {
	data []byte
	hash string
}

func (a *app) enqueueMedia(w http.ResponseWriter, r *http.Request) {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "multipart/form-data" {
		writeJSON(w, 415, map[string]string{"error": "Send number, type, file, and optional caption as multipart/form-data"})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxFileSize+(64<<10))
	err = r.ParseMultipartForm(1 << 20)
	if r.MultipartForm != nil {
		defer r.MultipartForm.RemoveAll()
	}
	if err != nil {
		var sizeError *http.MaxBytesError
		code := http.StatusBadRequest
		if errors.As(err, &sizeError) {
			code = http.StatusRequestEntityTooLarge
		}
		writeJSON(w, code, map[string]string{"error": "Invalid multipart upload or request exceeds the 16 MiB file limit"})
		return
	}
	form := r.MultipartForm
	for name, values := range form.Value {
		if (name != "number" && name != "type" && name != "caption") || len(values) != 1 {
			writeJSON(w, 400, map[string]string{"error": "Allowed fields are number, type, file, caption; send each once"})
			return
		}
	}
	if len(form.File) != 1 || len(form.File["file"]) != 1 {
		writeJSON(w, 400, map[string]string{"error": "Provide exactly one file in the file field"})
		return
	}
	j := job{Number: strings.TrimSpace(r.PostFormValue("number")), Type: r.PostFormValue("type"), Message: r.PostFormValue("caption")}
	if !phonePattern.MatchString(j.Number) {
		writeJSON(w, 400, map[string]string{"error": "number must use international format, e.g. +923205401101"})
		return
	}
	if j.Type != "pdf" && j.Type != "image" {
		writeJSON(w, 400, map[string]string{"error": "type must be pdf or image"})
		return
	}
	if !utf8.ValidString(j.Message) || utf8.RuneCountInString(j.Message) > 1024 {
		writeJSON(w, 400, map[string]string{"error": "caption must contain at most 1024 characters"})
		return
	}
	header := form.File["file"][0]
	if header.Size <= 0 || header.Size > maxFileSize {
		writeJSON(w, 413, map[string]string{"error": "file must contain 1 byte to 16 MiB"})
		return
	}
	file, err := header.Open()
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "Could not read uploaded file"})
		return
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxFileSize+1))
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": "Could not read uploaded file"})
		return
	}
	if len(data) > maxFileSize {
		writeJSON(w, 413, map[string]string{"error": "file exceeds 16 MiB"})
		return
	}
	j.MimeType, err = validateMedia(j.Type, data)
	if err != nil {
		writeJSON(w, 415, map[string]string{"error": err.Error()})
		return
	}
	j.Filename = mediaFilename(header.Filename, j.MimeType)
	j.Size = int64(len(data))
	if !a.client.IsConnected() || !a.client.IsLoggedIn() {
		writeJSON(w, 503, map[string]string{"error": "WhatsApp is not connected; scan the QR code in the server console"})
		return
	}
	// Include file contents in deduplication: different receipts with the same
	// caption are distinct messages, unlike a repeated identical attachment.
	hash := sha256.New()
	_, _ = hash.Write([]byte(j.Type + "\x00" + j.Message + "\x00"))
	_, _ = hash.Write(data)
	a.queue(w, r, j, &attachment{data: data, hash: hex.EncodeToString(hash.Sum(nil))})
}

func validateMedia(kind string, data []byte) (string, error) {
	if kind == "pdf" {
		if bytes.HasPrefix(data, []byte("%PDF-")) {
			return "application/pdf", nil
		}
		return "", errors.New("type pdf requires a PDF file with a PDF header")
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || (format != "jpeg" && format != "png") {
		return "", errors.New("type image requires a JPEG or PNG image")
	}
	if config.Width <= 0 || config.Height <= 0 || int64(config.Width) > 40_000_000/int64(config.Height) {
		return "", errors.New("image dimensions must be positive and at most 40 megapixels")
	}
	if format == "jpeg" {
		return "image/jpeg", nil
	}
	return "image/png", nil
}

func mediaFilename(name, mimeType string) string {
	name = filepath.Base(strings.ReplaceAll(name, "\\", "/"))
	name = strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, name))
	ext := map[string]string{"application/pdf": ".pdf", "image/jpeg": ".jpg", "image/png": ".png"}[mimeType]
	if name == "" || name == "." || name == "blob" || len(name) > 200 {
		return "receipt" + ext
	}
	if !strings.EqualFold(filepath.Ext(name), ext) {
		name = strings.TrimSuffix(name, filepath.Ext(name)) + ext
	}
	return name
}

func (a *app) queue(w http.ResponseWriter, r *http.Request, j job, media *attachment) {
	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		writeJSON(w, 500, map[string]string{"error": "Could not create job"})
		return
	}
	j.ID, j.Status = hex.EncodeToString(id), "queued"
	tx, err := a.db.BeginTx(r.Context(), nil)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "Could not queue message"})
		return
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(r.Context(), `INSERT INTO jobs(id,number,message,status,created_at)
SELECT ?,?,?,?,? WHERE (SELECT COUNT(*) FROM jobs WHERE status IN ('queued','sending')) < 1000`, j.ID, j.Number, j.Message, j.Status, time.Now().UnixMilli())
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "Could not queue message"})
		return
	}
	count, err := result.RowsAffected()
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "Could not confirm queued message"})
		return
	}
	if count == 0 {
		writeJSON(w, 429, map[string]string{"error": "Message queue is full"})
		return
	}
	if media != nil {
		_, err = tx.ExecContext(r.Context(), `INSERT INTO job_media(job_id,type,filename,mime_type,file_size,data,content_hash) VALUES(?,?,?,?,?,?,?)`, j.ID, j.Type, j.Filename, j.MimeType, j.Size, media.data, media.hash)
		if err != nil {
			writeJSON(w, 500, map[string]string{"error": "Could not save attachment"})
			return
		}
	}
	if err = tx.Commit(); err != nil {
		writeJSON(w, 500, map[string]string{"error": "Could not save queued message"})
		return
	}
	w.Header().Set("Location", "/api/messages/"+j.ID)
	writeJSON(w, http.StatusAccepted, j)
}

func (a *app) prepareMessage(ctx context.Context, j job) (*waE2E.Message, error) {
	if j.Type == "text" {
		return &waE2E.Message{Conversation: proto.String(j.Message)}, nil
	}
	var data []byte
	if err := a.db.QueryRowContext(ctx, `SELECT data FROM job_media WHERE job_id=?`, j.ID).Scan(&data); err != nil {
		return nil, err
	}
	mediaType := whatsmeow.MediaDocument
	if j.Type == "image" {
		mediaType = whatsmeow.MediaImage
	}
	response, err := a.client.Upload(ctx, data, mediaType)
	if err != nil {
		return nil, fmt.Errorf("upload attachment: %w", err)
	}
	if j.Type == "pdf" {
		return &waE2E.Message{DocumentMessage: &waE2E.DocumentMessage{
			URL: &response.URL, DirectPath: &response.DirectPath, MediaKey: response.MediaKey,
			FileEncSHA256: response.FileEncSHA256, FileSHA256: response.FileSHA256, FileLength: &response.FileLength,
			Mimetype: proto.String(j.MimeType), FileName: proto.String(j.Filename), Title: proto.String(j.Filename), Caption: proto.String(j.Message),
		}}, nil
	}
	config, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	return &waE2E.Message{ImageMessage: &waE2E.ImageMessage{
		URL: &response.URL, DirectPath: &response.DirectPath, MediaKey: response.MediaKey,
		FileEncSHA256: response.FileEncSHA256, FileSHA256: response.FileSHA256, FileLength: &response.FileLength,
		Mimetype: proto.String(j.MimeType), Caption: proto.String(j.Message), Width: proto.Uint32(uint32(config.Width)), Height: proto.Uint32(uint32(config.Height)),
	}}, nil
}

func (a *app) setWorkerRunning(running bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.workerRunning = running
}
