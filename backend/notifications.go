package main

import (
	"crypto/tls"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime/quotedprintable"
	"net"
	"net/http"
	"net/mail"
	"net/smtp"
	"strconv"
	"strings"
	"time"
)

type notificationSettings struct {
	Recipient      string `json:"recipient"`
	NotifySuccess  bool   `json:"notifySuccess"`
	NotifyFailure  bool   `json:"notifyFailure"`
	SMTPConfigured bool   `json:"smtpConfigured"`
}

func (s *Server) handleNotificationSettings(w http.ResponseWriter, r *http.Request) {
	if s.config.Mode != "private" {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "Pengaturan notifikasi email hanya tersedia dalam mode privat."})
		return
	}

	switch r.Method {
	case http.MethodGet:
		settings, err := s.readNotificationSettings()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Gagal membaca pengaturan notifikasi."})
			return
		}
		settings.SMTPConfigured = s.smtpConfigured()
		writeJSON(w, http.StatusOK, settings)
	case http.MethodPut:
		r.Body = http.MaxBytesReader(w, r.Body, 4096)
		var settings notificationSettings
		if err := json.NewDecoder(r.Body).Decode(&settings); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Pengaturan notifikasi tidak valid."})
			return
		}
		recipient := strings.TrimSpace(settings.Recipient)
		if len(recipient) > 320 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Alamat email penerima terlalu panjang."})
			return
		}
		if recipient != "" {
			address, err := mail.ParseAddress(recipient)
			if err != nil || address.Address == "" {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Format alamat email penerima tidak valid."})
				return
			}
			recipient = address.Address
		}
		_, err := s.db.Exec(`UPDATE notification_settings
			SET recipient=?,notify_success=?,notify_failure=?,updated_at=? WHERE id=1`,
			recipient, settings.NotifySuccess, settings.NotifyFailure, time.Now().UTC().Format(time.RFC3339))
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Gagal menyimpan pengaturan notifikasi."})
			return
		}
		saved, err := s.readNotificationSettings()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Pengaturan tersimpan, tetapi gagal dibaca kembali."})
			return
		}
		saved.SMTPConfigured = s.smtpConfigured()
		writeJSON(w, http.StatusOK, saved)
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "Metode HTTP tidak diizinkan."})
	}
}

func (s *Server) readNotificationSettings() (notificationSettings, error) {
	var settings notificationSettings
	err := s.db.QueryRow(`SELECT recipient,notify_success,notify_failure
		FROM notification_settings WHERE id=1`).
		Scan(&settings.Recipient, &settings.NotifySuccess, &settings.NotifyFailure)
	return settings, err
}

func (s *Server) smtpConfigured() bool {
	config := s.config.SMTP
	from := config.From
	if from == "" {
		from = config.Username
	}
	_, fromErr := mail.ParseAddress(from)
	return strings.TrimSpace(config.Host) != "" &&
		config.Port > 0 && config.Port <= 65535 &&
		strings.TrimSpace(config.Username) != "" &&
		config.Password != "" &&
		strings.TrimSpace(from) != "" && fromErr == nil
}

