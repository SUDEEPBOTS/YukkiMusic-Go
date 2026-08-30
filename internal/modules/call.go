/*
 * ● YukkiMusic
 * ○ A high-performance engine for streaming music in Telegram voicechats.
 *
 * Copyright (C) 2026 TheTeamVivek
 *
 * This program is free software: you can redistribute it and/or modify it under the
 * terms of the GNU General Public License as published by the Free Software Foundation,
 * either version 3 of the License, or (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful, but WITHOUT ANY
 * WARRANTY; without even the implied warranty of MERCHANTABILITY or FITNESS FOR A
 * PARTICULAR PURPOSE. See the GNU General Public License for more details.
 *
 * Repository: https://github.com/TheTeamVivek/YukkiMusic
 */

package modules

import (
	"context"
	"crypto/rand"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"time"

	"github.com/Laky-64/gologging"
	"github.com/amarnathcjd/gogram/telegram"

	"main/internal/core"
	state "main/internal/core/models"
	"main/internal/locales"
	"main/internal/platforms"
	"main/internal/utils"
	"main/ntgcalls"
)

// ── In-Memory Fast Anti-Repeat Autoplay History ───────────────────────────────
var autoplayHistory sync.Map // map[int64]*sync.Map (chatID -> videoID -> struct{})

func getChatHistoryMap(chatID int64) *sync.Map {
	val, ok := autoplayHistory.Load(chatID)
	if !ok {
		m := &sync.Map{}
		autoplayHistory.Store(chatID, m)
		return m
	}
	return val.(*sync.Map)
}

func apMarkPlayed(chatID int64, videoID string) {
	if videoID == "" {
		return
	}
	m := getChatHistoryMap(chatID)
	m.Store(videoID, time.Now())
}

func apIsPlayed(chatID int64, videoID string) bool {
	if videoID == "" {
		return false
	}
	m := getChatHistoryMap(chatID)
	_, exists := m.Load(videoID)
	return exists
}

func apClearChat(chatID int64) {
	autoplayHistory.Delete(chatID)
}

// ── Stream end handler ────────────────────────────────────────────────────────

func streamEndHandler(
	chatID int64,
	streamType ntgcalls.StreamType,
	_ ntgcalls.StreamDevice,
) {
	if streamType == ntgcalls.VideoStream {
		gologging.Debug("[onStreamEndHandler] Video stream ended, returning")
		return
	}

	gologging.DebugF("[onStreamEndHandler] Stream ended in chat %d", chatID)
	ass, err := core.Assistants.ForChat(chatID)
	if err != nil {
		gologging.ErrorF("Failed to get Assistant for %d: %v", chatID, err)
		return
	}
	r, ok := core.GetRoom(chatID, ass, false)
	if !ok {
		return
	}
	scheduleOldPlayingMessage(r)

	if ok, v := r.GetData("is_transitioning"); ok {
		if ok, v := v.(bool); ok && v {
			return
		}
	}

	r.SetData("is_transitioning", true)
	defer r.DeleteData("is_transitioning")

	cid := r.ChatID
	r.Parse()

	prevTrack := r.Track()

	var t *state.Track
	var wasLooping bool
	var isAutoplay bool

	t = r.NextTrack()
	if t == nil {
		t = tryAutoplay(chatID, r)
		if t == nil {
			apClearChat(chatID)
			core.DeleteRoom(chatID)
			core.Bot.SendMessage(cid, F(cid, "stream_queue_finished"))
			return
		}
		t.Requester = "🎵 ᴀᴜᴛᴏᴘʟᴀʏ"
		isAutoplay = true
	} else if prevTrack != nil && t == prevTrack {
		wasLooping = true
	}

	statusText := F(cid, "stream_downloading_next")
	if isAutoplay {
		statusText = F(cid, "autoplay_fetching_next")
	} else if wasLooping && r.FilePath() != "" {
		statusText = F(cid, "cb_replaying")
	}

	statusMsg, sendErr := core.Bot.SendMessage(cid, statusText)
	if sendErr != nil {
		gologging.ErrorF("[call.go] Failed to send status msg: %v", sendErr)
	}

	var filePath string
	var dlErr error
	if wasLooping && r.FilePath() != "" {
		filePath = r.FilePath()
	} else {
		// Download with smart retry in autoplay mode
		const maxRetries = 5
		const downloadTimeout = 60 * time.Second
		for attempt := 0; attempt < maxRetries; attempt++ {
			dlCtx, cancel := context.WithTimeout(context.Background(), downloadTimeout)
			filePath, dlErr = platforms.Download(dlCtx, t, statusMsg)
			cancel()
			if dlErr == nil {
				break
			}
			if isAutoplay {
				gologging.WarnF("[Autoplay] Download failed (attempt %d/%d) for %q: %v — trying next candidate", attempt+1, maxRetries, t.Title, dlErr)
				nextT := pickAutoplayCandidate(chatID, t)
				if nextT == nil {
					break
				}
				nextT.Requester = "🎵 ᴀᴜᴛᴏᴘʟᴀʏ"
				t = nextT
			} else {
				break
			}
		}
	}

	if dlErr != nil {
		gologging.ErrorF("[onStreamEndHandler] Download failed for %s: %v", t.URL, dlErr)
		utils.EOR(statusMsg, F(cid, "stream_download_fail", locales.Arg{
			"error": dlErr.Error(),
		}))
		core.DeleteRoom(chatID)
		return
	}

	if err := r.Play(t, filePath, true); err != nil {
		gologging.ErrorF("[onStreamEndHandler] Play failed for %s: %v", t.URL, err)
		utils.EOR(statusMsg, F(cid, "stream_play_fail"))
		core.DeleteRoom(chatID)
		return
	}

	title := utils.ShortTitle(t.Title, 25)
	safeTitle := utils.EscapeHTML(title)

	msgText := F(cid, "stream_now_playing", locales.Arg{
		"url":      t.URL,
		"title":    safeTitle,
		"duration": utils.FormatDuration(t.Duration),
		"by":       t.Requester,
	})

	opt := &telegram.SendOptions{
		ParseMode:   "HTML",
		ReplyMarkup: core.GetPlayMarkup(cid, r, false),
	}
	if t.Artwork != "" && shouldShowThumb(chatID) {
		opt.Media = utils.CleanURL(t.Artwork)
	}

	statusMsg, _ = utils.EOR(statusMsg, msgText, opt)
	r.SetStatusMsg(statusMsg)
}

