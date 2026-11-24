package main

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"
)

const vapidSubscriber = "mailto:admin@example.com"

type pushPayload struct {
	Title string `json:"title"`
	Body  string `json:"body"`
	URL   string `json:"url"`
	Tag   string `json:"tag"`
}

func (s *Server) handlePushVAPIDKey(w http.ResponseWriter, r *http.Request) {
	if s.config.Mode != "private" {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "browser notifications require private mode"})
		return
	}
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	_, publicKey, err := s.vapidKeys()
	if err != nil {
		log.Printf("failed to load VAPID key: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "gagal membaca kunci browser notification"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"publicKey": publicKey})
}

func (s *Server) handlePushSubscription(w http.ResponseWriter, r *http.Request) {
	if s.config.Mode != "private" {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "browser notifications require private mode"})
		return
	}
	ownerID, err := s.ensureOwner(w, r)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "gagal membaca identitas browser"})
		return
	}

	switch r.Method {
	case http.MethodGet:
		var count int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM push_subscriptions WHERE owner_id=?`, ownerID).Scan(&count); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "gagal membaca status browser notification"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"subscribed": count > 0})
	case http.MethodPut:
		r.Body = http.MaxBytesReader(w, r.Body, 8192)
		var subscription webpush.Subscription
		if err := json.NewDecoder(r.Body).Decode(&subscription); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "subscription browser tidak valid"})
			return
		}
		if err := validatePushSubscription(subscription); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		encoded, err := json.Marshal(subscription)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "gagal menyimpan subscription browser"})
			return
		}
		now := time.Now().UTC().Format(time.RFC3339)
		_, err = s.db.Exec(`INSERT INTO push_subscriptions(endpoint,owner_id,subscription,created_at,updated_at)
			VALUES(?,?,?,?,?) ON CONFLICT(endpoint) DO UPDATE SET owner_id=excluded.owner_id,
			subscription=excluded.subscription,updated_at=excluded.updated_at`,
			subscription.Endpoint, ownerID, string(encoded), now, now)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "gagal menyimpan subscription browser"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"subscribed": true})
	case http.MethodDelete:
		r.Body = http.MaxBytesReader(w, r.Body, 4096)
		var request struct {
			Endpoint string `json:"endpoint"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "subscription browser tidak valid"})
			return
		}
		var result sql.Result
		if request.Endpoint == "" {
			result, err = s.db.Exec(`DELETE FROM push_subscriptions WHERE owner_id=?`, ownerID)
		} else {
			if err := validatePushEndpoint(request.Endpoint); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "endpoint subscription tidak valid"})
				return
			}
			result, err = s.db.Exec(`DELETE FROM push_subscriptions WHERE endpoint=? AND owner_id=?`, request.Endpoint, ownerID)
		}
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "gagal menghapus subscription browser"})
			return
		}
		if _, err := result.RowsAffected(); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "gagal memastikan subscription browser terhapus"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"subscribed": false})
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
	}
}

func (s *Server) handlePushTest(w http.ResponseWriter, r *http.Request) {
	if s.config.Mode != "private" {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "browser notifications require private mode"})
		return
	}
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	ownerID, err := s.ensureOwner(w, r)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "gagal membaca identitas browser"})
		return
	}
	var subscriptionJSON string
	if err := s.db.QueryRow(`SELECT subscription FROM push_subscriptions WHERE owner_id=? LIMIT 1`, ownerID).
		Scan(&subscriptionJSON); errors.Is(err, sql.ErrNoRows) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "aktifkan browser notification terlebih dahulu"})
		return
	} else if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "gagal membaca subscription browser"})
		return
	}
	var subscription webpush.Subscription
	if err := json.Unmarshal([]byte(subscriptionJSON), &subscription); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "subscription browser tersimpan tidak valid"})
		return
	}
	payload, err := json.Marshal(pushPayload{
		Title: "notifikasi browser aktif",
		Body:  "browser ini siap menerima notifikasi hasil build.",
		URL:   "/",
		Tag:   "push-test",
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "gagal membuat notifikasi test"})
		return
	}
	if err := s.sendWebPush(subscription, payload); err != nil {
		log.Printf("test browser notification delivery failed")
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "notifikasi test gagal dikirim"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"sent": true})
}

