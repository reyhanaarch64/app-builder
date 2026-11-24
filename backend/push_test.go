package main

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"
)

func testPushSubscription(endpoint string) string {
	publicKey := base64.RawURLEncoding.EncodeToString(append([]byte{4}, make([]byte, 64)...))
	authKey := base64.RawURLEncoding.EncodeToString(make([]byte, 16))
	encoded, _ := json.Marshal(map[string]any{
		"endpoint": endpoint,
		"keys":     map[string]string{"p256dh": publicKey, "auth": authKey},
	})
	return string(encoded)
}

func TestValidatePushEndpointRejectsUnsafeTargets(t *testing.T) {
	for _, endpoint := range []string{
		"http://push.example.test/subscription",
		"https://localhost/subscription",
		"https://127.0.0.1/subscription",
		"https://push.example.test:8443/subscription",
	} {
		if err := validatePushEndpoint(endpoint); err == nil {
			t.Errorf("validatePushEndpoint(%q) succeeded, want an error", endpoint)
		}
	}
	if err := validatePushEndpoint("https://fcm.googleapis.com/fcm/send/token"); err != nil {
		t.Fatalf("valid HTTPS push endpoint rejected: %v", err)
	}
}

func TestPublicPushIPRejectsPrivateAndReservedAddresses(t *testing.T) {
	for _, address := range []string{"127.0.0.1", "10.0.0.1", "192.168.1.1", "100.64.0.1", "2001:db8::1"} {
		ip := netip.MustParseAddr(address)
		if isPublicPushIP(ip) {
			t.Errorf("isPublicPushIP(%s) = true, want false", address)
		}
	}
	for _, address := range []string{"8.8.8.8", "2606:4700:4700::1111"} {
		ip := netip.MustParseAddr(address)
		if !isPublicPushIP(ip) {
			t.Errorf("isPublicPushIP(%s) = false, want true", address)
		}
	}
}

func TestVAPIDKeysAreGeneratedOnceAndPersisted(t *testing.T) {
	server := &Server{db: testDB(t)}
	private1, public1, err := server.vapidKeys()
	if err != nil {
		t.Fatal(err)
	}
	private2, public2, err := server.vapidKeys()
	if err != nil {
		t.Fatal(err)
	}
	if private1 == "" || public1 == "" || private1 != private2 || public1 != public2 {
		t.Fatal("VAPID keys were empty or changed between reads")
	}
	var count int
	if err := server.db.QueryRow(`SELECT COUNT(*) FROM webpush_keys`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("stored %d VAPID key pairs, want one", count)
	}
}

func TestPushSubscriptionAPIStoresAndRemovesCurrentBrowser(t *testing.T) {
	server := &Server{db: testDB(t), config: AppConfig{Mode: "private"}}
	request := httptest.NewRequest(http.MethodGet, "/api/notifications/push/subscription", nil)
	response := httptest.NewRecorder()
	server.handlePushSubscription(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("read push subscription returned %d: %s", response.Code, response.Body.String())
	}
	cookie := response.Result().Cookies()[0]

	subscription := testPushSubscription("https://fcm.googleapis.com/fcm/send/browser-token")
	request = httptest.NewRequest(http.MethodPut, "/api/notifications/push/subscription", strings.NewReader(subscription))
	request.AddCookie(cookie)
	response = httptest.NewRecorder()
	server.handlePushSubscription(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("save push subscription returned %d: %s", response.Code, response.Body.String())
	}

	request = httptest.NewRequest(http.MethodGet, "/api/notifications/push/subscription", nil)
	request.AddCookie(cookie)
	response = httptest.NewRecorder()
	server.handlePushSubscription(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"subscribed":true`) {
		t.Fatalf("push subscription status = %d: %s", response.Code, response.Body.String())
	}

	request = httptest.NewRequest(http.MethodDelete, "/api/notifications/push/subscription",
		strings.NewReader(`{"endpoint":"https://fcm.googleapis.com/fcm/send/browser-token"}`))
	request.AddCookie(cookie)
	response = httptest.NewRecorder()
	server.handlePushSubscription(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("delete push subscription returned %d: %s", response.Code, response.Body.String())
	}
	var count int
	if err := server.db.QueryRow(`SELECT COUNT(*) FROM push_subscriptions`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("stored %d push subscriptions after delete, want zero", count)
	}
}

func TestBuildPushQueueIsOwnerScopedAndDeduplicated(t *testing.T) {
	db := testDB(t)
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := db.Exec(`INSERT INTO builds(id,project_id,owner_id,status,variant,created_at)
		VALUES('push-build','project-1','owner-1','success','debug',?)`, now); err != nil {
		t.Fatal(err)
	}
	for _, owner := range []string{"owner-1", "owner-2"} {
		endpoint := "https://fcm.googleapis.com/fcm/send/" + owner
		if _, err := db.Exec(`INSERT INTO push_subscriptions(endpoint,owner_id,subscription,created_at,updated_at)
			VALUES(?,?,?,?,?)`, endpoint, owner, testPushSubscription(endpoint), now, now); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`UPDATE notification_settings SET notify_success=1,notify_failure=0 WHERE id=1`); err != nil {
		t.Fatal(err)
	}

	server := &Server{db: db}
	server.enqueueBuildPushNotifications("push-build", "success")
	server.enqueueBuildPushNotifications("push-build", "success")
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM push_outbox`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("queued %d push notifications, want one for the build owner", count)
	}
	var endpoint, payload string
	if err := db.QueryRow(`SELECT endpoint,payload FROM push_outbox`).Scan(&endpoint, &payload); err != nil {
		t.Fatal(err)
	}
	if endpoint != "https://fcm.googleapis.com/fcm/send/owner-1" ||
		!strings.Contains(payload, "build berhasil") || !strings.Contains(payload, "project-1") {
		t.Fatalf("unexpected push notification target or payload: %s %s", endpoint, payload)
	}

	if _, err := db.Exec(`UPDATE notification_settings SET notify_success=0 WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DELETE FROM push_outbox`); err != nil {
		t.Fatal(err)
	}
	server.enqueueBuildPushNotifications("push-build", "success")
	if err := db.QueryRow(`SELECT COUNT(*) FROM push_outbox`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("queued %d notifications while success notifications were disabled", count)
	}
}

func TestPushEndpointsArePrivateModeOnly(t *testing.T) {
	server := &Server{db: testDB(t), config: AppConfig{Mode: "public"}}
	response := httptest.NewRecorder()
	server.handlePushVAPIDKey(response, httptest.NewRequest(http.MethodGet, "/api/notifications/push/key", nil))
	if response.Code != http.StatusForbidden {
		t.Fatalf("public mode VAPID key returned %d, want forbidden", response.Code)
	}
}
