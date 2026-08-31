package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Kori1c/ecs-controller/internal/store"
)

func TestBrandNameConfigurationIsReturnedToTheClient(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	srv := New(st, t.TempDir(), "", "setup-token", nil)
	if err := srv.saveConfig(map[string]any{
		"traffic_threshold": 95,
		"api_interval":      600,
		"AppBrand": map[string]any{
			"name":     "我的 ECS 面板",
			"logo_url": "",
		},
		"Accounts": []any{},
	}); err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	srv.config(recorder)
	var payload map[string]any
	if err := json.NewDecoder(recorder.Result().Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	brand, ok := payload["AppBrand"].(map[string]any)
	if !ok || brand["name"] != "我的 ECS 面板" {
		t.Fatalf("AppBrand=%#v", payload["AppBrand"])
	}

	tooLong := map[string]any{
		"traffic_threshold": 95,
		"api_interval":      600,
		"AppBrand":          map[string]any{"name": strings.Repeat("a", 41)},
	}
	if err := srv.saveConfig(tooLong); err == nil {
		t.Fatal("overlong brand name was accepted")
	}

	initRecorder := httptest.NewRecorder()
	srv.checkInit(initRecorder)
	if initRecorder.Code != http.StatusOK || !strings.Contains(initRecorder.Body.String(), "我的 ECS 面板") {
		t.Fatalf("check init response=%s", initRecorder.Body.String())
	}
}