func validatePushSubscription(subscription webpush.Subscription) error {
	if err := validatePushEndpoint(subscription.Endpoint); err != nil {
		return err
	}
	p256dh, err := base64.RawURLEncoding.DecodeString(subscription.Keys.P256dh)
	if err != nil || len(p256dh) != 65 || p256dh[0] != 4 {
		return errors.New("public key subscription browser tidak valid")
	}
	auth, err := base64.RawURLEncoding.DecodeString(subscription.Keys.Auth)
	if err != nil || len(auth) != 16 {
		return errors.New("auth key subscription browser tidak valid")
	}
	return nil
}

func validatePushEndpoint(endpoint string) error {
	parsed, err := url.ParseRequestURI(endpoint)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil ||
		(parsed.Port() != "" && parsed.Port() != "443") {
		return errors.New("endpoint push harus berupa URL HTTPS yang valid")
	}
	host := parsed.Hostname()
	if net.ParseIP(host) != nil || !strings.Contains(host, ".") || strings.ContainsAny(host, "\r\n") ||
		strings.EqualFold(host, "localhost") || strings.HasSuffix(strings.ToLower(host), ".localhost") {
		return errors.New("host endpoint push tidak valid")
	}
	return nil
}

func (s *Server) vapidKeys() (string, string, error) {
	var privateKey, publicKey string
	err := s.db.QueryRow(`SELECT private_key,public_key FROM webpush_keys WHERE id=1`).
		Scan(&privateKey, &publicKey)
	if err == nil {
		return privateKey, publicKey, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", "", err
	}
	privateKey, publicKey, err = webpush.GenerateVAPIDKeys()
	if err != nil {
		return "", "", err
	}
	if _, err := s.db.Exec(`INSERT OR IGNORE INTO webpush_keys(id,public_key,private_key) VALUES(1,?,?)`,
		publicKey, privateKey); err != nil {
		return "", "", err
	}
	err = s.db.QueryRow(`SELECT private_key,public_key FROM webpush_keys WHERE id=1`).
		Scan(&privateKey, &publicKey)
	return privateKey, publicKey, err
}

func (s *Server) enqueueBuildPushNotifications(buildID, status string) {
	if status != "success" && status != "failed" {
		return
	}
	settings, err := s.readNotificationSettings()
	if err != nil {
		log.Printf("failed to read push notification settings for %s: %v", buildID, err)
		return
	}
	if (status == "success" && !settings.NotifySuccess) || (status == "failed" && !settings.NotifyFailure) {
		return
	}

	var ownerID, projectID, variant string
	if err := s.db.QueryRow(`SELECT owner_id,project_id,variant FROM builds WHERE id=?`, buildID).
		Scan(&ownerID, &projectID, &variant); err != nil {
		log.Printf("failed to read build for browser notification %s: %v", buildID, err)
		return
	}
	if ownerID == "" {
		return
	}
	subscriptions, err := s.db.Query(`SELECT endpoint,subscription FROM push_subscriptions WHERE owner_id=?`, ownerID)
	if err != nil {
		log.Printf("failed to read browser subscriptions for build %s: %v", buildID, err)
		return
	}
	type queuedSubscription struct{ endpoint, subscription string }
	var targets []queuedSubscription
	for subscriptions.Next() {
		var target queuedSubscription
		if err := subscriptions.Scan(&target.endpoint, &target.subscription); err != nil {
			subscriptions.Close()
			log.Printf("failed to scan browser subscriptions for build %s: %v", buildID, err)
			return
		}
		targets = append(targets, target)
	}
	if err := subscriptions.Err(); err != nil {
		subscriptions.Close()
		log.Printf("failed to list browser subscriptions for build %s: %v", buildID, err)
		return
	}
	subscriptions.Close()
	if len(targets) == 0 {
		return
	}

	title := "build berhasil"
	if status == "failed" {
		title = "build gagal"
	}
	payload, err := json.Marshal(pushPayload{
		Title: title,
		Body:  fmt.Sprintf("%s · %s · %.8s", projectID, variant, buildID),
		URL:   "/",
		Tag:   "build-" + buildID,
	})
	if err != nil {
		log.Printf("failed to encode browser notification for build %s: %v", buildID, err)
		return
	}
	now := time.Now().UTC().Format(time.RFC3339)
	for _, target := range targets {
		if _, err := s.db.Exec(`INSERT OR IGNORE INTO push_outbox
			(build_id,event,endpoint,subscription,payload,status,next_attempt_at,created_at)
			VALUES(?,?,?, ?,?,'pending',?,?)`,
			buildID, status, target.endpoint, target.subscription, string(payload), now, now); err != nil {
			log.Printf("failed to queue browser notification for build %s: %v", buildID, err)
		}
	}
}

