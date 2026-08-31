package store

import (
	"path/filepath"
	"testing"

	"github.com/Kori1c/ecs-controller/internal/app"
)

func TestSnapshotAndRestoreKeepsDurableDataAndDropsRuntimeState(t *testing.T) {
	source, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	if err := source.SetSetting("app_name", "示例控制台"); err != nil {
		t.Fatal(err)
	}
	if err := source.SaveGroups([]app.AccountGroup{{GroupKey: "group-a", AccessKeyID: "ak", AccessKeySecret: "secret", RegionID: "cn-hongkong", MaxTraffic: 200}}); err != nil {
		t.Fatal(err)
	}
	if err := source.UpsertAccount(app.Account{AccessKeyID: "ak", AccessKeySecret: "secret", RegionID: "cn-hongkong", GroupKey: "group-a", InstanceID: "i-source", InstanceStatus: "Running"}); err != nil {
		t.Fatal(err)
	}
	if err := source.SavePasskeyCredential("credential", `{"id":"credential"}`); err != nil {
		t.Fatal(err)
	}

	snapshot := filepath.Join(t.TempDir(), "snapshot.sqlite")
	if err := source.Snapshot(snapshot); err != nil {
		t.Fatal(err)
	}

	target, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	if err := target.SetSetting("app_name", "旧控制台"); err != nil {
		t.Fatal(err)
	}
	if err := target.CreateSession("runtime-session", "csrf", 0); err != nil {
		t.Fatal(err)
	}
	if err := target.RestoreSnapshot(snapshot, source.EncryptionKey()); err != nil {
		t.Fatal(err)
	}
	if got := target.GetSetting("app_name", ""); got != "示例控制台" {
		t.Fatalf("app name=%q, want restored value", got)
	}
	groups, err := target.LoadGroups()
	if err != nil || len(groups) != 1 || groups[0].AccessKeySecret != "secret" {
		t.Fatalf("groups=%#v err=%v", groups, err)
	}
	accounts, err := target.LoadAccounts(false)
	if err != nil || len(accounts) != 1 || accounts[0].InstanceID != "i-source" {
		t.Fatalf("accounts=%#v err=%v", accounts, err)
	}
	if target.PasskeyCount() != 1 {
		t.Fatalf("passkey count=%d, want 1", target.PasskeyCount())
	}
	if _, ok := target.Session("runtime-session"); ok {
		t.Fatal("runtime session was restored")
	}
}
