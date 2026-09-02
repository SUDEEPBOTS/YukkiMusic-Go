/*
 * ● YukkiMusic
 * ○ A high-performance engine for streaming music in Telegram voicechats.
 *
 * Copyright (C) 2026 TheTeamVivek
 */

package modules

import (
	"fmt"
	"strings"

	tg "github.com/amarnathcjd/gogram/telegram"

	"main/internal/database"
	"main/internal/utils"
)

func init() {
	helpTexts["/autoplay"] = `<i>Control automatic playback of related songs when the current queue ends.</i>

<u>Usage:</u>
<b>/autoplay</b> — Open interactive inline controls for Autoplay
<b>/autoplay [on|off|enable|disable]</b> — Quickly enable or disable autoplay`
}

func buildAutoplayMarkup(chatID int64, enabled bool) *tg.ReplyInlineMarkup {
	kb := tg.NewKeyboard()

	toggleText := "🔴 Turn ON"
	toggleAction := "on"
	if enabled {
		toggleText = "🟢 Turn OFF"
		toggleAction = "off"
	}

	kb.AddRow(
		tg.Button.Data(toggleText, fmt.Sprintf("autoplay:%s:%d", toggleAction, chatID)),
		tg.Button.Data("🗑 Close", "close"),
	)
	return kb.Build()
}

func getAutoplayMessage(enabled bool) string {
	status := "🔴 <b>Disabled (OFF)</b>"
	explanation := "When the current queue finishes, music playback will <b>stop automatically</b>."
	if enabled {
		status = "🟢 <b>Enabled (ON)</b>"
		explanation = "When the current queue finishes, the bot will <b>automatically discover and stream related songs</b> seamlessly."
	}

	return fmt.Sprintf(
		"📻 <b>Autoplay Mode Settings</b>\n\n"+
			"• <b>Current Status:</b> %s\n\n"+
			"<i>%s</i>\n\n"+
			"<i>Click the button below to toggle the autoplay state:</i>",
		status,
		explanation,
	)
}

func autoplayHandler(m *tg.NewMessage) error {
	args := strings.Fields(m.Text())
	chatID := m.ChannelID()

	if len(args) > 1 {
		argLower := strings.ToLower(args[1])
		switch argLower {
		case "enable", "on", "true", "1":
			if err := database.SetAutoplay(chatID, true); err != nil {
				m.Reply("❌ Failed to update autoplay settings: " + err.Error())
				return tg.ErrEndGroup
			}
			msg := getAutoplayMessage(true)
			kb := buildAutoplayMarkup(chatID, true)
			m.Reply(msg, &tg.SendOptions{ParseMode: "HTML", ReplyMarkup: kb})
			return tg.ErrEndGroup

		case "disable", "off", "false", "0":
			if err := database.SetAutoplay(chatID, false); err != nil {
				m.Reply("❌ Failed to update autoplay settings: " + err.Error())
				return tg.ErrEndGroup
			}
			msg := getAutoplayMessage(false)
			kb := buildAutoplayMarkup(chatID, false)
			m.Reply(msg, &tg.SendOptions{ParseMode: "HTML", ReplyMarkup: kb})
			return tg.ErrEndGroup
		}
	}

	enabled := database.IsAutoplayEnabled(chatID)
	msg := getAutoplayMessage(enabled)
	kb := buildAutoplayMarkup(chatID, enabled)

	m.Reply(msg, &tg.SendOptions{ParseMode: "HTML", ReplyMarkup: kb})
	return tg.ErrEndGroup
}

func autoplayCallbackHandler(cb *tg.CallbackQuery) error {
	data := cb.DataString()
	parts := strings.Split(data, ":")
	if len(parts) < 3 {
		return nil
	}

	action := parts[1]
	chatID := cb.ChannelID()

	// Admin / Auth permission check
	if isAdmin, err := utils.IsChatAdmin(cb.Client, chatID, cb.SenderID); err != nil || !isAdmin {
		cb.Answer("⚠️ Only group admins or authorized users can change Autoplay settings!", &tg.CallbackOptions{Alert: true})
		return nil
	}

	newStatus := false
	if action == "on" {
		newStatus = true
		_ = database.SetAutoplay(chatID, true)
		cb.Answer("✅ Autoplay has been enabled!", &tg.CallbackOptions{Alert: false})
	} else if action == "off" {
		newStatus = false
		_ = database.SetAutoplay(chatID, false)
		cb.Answer("🚫 Autoplay has been disabled!", &tg.CallbackOptions{Alert: false})
	}

	msg := getAutoplayMessage(newStatus)
	kb := buildAutoplayMarkup(chatID, newStatus)

	cb.Edit(msg, &tg.SendOptions{ParseMode: "HTML", ReplyMarkup: kb})
	return nil
}
