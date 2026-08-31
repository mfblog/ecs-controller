package server

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kori1c/ecs-controller/internal/app"
	"github.com/Kori1c/ecs-controller/internal/cloud"
	"github.com/Kori1c/ecs-controller/internal/store"
)

func TestTemplateAndStaticAssetsUseCompressionAndCacheHeaders(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	root := t.TempDir()
	templatePath := filepath.Join(root, "template.html")
	if err := os.WriteFile(templatePath, []byte("<!doctype html><link href=\"static/app.css?v=__ASSET_VERSION__\">"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "static"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "static", "app.css"), []byte("body { color: #123; }"), 0600); err != nil {
		t.Fatal(err)
	}

	handler := New(st, t.TempDir(), templatePath, "setup-token", nil).Handler()
	for _, test := range []struct {
		path     string
		cache    string
		wantBody string
		noToken  bool
	}{
		{path: "/", cache: "no-cache", wantBody: "static/app.css?v=", noToken: true},
		{path: "/static/app.css", cache: "public, max-age=31536000, immutable", wantBody: "body { color: #123; }"},
		{path: "/healthz", cache: "", wantBody: "\"ok\":true"},
	} {
		t.Run(test.path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, test.path, nil)
			req.Header.Set("Accept-Encoding", "gzip")
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, req)

			response := recorder.Result()
			defer response.Body.Close()
			if response.StatusCode != http.StatusOK {
				t.Fatalf("status=%d", response.StatusCode)
			}
			if got := response.Header.Get("Content-Encoding"); got != "gzip" {
				t.Fatalf("Content-Encoding=%q, want gzip", got)
			}
			if test.cache != "" && response.Header.Get("Cache-Control") != test.cache {
				t.Fatalf("Cache-Control=%q, want %q", response.Header.Get("Cache-Control"), test.cache)
			}
			reader, err := gzip.NewReader(response.Body)
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(reader)
			if err != nil {
				t.Fatal(err)
			}
			if err := reader.Close(); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(body), test.wantBody) {
				t.Fatalf("response body %q does not contain %q", body, test.wantBody)
			}
			if test.noToken && strings.Contains(string(body), "__ASSET_VERSION__") {
				t.Fatal("template did not replace asset version token")
			}
		})
	}
}

func TestVersionTagsForCommitsUsesOnlySemanticReleaseTags(t *testing.T) {
	firstCommit := strings.Repeat("a", 40)
	secondCommit := strings.Repeat("b", 40)
	invalidCommit := "not-a-commit"
	firstTag := updateVersionTag{Name: "v1.6.40"}
	firstTag.Commit.SHA = firstCommit
	duplicateTag := updateVersionTag{Name: "v9.9.9"}
	duplicateTag.Commit.SHA = firstCommit
	nonVersionTag := updateVersionTag{Name: "latest"}
	nonVersionTag.Commit.SHA = secondCommit
	invalidTag := updateVersionTag{Name: "v1.6.41"}
	invalidTag.Commit.SHA = invalidCommit

	versions := versionTagsForCommits([]updateVersionTag{firstTag, duplicateTag, nonVersionTag, invalidTag})
	if got := versions[firstCommit]; got != "v1.6.40" {
		t.Fatalf("version=%q, want v1.6.40", got)
	}
	if len(versions) != 1 {
		t.Fatalf("unexpected version tags: %#v", versions)
	}
}

