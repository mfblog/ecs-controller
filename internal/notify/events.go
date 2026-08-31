package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// EventConfig is intentionally independent from store.Settings so the
// notifier can be used by workers and tested with an in-memory configuration.
type EventConfig struct {
	EmailEnabled bool
	Email        string
	SMTPHost     string
	SMTPPort     int
	SMTPUser     string
	SMTPPassword string
	SMTPSecure   string

	TelegramEnabled bool
	TelegramToken   string
	TelegramChatID  string
	TelegramProxy   string
	TelegramURL     string
	TelegramIP      string
	TelegramPort    string
	TelegramUser    string
	TelegramPass    string

	WebhookEnabled bool
	WebhookURL     string
	WebhookMethod  string
	WebhookType    string
	WebhookHeaders string
	WebhookBody    string
}

type Event struct {
	Title     string
	Summary   string
	Text      string
	AccountID string
	Fields    map[string]string
}

type Dispatcher struct {
	Config EventConfig
}

func ConfigFromSettings(settings map[string]string, openSecret func(string) (string, error)) EventConfig {
	open := func(value string) string {
		if openSecret == nil {
			return value
		}
		plain, err := openSecret(value)
		if err != nil {
			return ""
		}
		return plain
	}
	port, _ := strconv.Atoi(settings["notify_port"])
	if port <= 0 {
		port = 465
	}
	return EventConfig{
		EmailEnabled:    settingBool(settings["notify_email_enabled"], true),
		Email:           settings["notify_email"],
		SMTPHost:        settings["notify_host"],
		SMTPPort:        port,
		SMTPUser:        settings["notify_username"],
		SMTPPassword:    open(settings["notify_password"]),
		SMTPSecure:      settings["notify_secure"],
		TelegramEnabled: settingBool(settings["notify_tg_enabled"], false),
		TelegramToken:   open(settings["notify_tg_token"]),
		TelegramChatID:  settings["notify_tg_chat_id"],
		TelegramProxy:   settings["notify_tg_proxy_type"],
		TelegramURL:     settings["notify_tg_proxy_url"],
		TelegramIP:      settings["notify_tg_proxy_ip"],
		TelegramPort:    settings["notify_tg_proxy_port"],
		TelegramUser:    settings["notify_tg_proxy_user"],
		TelegramPass:    open(settings["notify_tg_proxy_pass"]),
		WebhookEnabled:  settingBool(settings["notify_wh_enabled"], false),
		WebhookURL:      settings["notify_wh_url"],
		WebhookMethod:   settings["notify_wh_method"],
		WebhookType:     settings["notify_wh_request_type"],
		WebhookHeaders:  settings["notify_wh_headers"],
		WebhookBody:     settings["notify_wh_body"],
	}
}

func settingBool(value string, defaultValue bool) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return defaultValue
	}
	return value == "1" || strings.EqualFold(value, "true")
}

func (d Dispatcher) Dispatch(ctx context.Context, event Event) error {
	var errs []string
	attempts := 0
	succeeded := 0
	if d.Config.EmailEnabled && d.Config.Email != "" && d.Config.SMTPHost != "" {
		attempts++
		body := event.Text
		if err := Email(ctx, d.Config.SMTPHost, d.Config.SMTPPort, d.Config.SMTPUser, d.Config.SMTPPassword, d.Config.SMTPUser, d.Config.Email, "ECS 控制台通知 - "+event.Title, body, d.Config.SMTPSecure); err != nil {
			errs = append(errs, "邮件通知: "+err.Error())
		} else {
			succeeded++
		}
	}
	if d.Config.TelegramEnabled && d.Config.TelegramToken != "" && d.Config.TelegramChatID != "" {
		attempts++
		client, err := NewTelegramClient(d.Config.TelegramToken, d.Config.TelegramProxy, d.Config.TelegramURL, d.Config.TelegramIP, d.Config.TelegramPort, d.Config.TelegramUser, d.Config.TelegramPass)
		if err == nil {
			err = client.SendMessage(ctx, d.Config.TelegramChatID, event.Text, nil)
		}
		if err != nil {
			errs = append(errs, "Telegram: "+err.Error())
		} else {
			succeeded++
		}
	}
	if d.Config.WebhookEnabled && d.Config.WebhookURL != "" {
		attempts++
		if err := WebhookEvent(ctx, d.Config, event); err != nil {
			errs = append(errs, "Webhook: "+err.Error())
		} else {
			succeeded++
		}
	}
	if len(errs) == 0 {
		return nil
	}
	if succeeded > 0 {
		return fmt.Errorf("部分通知成功（%d/%d）: %s", succeeded, attempts, strings.Join(errs, " | "))
	}
	return fmt.Errorf("通知发送失败: %s", strings.Join(errs, " | "))
}

