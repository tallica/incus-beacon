package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"
	"unicode/utf8"
)

// postJSON sends v and wants a 2xx; its errors never carry the URL.
func postJSON(ctx context.Context, client *http.Client, u *url.URL, v any) error {
	body, err := json.Marshal(v)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(body))
	if err != nil {
		return errors.New("bad request URL")
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		if ue, ok := errors.AsType[*url.Error](err); ok {
			err = ue.Err
		}
		return err
	}
	resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("answered %s", resp.Status)
	}
	return nil
}

func truncate(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	const more = "\n…"
	s = s[:limit-len(more)]
	for !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s + more
}

type webhook struct {
	url    *url.URL
	client *http.Client
}

// Payload is the JSON a webhook receives and --notify-command reads on stdin.
type Payload struct {
	Time        time.Time    `json:"time"`
	Server      string       `json:"server,omitempty"`
	Title       string       `json:"title"`
	Message     string       `json:"message"`
	Transitions []Transition `json:"transitions"`
}

func newPayload(b Batch) Payload {
	title, body := Message(b)
	return Payload{Time: time.Now().UTC(), Server: b.Server, Title: title, Message: body, Transitions: b.Transitions}
}

func (w *webhook) Notify(ctx context.Context, b Batch) error {
	return postJSON(ctx, w.client, w.url, newPayload(b))
}

// Discord caps a message's content at 2000 characters.
const discordLimit = 2000

type discord struct {
	url    *url.URL
	client *http.Client
}

func newDiscord(u *url.URL, client *http.Client) (Notifier, error) {
	return &discord{url: u, client: client}, nil
}

func (d *discord) Notify(ctx context.Context, b Batch) error {
	title, body := message(b, discordStyle)
	return postJSON(ctx, d.client, d.url, map[string]string{
		"username": "incus-beacon",
		"content":  truncate("**"+title+"**\n"+body, discordLimit),
	})
}

// Slack truncates a message's text past 40,000 characters; stay well under.
const slackLimit = 30000

type slack struct {
	url    *url.URL
	client *http.Client
}

func newSlack(u *url.URL, client *http.Client) (Notifier, error) {
	return &slack{url: u, client: client}, nil
}

func (s *slack) Notify(ctx context.Context, b Batch) error {
	title, body := message(b, slackStyle)
	return postJSON(ctx, s.client, s.url, map[string]string{"text": truncate("*"+title+"*\n"+body, slackLimit)})
}