func TestCheckForUpdateUsesGitHubReleaseTagsAsDisplayVersions(t *testing.T) {
	currentCommit := strings.Repeat("a", 40)
	latestCommit := strings.Repeat("b", 40)
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/repos/Kori1c/ecs-controller/commits/main":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"sha":      latestCommit,
				"commit":   map[string]any{"message": "release build"},
				"html_url": "https://github.com/Kori1c/ecs-controller/commit/" + latestCommit,
			})
		case "/repos/Kori1c/ecs-controller/tags":
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"name": "v1.6.40", "commit": map[string]any{"sha": latestCommit}},
				{"name": "v1.6.39", "commit": map[string]any{"sha": currentCommit}},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer api.Close()

	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	srv := New(st, t.TempDir(), "", "setup-token", nil)
	srv.githubAPIBase = api.URL
	srv.imageChecker = func(context.Context, string) (bool, string, error) {
		return true, "sha256:test", nil
	}

	previousCommit, previousVersion := app.Commit, app.Version
	app.Commit, app.Version = currentCommit, "sha-"+shortCommit(currentCommit)
	defer func() { app.Commit, app.Version = previousCommit, previousVersion }()

	recorder := httptest.NewRecorder()
	srv.checkForUpdate(recorder, httptest.NewRequest(http.MethodGet, "/index.php?action=check_update", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var result map[string]any
	if err := json.NewDecoder(recorder.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if got := result["current_version"]; got != "v1.6.39" {
		t.Fatalf("current_version=%q, want v1.6.39", got)
	}
	latest, ok := result["latest"].(map[string]any)
	if !ok || latest["version"] != "v1.6.40" {
		t.Fatalf("latest=%#v, want v1.6.40", result["latest"])
	}
}

func TestSetupLoginAndCSRF(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	dataDir := t.TempDir()
	templatePath := filepath.Join(t.TempDir(), "template.html")
	if err := os.WriteFile(templatePath, []byte("<!doctype html>ok"), 0600); err != nil {
		t.Fatal(err)
	}
	srv := New(st, dataDir, templatePath, "setup-token", nil)
	httpSrv := httptest.NewServer(srv.Handler())
	defer httpSrv.Close()
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}
	for _, path := range []string{"/index.php", "/index.php?action=view"} {
		resp, err := client.Get(httpSrv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			t.Fatalf("legacy page route %s status: %d", path, resp.StatusCode)
		}
		resp.Body.Close()
	}

	resp := postJSON(t, client, httpSrv.URL+"/index.php?action=setup", map[string]any{"admin_password": "correct horse battery staple", "traffic_threshold": 95}, map[string]string{"X-Setup-Token": "wrong"})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("wrong setup token status: %d", resp.StatusCode)
	}
	resp.Body.Close()
	resp = postJSON(t, client, httpSrv.URL+"/index.php?action=setup", map[string]any{"admin_password": "correct horse battery staple", "traffic_threshold": 95}, map[string]string{"X-Setup-Token": "setup-token"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("setup status: %d", resp.StatusCode)
	}
	csrf := resp.Header.Get("X-CSRF-Token")
	resp.Body.Close()
	if csrf == "" {
		t.Fatal("setup did not return csrf token")
	}

	resp = postJSON(t, client, httpSrv.URL+"/index.php?action=save_config", map[string]any{"traffic_threshold": 90, "api_interval": 600}, nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("missing csrf status: %d", resp.StatusCode)
	}
	resp.Body.Close()
	resp = postJSON(t, client, httpSrv.URL+"/index.php?action=save_config", map[string]any{"traffic_threshold": 90, "api_interval": 600}, map[string]string{"X-CSRF-Token": csrf})
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("valid csrf status: %d body=%s", resp.StatusCode, body)
	}
	resp.Body.Close()

	resp, err = client.Get(httpSrv.URL + "/index.php?action=check_login")
	if err != nil {
		t.Fatal(err)
	}
	var check map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&check)
	resp.Body.Close()
	if check["logged_in"] != true {
		t.Fatal("session was not established")
	}
	if got := resp.Header.Get("X-CSRF-Token"); got != csrf {
		t.Fatalf("check_login did not restore csrf token: got %q want %q", got, csrf)
	}

	var upload bytes.Buffer
	writer := multipart.NewWriter(&upload)
	part, err := writer.CreateFormFile("logo", "logo.png")
	if err != nil {
		t.Fatal(err)
	}
	// Minimal 1x1 PNG. The endpoint validates the detected MIME type before saving.
	if _, err := part.Write([]byte{
		0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a,
		0x00, 0x00, 0x00, 0x0d, 0x49, 0x48, 0x44, 0x52,
		0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
		0x08, 0x06, 0x00, 0x00, 0x00, 0x1f, 0x15, 0xc4,
		0x89,
	}); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, httpSrv.URL+"/index.php?action=upload_logo", &upload)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("X-CSRF-Token", csrf)
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("logo upload status: %d body=%s", resp.StatusCode, body)
	}
	resp.Body.Close()
	if _, err := os.Stat(dataDir + "/brand-logo.png"); err != nil {
		t.Fatalf("logo was not saved: %v", err)
	}
}

func TestAdminPasswordCanBeChangedFromConfig(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	srv := New(st, t.TempDir(), "", "setup-token", nil)
	httpSrv := httptest.NewServer(srv.Handler())
	defer httpSrv.Close()
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}

	resp := postJSON(t, client, httpSrv.URL+"/index.php?action=setup", map[string]any{"admin_password": "start1"}, map[string]string{"X-Setup-Token": "setup-token"})
	csrf := resp.Header.Get("X-CSRF-Token")
	resp.Body.Close()
	if csrf == "" {
		t.Fatal("setup did not return csrf token")
	}

	resp = postJSON(t, client, httpSrv.URL+"/index.php?action=save_config", map[string]any{"admin_password": "six123", "traffic_threshold": 95, "api_interval": 600}, map[string]string{"X-CSRF-Token": csrf})
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("password change status: %d body=%s", resp.StatusCode, body)
	}
	resp.Body.Close()
	if !st.CheckAdminPassword("six123") || st.CheckAdminPassword("start1") {
		t.Fatal("admin password was not changed")
	}

	resp = postJSON(t, client, httpSrv.URL+"/index.php?action=save_config", map[string]any{"admin_password": "********", "traffic_threshold": 95, "api_interval": 600}, map[string]string{"X-CSRF-Token": csrf})
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("masked password save status: %d body=%s", resp.StatusCode, body)
	}
	resp.Body.Close()
	if !st.CheckAdminPassword("six123") {
		t.Fatal("masked password overwrote the current password")
	}

	resp = postJSON(t, client, httpSrv.URL+"/index.php?action=save_config", map[string]any{"admin_password": "12345", "traffic_threshold": 95, "api_interval": 600}, map[string]string{"X-CSRF-Token": csrf})
	if resp.StatusCode != http.StatusBadRequest {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("short password status: %d body=%s", resp.StatusCode, body)
	}
	resp.Body.Close()
}

