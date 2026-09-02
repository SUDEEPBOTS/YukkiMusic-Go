package modules

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Laky-64/gologging"
	"github.com/gorilla/websocket"

	"main/internal/core"
	state "main/internal/core/models"
	"main/internal/platforms"
	"main/internal/utils"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

type wsClient struct {
	conn   *websocket.Conn
	send   chan []byte
	chatID int64
}

var (
	hubClients = make(map[*wsClient]bool)
	hubMu      sync.RWMutex
)

func broadcastToChat(chatID int64, msg []byte) {
	hubMu.RLock()
	defer hubMu.RUnlock()
	for c := range hubClients {
		if c.chatID == chatID {
			select {
			case c.send <- msg:
			default:
			}
		}
	}
}

func RegisterWebRoutes(mux *http.ServeMux) {
	dir := "/home/ubuntu/playeon_exact"

	// ── static assets ──
	mux.Handle("/_next/", http.StripPrefix("/_next/", http.FileServer(http.Dir(filepath.Join(dir, "_next")))))
	mux.Handle("/assets/", http.StripPrefix("/assets/", http.FileServer(http.Dir(filepath.Join(dir, "assets")))))
	mux.HandleFunc("/favicon.ico", func(w http.ResponseWriter, r *http.Request) { http.ServeFile(w, r, filepath.Join(dir, "favicon.ico")) })
	mux.HandleFunc("/icon.png", func(w http.ResponseWriter, r *http.Request) { http.ServeFile(w, r, filepath.Join(dir, "icon.png")) })
	mux.HandleFunc("/icon1.png", func(w http.ResponseWriter, r *http.Request) { http.ServeFile(w, r, filepath.Join(dir, "icon1.png")) })
	mux.HandleFunc("/apple-icon.png", func(w http.ResponseWriter, r *http.Request) { http.ServeFile(w, r, filepath.Join(dir, "apple-icon.png")) })
	mux.HandleFunc("/safari-pinned-tab.svg", func(w http.ResponseWriter, r *http.Request) { http.ServeFile(w, r, filepath.Join(dir, "safari-pinned-tab.svg")) })

	// ── /room  (Join screen) ──
	mux.HandleFunc("/room", func(w http.ResponseWriter, r *http.Request) {
		if websocket.IsWebSocketUpgrade(r) {
			handleWebSocket(w, r)
			return
		}
		if r.Header.Get("Next-Action") != "" || r.Method == http.MethodPost {
			handleServerAction(w, r)
			return
		}
		http.ServeFile(w, r, filepath.Join(dir, "room.html"))
	})

	// ── /room/player  (2D player) ──
	mux.HandleFunc("/room/player", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Next-Action") != "" || r.Method == http.MethodPost {
			handleServerAction(w, r)
			return
		}
		if r.Header.Get("RSC") == "1" || r.URL.Query().Get("_rsc") != "" {
			w.Header().Set("Content-Type", "text/x-component")
			http.ServeFile(w, r, filepath.Join(dir, "player.rsc"))
			return
		}
		http.ServeFile(w, r, filepath.Join(dir, "player.html"))
	})

	// ── /room/3d  (3D lounge) ──
	mux.HandleFunc("/room/3d", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Next-Action") != "" || r.Method == http.MethodPost {
			handleServerAction(w, r)
			return
		}
		if r.Header.Get("RSC") == "1" || r.URL.Query().Get("_rsc") != "" {
			w.Header().Set("Content-Type", "text/x-component")
			http.ServeFile(w, r, filepath.Join(dir, "3d.rsc"))
			return
		}
		http.ServeFile(w, r, filepath.Join(dir, "3d.html"))
	})

	mux.HandleFunc("/app", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/room", http.StatusFound)
	})

	// ── REST API ──
	mux.HandleFunc("/api/room", handleAPIRoomState)
	mux.HandleFunc("/api/room/control", handleAPIRoomControl)
	mux.HandleFunc("/api/room/search", handleAPIRoomSearch)
	mux.HandleFunc("/api/room/play", handleAPIRoomPlay)
}

// ─────────────────────────────────────────────────────────────────
// Next.js Server Action  (joinRoomAction)
// ─────────────────────────────────────────────────────────────────

