// matrix-bridge-mcp — a read-only MCP server over a Matrix homeserver's
// bridged chats (mautrix bridges). One instance serves one network, or all.
//
// It reads as one Matrix account and offers five tools — list_chats,
// read_chat, search, get_context, get_file — and nothing that sends,
// reacts, marks read, joins, leaves or deletes. Meant to run as an account
// the bridge does not let send (no bridge permission, no login), so the
// guarantee is the bridge's, not just this code's. See README.md.
//
// Configuration, from the environment:
//
//	MATRIX_SOCKET    path of the homeserver's client-API unix socket, or
//	MATRIX_URL       its base URL
//	MATRIX_USER      the account's localpart
//	MATRIX_PASSWORD  its password (or MATRIX_PASSWORD_FILE)
//	MATRIX_DEVICE    the device id it signs in as (default MCPSERVER); give
//	                 each instance on one account its own
//	MATRIX_OPERATOR  the MXID whose messages are "me"
//	MATRIX_NETWORK   the one network to serve, as its bridge's protocol id
//	                 (whatsapp, linkedin); unset serves every bridge
//	NETWORK_NAME     what the tools call the chats (default: the network)
//	MCP_TOKEN        the bearer every request must carry (or MCP_TOKEN_FILE)
//	MCP_LISTEN       address to listen on (default 127.0.0.1:8080)
//	HIDDEN_CHATS     room ids no tool ever shows, comma-separated
//	CALLS_PER_MINUTE tool calls allowed per minute, all callers (default 120)
package main

import (
	"crypto/subtle"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

var version = "dev"

func setting(name string, required bool) string {
	v := os.Getenv(name)
	if f := os.Getenv(name + "_FILE"); v == "" && f != "" {
		b, err := os.ReadFile(f)
		if err != nil {
			log.Fatalf("%s_FILE: %v", name, err)
		}
		v = strings.TrimSpace(string(b))
	}
	if v == "" && required {
		log.Fatalf("%s is not set", name)
	}
	return v
}

func main() {
	log.SetFlags(0)
	socket, base := setting("MATRIX_SOCKET", false), setting("MATRIX_URL", false)
	if socket == "" && base == "" {
		log.Fatal("set MATRIX_SOCKET or MATRIX_URL")
	}
	token := setting("MCP_TOKEN", true)
	if len(token) < 32 {
		log.Fatal("MCP_TOKEN must be at least 32 characters")
	}
	listen := setting("MCP_LISTEN", false)
	if listen == "" {
		listen = "127.0.0.1:8080"
	}
	perMinute := 120
	if v := setting("CALLS_PER_MINUTE", false); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			log.Fatalf("CALLS_PER_MINUTE: %q is not a positive number", v)
		}
		perMinute = n
	}
	hidden := map[string]bool{}
	for _, id := range strings.Split(setting("HIDDEN_CHATS", false), ",") {
		if id = strings.TrimSpace(id); id != "" {
			hidden[id] = true
		}
	}

	network := strings.ToLower(strings.TrimSpace(setting("MATRIX_NETWORK", false)))
	label := strings.TrimSpace(setting("NETWORK_NAME", false))
	if label == "" {
		label = "bridged"
		if network != "" {
			label = network
		}
	}

	mx := newMatrixClient(socket, base, setting("MATRIX_USER", true), setting("MATRIX_PASSWORD", true))
	if d := setting("MATRIX_DEVICE", false); d != "" {
		mx.device = d
	}
	s := &server{
		mx:    mx,
		chats: &chats{mx: mx, operator: setting("MATRIX_OPERATOR", true), hidden: hidden, network: network},
		limit: &limiter{max: perMinute},
		label: label,
	}
	m := mcp.NewServer(&mcp.Implementation{Name: "matrix-bridge-mcp", Version: version}, &mcp.ServerOptions{
		Instructions: instructions(label),
	})
	if network != "" {
		log.Printf("serving the %s network only", network)
	}
	s.register(m)

	mux := http.NewServeMux()
	mux.Handle("/mcp", bearer(token, mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return m }, &mcp.StreamableHTTPOptions{Stateless: true})))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprintln(w, "ok") })
	log.Printf("matrix-bridge-mcp %s listening on %s", version, listen)
	srv := &http.Server{Addr: listen, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	log.Fatal(srv.ListenAndServe())
}

// bearer refuses any request without the token, compared in constant time.
func bearer(token string, next http.Handler) http.Handler {
	want := []byte("Bearer " + token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), want) != 1 {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// limiter allows max calls in any sliding minute, across every caller.
type limiter struct {
	max   int
	mu    sync.Mutex
	calls []time.Time
}

func (l *limiter) allow() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	cut := 0
	for cut < len(l.calls) && now.Sub(l.calls[cut]) > time.Minute {
		cut++
	}
	l.calls = l.calls[cut:]
	if len(l.calls) >= l.max {
		return false
	}
	l.calls = append(l.calls, now)
	return true
}
