package inventory

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Kori1c/ecs-controller/internal/app"
	"github.com/Kori1c/ecs-controller/internal/cloud"
	"github.com/Kori1c/ecs-controller/internal/store"
)

type fakeClient struct {
	cloud.Client
	instances []cloud.Instance
	err       error
}

func (f *fakeClient) DescribeInstances(context.Context, string) ([]cloud.Instance, error) {
	return f.instances, f.err
}

func (f *fakeClient) DescribeInstance(_ context.Context, _ string, id string) (*cloud.Instance, error) {
	for _, instance := range f.instances {
		if instance.ID == id {
			copy := instance
			return &copy, nil
		}
	}
	if f.err != nil {
		return nil, f.err
	}
	return nil, &cloud.APIError{Code: "InvalidInstanceId.NotFound", HTTPStatus: 404}
}

func newInventoryStore(t *testing.T) (*store.Store, app.AccountGroup) {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	group := app.AccountGroup{GroupKey: "group-1", AccessKeyID: "ak", AccessKeySecret: "sk", RegionID: "cn-test", MaxTraffic: 200}
	if err := st.SaveGroups([]app.AccountGroup{group}); err != nil {
		st.Close()
		t.Fatal(err)
	}
	return st, group
}

func TestMissingInstanceIsPurgedAfterCompleteInventory(t *testing.T) {
	st, group := newInventoryStore(t)
	defer st.Close()
	if err := st.UpsertAccount(app.Account{AccessKeyID: "ak", AccessKeySecret: "sk", RegionID: "cn-test", GroupKey: group.GroupKey, InstanceID: "i-old", InstanceStatus: "Running"}); err != nil {
		t.Fatal(err)
	}
	accounts, err := st.LoadAccounts(false)
	if err != nil || len(accounts) != 1 {
		t.Fatalf("load account: %#v err=%v", accounts, err)
	}
	if err := st.AddTrafficHistory(accounts[0].ID, 12, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := st.SetBillingCache(accounts[0].ID, "bill_overview", "2026-08", map[string]any{"monthly_cost": 1}); err != nil {
		t.Fatal(err)
	}

	start := time.Date(2026, 8, 31, 12, 0, 0, 0, time.Local)
	result, err := SyncGroup(context.Background(), st, group, &fakeClient{}, start, nil)
	if err != nil || result.Purged != 1 {
		t.Fatalf("missing instance was not purged: result=%+v err=%v", result, err)
	}
	if _, err := st.Account(1, true); err == nil {
		t.Fatal("purged account remained in the database")
	}
	if all, err := st.LoadAccounts(true); err != nil || len(all) != 0 {
		t.Fatalf("purged account remained in database: %#v err=%v", all, err)
	}
	if cached, ok := st.GetBillingCache(1, "bill_overview", "2026-08", time.Hour); ok || cached != nil {
		t.Fatalf("billing history remained after purge: %#v %v", cached, ok)
	}
	groupAfter, err := st.LoadGroups()
	if err != nil || len(groupAfter) != 1 || groupAfter[0].AccessKeySecret != "sk" {
		t.Fatalf("account credentials were removed with instance: %#v err=%v", groupAfter, err)
	}
	job, err := st.ClaimJob(time.Minute)
	if err != nil || job != nil {
		t.Fatalf("inventory purge queued an ECS release: job=%#v err=%v", job, err)
	}
}

func TestFailedInventoryDoesNotMarkInstancesMissing(t *testing.T) {
	st, group := newInventoryStore(t)
	defer st.Close()
	if err := st.UpsertAccount(app.Account{AccessKeyID: "ak", AccessKeySecret: "sk", RegionID: "cn-test", GroupKey: group.GroupKey, InstanceID: "i-1", InstanceStatus: "Running"}); err != nil {
		t.Fatal(err)
	}
	if _, err := SyncGroup(context.Background(), st, group, &fakeClient{err: errors.New("temporary provider failure")}, time.Now(), nil); err == nil {
		t.Fatal("provider failure was ignored")
	}
	account, err := st.Account(1, false)
	if err != nil || account.CloudPresence != "present" || account.MissingCount != 0 || account.InstanceStatus != "Running" {
		t.Fatalf("failed inventory changed local presence: account=%#v err=%v", account, err)
	}
}

func TestInventoryPurgingQueuesOnlySanitizedDDNSCleanup(t *testing.T) {
	st, group := newInventoryStore(t)
	defer st.Close()
	if err := st.SetSetting("ddns_enabled", "1"); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertAccount(app.Account{AccessKeyID: "ak", AccessKeySecret: "secret-value", RegionID: "cn-test", GroupKey: group.GroupKey, InstanceID: "i-old", InstanceStatus: "Running"}); err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 8, 31, 12, 0, 0, 0, time.Local)
	if _, err := SyncGroup(context.Background(), st, group, &fakeClient{}, start, nil); err != nil {
		t.Fatal(err)
	}
	job, err := st.ClaimJob(time.Minute)
	if err != nil || job == nil || job.Kind != "delete_ddns" {
		t.Fatalf("DDNS cleanup job=%#v err=%v", job, err)
	}
	if strings.Contains(job.Payload, "secret-value") || strings.Contains(job.Payload, "AccessKeySecret") {
		t.Fatalf("DDNS cleanup payload contains credentials: %s", job.Payload)
	}
}

func TestPurgedInstanceCanBeDiscoveredAsANewRecord(t *testing.T) {
	st, group := newInventoryStore(t)
	defer st.Close()
	if err := st.UpsertAccount(app.Account{AccessKeyID: "ak", AccessKeySecret: "sk", RegionID: "cn-test", GroupKey: group.GroupKey, InstanceID: "i-1", InstanceStatus: "Running"}); err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 8, 31, 12, 0, 0, 0, time.Local)
	if _, err := SyncGroup(context.Background(), st, group, &fakeClient{}, start, nil); err != nil {
		t.Fatal(err)
	}
	result, err := SyncGroup(context.Background(), st, group, &fakeClient{instances: []cloud.Instance{{ID: "i-1", Status: "Stopped"}}}, start.Add(time.Minute), nil)
	if err != nil || result.Recovered != 0 {
		t.Fatalf("new instance was incorrectly treated as recovery: result=%+v err=%v", result, err)
	}
	accounts, err := st.LoadAccounts(false)
	if err != nil || len(accounts) != 1 || accounts[0].InstanceID != "i-1" || accounts[0].InstanceStatus != "Stopped" {
		t.Fatalf("rediscovered account state: accounts=%#v err=%v", accounts, err)
	}
}