func handleServerAction(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/x-component")
	w.Header().Set("Vary", "rsc, next-router-state-tree, next-router-prefetch")
	w.Header().Set("Cache-Control", "no-cache, no-store, max-age=0, must-revalidate")

	body, _ := io.ReadAll(r.Body)

	type Arg struct {
		InitDataRaw string `json:"initDataRaw"`
		StartParam  string `json:"startParam"`
	}
	var args []Arg
	_ = json.Unmarshal(body, &args)

	startParam := ""
	initDataRaw := ""
	if len(args) > 0 {
		startParam = args[0].StartParam
		initDataRaw = args[0].InitDataRaw
	}

	// parse user info from initData
	userName := "Listener"
	userID := "0"
	userUsername := ""
	userPhoto := ""

	if initDataRaw != "" {
		if vals, err := url.ParseQuery(initDataRaw); err == nil {
			if uj := vals.Get("user"); uj != "" {
				var u struct {
					ID        int64  `json:"id"`
					FirstName string `json:"first_name"`
					LastName  string `json:"last_name"`
					Username  string `json:"username"`
					PhotoURL  string `json:"photo_url"`
				}
				if json.Unmarshal([]byte(uj), &u) == nil {
					userID = fmt.Sprintf("%d", u.ID)
					userName = u.FirstName
					if u.LastName != "" {
						userName += " " + u.LastName
					}
					userUsername = u.Username
					userPhoto = u.PhotoURL
				}
			}
			if sp := vals.Get("start_param"); sp != "" && startParam == "" {
				startParam = sp
			}
		}
	}

	if startParam == "" {
		startParam = r.URL.Query().Get("startapp")
	}

	chatID, _ := strconv.ParseInt(startParam, 10, 64)

	// If no chatID from params, try to find any active room
	if chatID == 0 {
		_, chatID = core.GetAnyActiveRoom()
		if chatID != 0 {
			startParam = fmt.Sprintf("%d", chatID)
		}
	}

	// ── room title ──
	roomTitle := "Voice Chat"
	if chatID != 0 {
		if ch, err := core.Bot.GetChat(chatID); err == nil && ch != nil && ch.Title != "" {
			roomTitle = ch.Title
		}
	}

	// ── current track info ──
	trackTitle := ""
	trackThumb := ""
	isPlaying := false
	queueCount := 0

	if chatID != 0 {
		if ass, err := core.Assistants.ForChat(chatID); err == nil {
			if rm, ok := core.GetRoom(chatID, ass, false); ok && rm.IsActiveChat() {
				rm.Parse()
				if t := rm.Track(); t != nil {
					trackTitle = t.Title
					trackThumb = t.Artwork
					isPlaying = !rm.IsPaused()
					queueCount = len(rm.Queue())
				}
			}
		}
	}

	// ── build session exactly like Playeon ──
	session := map[string]any{
		"token":      startParam,
		"groupId":    startParam,
		"canControl": true,
		"mode":       "2d",
		"style":      "default",
		"user": map[string]any{
			"id":       userID,
			"name":     userName,
			"username": userUsername,
			"photoUrl": userPhoto,
			"role":     "user",
		},
		"preview": map[string]any{
			"title":    roomTitle,
			"roomName": nil,
			"avatarUrl": func() string {
				if trackThumb != "" {
					return trackThumb
				}
				return ""
			}(),
			"playing": isPlaying,
			"current": func() any {
				if trackTitle != "" {
					return map[string]any{"title": trackTitle, "video": false, "thumbnail": trackThumb}
				}
				return nil
			}(),
			"queued": queueCount,
			"participants": []map[string]any{
				{"id": userID, "name": userName, "photoUrl": userPhoto},
			},
		},
	}

	res, _ := json.Marshal(map[string]any{"ok": true, "session": session})
	flight := fmt.Sprintf("0:{\"a\":\"$@1\",\"f\":\"\",\"q\":\"\",\"i\":false,\"b\":\"-f8S6YYUDCNcCKFV9ytMu\"}\n1:%s\n", string(res))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(flight))
}

// ─────────────────────────────────────────────────────────────────
// WebSocket
// ─────────────────────────────────────────────────────────────────

func handleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}

	token := r.URL.Query().Get("token")
	chatID, _ := strconv.ParseInt(token, 10, 64)
	if chatID == 0 {
		_, chatID = core.GetAnyActiveRoom()
	}

	client := &wsClient{conn: conn, send: make(chan []byte, 64), chatID: chatID}
	hubMu.Lock()
	hubClients[client] = true
	hubMu.Unlock()

	defer func() {
		hubMu.Lock()
		delete(hubClients, client)
		hubMu.Unlock()
		close(client.send)
		conn.Close()
	}()

	// welcome
	wb, _ := json.Marshal(map[string]any{"type": "welcome", "self": map[string]any{"id": "guest", "name": "Listener"}})
	_ = conn.WriteMessage(websocket.TextMessage, wb)
	sendSnapshot(client)

	// writer
	go func() {
		for msg := range client.send {
			if client.conn.WriteMessage(websocket.TextMessage, msg) != nil {
				return
			}
		}
	}()

	// ticker
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	go func() {
		for range tick.C {
			sendSnapshot(client)
		}
	}()

	// reader
	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			break
		}
		var p map[string]any
		if json.Unmarshal(msg, &p) != nil {
			continue
		}
		typ, _ := p["type"].(string)
		switch typ {
		case "ping":
			_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"pong"}`))
		case "presence":
			sendSnapshot(client)
		case "react":
			broadcastToChat(client.chatID, msg)
		case "say":
			broadcastToChat(client.chatID, msg)
		case "control":
			act, _ := p["action"].(string)
			rm := findRoom(client.chatID)
			if rm != nil && rm.IsActiveChat() {
				switch act {
				case "pause":
					rm.Pause()
				case "resume":
					rm.Resume()
				case "skip":
					rm.Replay()
				case "replay":
					rm.Replay()
				case "stop":
					rm.Stop()
					core.DeleteRoom(client.chatID)
				}
			}
			sendSnapshot(client)
		}
	}
}

func findRoom(chatID int64) *core.RoomState {
	if chatID != 0 {
		if ass, err := core.Assistants.ForChat(chatID); err == nil {
			if rm, ok := core.GetRoom(chatID, ass, false); ok {
				return rm
			}
		}
	}
	rm, _ := core.GetAnyActiveRoom()
	return rm
}

func sendSnapshot(c *wsClient) {
	now := time.Now().UnixMilli()
	rm := findRoom(c.chatID)

	if rm == nil || !rm.IsActiveChat() {
		b, _ := json.Marshal(map[string]any{
			"type": "snapshot", "rev": 1, "serverTime": now,
			"playing": false, "current": nil, "queue": []any{},
			"lights": []bool{false, false, false}, "poses": []any{},
		})
		c.conn.WriteMessage(websocket.TextMessage, b)
		return
	}

	rm.Parse()
	t := rm.Track()
	if t == nil {
		return
	}

	queue := rm.Queue()
	ql := make([]map[string]any, 0, len(queue))
	for _, q := range queue {
		ql = append(ql, map[string]any{
			"id": q.ID, "title": q.Title, "duration": q.Duration,
			"artwork": q.Artwork, "url": q.URL, "requester": q.Requester,
		})
	}

	b, _ := json.Marshal(map[string]any{
		"type": "snapshot", "rev": 1, "serverTime": now,
		"playing": !rm.IsPaused(),
		"startedAt":         now - int64(rm.Position())*1000,
		"pausedPositionSec": rm.Position(),
		"current": map[string]any{
			"id": t.ID, "title": t.Title, "duration": t.Duration,
			"artwork": t.Artwork, "url": t.URL, "requester": t.Requester,
		},
		"queue": ql, "lights": []bool{true, false, true}, "poses": []any{},
	})
	c.conn.WriteMessage(websocket.TextMessage, b)
}

// ─────────────────────────────────────────────────────────────────
// REST helpers
// ─────────────────────────────────────────────────────────────────

func parseChatID(v any) int64 {
	switch val := v.(type) {
	case float64:
		return int64(val)
	case string:
		id, _ := strconv.ParseInt(val, 10, 64)
		return id
	}
	return 0
}

func handleAPIRoomState(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	chatID, _ := strconv.ParseInt(r.URL.Query().Get("chat_id"), 10, 64)
	rm := findRoom(chatID)
	if rm == nil || !rm.IsActiveChat() {
		json.NewEncoder(w).Encode(map[string]any{"active": false})
		return
	}
	rm.Parse()
	t := rm.Track()
	if t == nil {
		json.NewEncoder(w).Encode(map[string]any{"active": false})
		return
	}
	queue := rm.Queue()
	qd := make([]map[string]any, 0, len(queue))
	for _, q := range queue {
		qd = append(qd, map[string]any{
			"id": q.ID, "title": utils.ShortTitle(q.Title, 40), "duration": q.Duration,
			"artwork": q.Artwork, "url": q.URL, "requester": q.Requester,
		})
	}
	json.NewEncoder(w).Encode(map[string]any{
		"active": true, "chat_id": chatID, "is_paused": rm.IsPaused(), "shuffle": rm.Shuffle(),
		"track": map[string]any{
			"id": t.ID, "title": t.Title, "duration": t.Duration, "position": rm.Position(),
			"artwork": t.Artwork, "url": t.URL, "requester": t.Requester,
		},
		"queue": qd,
	})
}

func handleAPIRoomControl(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
	if r.Method == http.MethodOptions {
		return
	}
	var req struct {
		ChatID any    `json:"chat_id"`
		Action string `json:"action"`
		Value  int    `json:"value"`
	}
	if json.NewDecoder(r.Body).Decode(&req) != nil {
		http.Error(w, "bad request", 400)
		return
	}
	chatID := parseChatID(req.ChatID)
	rm := findRoom(chatID)
	if rm == nil || !rm.IsActiveChat() {
		http.Error(w, "no room", 404)
		return
	}
	switch req.Action {
	case "pause":
		rm.Pause()
	case "resume":
		rm.Resume()
	case "replay":
		rm.Replay()
	case "stop":
		rm.Stop()
		core.DeleteRoom(chatID)
	case "seek":
		p := rm.Position() + req.Value
		if p < 0 {
			p = 0
		}
		rm.Seek(p)
	case "seek_absolute":
		rm.Seek(req.Value)
	case "shuffle":
		rm.SetShuffle(!rm.Shuffle())
	}
	json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

func handleAPIRoomSearch(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
	if r.Method == http.MethodOptions {
		return
	}
	var req struct {
		Query string `json:"query"`
	}
	if json.NewDecoder(r.Body).Decode(&req) != nil {
		http.Error(w, "bad", 400)
		return
	}
	q := strings.TrimSpace(req.Query)
	if q == "" {
		json.NewEncoder(w).Encode([]any{})
		return
	}
	tracks, err := platforms.SearchTracks(q, false)
	if err != nil || len(tracks) == 0 {
		json.NewEncoder(w).Encode([]any{})
		return
	}
	lim := 8
	if len(tracks) < lim {
		lim = len(tracks)
	}
	res := make([]map[string]any, 0, lim)
	for _, t := range tracks[:lim] {
		res = append(res, map[string]any{"id": t.ID, "title": t.Title, "duration": t.Duration, "artwork": t.Artwork, "url": t.URL})
	}
	json.NewEncoder(w).Encode(res)
}

func handleAPIRoomPlay(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
	if r.Method == http.MethodOptions {
		return
	}
	var req struct {
		ChatID any    `json:"chat_id"`
		Query  string `json:"query"`
		By     string `json:"by"`
	}
	if json.NewDecoder(r.Body).Decode(&req) != nil {
		http.Error(w, "bad", 400)
		return
	}
	chatID := parseChatID(req.ChatID)
	if chatID == 0 {
		_, chatID = core.GetAnyActiveRoom()
	}
	if chatID == 0 {
		http.Error(w, "no chat", 400)
		return
	}
	go func() {
		tracks, err := platforms.SearchTracks(req.Query, false)
		if err != nil || len(tracks) == 0 {
			gologging.ErrorF("WebApp play: %v", err)
			return
		}
		track := tracks[0]
		if req.By != "" {
			track.Requester = req.By
		} else {
			track.Requester = "WebApp"
		}
		ass, err := core.Assistants.ForChat(chatID)
		if err != nil {
			return
		}
		room, _ := core.GetRoom(chatID, ass, true)
		room.Parse()
		if !room.IsActiveChat() {
			fp, err := platforms.Download(context.Background(), track, nil)
			if err != nil {
				return
			}
			room.Play(track, fp)
		} else {
			room.AddTracksToQueue([]*state.Track{track})
		}
	}()
	json.NewEncoder(w).Encode(map[string]any{"ok": true})
}
