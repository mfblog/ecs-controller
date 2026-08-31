package worker

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"

	"github.com/Kori1c/ecs-controller/internal/app"
	"github.com/Kori1c/ecs-controller/internal/notify"
)

var ddnsSlugPattern = regexp.MustCompile(`[^a-z0-9]+`)

func (w *Worker) syncDDNSAccount(ctx context.Context, account app.Account) {
	if !w.ddnsEnabled() || account.InstanceID == "" || account.CloudPresence == "missing" {
		return
	}
	address := account.PublicIP
	if account.PublicIPMode == "eip" && account.EIPAddress != "" {
		address = account.EIPAddress
	}
	if address == "" {
		return
	}
	accounts, _ := w.Store.LoadAccounts(false)
	name := w.ddnsRecordName(account, accounts)
	token, _ := w.Store.OpenSecret(w.Store.GetSetting("ddns_cf_token", ""))
	err := notify.CloudflareUpdateRecord(ctx, token, w.Store.GetSetting("ddns_cf_zone_id", ""), w.Store.GetSetting("ddns_domain", ""), name, address, w.Store.GetSetting("ddns_cf_proxied", "0") == "1")
	if err != nil {
		w.Store.AddLog("warning", "Cloudflare DDNS 同步失败: "+err.Error())
		return
	}
	w.Store.AddLog("info", fmt.Sprintf("DDNS 已同步 [%s] %s -> %s", accountLabel(account), name, address))
}

func (w *Worker) deleteDDNSAccount(ctx context.Context, account app.Account, before []app.Account) error {
	if !w.ddnsEnabled() || account.InstanceID == "" {
		return nil
	}
	name := w.ddnsRecordName(account, before)
	token, _ := w.Store.OpenSecret(w.Store.GetSetting("ddns_cf_token", ""))
	if err := notify.CloudflareDeleteRecord(ctx, token, w.Store.GetSetting("ddns_cf_zone_id", ""), w.Store.GetSetting("ddns_domain", ""), name); err != nil {
		w.Store.AddLog("warning", "Cloudflare DDNS 清理失败: "+err.Error())
		return err
	}
	w.Store.AddLog("info", "DDNS 已删除: "+name)
	return nil
}

func ddnsPayloadAccount(account app.Account) map[string]any {
	return map[string]any{"GroupKey": account.GroupKey, "AccessKeyID": account.AccessKeyID, "RegionID": account.RegionID, "InstanceID": account.InstanceID, "Remark": account.Remark, "InstanceName": account.InstanceName}
}

func ddnsPayloadAccounts(accounts []app.Account) []map[string]any {
	result := make([]map[string]any, 0, len(accounts))
	for _, account := range accounts {
		result = append(result, ddnsPayloadAccount(account))
	}
	return result
}

func (w *Worker) syncAllDDNS(ctx context.Context) {
	if !w.ddnsEnabled() {
		return
	}
	accounts, _ := w.Store.LoadAccounts(false)
	for _, account := range accounts {
		w.syncDDNSAccount(ctx, account)
	}
}

func (w *Worker) ddnsEnabled() bool {
	return w.Store.GetSetting("ddns_enabled", "0") == "1" && w.Store.GetSetting("ddns_provider", "cloudflare") == "cloudflare" && w.Store.GetSetting("ddns_domain", "") != ""
}

func (w *Worker) ddnsRecordName(account app.Account, accounts []app.Account) string {
	domain := normalizeDDNSDomain(w.Store.GetSetting("ddns_domain", ""))
	groupKey := account.GroupKey
	if groupKey == "" {
		groupKey = account.AccessKeyID + "|" + account.RegionID
	}
	count := 0
	for _, item := range accounts {
		key := item.GroupKey
		if key == "" {
			key = item.AccessKeyID + "|" + item.RegionID
		}
		if key == groupKey && item.InstanceID != "" && item.CloudPresence != "missing" {
			count++
		}
	}
	base := ""
	if groups, err := w.Store.LoadGroups(); err == nil {
		for _, group := range groups {
			if group.GroupKey == groupKey {
				base = group.Remark
				break
			}
		}
	}
	if base == "" {
		base = account.Remark
	}
	if base == "" {
		base = account.InstanceName
	}
	if base == "" {
		base = strings.TrimPrefix(account.InstanceID, "i-")
	}
	base = slugDDNS(base)
	if base == "" {
		base = shortHash(account.InstanceID)
	}
	if count > 1 {
		suffix := slugDDNS(account.InstanceName)
		if suffix == "" {
			suffix = slugDDNS(strings.TrimPrefix(account.InstanceID, "i-"))
		}
		if suffix != "" && suffix != base {
			base += "-" + suffix
		}
	}
	if domain == "" {
		return base
	}
	return base + "." + domain
}

func normalizeDDNSDomain(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = strings.TrimPrefix(strings.TrimPrefix(value, "https://"), "http://")
	if index := strings.IndexByte(value, '/'); index >= 0 {
		value = value[:index]
	}
	return strings.Trim(value, ".")
}

func slugDDNS(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = ddnsSlugPattern.ReplaceAllString(value, "-")
	return strings.Trim(strings.TrimSpace(value), "-")
}

func shortHash(value string) string {
	hash := sha1.Sum([]byte(value))
	return hex.EncodeToString(hash[:])[:8]
}
