// Package notify sends scriven's messages to Slack, Teams and email.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/smtp"
	"strconv"
	"strings"
	"time"

	"scriven/internal/config"
	"scriven/internal/secrets"
)

type Message struct {
	Title  string
	Text   string            // plain text, may contain newlines
	Facts  map[string]string // short key/value pairs: environment, resource, actor...
	Link   string
	Detail string // longer block such as a diff; shown monospaced
}

type Notifier struct {
	cfg    config.Notify
	res    *secrets.Resolver
	client *http.Client
}

func New(cfg config.Notify, res *secrets.Resolver) *Notifier {
	return &Notifier{cfg: cfg, res: res, client: &http.Client{Timeout: 15 * time.Second}}
}

// Send delivers to every configured channel and returns one error per failed channel.
func (n *Notifier) Send(ctx context.Context, m Message) error {
	var errs []error
	for _, ch := range n.cfg.Channels {
		if err := n.SendTo(ctx, ch, m); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", ch, err))
		}
	}
	return errors.Join(errs...)
}

func (n *Notifier) SendTo(ctx context.Context, channel string, m Message) error {
	switch channel {
	case "slack":
		return n.post(ctx, n.cfg.SlackWebhook, slackPayload(m))
	case "teams":
		return n.post(ctx, n.cfg.TeamsWebhook, teamsPayload(m))
	case "email":
		return n.email(m)
	}
	return fmt.Errorf("unknown channel %q", channel)
}

func (n *Notifier) post(ctx context.Context, ref string, payload any) error {
	u, err := n.res.Resolve(ref)
	if err != nil {
		return err
	}
	b, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, "POST", u, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := n.client.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("webhook returned %s", resp.Status)
	}
	return nil
}

func factLines(m Message) []string {
	var out []string
	for _, k := range sortedKeys(m.Facts) {
		out = append(out, k+": "+m.Facts[k])
	}
	return out
}

func slackPayload(m Message) map[string]any {
	text := "*" + m.Title + "*\n" + m.Text
	if len(m.Facts) > 0 {
		text += "\n" + strings.Join(factLines(m), "\n")
	}
	if m.Link != "" {
		text += "\n<" + m.Link + "|Open in library>"
	}
	if m.Detail != "" {
		text += "\n```" + truncate(m.Detail, 2500) + "```"
	}
	return map[string]any{"text": text}
}

// teamsPayload is an Adaptive Card, the format Teams Workflows webhooks accept.
func teamsPayload(m Message) map[string]any {
	body := []any{
		map[string]any{"type": "TextBlock", "size": "Medium", "weight": "Bolder", "text": m.Title, "wrap": true},
		map[string]any{"type": "TextBlock", "text": m.Text, "wrap": true},
	}
	if len(m.Facts) > 0 {
		var facts []any
		for _, k := range sortedKeys(m.Facts) {
			facts = append(facts, map[string]any{"title": k, "value": m.Facts[k]})
		}
		body = append(body, map[string]any{"type": "FactSet", "facts": facts})
	}
	if m.Detail != "" {
		body = append(body, map[string]any{"type": "TextBlock", "fontType": "Monospace", "wrap": true, "text": truncate(m.Detail, 2500)})
	}
	card := map[string]any{"$schema": "http://adaptivecards.io/schemas/adaptive-card.json", "type": "AdaptiveCard",
		"version": "1.4", "body": body}
	if m.Link != "" {
		card["actions"] = []any{map[string]any{"type": "Action.OpenUrl", "title": "Open in library", "url": m.Link}}
	}
	return map[string]any{"type": "message", "attachments": []any{
		map[string]any{"contentType": "application/vnd.microsoft.card.adaptive", "content": card}}}
}

func (n *Notifier) email(m Message) error {
	s := n.cfg.SMTP
	if s == nil {
		return fmt.Errorf("notify.smtp is not set")
	}
	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\nTo: %s\r\nSubject: [scriven] %s\r\nDate: %s\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n",
		s.From, strings.Join(n.cfg.EmailTo, ", "), m.Title, time.Now().Format(time.RFC1123Z))
	b.WriteString(m.Text + "\r\n\r\n")
	for _, l := range factLines(m) {
		b.WriteString(l + "\r\n")
	}
	if m.Link != "" {
		b.WriteString("\r\n" + m.Link + "\r\n")
	}
	if m.Detail != "" {
		b.WriteString("\r\n" + strings.ReplaceAll(m.Detail, "\n", "\r\n") + "\r\n")
	}
	var auth smtp.Auth
	if s.Username != "" {
		pw, err := n.res.Resolve(s.Password)
		if err != nil {
			return err
		}
		auth = smtp.PlainAuth("", s.Username, pw, s.Host)
	}
	return smtp.SendMail(net.JoinHostPort(s.Host, strconv.Itoa(s.Port)), auth, s.From, n.cfg.EmailTo, []byte(b.String()))
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "\n... (truncated)"
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}