func (s *Server) runPushOutbox() {
	if _, err := s.db.Exec(`UPDATE push_outbox SET status='pending' WHERE status='sending'`); err != nil {
		log.Printf("failed to recover pending browser notifications: %v", err)
	}
	s.pruneSentPushNotifications()
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	pruneTicker := time.NewTicker(24 * time.Hour)
	defer pruneTicker.Stop()
	for {
		sent, err := s.sendNextPushNotification()
		if err != nil {
			log.Printf("browser notification queue error: %v", err)
		}
		if sent {
			continue
		}
		select {
		case <-ticker.C:
		case <-pruneTicker.C:
			s.pruneSentPushNotifications()
		}
	}
}

func (s *Server) pruneSentPushNotifications() {
	cutoff := time.Now().UTC().AddDate(0, 0, -30).Format(time.RFC3339)
	if _, err := s.db.Exec(`DELETE FROM push_outbox WHERE status IN ('sent','failed') AND created_at<?`, cutoff); err != nil {
		log.Printf("failed to prune browser notifications: %v", err)
	}
}

func (s *Server) sendNextPushNotification() (bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	var item struct {
		id           int64
		endpoint     string
		subscription string
		payload      string
		attempts     int
	}
	now := time.Now().UTC()
	err = tx.QueryRow(`SELECT id,endpoint,subscription,payload,attempts FROM push_outbox
		WHERE status='pending' AND next_attempt_at<=? ORDER BY id LIMIT 1`,
		now.Format(time.RFC3339)).Scan(&item.id, &item.endpoint, &item.subscription, &item.payload, &item.attempts)
	if errors.Is(err, sql.ErrNoRows) {
		tx.Rollback()
		return false, nil
	}
	if err != nil {
		tx.Rollback()
		return false, err
	}
	if _, err := tx.Exec(`UPDATE push_outbox SET status='sending',attempts=attempts+1 WHERE id=?`, item.id); err != nil {
		tx.Rollback()
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}

	var subscription webpush.Subscription
	if err := json.Unmarshal([]byte(item.subscription), &subscription); err != nil {
		return true, s.failPushDelivery(item.id, item.attempts+1, err)
	}
	response, sendErr := s.sendWebPushResponse(subscription, []byte(item.payload))
	if response != nil {
		defer response.Body.Close()
	}
	if sendErr == nil && response != nil {
		if response.StatusCode >= http.StatusOK && response.StatusCode < http.StatusMultipleChoices {
			if _, err := s.db.Exec(`UPDATE push_outbox SET status='sent',last_error='',sent_at=? WHERE id=?`,
				time.Now().UTC().Format(time.RFC3339), item.id); err != nil {
				return true, fmt.Errorf("browser notification sent but outbox update failed: %w", err)
			}
			return true, nil
		}
		if response.StatusCode == http.StatusGone || response.StatusCode == http.StatusNotFound {
			if _, err := s.db.Exec(`DELETE FROM push_subscriptions WHERE endpoint=? AND subscription=?`,
				item.endpoint, item.subscription); err != nil {
				return true, err
			}
			_, err := s.db.Exec(`UPDATE push_outbox SET status='failed',last_error=? WHERE id=?`,
				"push service expired the subscription", item.id)
			return true, err
		}
		sendErr = errors.New(pushFailureSummary(response, nil))
	}
	if sendErr == nil {
		sendErr = errors.New("push service returned no response")
	}
	sendErr = errors.New(pushFailureSummary(response, sendErr))
	if err := s.failPushDelivery(item.id, item.attempts+1, sendErr); err != nil {
		return true, err
	}
	log.Printf("browser notification %d failed: %s", item.id, sendErr)
	return true, nil
}

