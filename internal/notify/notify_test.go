package notify

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestTelegramClientPollingAndKeyboard(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/bottoken/getUpdates" && r.URL.Path != "/bottoken/sendMessage" {
			t.Fatalf("unexpected Telegram path: %s", r.URL.Path)
		}
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "getUpdates") {
			_, _ = w.Write([]byte(`{"ok":true,"result":[{"update_id":7,"message":{"chat":{"id":42},"from":{"id":42},"text":"/start"}}]}`))
			return
		}
		if r.Form.Get("chat_id") != "42" || r.Form.Get("text") != "hello" {
			t.Fatalf("unexpected send form: %v", r.Form)
		}
		if !strings.Contains(r.Form.Get("reply_markup"), "m:home") {
			t.Fatalf("keyboard was not encoded: %s", r.Form.Get("reply_markup"))
		}
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":1}}`))
	}))
	defer server.Close()

	client, err := NewTelegramClient("token", "custom", server.URL, "", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	updates, err := client.GetUpdates(context.Background(), 1)
	if err != nil || len(updates) != 1 {
		t.Fatalf("GetUpdates: updates=%v err=%v", updates, err)
	}
	if err := client.SendMessage(context.Background(), "42", "hello", map[string]any{"inline_keyboard": [][]map[string]string{{{"text": "Home", "callback_data": "m:home"}}}}); err != nil {
		t.Fatal(err)
	}
}

func TestTelegramClientRejectsInvalidCustomURLOnlyAtRequestTime(t *testing.T) {
	client, err := NewTelegramClient("token", "custom", "not a URL", "", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Call(context.Background(), "sendMessage", url.Values{"chat_id": {"1"}})
	if err == nil {
		t.Fatal("expected invalid URL error")
	}
}

func TestTelegramClientExplainsInvalidToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"ok":false,"description":"Not Found"}`))
	}))
	defer server.Close()

	client, err := NewTelegramClient("  token  ", "custom", server.URL, "", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if client.Token != "token" {
		t.Fatalf("token was not trimmed: %q", client.Token)
	}
	_, err = client.Call(context.Background(), "getMe", nil)
	if err == nil || !strings.Contains(err.Error(), "Bot Token 无效") {
		t.Fatalf("unexpected invalid-token error: %v", err)
	}
}
