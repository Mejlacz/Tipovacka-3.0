// handlers/chat_hub.go — Tipovačka 3.0
// WebSocket chat hub: správa připojení, broadcast, DB persistence, cross-machine sync.
package handlers

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"tipovacka/db"
	"tipovacka/models"
)

// ── Zprávy ───────────────────────────────────────────────────────────────────

// WsMsg je JSON struktura posílaná přes WebSocket.
type WsMsg struct {
	Type      string   `json:"type"`              // message | system | online | history
	ID        int64    `json:"id,omitempty"`
	UserID    int      `json:"user_id,omitempty"`
	Username  string   `json:"username,omitempty"`
	Text      string   `json:"text,omitempty"`
	TS        string   `json:"ts,omitempty"`      // UTC ISO
	Count     int      `json:"count,omitempty"`
	Users     []string `json:"users,omitempty"`
	History   []WsMsg  `json:"history,omitempty"`
}

// ── Client ───────────────────────────────────────────────────────────────────

type chatClient struct {
	hub  *ChatHub
	conn *websocket.Conn
	send chan []byte
	user *models.User
}

// writePump čte ze send kanálu a zapisuje do WS spojení (+ ping každých 54s).
func (c *chatClient) writePump() {
	ping := time.NewTicker(54 * time.Second)
	defer func() {
		ping.Stop()
		c.conn.Close()
	}()
	for {
		select {
		case msg, ok := <-c.send:
			_ = c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if !ok {
				_ = c.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}
			if err := c.conn.WriteMessage(websocket.TextMessage, msg); err != nil {
				return
			}
		case <-ping.C:
			_ = c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

// readPump čte zprávy od klienta, ukládá do DB a odesílá do hub.broadcast.
func (c *chatClient) readPump() {
	defer func() {
		c.hub.unregister <- c
		c.conn.Close()
	}()
	c.conn.SetReadLimit(4096)
	_ = c.conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	c.conn.SetPongHandler(func(string) error {
		_ = c.conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		return nil
	})

	for {
		_, raw, err := c.conn.ReadMessage()
		if err != nil {
			break
		}
		var incoming struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(raw, &incoming); err != nil {
			continue
		}
		text := strings.TrimSpace(incoming.Text)
		if text == "" || len([]rune(text)) > 1000 {
			continue
		}

		// Ulož do DB
		ctx := context.Background()
		var newID int64
		var createdAt time.Time
		err = db.Pool.QueryRow(ctx,
			`INSERT INTO chat_messages (user_id, username, message, created_at)
			 VALUES ($1, $2, $3, NOW()) RETURNING id, created_at`,
			c.user.ID, c.user.Username, text).Scan(&newID, &createdAt)
		if err != nil {
			log.Printf("[chat] insert: %v", err)
			continue
		}

		// Aktualizuj lastID, aby poller tuto zprávu znovu nevyslal
		c.hub.lastID.Store(newID)

		msg := WsMsg{
			Type:     "message",
			ID:       newID,
			UserID:   c.user.ID,
			Username: c.user.Username,
			Text:     text,
			TS:       createdAt.UTC().Format("2006-01-02T15:04:05Z"),
		}
		b, _ := json.Marshal(msg)
		select {
		case c.hub.broadcast <- b:
		default:
		}
	}
}

// ── Hub ──────────────────────────────────────────────────────────────────────

type ChatHub struct {
	mu         sync.RWMutex
	clients    map[*chatClient]bool
	broadcast  chan []byte
	register   chan *chatClient
	unregister chan *chatClient
	lastID     atomic.Int64
}

// GlobalChatHub je singleton spuštěný při startu aplikace.
var GlobalChatHub = &ChatHub{
	clients:    make(map[*chatClient]bool),
	broadcast:  make(chan []byte, 512),
	register:   make(chan *chatClient, 32),
	unregister: make(chan *chatClient, 32),
}

var wsUpgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 4096,
	CheckOrigin:     func(r *http.Request) bool { return true },
}

// Run spustí event loop hubu — volat jako goroutine.
func (h *ChatHub) Run(ctx context.Context) {
	// Inicializuj lastID z DB, aby poller nepřeposílal staré zprávy
	var lastID int64
	_ = db.Pool.QueryRow(ctx, `SELECT COALESCE(MAX(id), 0) FROM chat_messages`).Scan(&lastID)
	h.lastID.Store(lastID)

	// Spusť DB poller pro cross-machine sync (druhá Fly.io mašina)
	go h.pollNewMessages(ctx)

	for {
		select {
		case <-ctx.Done():
			return

		case c := <-h.register:
			h.mu.Lock()
			h.clients[c] = true
			h.mu.Unlock()
			go h.sendHistory(c)
			h.broadcastOnline()

		case c := <-h.unregister:
			h.mu.Lock()
			if _, ok := h.clients[c]; ok {
				delete(h.clients, c)
				close(c.send)
			}
			h.mu.Unlock()
			h.broadcastOnline()

		case msg := <-h.broadcast:
			h.mu.RLock()
			for c := range h.clients {
				select {
				case c.send <- msg:
				default:
					// pomalý klient — přeskočit
				}
			}
			h.mu.RUnlock()
		}
	}
}

// sendHistory pošle novému klientovi posledních 50 zpráv.
func (h *ChatHub) sendHistory(c *chatClient) {
	ctx := context.Background()
	rows, err := db.Pool.Query(ctx,
		`SELECT id, user_id, username, message, created_at
		 FROM chat_messages
		 ORDER BY created_at DESC
		 LIMIT 50`)
	if err != nil {
		log.Printf("[chat] history: %v", err)
		return
	}
	defer rows.Close()

	var msgs []WsMsg
	for rows.Next() {
		var m WsMsg
		var t time.Time
		var uid *int
		_ = rows.Scan(&m.ID, &uid, &m.Username, &m.Text, &t)
		if uid != nil {
			m.UserID = *uid
		}
		m.Type = "message"
		m.TS = t.UTC().Format("2006-01-02T15:04:05Z")
		msgs = append(msgs, m)
	}
	// Obrať pořadí: nejstarší první
	for i, j := 0, len(msgs)-1; i < j; i, j = i+1, j-1 {
		msgs[i], msgs[j] = msgs[j], msgs[i]
	}
	if msgs == nil {
		msgs = []WsMsg{} // JSON [] místo null
	}
	env := WsMsg{Type: "history", History: msgs}
	b, _ := json.Marshal(env)
	select {
	case c.send <- b:
	default:
	}
}

// broadcastOnline rozešle všem aktuální seznam online uživatelů.
func (h *ChatHub) broadcastOnline() {
	h.mu.RLock()
	names := make([]string, 0, len(h.clients))
	for c := range h.clients {
		names = append(names, c.user.Username)
	}
	count := len(h.clients)
	h.mu.RUnlock()

	msg := WsMsg{Type: "online", Count: count, Users: names}
	b, _ := json.Marshal(msg)
	select {
	case h.broadcast <- b:
	default:
	}
}

// pollNewMessages každé 2s kontroluje DB pro zprávy z ostatních Fly.io mašin.
func (h *ChatHub) pollNewMessages(ctx context.Context) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			lastID := h.lastID.Load()
			rows, err := db.Pool.Query(ctx,
				`SELECT id, user_id, username, message, created_at
				 FROM chat_messages WHERE id > $1 ORDER BY id ASC LIMIT 50`, lastID)
			if err != nil {
				continue
			}
			for rows.Next() {
				var m WsMsg
				var t time.Time
				var uid *int
				_ = rows.Scan(&m.ID, &uid, &m.Username, &m.Text, &t)
				if uid != nil {
					m.UserID = *uid
				}
				m.Type = "message"
				m.TS = t.UTC().Format("2006-01-02T15:04:05Z")
				b, _ := json.Marshal(m)
				h.lastID.Store(m.ID)
				select {
				case h.broadcast <- b:
				default:
				}
			}
			rows.Close()
		}
	}
}
