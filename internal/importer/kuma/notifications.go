package kuma

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Notification channels.
//
// Kuma stores a channel as one JSON blob in `notification.config`, with a
// `type` key inside it and the provider's fields flat beside it. The names are
// the provider's own, so `slack` has `slackwebhookURL` while `discord` has
// `discordWebhookUrl` — there is no convention to exploit, only a table.
//
// Thirteen of Kuma's ninety-odd providers have an equivalent here, which sounds
// worse than it is: Apprise is one of the thirteen, and Kuma's Apprise config
// carries the URLs that stand for another ninety. What is left over is named in
// the report rather than dropped, on the same principle as the monitor types —
// the user gets a list they can work from, in their own install's order.

// mappedChannel is what one Kuma notification becomes.
type mappedChannel struct {
	Type   string
	Config map[string]any
	Notes  []string
}

// unsupportedProvider is returned for a Kuma provider with no equivalent.
type unsupportedProvider struct{ provider string }

func (e *unsupportedProvider) Error() string {
	return fmt.Sprintf("Uptime Kuma's %q notification provider has no equivalent in this build; "+
		"many providers it covers are reachable through the Apprise channel instead", e.provider)
}

// mapNotification reads one row of Kuma's `notification` table.
func mapNotification(raw string) (mappedChannel, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return mappedChannel{}, fmt.Errorf("the notification has no configuration")
	}

	var cfg map[string]any
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		return mappedChannel{}, fmt.Errorf("the notification's configuration is not valid JSON")
	}

	provider := strings.TrimSpace(text(cfg["type"]))
	switch strings.ToLower(provider) {
	case "smtp":
		return mapSMTP(cfg)
	case "webhook":
		return mapWebhookChannel(cfg)
	case "slack":
		return single("slack", "webhook_url", cfg, "slackwebhookURL", "slackWebhookURL")
	case "discord":
		return single("discord", "webhook_url", cfg, "discordWebhookUrl", "discordWebhookURL")
	case "telegram":
		return mapTelegram(cfg)
	case "matrix":
		return mapMatrix(cfg)
	case "gotify":
		return mapGotify(cfg)
	case "ntfy":
		return mapNtfy(cfg)
	case "teams", "msteams":
		return single("msteams", "webhook_url", cfg, "webhookUrl", "teamsWebhookUrl")
	case "pagerduty", "pagertree":
		return mapPagerDuty(cfg)
	case "opsgenie":
		return mapOpsgenie(cfg)
	case "twilio":
		return mapTwilio(cfg)
	case "apprise":
		return mapApprise(cfg)
	default:
		return mappedChannel{}, &unsupportedProvider{provider: provider}
	}
}

// pick returns the first key present and non-empty. Kuma renamed several of
// these between releases and kept reading the old name, so an importer that
// knows only the current one silently produces an empty channel.
func pick(cfg map[string]any, keys ...string) string {
	for _, key := range keys {
		if value := strings.TrimSpace(text(cfg[key])); value != "" {
			return value
		}
	}
	return ""
}

// single is the shape most providers have: one required field and nothing else
// worth carrying.
func single(channelType, field string, cfg map[string]any, keys ...string) (mappedChannel, error) {
	value := pick(cfg, keys...)
	if value == "" {
		return mappedChannel{}, fmt.Errorf("the notification has no %s", strings.ReplaceAll(field, "_", " "))
	}
	return mappedChannel{Type: channelType, Config: map[string]any{field: value}}, nil
}