func TestPasswordLoginCanBeDisabledAfterPasskeyIsRegistered(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.SetAdminPassword("start1"); err != nil {
		t.Fatal(err)
	}

	srv := New(st, t.TempDir(), "", "setup-token", nil)
	httpSrv := httptest.NewServer(srv.Handler())
	defer httpSrv.Close()
	client := &http.Client{}

	if err := srv.saveConfig(map[string]any{"password_login_enabled": false}); err == nil {
		t.Fatal("password login was disabled without a Passkey")
	}
	if got := st.GetSetting("password_login_enabled", ""); got != "" && got != "1" {
		t.Fatalf("failed disable changed the setting: %q", got)
	}

	if err := st.SavePasskeyCredential("test-credential", `{"id":"test-credential"}`); err != nil {
		t.Fatal(err)
	}
	if err := srv.saveConfig(map[string]any{"password_login_enabled": false}); err != nil {
		t.Fatalf("disable password login: %v", err)
	}
	if got := st.GetSetting("password_login_enabled", ""); got != "0" {
		t.Fatalf("password login setting=%q, want 0", got)
	}

	resp := postJSON(t, client, httpSrv.URL+"/index.php?action=login", map[string]any{"password": "start1"}, nil)
	if resp.StatusCode != http.StatusForbidden {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("disabled password login status: %d body=%s", resp.StatusCode, body)
	}
	var disabledResponse map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&disabledResponse); err != nil {
		resp.Body.Close()
		t.Fatal(err)
	}
	resp.Body.Close()
	message, _ := disabledResponse["message"].(string)
	if !strings.Contains(message, "Passkey") {
		t.Fatalf("disabled password login message=%#v", disabledResponse)
	}

	if err := srv.saveConfig(map[string]any{"password_login_enabled": true}); err != nil {
		t.Fatalf("re-enable password login: %v", err)
	}
	resp = postJSON(t, client, httpSrv.URL+"/index.php?action=login", map[string]any{"password": "start1"}, nil)
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("re-enabled password login status: %d body=%s", resp.StatusCode, body)
	}
	resp.Body.Close()
}

func TestPasskeyWebAuthnUsesForwardedBrowserOrigin(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	srv := New(st, t.TempDir(), "", "setup-token", nil)

	req := httptest.NewRequest(http.MethodPost, "https://internal:8080/index.php?action=passkey_login_start", nil)
	req.Host = "internal:8080"
	req.Header.Set("X-Forwarded-Host", "console.example.com")
	req.Header.Set("X-Forwarded-Proto", "https")
	wa, err := srv.passkeyWebAuthn(req)
	if err != nil {
		t.Fatal(err)
	}
	if wa.Config.RPID != "console.example.com" {
		t.Fatalf("RP ID=%q, want console.example.com", wa.Config.RPID)
	}
	if len(wa.Config.RPOrigins) != 1 || wa.Config.RPOrigins[0] != "https://console.example.com" {
		t.Fatalf("RP origins=%#v, want https://console.example.com", wa.Config.RPOrigins)
	}
}

func TestNotificationSwitchesPersistAndReadBack(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	srv := New(st, t.TempDir(), "", "setup-token", nil)
	config := map[string]any{
		"traffic_threshold": 95,
		"api_interval":      600,
		"Notification": map[string]any{
			"email_enabled": false,
			"email":         "ops@example.com",
			"daily_enabled": true,
			"daily_time":    "01:30",
			"telegram": map[string]any{
				"enabled":     true,
				"confirm_ttl": 60,
			},
			"webhook": map[string]any{
				"enabled": true,
			},
		},
	}
	if err := srv.saveConfig(config); err != nil {
		t.Fatal(err)
	}

	settings := st.Settings()
	if settings["notify_email_enabled"] != "0" || settings["notify_tg_enabled"] != "1" || settings["notify_wh_enabled"] != "1" {
		t.Fatalf("notification switches were not normalized: email=%q telegram=%q webhook=%q", settings["notify_email_enabled"], settings["notify_tg_enabled"], settings["notify_wh_enabled"])
	}
	if settings["notify_daily_enabled"] != "1" || settings["notify_daily_time"] != "01:30" {
		t.Fatalf("daily summary settings were not normalized: enabled=%q time=%q", settings["notify_daily_enabled"], settings["notify_daily_time"])
	}
	readBack := notificationSettings(settings)
	telegram := readBack["telegram"].(map[string]any)
	webhook := readBack["webhook"].(map[string]any)
	if readBack["email_enabled"] != false || readBack["daily_enabled"] != true || readBack["daily_time"] != "01:30" || telegram["enabled"] != true || webhook["enabled"] != true {
		t.Fatalf("notification switches did not read back: %#v", readBack)
	}

	legacy := notificationSettings(map[string]string{
		"notify_email_enabled": "false",
		"notify_tg_enabled":    "true",
		"notify_wh_enabled":    "false",
	})
	legacyTelegram := legacy["telegram"].(map[string]any)
	legacyWebhook := legacy["webhook"].(map[string]any)
	if legacy["email_enabled"] != false || legacyTelegram["enabled"] != true || legacyWebhook["enabled"] != false {
		t.Fatalf("legacy notification switches were not interpreted: %#v", legacy)
	}
}

func TestSaveConfigRejectsDuplicateAccountRegion(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	srv := New(st, t.TempDir(), "", "setup-token", nil)
	err = srv.saveConfig(map[string]any{
		"Accounts": []any{
			map[string]any{"AccessKeyId": "ak", "AccessKeySecret": "sk", "regionId": "cn-hongkong"},
			map[string]any{"AccessKeyId": "ak", "AccessKeySecret": "sk", "regionId": "CN-HONGKONG"},
		},
	})
	if err == nil {
		t.Fatal("saveConfig accepted duplicate account and region")
	}
	if got := st.GetSetting("traffic_threshold", ""); got != "" {
		t.Fatalf("duplicate configuration partially changed settings: %q", got)
	}
}

func TestSaveConfigSyncsGroupWithGeneratedKey(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	srv := New(st, t.TempDir(), "", "setup-token", nil)
	srv.CloudFactory = func(app.Account) cloud.Client {
		return &fakeSyncClient{instances: []cloud.Instance{{ID: "i-generated", Status: "Running"}}}
	}
	if err := srv.saveConfig(map[string]any{
		"Accounts": []any{map[string]any{
			"AccessKeyId":     "ak",
			"AccessKeySecret": "sk",
			"regionId":        "cn-test",
			"maxTraffic":      200,
		}},
	}); err != nil {
		t.Fatal(err)
	}

	groups, err := st.LoadGroups()
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 1 || groups[0].GroupKey == "" {
		t.Fatalf("generated group key was not persisted: %#v", groups)
	}
	for _, log := range st.Logs("", 20) {
		if log["message"] == "账号组同步失败: 账号组不存在" {
			t.Fatalf("sync used the pre-save empty group key: %#v", log)
		}
	}
	accounts, err := st.LoadAccounts(false)
	if err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 1 || accounts[0].GroupKey != groups[0].GroupKey {
		t.Fatalf("synced account was not linked to the generated group: %#v", accounts)
	}
}

