package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

// A bridged chat as this server shows it. Only rooms a bridge made (they
// carry m.bridge state) are chats: the bridge's space and its bot's
// management room are not, and no tool reads them.
type chat struct {
	ID          string `json:"chat"`
	Name        string `json:"name"`
	Kind        string `json:"kind"`
	Network     string `json:"network"`
	Members     int    `json:"members,omitempty"`
	LastAt      string `json:"last_at,omitempty"`
	LastFrom    string `json:"last_from,omitempty"`
	LastPreview string `json:"last_preview,omitempty"`
	last        int64
}

const overviewTTL = time.Minute

type chats struct {
	mx       *matrixClient
	operator string
	hidden   map[string]bool

	mu  sync.Mutex
	at  time.Time
	all map[string]*chat
}

// overviewFilter asks /sync for what a chat list needs and nothing else: each
// room's name, bridge info and member count, the senders of its newest
// message (lazy-loaded), and that one message.
var overviewFilter = mustJSON(map[string]any{
	"presence":     map[string]any{"not_types": []string{"*"}},
	"account_data": map[string]any{"not_types": []string{"*"}},
	"room": map[string]any{
		"timeline": map[string]any{"limit": 1, "types": []string{"m.room.message", "m.sticker"}},
		"state": map[string]any{
			"types":             []string{"m.room.name", "m.bridge", "uk.half-shot.bridge", "m.room.member"},
			"lazy_load_members": true,
		},
		"ephemeral":    map[string]any{"not_types": []string{"*"}},
		"account_data": map[string]any{"not_types": []string{"*"}},
	},
})

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func (c *chats) overview(ctx context.Context) (map[string]*chat, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.all != nil && time.Since(c.at) < overviewTTL {
		return c.all, nil
	}
	var r struct {
		Rooms struct {
			Join map[string]struct {
				Summary struct {
					Joined int `json:"m.joined_member_count"`
				} `json:"summary"`
				State    struct{ Events []event } `json:"state"`
				Timeline struct{ Events []event } `json:"timeline"`
			} `json:"join"`
		} `json:"rooms"`
	}
	q := url.Values{"filter": {overviewFilter}, "timeout": {"0"}}
	if err := c.mx.do(ctx, "GET", "/_matrix/client/v3/sync", q, nil, &r); err != nil {
		return nil, err
	}
	all := make(map[string]*chat, len(r.Rooms.Join))
	for id, room := range r.Rooms.Join {
		if c.hidden[id] {
			continue
		}
		ch := &chat{ID: id, Members: room.Summary.Joined}
		names := map[string]string{}
		bridged := false
		for _, ev := range room.State.Events {
			switch ev.Type {
			case "m.room.name":
				var n struct {
					Name string `json:"name"`
				}
				_ = json.Unmarshal(ev.Content, &n)
				ch.Name = n.Name
			case "m.bridge", "uk.half-shot.bridge":
				var b bridgeContent
				if json.Unmarshal(ev.Content, &b) != nil {
					continue
				}
				bridged = true
				ch.Network = b.Protocol.ID
				ch.Kind = kindOf(b)
				if ch.Name == "" {
					ch.Name = b.Channel.Displayname
				}
			case "m.room.member":
				var m memberContent
				if ev.StateKey != nil && json.Unmarshal(ev.Content, &m) == nil {
					names[*ev.StateKey] = personName(m.Displayname)
				}
			}
		}
		if !bridged || ch.Kind == "space" {
			continue
		}
		if ch.Name == "" {
			ch.Name = id
		}
		ch.Name = clean(ch.Name, 120)
		if n := len(room.Timeline.Events); n > 0 {
			ev := room.Timeline.Events[n-1]
			ch.last = ev.TS
			ch.LastAt = when(ev.TS)
			ch.LastFrom = c.who(ev.Sender, names)
			var m messageContent
			if json.Unmarshal(ev.Content, &m) == nil {
				ch.LastPreview = clean(preview(m), 160)
			}
		}
		all[id] = ch
	}
	c.all, c.at = all, time.Now()
	return all, nil
}

// get resolves a chat id from a tool call: only bridged, visible chats.
func (c *chats) get(ctx context.Context, id string) (*chat, error) {
	all, err := c.overview(ctx)
	if err != nil {
		return nil, err
	}
	ch, ok := all[id]
	if !ok {
		return nil, fmt.Errorf("no chat %q: use an id list_chats returned", id)
	}
	return ch, nil
}

func (c *chats) list(ctx context.Context, query, kind string, limit int) ([]*chat, int, error) {
	all, err := c.overview(ctx)
	if err != nil {
		return nil, 0, err
	}
	q := strings.ToLower(strings.TrimSpace(query))
	var out []*chat
	for _, ch := range all {
		if kind != "" && kind != "all" && ch.Kind != kind {
			continue
		}
		if q != "" && !strings.Contains(strings.ToLower(ch.Name), q) {
			continue
		}
		out = append(out, ch)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].last > out[j].last })
	total := len(out)
	if len(out) > limit {
		out = out[:limit]
	}
	return out, total, nil
}

func (c *chats) who(sender string, names map[string]string) string {
	if sender == c.operator {
		return "me"
	}
	if n := names[sender]; n != "" {
		return clean(n, 80)
	}
	return sender
}

func kindOf(b bridgeContent) string {
	switch {
	case b.RoomType == "space":
		return "space"
	case b.RoomType == "dm":
		return "dm"
	case strings.HasSuffix(b.Channel.ID, "@newsletter"):
		return "channel"
	case strings.HasPrefix(b.Channel.ID, "status@"):
		return "status"
	default:
		return "group"
	}
}

func when(ts int64) string {
	if ts == 0 {
		return ""
	}
	return time.UnixMilli(ts).Local().Format(time.RFC3339)
}

// preview is a message in a line: its text, or what kind of attachment it is.
func preview(m messageContent) string {
	switch m.MsgType {
	case "m.image", "m.video", "m.audio", "m.file":
		label := map[string]string{"m.image": "[image]", "m.video": "[video]", "m.audio": "[audio]", "m.file": "[file]"}[m.MsgType]
		if c := caption(m); c != "" {
			return label + " " + c
		}
		return label
	case "m.location":
		return "[location]"
	case "":
		return "[sticker]"
	default:
		return m.Body
	}
}

// caption is the text sent with an attachment: the bridge puts it in body
// and the file's name in filename; with no caption, body is the name.
func caption(m messageContent) string {
	if m.FileName != "" && m.Body != m.FileName {
		return m.Body
	}
	return ""
}