func (s *Server) enqueueBuildNotification(buildID, status string) {
	if status != "success" && status != "failed" {
		return
	}
	var accessMode string
	if err := s.db.QueryRow(`SELECT access_mode FROM builds WHERE id=?`, buildID).Scan(&accessMode); err != nil {
		log.Printf("failed to read build mode for notification %s: %v", buildID, err)
		return
	}
	if accessMode == "public" {
		return
	}
	settings, err := s.readNotificationSettings()
	if err != nil {
		log.Printf("failed to read build notification settings for %s: %v", buildID, err)
		return
	}
	if settings.Recipient == "" || (status == "success" && !settings.NotifySuccess) ||
		(status == "failed" && !settings.NotifyFailure) {
		return
	}

	var projectID, variant, createdAt, startedAt, finishedAt, buildError string
	if err := s.db.QueryRow(`SELECT project_id,variant,created_at,COALESCE(started_at,''),
		COALESCE(finished_at,''),COALESCE(error,'') FROM builds WHERE id=?`, buildID).
		Scan(&projectID, &variant, &createdAt, &startedAt, &finishedAt, &buildError); err != nil {
		log.Printf("failed to read build details for notification %s: %v", buildID, err)
		return
	}

	rows, err := s.db.Query(`SELECT stream,message FROM build_logs WHERE build_id=?
		ORDER BY sequence DESC LIMIT 20`, buildID)
	if err != nil {
		log.Printf("failed to read build logs for notification %s: %v", buildID, err)
		return
	}
	logLines := make([]string, 0, 20)
	for rows.Next() {
		var stream, message string
		if err := rows.Scan(&stream, &message); err != nil {
			rows.Close()
			log.Printf("failed to scan build logs for notification %s: %v", buildID, err)
			return
		}
		if len(message) > 500 {
			message = message[:500] + "…"
		}
		logLines = append(logLines, fmt.Sprintf("[%s] %s", stream, message))
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		log.Printf("failed to read build logs for notification %s: %v", buildID, err)
		return
	}
	rows.Close()
	for left, right := 0, len(logLines)-1; left < right; left, right = left+1, right-1 {
		logLines[left], logLines[right] = logLines[right], logLines[left]
	}

	eventLabel := "berhasil"
	if status == "failed" {
		eventLabel = "gagal"
	}
	subject := fmt.Sprintf("[App Builder] Build %s: %s", eventLabel, buildID)
	var body strings.Builder
	fmt.Fprintf(&body, "Build %s\n\nBuild ID: %s\nProject ID: %s\nVariant: %s\n",
		eventLabel, buildID, projectID, variant)
	fmt.Fprintf(&body, "Dibuat: %s\nMulai: %s\nSelesai: %s\n", createdAt, startedAt, finishedAt)
	if started, startErr := time.Parse(time.RFC3339, startedAt); startErr == nil {
		if finished, finishErr := time.Parse(time.RFC3339, finishedAt); finishErr == nil {
			fmt.Fprintf(&body, "Durasi: %s\n", finished.Sub(started).Round(time.Second))
		}
	}
	if buildError != "" {
		body.WriteString("\nError:\n")
		body.WriteString(buildError)
		body.WriteString("\n")
	}
	if len(logLines) > 0 {
		body.WriteString("\nCuplikan log terakhir:\n")
		body.WriteString(strings.Join(logLines, "\n"))
		body.WriteString("\n")
	}
	message := body.String()
	if len(message) > 12000 {
		message = message[:12000] + "\n[log dipotong]"
	}

	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := s.db.Exec(`INSERT OR IGNORE INTO email_outbox
		(build_id,event,recipient,subject,body,status,next_attempt_at,created_at)
		VALUES(?,?,?,?,?,'pending',?,?)`, buildID, status, settings.Recipient, subject, message, now, now); err != nil {
		log.Printf("failed to queue build notification for %s: %v", buildID, err)
	}
}

func (s *Server) runEmailOutbox() {
	if _, err := s.db.Exec(`UPDATE email_outbox SET status='pending' WHERE status='sending'`); err != nil {
		log.Printf("failed to recover pending email notifications: %v", err)
	}
	s.pruneSentEmailNotifications()
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	pruneTicker := time.NewTicker(24 * time.Hour)
	defer pruneTicker.Stop()
	for {
		sent, err := s.sendNextQueuedEmail()
		if err != nil {
			log.Printf("email notification queue error: %v", err)
		}
		if sent {
			continue
		}
		select {
		case <-ticker.C:
		case <-pruneTicker.C:
			s.pruneSentEmailNotifications()
		}
	}
}

func (s *Server) pruneSentEmailNotifications() {
	cutoff := time.Now().UTC().AddDate(0, 0, -30).Format(time.RFC3339)
	if _, err := s.db.Exec(`DELETE FROM email_outbox WHERE status='sent' AND sent_at<?`, cutoff); err != nil {
		log.Printf("failed to prune sent email notifications: %v", err)
	}
}

func (s *Server) sendNextQueuedEmail() (bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	var item struct {
		id        int64
		recipient string
		subject   string
		body      string
		attempts  int
	}
	now := time.Now().UTC()
	err = tx.QueryRow(`SELECT id,recipient,subject,body,attempts FROM email_outbox
		WHERE status='pending' AND next_attempt_at<=? ORDER BY id LIMIT 1`,
		now.Format(time.RFC3339)).Scan(&item.id, &item.recipient, &item.subject, &item.body, &item.attempts)
	if errors.Is(err, sql.ErrNoRows) {
		tx.Rollback()
		return false, nil
	}
	if err != nil {
		tx.Rollback()
		return false, err
	}
	if _, err := tx.Exec(`UPDATE email_outbox SET status='sending',attempts=attempts+1 WHERE id=?`, item.id); err != nil {
		tx.Rollback()
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}

	if err := sendSMTPMessage(s.config.SMTP, item.recipient, item.subject, item.body); err != nil {
		attempt := item.attempts + 1
		delay := 30 * time.Second * time.Duration(1<<min(attempt-1, 7))
		nextAttempt := now.Add(delay).Format(time.RFC3339)
		if _, updateErr := s.db.Exec(`UPDATE email_outbox SET status='pending',attempts=?,next_attempt_at=?,last_error=? WHERE id=?`,
			attempt, nextAttempt, err.Error(), item.id); updateErr != nil {
			return true, fmt.Errorf("email send failed (%v) and retry state could not be saved: %w", err, updateErr)
		}
		log.Printf("email notification %d failed; retry scheduled at %s: %v", item.id, nextAttempt, err)
		return true, nil
	}
	if _, err := s.db.Exec(`UPDATE email_outbox SET status='sent',last_error='',sent_at=? WHERE id=?`,
		time.Now().UTC().Format(time.RFC3339), item.id); err != nil {
		return true, fmt.Errorf("email sent but outbox update failed: %w", err)
	}
	log.Printf("email notification %d sent", item.id)
	return true, nil
}

