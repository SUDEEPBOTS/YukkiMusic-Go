/*
 * ● YukkiMusic
 * ○ A high-performance engine for streaming music in Telegram voicechats.
 *
 * Copyright (C) 2026 TheTeamVivek
 */

package database

func IsAutoplayEnabled(chatID int64) bool {
	settings, err := getChatSettings(chatID)
	if err != nil {
		return false // Default OFF
	}
	return settings.AutoplayEnabled
}

func SetAutoplay(chatID int64, enabled bool) error {
	return modifyChatSettings(chatID, func(s *ChatSettings) bool {
		s.AutoplayEnabled = enabled
		return true
	})
}
