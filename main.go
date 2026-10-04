package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/skip2/go-qrcode"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
)

type transport interface {
	IsConnected() bool
	IsLoggedIn() bool
	SendMessage(context.Context, types.JID, *waE2E.Message, ...whatsmeow.SendRequestExtra) (whatsmeow.SendResponse, error)
}

type app struct {
	db          *sql.DB
	client      transport
	token       string
	mu          sync.RWMutex
	loginStatus string
}

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	address := env("HTTP_ADDR", "127.0.0.1:8080")
	token := os.Getenv("API_TOKEN")
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("HTTP_ADDR: %w", err)
	}
	if ip := net.ParseIP(host); (ip == nil || !ip.IsLoopback()) && token == "" {
		return errors.New("set API_TOKEN when listening outside loopback")
	}
	dir, err := filepath.Abs(env("DATA_DIR", "data"))
	if err != nil {
		return err
	}
	if err = os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	dsn := (&url.URL{Scheme: "file", Path: filepath.Join(dir, "whatsapp.db")}).String() + "?_foreign_keys=on&_busy_timeout=5000&_journal_mode=WAL"
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	container := sqlstore.NewWithDB(db, "sqlite3", waLog.Stdout("Store", "WARN", true))
	if err = container.Upgrade(ctx); err != nil {
		return err
	}
	if err = initJobs(db); err != nil {
		return err
	}
	device, err := container.GetFirstDevice(ctx)
	if err != nil {
		return err
	}
	client := whatsmeow.NewClient(device, waLog.Stdout("WhatsApp", "WARN", true))
	a := &app{db: db, client: client, token: token, loginStatus: "connecting"}
	client.AddEventHandler(func(event any) {
		switch e := event.(type) {
		case *events.Connected:
			a.setLogin("connected")
			log.Print("WhatsApp connected")
		case *events.LoggedOut:
			a.setLogin("logged_out_restart_to_pair")
			log.Printf("WhatsApp logged out: %v", e.Reason)
		case *events.Disconnected:
			a.setLogin("disconnected")
		case *events.TemporaryBan:
			a.setLogin("temporarily_banned")
			log.Print(e)
		}
	})
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return err
	}
	defer listener.Close()
	server := &http.Server{Handler: a.routes(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 15 * time.Second, IdleTimeout: 60 * time.Second}
	serverErrors := make(chan error, 1)
	go func() { serverErrors <- server.Serve(listener) }()
	log.Printf("HTTP API listening on http://%s", address)
	if token != "" {
		log.Print("API requires Authorization: Bearer <API_TOKEN>")
	}
	var workers sync.WaitGroup
	workers.Add(2)
	go func() { defer workers.Done(); a.connect(ctx, client) }()
	go func() { defer workers.Done(); a.work(ctx) }()
	select {
	case <-ctx.Done():
	case err = <-serverErrors:
		stop()
	}
	stop()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = server.Shutdown(shutdownCtx)
	client.Disconnect()
	workers.Wait()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func (a *app) setLogin(status string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.loginStatus = status
}

func (a *app) connect(ctx context.Context, client *whatsmeow.Client) {
	if client.Store.ID != nil {
		if err := client.ConnectContext(ctx); err != nil {
			a.setLogin("connection_failed_restart_to_retry")
			log.Printf("Connect: %v", err)
		}
		return
	}
	// QR channels must be created before Connect, as specified by whatsmeow.
	for ctx.Err() == nil {
		qrChannel, err := client.GetQRChannel(ctx)
		if err != nil {
			log.Printf("QR channel: %v", err)
			return
		}
		if err = client.ConnectContext(ctx); err != nil {
			log.Printf("Connect: %v", err)
			a.setLogin("connection_failed_restart_to_retry")
			return
		}
		paired := false
		for event := range qrChannel {
			if event.Event == "code" {
				a.setLogin("scan_qr")
				log.Print("Scan this QR: WhatsApp → Linked devices → Link a device")
				if err := printQR(os.Stdout, event.Code); err != nil {
					log.Printf("Render QR: %v", err)
				}
			} else {
				log.Printf("Pairing: %s", event.Event)
				if event.Event == "success" {
					paired = true
				}
				a.setLogin(event.Event)
			}
		}
		if paired || ctx.Err() != nil {
			return
		}
		client.Disconnect()
		if !wait(ctx, 2*time.Second) {
			return
		}
	}
}

func wait(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// Two module rows per terminal line, with explicit black/white colors and
// the QR encoder's quiet border retained for phone cameras.
func printQR(w io.Writer, code string) error {
	qr, err := qrcode.New(code, qrcode.Medium)
	if err != nil {
		return err
	}
	bitmap := qr.Bitmap()
	var output strings.Builder
	for y := 0; y < len(bitmap); y += 2 {
		output.WriteString("\x1b[30;47m")
		for x, top := range bitmap[y] {
			bottom := y+1 < len(bitmap) && bitmap[y+1][x]
			switch {
			case top && bottom:
				output.WriteRune('█')
			case top:
				output.WriteRune('▀')
			case bottom:
				output.WriteRune('▄')
			default:
				output.WriteByte(' ')
			}
		}
		output.WriteString("\x1b[0m\n")
	}
	_, err = io.WriteString(w, output.String())
	return err
}
