package notify

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/smtp"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// httpClient is shared by the three channels that are HTTP. The timeout
// is short on purpose: a notification is worth a few seconds and no more,
// because whatever it is about has already happened.
var httpClient = &http.Client{Timeout: 20 * time.Second}

// Ntfy posts to an ntfy topic — ntfy.sh, or a server of one's own.
type Ntfy struct{}

func (n *Ntfy) Send(ctx context.Context, channel Channel, message Message) error {
	server := strings.TrimRight(strings.TrimSpace(channel.Config["server"]), "/")
	if server == "" {
		server = "https://ntfy.sh"
	}
	topic := strings.TrimSpace(channel.Config["topic"])
	if topic == "" {
		return fmt.Errorf("notify: ntfy needs a topic")
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPost,
		server+"/"+url.PathEscape(topic), strings.NewReader(message.Text()))
	if err != nil {
		return fmt.Errorf("notify: ntfy: %w", err)
	}
	request.Header.Set("Title", message.Line())
	request.Header.Set("Priority", ntfyPriority(message.Severity()))
	request.Header.Set("Tags", ntfyTags(message.Severity()))
	if token := strings.TrimSpace(channel.Secrets["token"]); token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	return doNotify(request, "ntfy")
}

func ntfyPriority(severity Severity) string {
	switch severity {
	case SeverityError:
		return "high"
	case SeverityWarning:
		return "default"
	default:
		return "low"
	}
}

func ntfyTags(severity Severity) string {
	switch severity {
	case SeverityError:
		return "rotating_light"
	case SeverityWarning:
		return "warning"
	default:
		return "floppy_disk"
	}
}

// Telegram sends through a bot to one chat.
type Telegram struct{}

