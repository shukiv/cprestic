package notify

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func testMessage() Message {
	return Message{
		Event:   EventBackupFailed,
		Subject: "The backup of studio failed",
		Body:    "not enough room to stage this account: it needs 7.6 GiB free and there is 6.3 GiB",
		Host:    "mx.7171.online",
		Account: "studio",
	}
}

// What a phone or a chat window shows is one line, and it has to say
// which server as well as what happened: an operator with three servers
// learns nothing from "the backup of studio failed".
func TestAMessageSaysWhichServer(t *testing.T) {
	message := testMessage()
	if got := message.Line(); got != "mx.7171.online: The backup of studio failed" {
		t.Errorf("line = %q", got)
	}
	if !strings.Contains(message.Text(), "7.6 GiB") {
		t.Error("the detail is not in the message body")
	}
}

func TestNtfyCarriesTheTitleAndUrgency(t *testing.T) {
	var got *http.Request
	var body string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		body, got = string(raw), r
	}))
	defer server.Close()

	channel := Channel{Kind: KindNtfy, Config: map[string]string{
		"server": server.URL, "topic": "gniza-alerts",
	}, Secrets: map[string]string{"token": "tk_secret"}}
	if err := Send(context.Background(), channel, testMessage()); err != nil {
		t.Fatalf("send: %v", err)
	}

	if got.URL.Path != "/gniza-alerts" {
		t.Errorf("posted to %q", got.URL.Path)
	}
	if got.Header.Get("Title") != "mx.7171.online: The backup of studio failed" {
		t.Errorf("title = %q", got.Header.Get("Title"))
	}
	if got.Header.Get("Priority") != "high" {
		t.Errorf("a failed backup went out at priority %q", got.Header.Get("Priority"))
	}
	if got.Header.Get("Authorization") != "Bearer tk_secret" {
		t.Error("the token was not sent")
	}
	if !strings.Contains(body, "7.6 GiB") {
		t.Error("the body does not say what went wrong")
	}
}

// An account name or a restic error containing an underscore would be
// read as markup, so nothing is sent as markdown.
func TestTelegramSendsPlainText(t *testing.T) {
	var payload map[string]any
	var path string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&payload)
	}))
	defer server.Close()

	channel := Channel{Kind: KindTelegram,
		Config:  map[string]string{"api": server.URL, "chat_id": "-100123"},
		Secrets: map[string]string{"token": "12345:abc"},
	}
	if err := Send(context.Background(), channel, testMessage()); err != nil {
		t.Fatalf("send: %v", err)
	}
	if path != "/bot12345:abc/sendMessage" {
		t.Errorf("posted to %q", path)
	}
	if payload["chat_id"] != "-100123" {
		t.Errorf("chat = %v", payload["chat_id"])
	}
	if _, marked := payload["parse_mode"]; marked {
		t.Error("the message asks Telegram to parse markup in a restic error")
	}
}

// One webhook covers Slack, Discord and anything that takes a POST.
func TestWebhookSpeaksWhatSlackAndDiscordRead(t *testing.T) {
	var payload map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&payload)
	}))
	defer server.Close()

	channel := Channel{Kind: KindWebhook, Config: map[string]string{"url": server.URL}}
	if err := Send(context.Background(), channel, testMessage()); err != nil {
		t.Fatalf("send: %v", err)
	}
	for _, key := range []string{"text", "content", "event", "severity", "host", "account"} {
		if payload[key] == nil || payload[key] == "" {
			t.Errorf("the webhook payload has no %q", key)
		}
	}
	if payload["severity"] != "error" {
		t.Errorf("severity = %v", payload["severity"])
	}
}

// A service that refuses says why, and that reason is what the operator
// needs — not "notification failed".
func TestARefusedSendCarriesTheReason(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"topic is reserved"}`))
	}))
	defer server.Close()

	err := Send(context.Background(), Channel{Kind: KindNtfy,
		Config: map[string]string{"server": server.URL, "topic": "taken"}}, testMessage())
	if err == nil {
		t.Fatal("a 403 was treated as sent")
	}
	if !strings.Contains(err.Error(), "topic is reserved") {
		t.Errorf("the error does not say why: %v", err)
	}
}

// A channel that named no events wants the ones that report a problem.
// Somebody has to opt in to being told about the backups that worked.
func TestAChannelWithNoEventsTakesTheProblems(t *testing.T) {
	quiet := Channel{}
	for _, event := range []Event{EventBackupFailed, EventOverdue, EventStuck, EventDestinationDown} {
		if !quiet.Wants(event) {
			t.Errorf("a default channel does not want %q", event)
		}
	}
	if quiet.Wants(EventBackupSucceeded) {
		t.Error("a default channel would report every backup that worked")
	}

	chosen := Channel{Events: []Event{EventBackupSucceeded}}
	if !chosen.Wants(EventBackupSucceeded) || chosen.Wants(EventBackupFailed) {
		t.Error("a channel is not getting the events it asked for, and only those")
	}
}

// The interface says which field is missing rather than letting the send
// fail later against a real server.
func TestMissingNamesWhatIsNotFilledIn(t *testing.T) {
	missing := Missing(Channel{Kind: KindSMTP, Config: map[string]string{"host": "mail.example.com"}})
	if strings.Join(missing, ",") != "from,to" {
		t.Errorf("missing = %v, want from and to", missing)
	}
	if got := Missing(Channel{Kind: KindTelegram,
		Config: map[string]string{"chat_id": "1"}, Secrets: map[string]string{"token": "t"}}); len(got) != 0 {
		t.Errorf("a complete channel reported %v missing", got)
	}
}