// ── Smart Autoplay Engine (Golden-Zone Relevance + Anti-Repeat Filter) ────────

func tryAutoplay(chatID int64, r *core.RoomState) *state.Track {
	// If autoplay is enabled for room/chat
	return pickAutoplayCandidate(chatID, r.Track())
}

func pickAutoplayCandidate(chatID int64, cur *state.Track) *state.Track {
	if cur == nil || cur.ID == "" {
		gologging.WarnF("[Autoplay] Current track is nil or empty for chat %d", chatID)
		return nil
	}

	// Mark current track as played in session
	apMarkPlayed(chatID, cur.ID)

	var candidates []*state.Track

	// ── Tier 1: YouTube Official Radio Mix (with proper "RD" prefix) ───────────
	mixPlaylistID := cur.ID
	if !strings.HasPrefix(mixPlaylistID, "RD") {
		mixPlaylistID = "RD" + cur.ID
	}

	mix, err := platforms.GetYouTubeMixPlaylist(context.Background(), mixPlaylistID)
	if err == nil && len(mix) > 0 {
		for i, t := range mix {
			// Top 15 songs in Radio Mix are the highest-quality relevant matches (Golden Zone)
			if i >= 15 {
				break
			}
			if t.ID != "" && t.ID != cur.ID && !apIsPlayed(chatID, t.ID) {
				candidates = append(candidates, t)
			}
		}
	}

	// ── Tier 2: Smart Search Fallback (Exact Vibe / Artist Matching) ───────────
	if len(candidates) == 0 {
		cleanTitle := cleanSongTitleForSearch(cur.Title)
		gologging.InfoF("[Autoplay] Radio mix empty for %s, falling back to smart search: %q", cur.Title, cleanTitle)
		query := fmt.Sprintf("%s similar songs", cleanTitle)
		if searchTracks, sErr := platforms.GetYouTubePlaylist(context.Background(), query); sErr == nil && len(searchTracks) > 0 {
			for _, t := range searchTracks {
				if t.ID != "" && t.ID != cur.ID && !apIsPlayed(chatID, t.ID) {
					candidates = append(candidates, t)
				}
			}
		}
	}

	// ── Tier 3: History Reset (If all songs have been played, recycle cleanly) ──
	if len(candidates) == 0 {
		gologging.InfoF("[Autoplay] All candidates exhausted for chat %d, refreshing session history", chatID)
		apClearChat(chatID)
		apMarkPlayed(chatID, cur.ID)
		if len(mix) > 1 {
			for _, t := range mix {
				if t.ID != "" && t.ID != cur.ID {
					candidates = append(candidates, t)
					if len(candidates) >= 5 {
						break
					}
				}
			}
		}
	}

	if len(candidates) == 0 {
		gologging.WarnF("[Autoplay] No candidates could be found for chat %d", chatID)
		return nil
	}

	// ── Golden-Zone Selection: Pick strictly from Top 4 closest hit matches ────
	maxPool := len(candidates)
	if maxPool > 4 {
		maxPool = 4
	}

	n, err := rand.Int(rand.Reader, big.NewInt(int64(maxPool)))
	var chosen *state.Track
	if err != nil {
		chosen = candidates[0]
	} else {
		chosen = candidates[n.Int64()]
	}

	// Record chosen song so it never repeats
	apMarkPlayed(chatID, chosen.ID)
	gologging.InfoF("[Autoplay] Successfully picked Golden-Zone song: %q (%s) for chat %d", chosen.Title, chosen.ID, chatID)
	return chosen
}

func cleanSongTitleForSearch(title string) string {
	title = strings.Split(title, "|")[0]
	title = strings.Split(title, "(")[0]
	title = strings.Split(title, "[")[0]
	return strings.TrimSpace(title)
}
