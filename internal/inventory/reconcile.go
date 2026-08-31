package inventory

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/Kori1c/ecs-controller/internal/app"
	"github.com/Kori1c/ecs-controller/internal/cloud"
	"github.com/Kori1c/ecs-controller/internal/store"
)

type Result struct {
	RemoteCount int
	Purged      int
	Recovered   int
}

func SyncGroup(ctx context.Context, st *store.Store, group app.AccountGroup, client cloud.Client, now time.Time, logf func(string, ...any)) (Result, error) {
	result := Result{}
	instances, err := client.DescribeInstances(ctx, group.RegionID)
	if err != nil {
		return result, err
	}
	accounts, err := st.LoadAccounts(true)
	if err != nil {
		return result, err
	}

	publicNetworks := map[string]cloud.InstancePublicNetwork{}
	publicNetworkSynced := false
	if networkClient, ok := client.(cloud.InstancePublicNetworkClient); ok {
		instanceIDs := make([]string, 0, len(instances))
		for _, instance := range instances {
			instanceIDs = append(instanceIDs, instance.ID)
		}
		if networks, networkErr := networkClient.DescribeInstancePublicNetworks(ctx, group.RegionID, instanceIDs); networkErr != nil {
			if logf != nil {
				logf("同步实例公网带宽失败（账号组 %s）: %v", group.GroupKey, networkErr)
			}
		} else {
			publicNetworks = networks
			publicNetworkSynced = true
		}
	}

	remoteIDs := make(map[string]bool, len(instances))
	for _, instance := range instances {
		if instance.ID == "" {
			continue
		}
		remoteIDs[instance.ID] = true
		result.RemoteCount++
		var existing *app.Account
		for i := range accounts {
			if !sameGroup(accounts[i], group) || accounts[i].InstanceID != instance.ID {
				continue
			}
			if existing == nil || accounts[i].IsDeleted == 0 {
				existing = &accounts[i]
			}
			if accounts[i].IsDeleted == 0 {
				break
			}
		}
		if existing != nil {
			if existing.InstanceStatus == "Releasing" {
				continue
			}
			if existing.IsDeleted != 0 {
				// A legacy tombstone must not bring old traffic or billing data
				// back when the same cloud instance ID is seen again.
				if err := st.DeleteInstanceData(existing.ID); err != nil {
					return result, err
				}
				existing = nil
			}
		}

		a := app.Account{
			AccessKeyID: group.AccessKeyID, AccessKeySecret: group.AccessKeySecret,
			RegionID: group.RegionID, InstanceID: instance.ID, MaxTraffic: group.MaxTraffic,
			ScheduleEnabled: group.ScheduleEnabled, ScheduleStartEnabled: group.ScheduleStartEnabled,
			ScheduleStopEnabled: group.ScheduleStopEnabled, StartTime: group.StartTime, StopTime: group.StopTime,
			Remark: group.Remark, SiteType: group.SiteType, GroupKey: group.GroupKey,
			InstanceName: instance.Name, InstanceType: instance.InstanceType,
			InternetBandwidth: instance.InternetBandwidth, PublicIP: instance.PublicIP,
			PublicIPMode: "ecs_public_ip", PrivateIP: instance.PrivateIP,
			CPU: instance.CPU, Memory: instance.Memory, OSName: instance.OSName,
			InstanceStatus: instance.Status, HealthStatus: "ok", UpdatedAt: now.Unix(),
			LastSeenAt: now.Unix(), CloudPresence: "present",
		}
		if network, hasEIP := publicNetworks[instance.ID]; hasEIP {
			a.PublicIPMode = "eip"
			a.EIPAllocationID, a.EIPAddress = network.AllocationID, network.Address
			if network.Address != "" {
				a.PublicIP = network.Address
			}
			if network.Bandwidth > 0 {
				a.InternetBandwidth = network.Bandwidth
			}
		}
		if existing != nil {
			a.ID = existing.ID
			a.TrafficUsed, a.TrafficBillingMonth = existing.TrafficUsed, existing.TrafficBillingMonth
			a.LastKeepAliveAt, a.AutoStartBlocked = existing.LastKeepAliveAt, existing.AutoStartBlocked
			a.ScheduleLastStartDate, a.ScheduleLastStopDate = existing.ScheduleLastStartDate, existing.ScheduleLastStopDate
			a.ScheduleStopActive, a.ScheduleBlockedByTraffic = existing.ScheduleStopActive, existing.ScheduleBlockedByTraffic
			a.TrafficAPIStatus, a.TrafficAPIMessage = existing.TrafficAPIStatus, existing.TrafficAPIMessage
			a.ProtectionSuspended, a.ProtectionSuspendReason, a.ProtectionNotifiedAt = existing.ProtectionSuspended, existing.ProtectionSuspendReason, existing.ProtectionNotifiedAt
			if existing.CloudPresence == "missing" || (existing.IsDeleted != 0 && existing.CloudPresence == "retired") {
				result.Recovered++
				a.TrafficAPIStatus = "unknown"
				a.TrafficAPIMessage = "实例已恢复，等待流量数据更新"
				if existing.ProtectionSuspendReason == "instance_missing" {
					a.ProtectionSuspended, a.ProtectionSuspendReason = false, ""
				}
			}
			if network, hasEIP := publicNetworks[instance.ID]; hasEIP {
				a.EIPManaged = existing.EIPManaged && existing.EIPAllocationID == network.AllocationID
			} else if !publicNetworkSynced {
				a.EIPAllocationID, a.EIPAddress, a.EIPManaged = existing.EIPAllocationID, existing.EIPAddress, existing.EIPManaged
				a.PublicIPMode = existing.PublicIPMode
				if a.InternetBandwidth < 1 {
					a.InternetBandwidth = existing.InternetBandwidth
				}
			}
			if a.PublicIPMode == "eip" && a.EIPAddress != "" {
				a.PublicIP = a.EIPAddress
			}
		}
		if err := st.UpsertAccount(a); err != nil {
			return result, err
		}
	}

	purgedKeys := make(map[string]struct{})
	for _, account := range accounts {
		if account.InstanceID == "" || remoteIDs[account.InstanceID] || !sameGroup(account, group) {
			continue
		}
		if account.InstanceStatus == "Releasing" {
			continue
		}
		key := instanceKey(account)
		if _, alreadyPurged := purgedKeys[key]; alreadyPurged {
			continue
		}
		// A local release job still owns its row until EIP and DDNS cleanup
		// completes. Let that job finish instead of orphaning its retry state.
		hasReleasingDuplicate := false
		for _, candidate := range accounts {
			if sameInstance(candidate, account) && candidate.InstanceStatus == "Releasing" {
				hasReleasingDuplicate = true
				break
			}
		}
		if hasReleasingDuplicate {
			continue
		}
		confirmedMissing, confirmErr := confirmMissing(ctx, client, group.RegionID, account.InstanceID)
		if confirmErr != nil {
			if logf != nil {
				logf("二次确认云端实例失败（账号组 %s，实例 %s）: %v", group.GroupKey, account.InstanceID, confirmErr)
			}
			continue
		}
		if !confirmedMissing {
			continue
		}
		purgedKeys[key] = struct{}{}
		for _, candidate := range accounts {
			if !sameInstance(candidate, account) {
				continue
			}
			if err := st.DeleteInstanceData(candidate.ID); err != nil {
				return result, err
			}
		}
		result.Purged++
		st.AddLog("info", "已彻底清理云端不存在的实例记录")
		if st.GetSetting("ddns_enabled", "0") == "1" {
			jobID := fmt.Sprintf("inventory-ddns-%s-%d", account.InstanceID, now.UnixNano())
			payload := map[string]any{"account": ddnsPayloadAccount(account), "before": ddnsPayloadAccounts(accounts)}
			if enqueueErr := st.EnqueueJob(jobID, "delete_ddns", strconv.FormatInt(account.ID, 10), payload); enqueueErr != nil {
				st.AddLog("warning", "云端实例已清理，但 DDNS 清理任务入队失败: "+enqueueErr.Error())
			}
		}
	}

	if (result.Purged > 0 || result.Recovered > 0) && st.GetSetting("ddns_enabled", "0") == "1" {
		_ = st.SetSetting("last_ddns_reconcile", "0")
	}
	return result, nil
}