func (s *Server) failPushDelivery(id int64, attempts int, sendErr error) error {
	if attempts >= 8 {
		_, err := s.db.Exec(`UPDATE push_outbox SET status='failed',attempts=?,last_error=? WHERE id=?`,
			attempts, sendErr.Error(), id)
		return err
	}
	delay := 30 * time.Second * time.Duration(1<<min(attempts-1, 7))
	nextAttempt := time.Now().UTC().Add(delay).Format(time.RFC3339)
	_, err := s.db.Exec(`UPDATE push_outbox SET status='pending',attempts=?,next_attempt_at=?,last_error=? WHERE id=?`,
		attempts, nextAttempt, sendErr.Error(), id)
	return err
}

func (s *Server) sendWebPush(subscription webpush.Subscription, payload []byte) error {
	response, err := s.sendWebPushResponse(subscription, payload)
	if response != nil {
		defer response.Body.Close()
	}
	if err != nil {
		return err
	}
	if response == nil {
		return errors.New("push service returned no response")
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return errors.New(pushFailureSummary(response, nil))
	}
	return nil
}

func (s *Server) sendWebPushResponse(subscription webpush.Subscription, payload []byte) (*http.Response, error) {
	if err := validatePushSubscription(subscription); err != nil {
		return nil, err
	}
	privateKey, publicKey, err := s.vapidKeys()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	subject := strings.TrimSpace(s.config.VAPIDSubject)
	if subject == "" {
		subject = vapidSubscriber
	}
	return webpush.SendNotificationWithContext(ctx, payload, &subscription, &webpush.Options{
		HTTPClient:      newSafePushHTTPClient(),
		Subscriber:      subject,
		TTL:             60 * 60,
		Urgency:         webpush.UrgencyNormal,
		VAPIDPublicKey:  publicKey,
		VAPIDPrivateKey: privateKey,
	})
}

func pushFailureSummary(response *http.Response, err error) string {
	if response != nil {
		return fmt.Sprintf("push service returned HTTP %d", response.StatusCode)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "push request timed out"
	}
	return "push request failed"
}

func newSafePushHTTPClient() *http.Client {
	return &http.Client{
		Timeout:   20 * time.Second,
		Transport: &http.Transport{DialContext: dialPublicPushHost},
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func dialPublicPushHost(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	if port != "443" {
		return nil, errors.New("push endpoint must use port 443")
	}
	ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, fmt.Errorf("resolve push endpoint: %w", err)
	}
	dialer := net.Dialer{Timeout: 10 * time.Second}
	var lastErr error
	for _, ip := range ips {
		if !isPublicPushIP(ip) {
			continue
		}
		conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, errors.New("push endpoint did not resolve to a public IP address")
}

func isPublicPushIP(ip netip.Addr) bool {
	if ip.Is4In6() {
		ip = ip.Unmap()
	}
	if !ip.IsValid() || !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() ||
		ip.IsLinkLocalUnicast() || ip.IsMulticast() || ip.IsUnspecified() {
		return false
	}
	blocked := [...]netip.Prefix{
		netip.MustParsePrefix("100.64.0.0/10"),
		netip.MustParsePrefix("192.0.0.0/24"),
		netip.MustParsePrefix("192.0.2.0/24"),
		netip.MustParsePrefix("198.18.0.0/15"),
		netip.MustParsePrefix("198.51.100.0/24"),
		netip.MustParsePrefix("203.0.113.0/24"),
		netip.MustParsePrefix("2001:db8::/32"),
	}
	for _, prefix := range blocked {
		if prefix.Contains(ip) {
			return false
		}
	}
	return true
}