func mapSMTP(cfg map[string]any) (mappedChannel, error) {
	to := splitAddresses(pick(cfg, "smtpTo"))
	if len(to) == 0 {
		return mappedChannel{}, fmt.Errorf("the notification has no recipient address")
	}

	out := map[string]any{
		"to": to,
		// Kuma's SMTP notification carries its own relay, always. This build
		// defaults to the instance relay, so the flag has to be turned off
		// explicitly or the per-channel host below would be stored and ignored.
		"use_instance_smtp": false,
	}
	if cc := splitAddresses(pick(cfg, "smtpCC")); len(cc) > 0 {
		out["cc"] = cc
	}
	if host := pick(cfg, "smtpHost"); host != "" {
		out["smtp_host"] = host
	}
	if port := int(number(cfg["smtpPort"])); port > 0 {
		out["smtp_port"] = port
	}
	if user := pick(cfg, "smtpUsername"); user != "" {
		out["smtp_username"] = user
	}
	if password := pick(cfg, "smtpPassword"); password != "" {
		out["smtp_password"] = password
	}
	if from := pick(cfg, "smtpFrom"); from != "" {
		// Kuma allows "Name <addr@example.com>" here; ours wants the two apart.
		name, address := splitMailbox(from)
		out["from_address"] = address
		if name != "" {
			out["from_name"] = name
		}
	}

	// Kuma has two booleans where this build has one enum, and the pair is not
	// orthogonal: secure means implicit TLS on 465, and ignoreTLSError is about
	// verification rather than about which TLS. Only the first maps.
	switch {
	case truthy(cfg["smtpSecure"]):
		out["smtp_encryption"] = "tls"
	case int(number(cfg["smtpPort"])) == 465:
		out["smtp_encryption"] = "tls"
	default:
		out["smtp_encryption"] = "starttls"
	}

	out2 := mappedChannel{Type: "email", Config: out}
	if truthy(cfg["smtpIgnoreTLSError"]) {
		out2.Notes = append(out2.Notes, "Uptime Kuma was configured to ignore this relay's TLS "+
			"certificate errors; this build has no such option, so the relay's certificate has to be valid")
	}
	return out2, nil
}

func mapWebhookChannel(cfg map[string]any) (mappedChannel, error) {
	url := pick(cfg, "webhookURL", "webhookUrl")
	if url == "" {
		return mappedChannel{}, fmt.Errorf("the notification has no webhook URL")
	}

	out := mappedChannel{Type: "webhook", Config: map[string]any{"url": url, "method": "POST"}}

	switch strings.ToLower(pick(cfg, "webhookContentType")) {
	case "form-data":
		out.Notes = append(out.Notes, "Uptime Kuma sent this webhook as multipart/form-data; "+
			"this build sends JSON, so a receiver parsing form fields will need adjusting")
	case "custombody":
		if body := text(cfg["webhookCustomBody"]); body != "" {
			out.Notes = append(out.Notes, "the custom body template was not imported: Uptime Kuma's "+
				"templates are Liquid and this build's are its own, so the payload was left as the "+
				"default event envelope rather than being half-translated")
		}
	}
	if headers := parseHeaders(text(cfg["webhookAdditionalHeaders"])); len(headers) > 0 {
		out.Config["headers"] = headers
	}
	return out, nil
}

func mapTelegram(cfg map[string]any) (mappedChannel, error) {
	token := pick(cfg, "telegramBotToken")
	chat := pick(cfg, "telegramChatID", "telegramChatId")
	if token == "" || chat == "" {
		return mappedChannel{}, fmt.Errorf("the notification has no bot token or chat id")
	}
	out := map[string]any{"bot_token": token, "chat_id": chat}
	if thread := pick(cfg, "telegramMessageThreadID", "telegramMessageThreadId"); thread != "" {
		out["message_thread_id"] = thread
	}
	if truthy(cfg["telegramSendSilently"]) {
		out["disable_notification"] = true
	}
	return mappedChannel{Type: "telegram", Config: out}, nil
}

func mapMatrix(cfg map[string]any) (mappedChannel, error) {
	homeserver := pick(cfg, "homeserverUrl", "homeserverURL")
	room := pick(cfg, "internalRoomId", "internalRoomID", "roomId")
	token := pick(cfg, "accessToken")
	if homeserver == "" || room == "" || token == "" {
		return mappedChannel{}, fmt.Errorf("the notification is missing its homeserver, room, or access token")
	}
	return mappedChannel{Type: "matrix", Config: map[string]any{
		"homeserver_url": homeserver, "room_id": room, "access_token": token,
	}}, nil
}

func mapGotify(cfg map[string]any) (mappedChannel, error) {
	server := pick(cfg, "gotifyserverurl", "gotifyServerUrl", "gotifyServerURL")
	token := pick(cfg, "gotifyapplicationToken", "gotifyApplicationToken")
	if server == "" || token == "" {
		return mappedChannel{}, fmt.Errorf("the notification is missing its server URL or application token")
	}
	out := map[string]any{"server_url": server, "application_token": token}
	if priority := int(number(cfg["gotifyPriority"])); priority > 0 && priority <= 10 {
		out["priority"] = priority
	}
	return mappedChannel{Type: "gotify", Config: out}, nil
}