func WebhookEvent(ctx context.Context, cfg EventConfig, event Event) error {
	method := methodOrDefault(cfg.WebhookMethod)
	typeName := strings.ToUpper(strings.TrimSpace(cfg.WebhookType))
	if typeName == "" {
		typeName = "JSON"
	}
	vars := map[string]string{"#TITLE#": event.Title, "#MSG#": event.Summary, "#ACCOUNT#": event.AccountID, "#TRAFFIC#": event.Fields["traffic"], "#MAX_TRAFFIC#": event.Fields["max_traffic"]}
	for key, value := range event.Fields {
		vars["#"+strings.ToUpper(key)+"#"] = value
	}
	replace := func(template string, escapeJSON bool) string {
		for key, value := range vars {
			if escapeJSON {
				raw, _ := json.Marshal(value)
				value = strings.Trim(string(raw), "\"")
			}
			template = strings.ReplaceAll(template, key, value)
		}
		return template
	}
	headers := map[string]string{}
	if strings.TrimSpace(cfg.WebhookHeaders) != "" {
		if err := json.Unmarshal([]byte(cfg.WebhookHeaders), &headers); err != nil {
			return fmt.Errorf("Webhook headers JSON 无效: %w", err)
		}
	}
	bodyTemplate := cfg.WebhookBody
	endpoint := replace(cfg.WebhookURL, false)
	var body []byte
	if method == http.MethodGet {
		if bodyTemplate != "" {
			endpoint = replace(endpoint, false)
			if parsed, err := url.Parse(endpoint); err == nil {
				values := parsed.Query()
				values.Set("message", replace(bodyTemplate, false))
				parsed.RawQuery = values.Encode()
				endpoint = parsed.String()
			}
		} else {
			parsed, err := url.Parse(endpoint)
			if err != nil {
				return err
			}
			values := parsed.Query()
			values.Set("title", event.Title)
			values.Set("text", event.Text)
			values.Set("time", time.Now().Format(time.RFC3339))
			parsed.RawQuery = values.Encode()
			endpoint = parsed.String()
		}
	} else if bodyTemplate != "" {
		bodyText := replace(bodyTemplate, typeName == "JSON")
		if typeName == "FORM" {
			if parsed := map[string]any{}; json.Unmarshal([]byte(bodyText), &parsed) == nil {
				values := url.Values{}
				for key, value := range parsed {
					values.Set(key, fmt.Sprint(value))
				}
				body = []byte(values.Encode())
			} else {
				body = []byte(bodyText)
			}
		} else {
			body = []byte(bodyText)
		}
	} else if typeName == "FORM" {
		body = []byte(url.Values{"title": {event.Title}, "text": {event.Text}, "time": {strconv.FormatInt(time.Now().Unix(), 10)}}.Encode())
	} else {
		body, _ = json.Marshal(map[string]string{"title": event.Title, "text": event.Text, "time": time.Now().Format(time.RFC3339)})
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	if method != http.MethodGet {
		if typeName == "FORM" {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		} else {
			req.Header.Set("Content-Type", "application/json")
		}
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}