func sameInstance(left, right app.Account) bool {
	if left.RegionID != right.RegionID || left.InstanceID != right.InstanceID {
		return false
	}
	return (left.GroupKey != "" && right.GroupKey != "" && left.GroupKey == right.GroupKey) ||
		(left.AccessKeyID != "" && left.AccessKeyID == right.AccessKeyID)
}

func instanceKey(account app.Account) string {
	identity := account.AccessKeyID
	if identity == "" {
		identity = account.GroupKey
	}
	return identity + "\x00" + account.RegionID + "\x00" + account.InstanceID
}

func confirmMissing(ctx context.Context, client cloud.Client, region, instanceID string) (bool, error) {
	instance, err := client.DescribeInstance(ctx, region, instanceID)
	if err != nil {
		if cloud.IsNotFound(err) {
			return true, nil
		}
		return false, err
	}
	if instance == nil {
		return false, fmt.Errorf("DescribeInstance returned an empty response")
	}
	if instance.ID != instanceID {
		return false, fmt.Errorf("DescribeInstance returned %q while checking %q", instance.ID, instanceID)
	}
	// A successful response is authoritative even if the instance was absent
	// from the full list due to a provider-side list inconsistency.
	return false, nil
}

func sameGroup(account app.Account, group app.AccountGroup) bool {
	return (account.GroupKey != "" && group.GroupKey != "" && account.GroupKey == group.GroupKey) ||
		(account.AccessKeyID != "" && account.AccessKeyID == group.AccessKeyID && account.RegionID == group.RegionID)
}

func ddnsPayloadAccount(account app.Account) map[string]any {
	return map[string]any{
		"GroupKey": account.GroupKey, "AccessKeyID": account.AccessKeyID,
		"RegionID": account.RegionID, "InstanceID": account.InstanceID,
		"Remark": account.Remark, "InstanceName": account.InstanceName,
	}
}

func ddnsPayloadAccounts(accounts []app.Account) []map[string]any {
	result := make([]map[string]any, 0, len(accounts))
	for _, account := range accounts {
		if account.IsDeleted == 0 {
			result = append(result, ddnsPayloadAccount(account))
		}
	}
	return result
}