func mapNtfy(cfg map[string]any) (mappedChannel, error) {
	topic := pick(cfg, "ntfytopic", "ntfyTopic")
	if topic == "" {
		return mappedChannel{}, fmt.Errorf("the notification has no topic")
	}
	out := map[string]any{"topic": topic}
	if server := pick(cfg, "ntfyserverurl", "ntfyServerUrl"); server != "" {
		out["server_url"] = server
	}
	if priority := int(number(cfg["ntfyPriority"])); priority >= 1 && priority <= 5 {
		out["priority"] = priority
	}
	switch strings.ToLower(pick(cfg, "ntfyAuthenticationMethod")) {
	case "usernamePassword", "usernamepassword":
		out["auth_type"] = "basic"
		out["username"] = pick(cfg, "ntfyusername", "ntfyUsername")
		out["password"] = pick(cfg, "ntfypassword", "ntfyPassword")
	case "accesstoken":
		out["auth_type"] = "token"
		out["token"] = pick(cfg, "ntfyaccesstoken", "ntfyAccessToken")
	}
	return mappedChannel{Type: "ntfy", Config: out}, nil
}

func mapPagerDuty(cfg map[string]any) (mappedChannel, error) {
	key := pick(cfg, "pagerdutyIntegrationKey")
	if key == "" {
		return mappedChannel{}, fmt.Errorf("the notification has no integration key")
	}
	out := map[string]any{"integration_key": key}
	switch severity := strings.ToLower(pick(cfg, "pagerdutyPriority")); severity {
	case "critical", "error", "warning", "info":
		out["severity"] = severity
	}
	if _, set := cfg["pagerdutyAutoResolve"]; set {
		out["auto_resolve"] = truthy(cfg["pagerdutyAutoResolve"])
	}
	return mappedChannel{Type: "pagerduty", Config: out}, nil
}

func mapOpsgenie(cfg map[string]any) (mappedChannel, error) {
	key := pick(cfg, "opsgenieApiKey", "opsgenieAPIKey")
	if key == "" {
		return mappedChannel{}, fmt.Errorf("the notification has no API key")
	}
	out := map[string]any{"api_key": key}
	if region := strings.ToLower(pick(cfg, "opsgenieRegion")); region == "us" || region == "eu" {
		out["region"] = region
	}
	if priority := strings.ToUpper(pick(cfg, "opsgeniePriority")); strings.HasPrefix(priority, "P") {
		out["priority"] = priority
	}
	return mappedChannel{Type: "opsgenie", Config: out}, nil
}

func mapTwilio(cfg map[string]any) (mappedChannel, error) {
	sid := pick(cfg, "twilioAccountSID", "twilioAccountSid")
	token := pick(cfg, "twilioAuthToken", "twilioApiKey")
	from := pick(cfg, "twilioFromNumber")
	to := splitAddresses(pick(cfg, "twilioToNumber"))
	if sid == "" || token == "" || from == "" || len(to) == 0 {
		return mappedChannel{}, fmt.Errorf("the notification is missing its account SID, token, from number, or recipient")
	}
	return mappedChannel{Type: "twilio", Config: map[string]any{
		"account_sid": sid, "auth_token": token, "from_number": from, "to_numbers": to,
	}}, nil
}

func mapApprise(cfg map[string]any) (mappedChannel, error) {
	urls := splitAddresses(pick(cfg, "appriseURL", "appriseUrl"))
	if len(urls) == 0 {
		return mappedChannel{}, fmt.Errorf("the notification has no Apprise URL")
	}
	out := mappedChannel{Type: "apprise", Config: map[string]any{"urls": urls}}
	out.Notes = append(out.Notes, "Apprise has to be installed on this host for the channel to deliver; "+
		"the channel reports itself unavailable rather than failing silently if it is not")
	return out, nil
}

// splitAddresses reads the comma-or-space separated lists Kuma stores for
// recipients and Apprise URLs.
func splitAddresses(raw string) []string {
	fields := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ';' || r == '\n' || r == ' ' || r == '\t'
	})
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// splitMailbox pulls "Name <addr>" apart. Anything that is not that shape is
// treated as a bare address, which is what it almost always is.
func splitMailbox(raw string) (name, address string) {
	open := strings.LastIndex(raw, "<")
	closing := strings.LastIndex(raw, ">")
	if open < 0 || closing < open {
		return "", strings.TrimSpace(raw)
	}
	return strings.Trim(strings.TrimSpace(raw[:open]), `"`), strings.TrimSpace(raw[open+1 : closing])
}
