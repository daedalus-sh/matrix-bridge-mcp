package main

import (
	"encoding/json"
	"sort"
)

// A message as the tools return it. Text, caption and file name are other
// people's words: untrusted, cleaned of invisible characters, cut long.
type message struct {
	ID        string     `json:"id"`
	At        string     `json:"at"`
	From      string     `json:"from"`
	FromMe    bool       `json:"from_me,omitempty"`
	Kind      string     `json:"kind"`
	Text      string     `json:"text,omitempty"`
	Caption   string     `json:"caption,omitempty"`
	File      *fileInfo  `json:"file,omitempty"`
	ReplyTo   string     `json:"reply_to,omitempty"`
	Edited    bool       `json:"edited,omitempty"`
	Deleted   bool       `json:"deleted,omitempty"`
	Reactions []reaction `json:"reactions,omitempty"`
	ts        int64
}

type fileInfo struct {
	Name      string `json:"name,omitempty"`
	Mime      string `json:"mime,omitempty"`
	Bytes     int64  `json:"bytes,omitempty"`
	Seconds   int64  `json:"seconds,omitempty"`
	Width     int    `json:"width,omitempty"`
	Height    int    `json:"height,omitempty"`
	Fetchable bool   `json:"fetchable,omitempty"`
}

type reaction struct {
	Emoji string   `json:"emoji"`
	From  []string `json:"from"`
}

const maxText = 4000

var msgKind = map[string]string{
	"m.text":     "text",
	"m.notice":   "notice",
	"m.emote":    "emote",
	"m.image":    "image",
	"m.video":    "video",
	"m.audio":    "audio",
	"m.file":     "file",
	"m.location": "location",
}

// toMessages turns a page of events (any order) into messages, oldest first:
// edits are folded into the message they edit, reactions are counted onto
// theirs, and everything that is not a message is dropped. An edit or a
// reaction whose target is outside the page is dropped too, rather than
// shown without its context.
func toMessages(events []event, names map[string]string, who func(string, map[string]string) string, operator string) []message {
	byID := map[string]*message{}
	var order []*message
	type pending struct {
		target string
		ev     event
		m      messageContent
	}
	var edits, reacts []pending
	for _, ev := range events {
		if ev.Type != "m.room.message" && ev.Type != "m.sticker" && ev.Type != "m.reaction" {
			continue
		}
		var m messageContent
		_ = json.Unmarshal(ev.Content, &m)
		if ev.Type == "m.reaction" {
			if m.RelatesTo.RelType == "m.annotation" && len(ev.Unsigned.RedactedBecause) == 0 {
				reacts = append(reacts, pending{m.RelatesTo.EventID, ev, m})
			}
			continue
		}
		if m.RelatesTo.RelType == "m.replace" {
			edits = append(edits, pending{m.RelatesTo.EventID, ev, m})
			continue
		}
		msg := &message{
			ID:      ev.EventID,
			At:      when(ev.TS),
			From:    who(ev.Sender, names),
			FromMe:  ev.Sender == operator,
			ReplyTo: m.RelatesTo.InReplyTo.EventID,
			ts:      ev.TS,
		}
		if len(ev.Unsigned.RedactedBecause) > 0 {
			msg.Kind, msg.Deleted = "deleted", true
		} else {
			fill(msg, ev.Type, m)
		}
		byID[ev.EventID] = msg
		order = append(order, msg)
	}
	sort.SliceStable(edits, func(i, j int) bool { return edits[i].ev.TS < edits[j].ev.TS })
	for _, e := range edits {
		if t, ok := byID[e.target]; ok && !t.Deleted && e.m.NewContent != nil {
			fill(t, "m.room.message", *e.m.NewContent)
			t.Edited = true
		}
	}
	for _, r := range reacts {
		t, ok := byID[r.target]
		if !ok || r.m.RelatesTo.Key == "" {
			continue
		}
		from := who(r.ev.Sender, names)
		found := false
		for i := range t.Reactions {
			if t.Reactions[i].Emoji == r.m.RelatesTo.Key {
				t.Reactions[i].From = append(t.Reactions[i].From, from)
				found = true
			}
		}
		if !found {
			t.Reactions = append(t.Reactions, reaction{Emoji: clean(r.m.RelatesTo.Key, 16), From: []string{from}})
		}
	}
	sort.SliceStable(order, func(i, j int) bool { return order[i].ts < order[j].ts })
	out := make([]message, len(order))
	for i, m := range order {
		out[i] = *m
	}
	return out
}

func fill(msg *message, evType string, m messageContent) {
	if evType == "m.sticker" {
		msg.Kind = "sticker"
		msg.Text = clean(m.Body, 200)
		return
	}
	msg.Kind = msgKind[m.MsgType]
	if msg.Kind == "" {
		msg.Kind = "other"
	}
	switch m.MsgType {
	case "m.image", "m.video", "m.audio", "m.file":
		msg.Text = ""
		msg.Caption = clean(caption(m), maxText)
		msg.File = &fileInfo{
			Name:      clean(fileName(m), 200),
			Mime:      m.Info.MimeType,
			Bytes:     m.Info.Size,
			Seconds:   m.Info.Duration / 1000,
			Width:     m.Info.W,
			Height:    m.Info.H,
			Fetchable: hasMedia(m),
		}
	default:
		msg.Text = clean(m.Body, maxText)
	}
}

// membersOf reads display names from the member events a lazy-loading
// response carries in `state`.
func membersOf(state []event) map[string]string {
	names := map[string]string{}
	for _, ev := range state {
		if ev.Type != "m.room.member" || ev.StateKey == nil {
			continue
		}
		var m memberContent
		if json.Unmarshal(ev.Content, &m) == nil && m.Displayname != "" {
			names[*ev.StateKey] = personName(m.Displayname)
		}
	}
	return names
}
