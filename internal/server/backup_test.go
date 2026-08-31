package server

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/Kori1c/ecs-controller/internal/store"
)

func TestEncryptedBackupRoundTrip(t *testing.T) {
	root := t.TempDir()
	snapshot := filepath.Join(root, "data.sqlite")
	if err := os.WriteFile(snapshot, []byte("snapshot-data"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "brand-logo.png"), []byte("logo-data"), 0600); err != nil {
		t.Fatal(err)
	}
	var key [32]byte
	for i := range key {
		key[i] = byte(i)
	}
	archive, err := buildBackupArchive(snapshot, root, key)
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := encryptBackup(archive, "backup-pass")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decryptBackup(encrypted, "wrong-pass"); err == nil {
		t.Fatal("wrong backup password was accepted")
	}
	decoded, err := decryptBackup(encrypted, "backup-pass")
	if err != nil {
		t.Fatal(err)
	}
	restored, err := extractBackup(encrypted, "backup-pass", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if string(decoded) != string(archive) {
		t.Fatal("decrypted archive differs from original")
	}
	if restored.EncryptionKey != key {
		t.Fatalf("restored encryption key=%v, want %v", restored.EncryptionKey, key)
	}
	if string(restored.Logos["brand-logo.png"]) != "logo-data" {
		t.Fatalf("restored logo=%q", restored.Logos["brand-logo.png"])
	}
}

func TestBackupEndpointsUseSessionAndCSRFAndRestoreData(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.SetAdminPassword("admin1234"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSetting("app_name", "备份前名称"); err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(New(st, t.TempDir(), "", "setup-token", nil).Handler())
	defer httpServer.Close()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Jar: jar}
	loginResponse, err := client.Post(httpServer.URL+"/index.php?action=login", "application/json", bytes.NewBufferString(`{"password":"admin1234"}`))
	if err != nil {
		t.Fatal(err)
	}
	loginResponse.Body.Close()
	if loginResponse.StatusCode != http.StatusOK {
		t.Fatalf("login status=%d", loginResponse.StatusCode)
	}
	csrf := loginResponse.Header.Get("X-CSRF-Token")
	if csrf == "" {
		t.Fatal("login did not return csrf token")
	}

	createRequest, err := http.NewRequest(http.MethodPost, httpServer.URL+"/index.php?action=create_backup", bytes.NewBufferString(`{"password":"backup-pass"}`))
	if err != nil {
		t.Fatal(err)
	}
	createRequest.Header.Set("Content-Type", "application/json")
	createRequest.Header.Set("X-CSRF-Token", csrf)
	createResponse, err := client.Do(createRequest)
	if err != nil {
		t.Fatal(err)
	}
	backupData, err := io.ReadAll(createResponse.Body)
	createResponse.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if createResponse.StatusCode != http.StatusOK || !bytes.HasPrefix(backupData, []byte(backupMagic)) {
		t.Fatalf("create backup status=%d content-type=%s", createResponse.StatusCode, createResponse.Header.Get("Content-Type"))
	}

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("password", "backup-pass"); err != nil {
		t.Fatal(err)
	}
	part, err := writer.CreateFormFile("backup", "restore.ecsbkp")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(backupData); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	restoreRequest, err := http.NewRequest(http.MethodPost, httpServer.URL+"/index.php?action=restore_backup", &body)
	if err != nil {
		t.Fatal(err)
	}
	restoreRequest.Header.Set("Content-Type", writer.FormDataContentType())
	restoreRequest.Header.Set("X-CSRF-Token", csrf)
	restoreResponse, err := client.Do(restoreRequest)
	if err != nil {
		t.Fatal(err)
	}
	defer restoreResponse.Body.Close()
	var result map[string]any
	if err := json.NewDecoder(restoreResponse.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if restoreResponse.StatusCode != http.StatusOK || result["success"] != true {
		t.Fatalf("restore status=%d result=%#v", restoreResponse.StatusCode, result)
	}
	if got := st.GetSetting("app_name", ""); got != "备份前名称" {
		t.Fatalf("restored app name=%q", got)
	}
}
