// handlers/chat.go — Tipovačka 3.0
// HTTP handlery pro chat: stránka + WebSocket endpoint.
package handlers

import (
	"html/template"
	"log"
	"net/http"
)

// GET /chat — chat stránka
func ChatPage(tmpl *template.Template) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user := RequireLogin(w, r)
		if user == nil {
			return
		}
		RenderTemplate(w, r, tmpl, "chat.html", TemplateData{
			"User":  user,
			"Title": "Chat",
		})
	}
}

// GET /chat/ws — WebSocket upgrade
func ChatWS(w http.ResponseWriter, r *http.Request) {
	// Pro WS nemůžeme použít RequireLogin (ten přesměrovává a zapisuje hlavičky).
	// Použijeme GetCurrentUser, který jen čte session.
	user := GetCurrentUser(r)
	if user == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	conn, err := wsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("[chat/ws] upgrade: %v", err)
		return
	}

	client := &chatClient{
		hub:  GlobalChatHub,
		conn: conn,
		send: make(chan []byte, 256),
		user: user,
	}

	GlobalChatHub.register <- client

	go client.writePump()
	go client.readPump()
}