func TestTestAccountResolvesMaskedSecret(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.SaveGroups([]app.AccountGroup{{GroupKey: "group-1", AccessKeyID: "ak", AccessKeySecret: "sk", RegionID: "cn-hongkong"}}); err != nil {
		t.Fatal(err)
	}

	srv := New(st, t.TempDir(), "", "setup-token", nil)
	secret, err := srv.resolveMaskedAccountSecret(map[string]any{"groupKey": "group-1"}, "ak", "cn-hongkong")
	if err != nil || secret != "sk" {
		t.Fatalf("masked account secret was not restored: secret=%q err=%v", secret, err)
	}
	if _, err := srv.resolveMaskedAccountSecret(map[string]any{"groupKey": "group-1"}, "other-ak", "cn-hongkong"); err == nil {
		t.Fatal("secret was restored for a different access key")
	}
}

func TestTaskResponsePreservesFrontendCredentialFields(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.CreateTask("task-1", "preview-1", "group-1", "cn-test", "ecs.test", map[string]any{"loginPassword": "Password123!"}); err != nil {
		t.Fatal(err)
	}
	publicTask, err := st.GetTask("task-1")
	if err != nil {
		t.Fatal(err)
	}
	if publicTask.LoginPassword != "" {
		t.Fatal("queued task exposed its password")
	}
	task, err := st.GetTaskForWorker("task-1")
	if err != nil {
		t.Fatal(err)
	}
	response := taskResponse(task)
	if response["loginPassword"] != "Password123!" {
		t.Fatalf("camelCase password missing: %#v", response)
	}
	if response["task_id"] != "task-1" || response["taskId"] != "task-1" {
		t.Fatalf("task aliases missing: %#v", response)
	}
	if err := st.UpdateTask("task-1", map[string]any{"status": "success"}); err != nil {
		t.Fatal(err)
	}
	first, err := st.ConsumeTaskPassword("task-1")
	if err != nil || first.LoginPassword != "Password123!" {
		t.Fatalf("first credential read: task=%+v err=%v", first, err)
	}
	second, err := st.ConsumeTaskPassword("task-1")
	if err != nil || second.LoginPassword != "" {
		t.Fatalf("credential was not one-time: task=%+v err=%v", second, err)
	}
}

func TestConfigDoesNotDoubleCountCDTAggregateAcrossInstances(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.SaveGroups([]app.AccountGroup{{GroupKey: "group-1", AccessKeyID: "ak", AccessKeySecret: "sk", RegionID: "cn-test", MaxTraffic: 200}}); err != nil {
		t.Fatal(err)
	}
	for _, instanceID := range []string{"i-1", "i-2"} {
		if err := st.UpsertAccount(app.Account{AccessKeyID: "ak", AccessKeySecret: "sk", RegionID: "cn-test", GroupKey: "group-1", InstanceID: instanceID, TrafficUsed: 12.5, TrafficAPIStatus: "fallback_cdt", TrafficAPIMessage: "CDT aggregate", UpdatedAt: 100}); err != nil {
			t.Fatal(err)
		}
	}
	srv := New(st, t.TempDir(), "", "setup-token", nil)
	recorder := httptest.NewRecorder()
	srv.config(recorder)
	if recorder.Code != http.StatusOK {
		t.Fatalf("config status: %d", recorder.Code)
	}
	var response map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	items, ok := response["Accounts"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("unexpected account groups: %#v", response["Accounts"])
	}
	item := items[0].(map[string]any)
	if item["usageUsed"] != 12.5 {
		t.Fatalf("CDT aggregate was double-counted: %#v", item["usageUsed"])
	}
}

type fakePreflightClient struct{ cloud.Client }

func (f *fakePreflightClient) DescribeInstanceType(context.Context, string, string) (map[string]any, error) {
	return map[string]any{"InstanceTypeId": "ecs.test", "CpuArchitecture": "X86"}, nil
}

func (f *fakePreflightClient) DescribeAvailableZones(context.Context, string, string, string) ([]map[string]any, error) {
	return []map[string]any{{"ZoneId": "zone-a", "Status": "Available"}}, nil
}

func (f *fakePreflightClient) DescribeImagesForArchitecture(context.Context, string, string, string) ([]map[string]any, error) {
	return []map[string]any{{"ImageId": "img-x86", "OSName": "Windows Server 2022"}}, nil
}

func (f *fakePreflightClient) GetSystemDiskOptions(context.Context, string, string, string) ([]map[string]any, error) {
	return []map[string]any{{"value": "cloud_essd", "label": "ESSD", "min": 40, "max": 100, "unit": "GB"}}, nil
}

type fakeRefreshClient struct {
	cloud.Client
	instance      cloud.Instance
	monthlyBytes  float64
	monthlyPoints int
	monthlyErr    error
	monthlyCalls  int
	deltaBytes    float64
	deltaLastMS   int64
	deltaPoints   int
	deltaErr      error
	deltaCalls    int
}

func (f *fakeRefreshClient) DescribeInstance(context.Context, string, string) (*cloud.Instance, error) {
	instance := f.instance
	return &instance, nil
}

