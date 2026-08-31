package notify

import "testing"

func TestConfigFromSettingsAcceptsLegacyBooleanValues(t *testing.T) {
	config := ConfigFromSettings(map[string]string{
		"notify_email_enabled": "false",
		"notify_tg_enabled":    "true",
		"notify_wh_enabled":    "false",
	}, nil)
	if config.EmailEnabled || !config.TelegramEnabled || config.WebhookEnabled {
		t.Fatalf("legacy notification switches were not interpreted: %#v", config)
	}
}