func sendSMTPMessage(config SMTPConfig, recipient, subject, body string) error {
	if strings.TrimSpace(config.Host) == "" || config.Port < 1 || config.Port > 65535 ||
		strings.TrimSpace(config.Username) == "" || config.Password == "" {
		return errors.New("SMTP sender is not fully configured")
	}
	if strings.ContainsAny(config.Host, "\r\n") || strings.ContainsAny(config.Username, "\r\n") {
		return errors.New("SMTP configuration contains invalid header characters")
	}
	fromValue := config.From
	if fromValue == "" {
		fromValue = config.Username
	}
	from, err := mail.ParseAddress(fromValue)
	if err != nil {
		return fmt.Errorf("invalid SMTP sender address: %w", err)
	}
	to, err := mail.ParseAddress(recipient)
	if err != nil {
		return fmt.Errorf("invalid recipient address: %w", err)
	}
	if strings.ContainsAny(subject, "\r\n") {
		return errors.New("email subject contains invalid header characters")
	}
	host := strings.TrimSpace(config.Host)
	address := net.JoinHostPort(host, strconv.Itoa(config.Port))
	dialer := net.Dialer{Timeout: 10 * time.Second}
	conn, err := dialer.Dial("tcp", address)
	if err != nil {
		return fmt.Errorf("connect SMTP server: %w", err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(25 * time.Second)); err != nil {
		return err
	}
	clientConn := net.Conn(conn)
	if config.Port == 465 {
		tlsConn := tls.Client(conn, &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12})
		if err := tlsConn.Handshake(); err != nil {
			return fmt.Errorf("start SMTP TLS: %w", err)
		}
		clientConn = tlsConn
	}
	client, err := smtp.NewClient(clientConn, host)
	if err != nil {
		return fmt.Errorf("initialize SMTP client: %w", err)
	}
	defer client.Close()
	if config.Port != 465 {
		if ok, _ := client.Extension("STARTTLS"); !ok {
			return errors.New("SMTP server does not support STARTTLS")
		}
		if err := client.StartTLS(&tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}); err != nil {
			return fmt.Errorf("start SMTP STARTTLS: %w", err)
		}
	}
	if err := client.Auth(smtp.PlainAuth("", config.Username, config.Password, host)); err != nil {
		return fmt.Errorf("SMTP authentication failed: %w", err)
	}
	if err := client.Mail(from.Address); err != nil {
		return fmt.Errorf("SMTP sender rejected: %w", err)
	}
	if err := client.Rcpt(to.Address); err != nil {
		return fmt.Errorf("SMTP recipient rejected: %w", err)
	}
	writer, err := client.Data()
	if err != nil {
		return fmt.Errorf("open SMTP message: %w", err)
	}
	headers := fmt.Sprintf("From: %s\r\nTo: %s\r\nSubject: %s\r\nDate: %s\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=UTF-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n",
		from.String(), to.String(), subject, time.Now().Format(time.RFC1123Z))
	if _, err := io.WriteString(writer, headers); err != nil {
		writer.Close()
		return fmt.Errorf("write SMTP headers: %w", err)
	}
	encoded := quotedprintable.NewWriter(writer)
	if _, err := io.WriteString(encoded, body); err != nil {
		encoded.Close()
		writer.Close()
		return fmt.Errorf("write SMTP message: %w", err)
	}
	if err := encoded.Close(); err != nil {
		writer.Close()
		return fmt.Errorf("encode SMTP message: %w", err)
	}
	if err := writer.Close(); err != nil {
		return fmt.Errorf("finish SMTP message: %w", err)
	}
	if err := client.Quit(); err != nil {
		return fmt.Errorf("finish SMTP session: %w", err)
	}
	return nil
}
