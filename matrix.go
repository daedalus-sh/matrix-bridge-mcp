package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// matrixClient is the few Client-Server API calls this server makes, as one
// account (the reader), over a unix socket or plain HTTP. It never sends an
// event: the only POST it makes besides login is a search.
type matrixClient struct {
	base string
	http *http.Client
	// Over a unix socket the homeserver sees no client address; Synapse
	// takes it from X-Forwarded-For on such a listener, and fails a login
	// without one. Only this server can reach that socket.
	socket   bool
	user     string
	password string
	// The device every login of this instance signs in as, so a restart
	// replaces its token instead of adding devices. Two instances on one
	// account need two devices, or each login revokes the other's token.
	device string

	mu    sync.Mutex
	token string
}

func newMatrixClient(socket, baseURL, user, password string) *matrixClient {
	c := &matrixClient{base: strings.TrimRight(baseURL, "/"), user: user, password: password, device: "MCPSERVER"}
	tr := &http.Transport{MaxIdleConns: 8, IdleConnTimeout: 90 * time.Second}
	if socket != "" {
		c.base = "http://synapse"
		c.socket = true
		tr.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		}
	}
	c.http = &http.Client{Transport: tr, Timeout: 60 * time.Second}
	return c
}

type matrixError struct {
	Status  int
	ErrCode string `json:"errcode"`
	Message string `json:"error"`
}

func (e *matrixError) Error() string {
	if e.ErrCode == "" {
		return fmt.Sprintf("homeserver answered HTTP %d", e.Status)
	}
	return fmt.Sprintf("%s: %s", e.ErrCode, e.Message)
}

func (c *matrixClient) login(ctx context.Context) (string, error) {
	var r struct {
		AccessToken string `json:"access_token"`
	}
	err := c.raw(ctx, "POST", "/_matrix/client/v3/login", nil, "", map[string]any{
		"type":                        "m.login.password",
		"identifier":                  map[string]string{"type": "m.id.user", "user": c.user},
		"password":                    c.password,
		"device_id":                   c.device,
		"initial_device_display_name": "matrix-bridge-mcp",
	}, &r)
	if err != nil {
		return "", fmt.Errorf("login: %w", err)
	}
	return r.AccessToken, nil
}

func (c *matrixClient) currentToken(ctx context.Context, renew bool) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && !renew {
		return c.token, nil
	}
	t, err := c.login(ctx)
	if err != nil {
		return "", err
	}
	c.token = t
	return t, nil
}

// do calls the API as the account, signing in again once when the token was
// revoked.
func (c *matrixClient) do(ctx context.Context, method, path string, q url.Values, body, out any) error {
	tok, err := c.currentToken(ctx, false)
	if err != nil {
		return err
	}
	err = c.raw(ctx, method, path, q, tok, body, out)
	var me *matrixError
	if errors.As(err, &me) && me.Status == http.StatusUnauthorized {
		if tok, err = c.currentToken(ctx, true); err != nil {
			return err
		}
		err = c.raw(ctx, method, path, q, tok, body, out)
	}
	return err
}

func (c *matrixClient) raw(ctx context.Context, method, path string, q url.Values, tok string, body, out any) error {
	u := c.base + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	c.forwarded(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		e := &matrixError{Status: resp.StatusCode}
		_ = json.Unmarshal(data, e)
		return e
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

func (c *matrixClient) forwarded(req *http.Request) {
	if c.socket {
		req.Header.Set("X-Forwarded-For", "127.0.0.1")
	}
}

// media downloads an mxc:// URI through the authenticated media API, refusing
// anything over max bytes.
func (c *matrixClient) media(ctx context.Context, mxc string, max int64) ([]byte, string, error) {
	rest, ok := strings.CutPrefix(mxc, "mxc://")
	server, id, ok2 := strings.Cut(rest, "/")
	if !ok || !ok2 || server == "" || id == "" || strings.ContainsAny(id, "/?#") {
		return nil, "", fmt.Errorf("not a media URI: %q", mxc)
	}
	tok, err := c.currentToken(ctx, false)
	if err != nil {
		return nil, "", err
	}
	path := "/_matrix/client/v1/media/download/" + url.PathEscape(server) + "/" + url.PathEscape(id)
	req, err := http.NewRequestWithContext(ctx, "GET", c.base+path, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	c.forwarded(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("media download answered HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, max+1))
	if err != nil {
		return nil, "", err
	}
	if int64(len(data)) > max {
		return nil, "", fmt.Errorf("media is larger than %d bytes", max)
	}
	return data, resp.Header.Get("Content-Type"), nil
}

// ── the event shapes this server reads ──────────────────────────────────

type event struct {
	Type     string          `json:"type"`
	EventID  string          `json:"event_id"`
	RoomID   string          `json:"room_id"`
	Sender   string          `json:"sender"`
	TS       int64           `json:"origin_server_ts"`
	StateKey *string         `json:"state_key,omitempty"`
	Content  json.RawMessage `json:"content"`
	Unsigned struct {
		RedactedBecause json.RawMessage `json:"redacted_because,omitempty"`
	} `json:"unsigned"`
}

type messageContent struct {
	MsgType string `json:"msgtype"`
	Body    string `json:"body"`
	URL     string `json:"url"`
	Info    struct {
		MimeType string `json:"mimetype"`
		Size     int64  `json:"size"`
		Duration int64  `json:"duration"`
		W        int    `json:"w"`
		H        int    `json:"h"`
	} `json:"info"`
	FileName  string `json:"filename"`
	RelatesTo struct {
		RelType   string `json:"rel_type"`
		EventID   string `json:"event_id"`
		Key       string `json:"key"`
		InReplyTo struct {
			EventID string `json:"event_id"`
		} `json:"m.in_reply_to"`
	} `json:"m.relates_to"`
	NewContent *messageContent `json:"m.new_content"`
}

type memberContent struct {
	Membership  string `json:"membership"`
	Displayname string `json:"displayname"`
}

type bridgeContent struct {
	Protocol struct {
		ID          string `json:"id"`
		Displayname string `json:"displayname"`
	} `json:"protocol"`
	Channel struct {
		ID          string `json:"id"`
		Displayname string `json:"displayname"`
	} `json:"channel"`
	RoomType string `json:"com.beeper.room_type"`
}
