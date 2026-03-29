/*
 * ● ArcMusic
 * ○ A high-performance engine for streaming music in Telegram voicechats.
 *
 * Copyright (C) 2026 Team Arc
 */

package database

func PlayModeAdminsOnly(chatID int64) (bool, error) {
	settings, err := getChatSettings(chatID)
	if err != nil {
		return false, err
	}
	return settings.PlayModeAdminsOnly, nil
}

func SetPlayModeAdminsOnly(chatID int64, adminsOnly bool) error {
	return modifyChatSettings(chatID, func(s *ChatSettings) bool {
		if s.PlayModeAdminsOnly == adminsOnly {
			return false
		}
		s.PlayModeAdminsOnly = adminsOnly
		return true
	})
}