func (t *Telegram) Send(ctx context.Context, channel Channel, message Message) error {
	token := strings.TrimSpace(channel.Secrets["token"])
	chat := strings.TrimSpace(channel.Config["chat_id"])
	if token == "" || chat == "" {
		return fmt.Errorf("notify: telegram needs a bot token and a chat id")
	}

	body, err := json.Marshal(map[string]any{
		"chat_id": chat,
		"text":    message.Text(),
		// No parse mode: an account name or a restic error containing an
		// underscore or an asterisk would otherwise be read as markup and
		// the message rejected or mangled.
		"disable_web_page_preview": true,
	})
	if err != nil {
		return fmt.Errorf("notify: telegram: %w", err)
	}

	endpoint := strings.TrimRight(strings.TrimSpace(channel.Config["api"]), "/")
	if endpoint == "" {
		endpoint = "https://api.telegram.org"
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost,
		endpoint+"/bot"+token+"/sendMessage", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("notify: telegram: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	return doNotify(request, "telegram")
}

// Webhook posts JSON, in the shape Slack and Discord both accept, so one
// channel covers those and anything that takes a POST.
type Webhook struct{}

func (h *Webhook) Send(ctx context.Context, channel Channel, message Message) error {
	endpoint := strings.TrimSpace(channel.Config["url"])
	if endpoint == "" {
		return fmt.Errorf("notify: a webhook needs a url")
	}

	// "text" and "content" are what Slack and Discord read; the rest is
	// for anything that wants the detail rather than the sentence.
	payload := map[string]any{
		"text":     message.Text(),
		"content":  message.Text(),
		"event":    string(message.Event),
		"severity": string(message.Severity()),
		"host":     message.Host,
		"subject":  message.Subject,
		"detail":   message.Body,
	}
	if message.Account != "" {
		payload["account"] = message.Account
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("notify: webhook: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("notify: webhook: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	if secret := strings.TrimSpace(channel.Secrets["token"]); secret != "" {
		request.Header.Set("Authorization", "Bearer "+secret)
	}
	return doNotify(request, "webhook")
}

// doNotify sends a request and turns anything but success into an error an
// operator can act on.
func doNotify(request *http.Request, what string) error {
	response, err := httpClient.Do(request)
	if err != nil {
		return fmt.Errorf("notify: %s: %w", what, err)
	}
	defer response.Body.Close()
	if response.StatusCode >= 300 {
		// The body is where these services say what was wrong with the
		// request, and it is short.
		detail, _ := io.ReadAll(io.LimitReader(response.Body, 512))
		return fmt.Errorf("notify: %s answered %s: %s",
			what, response.Status, strings.TrimSpace(string(detail)))
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	return nil
}

// SMTP sends mail.
type SMTP struct{}

func (s *SMTP) Send(ctx context.Context, channel Channel, message Message) error {
	host := strings.TrimSpace(channel.Config["host"])
	from := strings.TrimSpace(channel.Config["from"])
	to := splitAddresses(channel.Config["to"])
	if host == "" || from == "" || len(to) == 0 {
		return fmt.Errorf("notify: email needs a server, a from address and someone to send to")
	}
	port := strings.TrimSpace(channel.Config["port"])
	if port == "" {
		port = "587"
	}
	address := net.JoinHostPort(host, port)

	body := mailBody(from, to, message)
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(30 * time.Second)
	}

	client, err := dialSMTP(address, host, port, time.Until(deadline))
	if err != nil {
		return err
	}
	defer client.Close()

	// STARTTLS wherever the server offers it. Mail carrying an account
	// name and what went wrong with it is not worth sending in the clear.
	if port != "465" {
		if ok, _ := client.Extension("STARTTLS"); ok {
			if err := client.StartTLS(&tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}); err != nil {
				return fmt.Errorf("notify: email: start tls: %w", err)
			}
		} else if strings.TrimSpace(channel.Secrets["password"]) != "" {
			return fmt.Errorf(
				"notify: %s offers no encryption, and this would send the mailbox password "+
					"across it in the clear", address)
		}
	}

	if user := strings.TrimSpace(channel.Config["username"]); user != "" {
		auth := smtp.PlainAuth("", user, channel.Secrets["password"], host)
		if err := client.Auth(auth); err != nil {
			return fmt.Errorf("notify: email: sign in as %s: %w", user, err)
		}
	}
	if err := client.Mail(from); err != nil {
		return fmt.Errorf("notify: email: from %s: %w", from, err)
	}
	for _, recipient := range to {
		if err := client.Rcpt(recipient); err != nil {
			return fmt.Errorf("notify: email: to %s: %w", recipient, err)
		}
	}
	writer, err := client.Data()
	if err != nil {
		return fmt.Errorf("notify: email: %w", err)
	}
	if _, err := writer.Write([]byte(body)); err != nil {
		return fmt.Errorf("notify: email: %w", err)
	}
	if err := writer.Close(); err != nil {
		return fmt.Errorf("notify: email: %w", err)
	}
	return client.Quit()
}

// dialSMTP connects, wrapping the connection in TLS first on the port
// where that is the protocol rather than an upgrade.
func dialSMTP(address, host, port string, timeout time.Duration) (*smtp.Client, error) {
	dialer := &net.Dialer{Timeout: timeout}
	if port == "465" {
		conn, err := tls.DialWithDialer(dialer, "tcp", address,
			&tls.Config{ServerName: host, MinVersion: tls.VersionTLS12})
		if err != nil {
			return nil, fmt.Errorf("notify: email: connect to %s: %w", address, err)
		}
		client, err := smtp.NewClient(conn, host)
		if err != nil {
			return nil, fmt.Errorf("notify: email: %s: %w", address, err)
		}
		return client, nil
	}
	conn, err := dialer.Dial("tcp", address)
	if err != nil {
		return nil, fmt.Errorf("notify: email: connect to %s: %w", address, err)
	}
	client, err := smtp.NewClient(conn, host)
	if err != nil {
		return nil, fmt.Errorf("notify: email: %s: %w", address, err)
	}
	return client, nil
}

// mailBody builds the message, with the subject encoded so a non-ASCII
// account name does not arrive as mojibake.
func mailBody(from string, to []string, message Message) string {
	var out strings.Builder
	out.WriteString("From: Gniza <" + from + ">\r\n")
	out.WriteString("To: " + strings.Join(to, ", ") + "\r\n")
	out.WriteString("Subject: " + mime.QEncoding.Encode("utf-8", message.Line()) + "\r\n")
	out.WriteString("Date: " + time.Now().Format(time.RFC1123Z) + "\r\n")
	out.WriteString("MIME-Version: 1.0\r\n")
	out.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
	out.WriteString("Content-Transfer-Encoding: 8bit\r\n")
	out.WriteString("Auto-Submitted: auto-generated\r\n\r\n")
	out.WriteString(message.Text())
	out.WriteString("\r\n")
	return out.String()
}

// splitAddresses reads a list of recipients written the way people write
// them: separated by commas, or spaces, or both.
func splitAddresses(raw string) []string {
	fields := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ';' || r == ' ' || r == '\n' || r == '\t'
	})
	var addresses []string
	for _, field := range fields {
		if trimmed := strings.TrimSpace(field); trimmed != "" {
			addresses = append(addresses, trimmed)
		}
	}
	return addresses
}

// portOf is used by the interface to show what a channel will connect to.
func portOf(channel Channel) int {
	port, err := strconv.Atoi(strings.TrimSpace(channel.Config["port"]))
	if err != nil {
		return 587
	}
	return port
}

var _ = portOf