func (f *fakeRefreshClient) GetInstanceMonthlyTraffic(context.Context, string, string, string, int64, int64) (float64, int, error) {
	f.monthlyCalls++
	return f.monthlyBytes, f.monthlyPoints, f.monthlyErr
}

func (f *fakeRefreshClient) GetOutboundTrafficDelta(context.Context, string, string, string, int64, int64) (float64, int64, int, string, error) {
	f.deltaCalls++
	return f.deltaBytes, f.deltaLastMS, f.deltaPoints, "InternetOutRate", f.deltaErr
}

func TestRefreshAccountUsesMonthlyCMSValueBeforeDelta(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	if err := st.UpsertAccount(app.Account{
		ID:              1,
		AccessKeyID:     "ak",
		AccessKeySecret: "sk",
		RegionID:        "cn-hongkong",
		InstanceID:      "i-1",
		PublicIP:        "203.0.113.10",
		TrafficUsed:     0.0008,
	}); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	month := now.Format("2006-01")
	if _, err := st.SetInstanceTraffic(1, "i-1", month, 0.0008*1024*1024*1024, now.Add(-5*time.Minute).UnixMilli()); err != nil {
		t.Fatal(err)
	}

	fake := &fakeRefreshClient{
		instance:      cloud.Instance{ID: "i-1", Status: "Running", PublicIP: "203.0.113.10"},
		monthlyBytes:  8 * 1024 * 1024 * 1024,
		monthlyPoints: 1,
		deltaBytes:    10 * 1024 * 1024,
		deltaLastMS:   now.UnixMilli(),
		deltaPoints:   1,
	}
	srv := New(st, t.TempDir(), "", "setup-token", nil)
	srv.CloudFactory = func(app.Account) cloud.Client { return fake }
	recorder := httptest.NewRecorder()
	srv.refreshAccount(recorder, map[string]any{"id": 1})
	if recorder.Code != http.StatusOK {
		t.Fatalf("refresh status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if fake.monthlyCalls != 1 || fake.deltaCalls != 0 {
		t.Fatalf("unexpected CMS calls: monthly=%d delta=%d", fake.monthlyCalls, fake.deltaCalls)
	}

	sample, err := st.InstanceTrafficUsage(1, "i-1", month)
	if err != nil {
		t.Fatal(err)
	}
	if sample.TrafficBytes != 8*1024*1024*1024 {
		t.Fatalf("monthly CMS value was not stored absolutely: got=%v", sample.TrafficBytes)
	}
	account, err := st.Account(1, false)
	if err != nil {
		t.Fatal(err)
	}
	if account.TrafficUsed != 8 || account.TrafficBillingMonth != month || account.TrafficAPIStatus != "ok" {
		t.Fatalf("account traffic state=%+v", account)
	}
}

func TestRefreshAccountFallsBackToDeltaWhenMonthlyCMSHasNoPoints(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	if err := st.UpsertAccount(app.Account{ID: 1, AccessKeyID: "ak", AccessKeySecret: "sk", RegionID: "cn-test", InstanceID: "i-1"}); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	month := now.Format("2006-01")
	fake := &fakeRefreshClient{
		instance:      cloud.Instance{ID: "i-1", Status: "Running"},
		monthlyPoints: 0,
		deltaBytes:    12 * 1024 * 1024,
		deltaLastMS:   now.UnixMilli(),
		deltaPoints:   1,
	}
	srv := New(st, t.TempDir(), "", "setup-token", nil)
	srv.CloudFactory = func(app.Account) cloud.Client { return fake }
	recorder := httptest.NewRecorder()
	srv.refreshAccount(recorder, map[string]any{"id": 1})
	if recorder.Code != http.StatusOK {
		t.Fatalf("refresh status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if fake.monthlyCalls != 1 || fake.deltaCalls != 1 {
		t.Fatalf("monthly no-data did not fall back to delta: monthly=%d delta=%d", fake.monthlyCalls, fake.deltaCalls)
	}
	sample, err := st.InstanceTrafficUsage(1, "i-1", month)
	if err != nil {
		t.Fatal(err)
	}
	if sample.TrafficBytes != 12*1024*1024 {
		t.Fatalf("delta fallback was not stored: got=%v", sample.TrafficBytes)
	}
}

type fakeSyncClient struct {
	cloud.Client
	instances        []cloud.Instance
	describeErr      error
	publicNetworks   map[string]cloud.InstancePublicNetwork
	publicNetworkErr error
}

type fakeMissingInstanceClient struct{ cloud.Client }

func (f *fakeMissingInstanceClient) DescribeInstance(context.Context, string, string) (*cloud.Instance, error) {
	return nil, &cloud.APIError{Code: "InvalidInstanceId.NotFound", Message: "instance not found"}
}

func (f *fakeSyncClient) DescribeInstances(context.Context, string) ([]cloud.Instance, error) {
	return f.instances, f.describeErr
}

func (f *fakeSyncClient) DescribeInstance(_ context.Context, _ string, id string) (*cloud.Instance, error) {
	for _, instance := range f.instances {
		if instance.ID == id {
			copy := instance
			return &copy, nil
		}
	}
	if f.describeErr != nil {
		return nil, f.describeErr
	}
	return nil, &cloud.APIError{Code: "InvalidInstanceId.NotFound", HTTPStatus: 404}
}

func (f *fakeSyncClient) DescribeInstancePublicNetworks(context.Context, string, []string) (map[string]cloud.InstancePublicNetwork, error) {
	return f.publicNetworks, f.publicNetworkErr
}

func TestRefreshAccountPurgesProviderMissingInstance(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.UpsertAccount(app.Account{AccessKeyID: "ak", AccessKeySecret: "sk", RegionID: "cn-test", GroupKey: "group-1", InstanceID: "i-missing", InstanceStatus: "Running"}); err != nil {
		t.Fatal(err)
	}
	srv := New(st, t.TempDir(), "", "setup-token", nil)
	srv.CloudFactory = func(app.Account) cloud.Client { return &fakeMissingInstanceClient{} }
	recorder := httptest.NewRecorder()
	srv.refreshAccount(recorder, map[string]any{"accountId": 1})
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if account, err := st.Account(1, true); err == nil || account != nil {
		t.Fatalf("missing account remained after refresh: %#v err=%v", account, err)
	}
	job, err := st.ClaimJob(time.Minute)
	if err != nil || job != nil {
		t.Fatalf("refresh queued a cloud deletion: job=%#v err=%v", job, err)
	}
}

type fakeBillingDetailsClient struct {
	cloud.Client
	details []cloud.BillingDetail
	calls   int
}

func (f *fakeBillingDetailsClient) GetBillingDetails(context.Context, string, string, string) ([]cloud.BillingDetail, error) {
	f.calls++
	return f.details, nil
}

func TestBillDetailsFiltersRecentItemsAndCachesResult(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.SetSetting("enable_billing", "1"); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveGroups([]app.AccountGroup{{GroupKey: "group-billing", AccessKeyID: "ak", AccessKeySecret: "sk", RegionID: "cn-test", SiteType: "china"}}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertAccount(app.Account{ID: 7, AccessKeyID: "ak", AccessKeySecret: "sk", RegionID: "cn-test", SiteType: "china", GroupKey: "group-billing", InstanceID: "i-1"}); err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	fake := &fakeBillingDetailsClient{details: []cloud.BillingDetail{
		{Date: now.Format("2006-01-02"), ProductName: "云服务器 ECS", Amount: 1.25, Currency: "CNY"},
		{Date: now.AddDate(0, 0, -20).Format("2006-01-02"), ProductName: "云盘", Amount: 0.10, Currency: "CNY"},
	}}
	srv := New(st, t.TempDir(), "", "setup-token", nil)
	srv.CloudFactory = func(app.Account) cloud.Client { return fake }
	body := strings.NewReader(`{"group_key":"group-billing","days":1}`)
	req := httptest.NewRequest(http.MethodPost, "/index.php?action=get_bill_details", body)
	recorder := httptest.NewRecorder()
	srv.billDetails(recorder, req, map[string]any{"group_key": "group-billing", "days": 1})
	if recorder.Code != http.StatusOK {
		t.Fatalf("bill details status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Data struct {
			Items []cloud.BillingDetail `json:"items"`
			Total float64               `json:"total"`
		} `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Data.Items) != 1 || response.Data.Items[0].ProductName != "云服务器 ECS" || response.Data.Total != 1.25 {
		t.Fatalf("unexpected filtered details: %#v", response.Data)
	}

	recorder = httptest.NewRecorder()
	srv.billDetails(recorder, req, map[string]any{"group_key": "group-billing", "days": 1})
	if recorder.Code != http.StatusOK || fake.calls != 1 {
		t.Fatalf("cached bill details status=%d calls=%d", recorder.Code, fake.calls)
	}
}

func TestBillingDatesIncludesPreviousMonth(t *testing.T) {
	dates := billingDates("2026-07-27", "2026-08-02")
	if len(dates) != 7 || dates[0] != "2026-07-27" || dates[6] != "2026-08-02" {
		t.Fatalf("unexpected billing dates: %#v", dates)
	}
}

func TestEnrichBillingDetailsAddsCurrentResourceWithoutChangingBillValues(t *testing.T) {
	items := []cloud.BillingDetail{{
		InstanceID: "eip-1",
		Usage:      22,
		Amount:     0.01,
	}}
	enrichBillingDetails(items, map[string]cloud.BillingResource{
		"eip-1": {
			InstanceID: "i-1",
			EIP: &cloud.BillingEIP{
				AllocationID: "eip-1",
				Status:       "InUse",
				Bandwidth:    200,
				Count:        1,
			},
		},
	})

	if items[0].CurrentResource == nil || items[0].CurrentResource.EIP == nil || items[0].CurrentResource.EIP.Bandwidth != 200 {
		t.Fatalf("current resource not attached: %#v", items[0])
	}
	if items[0].Usage != 22 || items[0].Amount != 0.01 {
		t.Fatalf("billing values changed during enrichment: %#v", items[0])
	}
}

func TestSyncGroupPreservesReleaseAndPurgesMissingInstances(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.SaveGroups([]app.AccountGroup{{GroupKey: "group-1", AccessKeyID: "ak", AccessKeySecret: "sk", RegionID: "cn-test", MaxTraffic: 200}}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertAccount(app.Account{AccessKeyID: "ak", AccessKeySecret: "sk", RegionID: "cn-test", GroupKey: "group-1", InstanceID: "i-release", InstanceStatus: "Releasing"}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertAccount(app.Account{AccessKeyID: "ak", AccessKeySecret: "sk", RegionID: "cn-test", GroupKey: "group-1", InstanceID: "i-missing", InstanceStatus: "Stopped"}); err != nil {
		t.Fatal(err)
	}
	srv := New(st, t.TempDir(), "", "setup-token", nil)
	srv.CloudFactory = func(app.Account) cloud.Client {
		return &fakeSyncClient{instances: []cloud.Instance{{ID: "i-release", Status: "Running", PublicIP: "203.0.113.20"}}}
	}
	count, err := srv.syncGroup("group-1")
	if err != nil || count != 1 {
		t.Fatalf("sync result: %d %v", count, err)
	}
	accounts, err := st.LoadAccounts(false)
	if err != nil {
		t.Fatal(err)
	}
	var release *app.Account
	for i := range accounts {
		switch accounts[i].InstanceID {
		case "i-release":
			release = &accounts[i]
		}
	}
	if release == nil || release.InstanceStatus != "Releasing" {
		t.Fatalf("release state was resurrected: %#v", release)
	}
	if _, err := st.Account(2, true); err == nil {
		t.Fatal("missing instance remained in the database")
	}
	job, err := st.ClaimJob(time.Minute)
	if err != nil || job != nil {
		t.Fatalf("external deletion unexpectedly queued a cloud release job: %#v %v", job, err)
	}
}

func TestSyncGroupRemovesCloudMissingReleaseFailedRecords(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.SaveGroups([]app.AccountGroup{{GroupKey: "group-1", AccessKeyID: "ak", AccessKeySecret: "sk", RegionID: "cn-test", MaxTraffic: 200}}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertAccount(app.Account{AccessKeyID: "ak", AccessKeySecret: "sk", RegionID: "cn-test", GroupKey: "group-1", InstanceID: "i-old", PublicIP: "203.0.113.10", InstanceStatus: "ReleaseFailed"}); err != nil {
		t.Fatal(err)
	}
	srv := New(st, t.TempDir(), "", "setup-token", nil)
	srv.CloudFactory = func(app.Account) cloud.Client {
		return &fakeSyncClient{instances: []cloud.Instance{{ID: "i-current", Status: "Running", PublicIP: "203.0.113.10"}}}
	}
	if count, err := srv.syncGroup("group-1"); err != nil || count != 1 {
		t.Fatalf("sync result: count=%d err=%v", count, err)
	}
	visible, err := st.LoadAccounts(false)
	if err != nil {
		t.Fatal(err)
	}
	if len(visible) != 1 || visible[0].InstanceID != "i-current" {
		t.Fatalf("unexpected visible accounts: %#v", visible)
	}
	all, err := st.LoadAccounts(true)
	if err != nil {
		t.Fatal(err)
	}
	for _, account := range all {
		if account.InstanceID == "i-old" {
			t.Fatalf("release-failed orphan remained in the database: %#v", account)
		}
	}
	if job, err := st.ClaimJob(time.Second); err != nil || job != nil {
		t.Fatalf("orphan cleanup unexpectedly queued another release job: job=%#v err=%v", job, err)
	}
}

func TestSyncGroupKeepsReleaseFailedRecordWhenCloudSyncFails(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.SaveGroups([]app.AccountGroup{{GroupKey: "group-1", AccessKeyID: "ak", AccessKeySecret: "sk", RegionID: "cn-test", MaxTraffic: 200}}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertAccount(app.Account{AccessKeyID: "ak", AccessKeySecret: "sk", RegionID: "cn-test", GroupKey: "group-1", InstanceID: "i-old", InstanceStatus: "ReleaseFailed"}); err != nil {
		t.Fatal(err)
	}
	srv := New(st, t.TempDir(), "", "setup-token", nil)
	srv.CloudFactory = func(app.Account) cloud.Client {
		return &fakeSyncClient{describeErr: fmt.Errorf("temporary DescribeInstances failure")}
	}
	if _, err := srv.syncGroup("group-1"); err == nil {
		t.Fatal("sync unexpectedly succeeded")
	}
	account, err := st.Account(1, false)
	if err != nil {
		t.Fatal(err)
	}
	if account.InstanceStatus != "ReleaseFailed" {
		t.Fatalf("failed cloud sync changed local release state: %#v", account)
	}
}

func TestSyncGroupKeepsReleaseFailedRecordWhenInstanceStillExists(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.SaveGroups([]app.AccountGroup{{GroupKey: "group-1", AccessKeyID: "ak", AccessKeySecret: "sk", RegionID: "cn-test", MaxTraffic: 200}}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertAccount(app.Account{AccessKeyID: "ak", AccessKeySecret: "sk", RegionID: "cn-test", GroupKey: "group-1", InstanceID: "i-existing", InstanceStatus: "ReleaseFailed"}); err != nil {
		t.Fatal(err)
	}
	srv := New(st, t.TempDir(), "", "setup-token", nil)
	srv.CloudFactory = func(app.Account) cloud.Client {
		return &fakeSyncClient{instances: []cloud.Instance{{ID: "i-existing", Status: "Running"}}}
	}
	if count, err := srv.syncGroup("group-1"); err != nil || count != 1 {
		t.Fatalf("sync result: count=%d err=%v", count, err)
	}
	account, err := st.Account(1, false)
	if err != nil {
		t.Fatal(err)
	}
	if account.InstanceID != "i-existing" || account.InstanceStatus != "Running" {
		t.Fatalf("existing cloud instance was not retained and refreshed: %#v", account)
	}
}

func TestSyncGroupRefreshesBoundEIPBandwidth(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	group := app.AccountGroup{GroupKey: "group-1", AccessKeyID: "ak", AccessKeySecret: "sk", RegionID: "cn-hongkong", MaxTraffic: 200}
	if err := st.SaveGroups([]app.AccountGroup{group}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertAccount(app.Account{AccessKeyID: "ak", AccessKeySecret: "sk", RegionID: "cn-hongkong", GroupKey: "group-1", InstanceID: "i-1", InternetBandwidth: 10, PublicIPMode: "ecs_public_ip"}); err != nil {
		t.Fatal(err)
	}

	srv := New(st, t.TempDir(), "", "setup-token", nil)
	srv.CloudFactory = func(app.Account) cloud.Client {
		return &fakeSyncClient{
			instances: []cloud.Instance{{ID: "i-1", Status: "Running", PublicIP: "203.0.113.20"}},
			publicNetworks: map[string]cloud.InstancePublicNetwork{
				"i-1": {AllocationID: "eip-1", Address: "203.0.113.20", Bandwidth: 200},
			},
		}
	}
	if count, err := srv.syncGroup("group-1"); err != nil || count != 1 {
		t.Fatalf("sync result: count=%d err=%v", count, err)
	}
	accounts, err := st.LoadAccounts(false)
	if err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 1 {
		t.Fatalf("accounts=%#v", accounts)
	}
	account := accounts[0]
	if account.InternetBandwidth != 200 || account.PublicIPMode != "eip" || account.EIPAllocationID != "eip-1" || account.PublicIP != "203.0.113.20" {
		t.Fatalf("EIP network was not refreshed: %#v", account)
	}
	if account.EIPManaged {
		t.Fatal("externally discovered EIP was incorrectly marked controller-managed")
	}
}

func TestPreviewUsesDynamicArchitectureZoneDiskAndWindowsPort(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.SaveGroups([]app.AccountGroup{{GroupKey: "group-1", AccessKeyID: "ak", AccessKeySecret: "sk", RegionID: "cn-test", Remark: "test"}}); err != nil {
		t.Fatal(err)
	}
	srv := New(st, t.TempDir(), "", "setup-token", nil)
	srv.CloudFactory = func(app.Account) cloud.Client { return &fakePreflightClient{} }
	httpSrv := httptest.NewServer(srv.Handler())
	defer httpSrv.Close()
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}
	resp := postJSON(t, client, httpSrv.URL+"/index.php?action=setup", map[string]any{"admin_password": "correct horse battery staple"}, map[string]string{"X-Setup-Token": "setup-token"})
	csrf := resp.Header.Get("X-CSRF-Token")
	resp.Body.Close()
	if csrf == "" {
		t.Fatal("setup did not return csrf token")
	}
	resp = postJSON(t, client, httpSrv.URL+"/index.php?action=preview_ecs_create", map[string]any{
		"accountGroupKey": "group-1", "instanceType": "ecs.test", "osKey": "windows_2022", "zoneId": "zone-a",
		"systemDiskCategory": "cloud_essd_entry", "systemDiskSize": 20, "publicIpMode": "ecs_public_ip", "clientCidrIp": "192.0.2.10/32",
	}, map[string]string{"X-CSRF-Token": csrf})
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("preview status: %d body=%s", resp.StatusCode, body)
	}
	var result map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		resp.Body.Close()
		t.Fatal(err)
	}
	resp.Body.Close()
	summary, ok := result["summary"].(map[string]any)
	if !ok || summary["imageId"] != "img-x86" || summary["loginPort"] != float64(3389) || summary["zoneId"] != "zone-a" {
		t.Fatalf("dynamic preview fields missing: %#v", result)
	}
	disk, ok := summary["systemDisk"].(map[string]any)
	if !ok || disk["category"] != "cloud_essd" || disk["size"] != float64(40) || disk["min"] != float64(40) {
		t.Fatalf("dynamic disk fields missing: %#v", summary["systemDisk"])
	}
}

func TestOnlineUpdateRequestRequiresUpdaterAndPersistsTarget(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	srv := New(st, t.TempDir(), "", "setup-token", nil)
	request := httptest.NewRequest(http.MethodPost, "/index.php?action=start_update", nil)
	recorder := httptest.NewRecorder()
	srv.startUpdate(recorder, request, map[string]any{"target_commit": strings.Repeat("a", 40)})
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("unconfigured updater status: %d", recorder.Code)
	}

	srv.UpdateDir = t.TempDir()
	srv.imageChecker = func(context.Context, string) (bool, string, error) {
		return true, "sha256:test", nil
	}
	recorder = httptest.NewRecorder()
	srv.startUpdate(recorder, request, map[string]any{"target_commit": strings.Repeat("b", 40)})
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("update request status: %d body=%s", recorder.Code, recorder.Body.String())
	}
	raw, err := os.ReadFile(filepath.Join(srv.UpdateDir, "request.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), strings.Repeat("b", 40)) {
		t.Fatalf("target commit was not persisted: %s", raw)
	}
}

func TestUpdateStatusCompletesWhenControllerAlreadyRunsTargetCommit(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	updateDir := t.TempDir()
	target := strings.Repeat("a", 40)
	status := fmt.Sprintf(`{"status":"running","phase":"restarting","message":"正在重启 ECS 控制台","progress":94,"target_commit":"%s","current_commit":"%s","request_id":"request-1","updated_at":%d}`, target, target, time.Now().Unix())
	if err := os.WriteFile(filepath.Join(updateDir, "status.json"), []byte(status), 0600); err != nil {
		t.Fatal(err)
	}

	previousCommit := app.Commit
	app.Commit = target
	defer func() { app.Commit = previousCommit }()

	srv := New(st, t.TempDir(), "", "setup-token", nil)
	srv.UpdateDir = updateDir
	recorder := httptest.NewRecorder()
	srv.updateStatus(recorder)
	if recorder.Code != http.StatusOK {
		t.Fatalf("update status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var response map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response["status"] != "success" || response["phase"] != "completed" || response["progress"] != float64(100) || response["current_commit"] != target {
		t.Fatalf("target commit was not recognized as complete: %#v", response)
	}
}

func postJSON(t *testing.T, client *http.Client, endpoint string, value map[string]any, headers map[string]string) *http.Response {
	t.Helper()
	raw, _ := json.Marshal(value)
	req, _ := http.NewRequest(http.MethodPost, endpoint, strings.NewReader(string(raw)))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}
