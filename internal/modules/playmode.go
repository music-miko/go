/*
 * ● ArcMusic
 * ○ A high-performance engine for streaming music in Telegram voicechats.
 *
 * Copyright (C) 2026 Team Arc
 */

package modules

import (
	"strings"

	tg "github.com/amarnathcjd/gogram/telegram"

	"main/internal/database"
	"main/internal/locales"
	"main/internal/utils"
)

func init() {
	helpTexts["/playmode"] = `<b>Control who can use the /play command in this chat.</b>

<u>Usage:</u>
<b>/playmode [enable|disable]</b> — Set play mode restriction

<b>⚙️ Options:</b>
• <b>enable</b> — Only admins and authorized users can play
• <b>disable</b> — Everyone can play (default)

<b>🔒 Restrictions:</b>
• Only <b>chat admins</b> can change this setting`
}

func playmodeHandler(m *tg.NewMessage) error {
	args := strings.Fields(m.Text())
	chatID := m.ChannelID()

	current, err := database.PlayModeAdminsOnly(chatID)
	if err != nil {
		return err
	}

	if len(args) < 2 {
		statusKey := "playmode_status_everyone"
		if current {
			statusKey = "playmode_status_admins"
		}

		m.Reply(F(chatID, "playmode_help", locales.Arg{
			"status": F(chatID, statusKey),
		}), &tg.SendOptions{ParseMode: "HTML"})
		return tg.ErrEndGroup
	}

	adminsOnly, err := utils.ParseBool(args[1])
	if err != nil {
		m.Reply(F(chatID, "invalid_bool"))
		return tg.ErrEndGroup
	}

	if err := database.SetPlayModeAdminsOnly(chatID, adminsOnly); err != nil {
		return err
	}

	successKey := "playmode_success_everyone"
	if adminsOnly {
		successKey = "playmode_success_admins"
	}

	m.Reply(F(chatID, successKey), &tg.SendOptions{ParseMode: "HTML"})
	return tg.ErrEndGroup
}
