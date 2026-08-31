package worker

import (
	"context"
	"fmt"
	"time"

	"github.com/Kori1c/ecs-controller/internal/app"
	"github.com/Kori1c/ecs-controller/internal/notify"
)

func (w *Worker) dispatchEvent(ctx context.Context, event notify.Event) error {
	settings := w.Store.Settings()
	cfg := notify.ConfigFromSettings(settings, w.Store.OpenSecret)
	notifyCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- (notify.Dispatcher{Config: cfg}).Dispatch(notifyCtx, event) }()
	var err error
	select {
	case err = <-done:
	case <-notifyCtx.Done():
		err = notifyCtx.Err()
	}
	if err != nil {
		w.Store.AddLog("warning", "通知发送失败: "+err.Error())
		return err
	}
	return nil
}

func accountLabel(account app.Account) string {
	if account.Remark != "" {
		return account.Remark
	}
	if account.InstanceName != "" {
		return account.InstanceName
	}
	if account.InstanceID != "" {
		return account.InstanceID
	}
	return account.AccessKeyID
}

func statusEvent(account app.Account, from, to, reason string) notify.Event {
	return notify.Event{Title: "实例状态变化", Summary: fmt.Sprintf("%s 状态从 %s 变为 %s", accountLabel(account), from, to), AccountID: accountLabel(account), Text: fmt.Sprintf("【ECS 控制台】实例状态变化\n账号/实例: %s\n实例 ID: %s\n区域: %s\n原状态: %s\n新状态: %s\n时间: %s\n说明: %s", accountLabel(account), account.InstanceID, account.RegionID, from, to, time.Now().Format("2006-01-02 15:04:05"), reason), Fields: map[string]string{"instance_id": account.InstanceID, "region": account.RegionID, "from_status": from, "to_status": to}}
}

func trafficEvent(account app.Account, used, percent, threshold float64, action string) notify.Event {
	return notify.Event{Title: "流量保护告警", Summary: fmt.Sprintf("%s 当前使用率 %.2f%%", accountLabel(account), percent), AccountID: accountLabel(account), Text: fmt.Sprintf("【ECS 控制台】流量保护告警\n账号/实例: %s\n当前流量: %.2f GB\n使用率: %.2f%%\n阈值: %.2f%%\n动作: %s\n时间: %s", accountLabel(account), used, percent, threshold, action, time.Now().Format("2006-01-02 15:04:05")), Fields: map[string]string{"traffic": fmt.Sprintf("%.2f GB", used), "max_traffic": fmt.Sprintf("%.2f%%", threshold), "percentage": fmt.Sprintf("%.2f%%", percent), "action": action, "instance_id": account.InstanceID}}
}
