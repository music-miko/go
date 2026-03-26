/*
 * ○ A high-performance engine for streaming music in Telegram voicechats.
 *
 * Copyright (C) 2026 Team Arc
 */

package modules

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/Laky-64/gologging"
	tg "github.com/amarnathcjd/gogram/telegram"

	"main/internal/config"
	"main/internal/core"
	"main/internal/database"
	"main/internal/locales"
	"main/internal/utils"
)

var (
	autoLeaveMu      sync.Mutex
	autoLeaveRunning bool
	limit            = 150 // Clean up to 150 groups daily per client
)

func init() {
	helpTexts["autoleave"] = fmt.Sprintf(
		`<i>Automatically makes the assistant leave inactive or unnecessary chats every day at 4:35 AM (IST).</i>

<u>Usage:</u>
<b>/autoleave </b>— Shows current auto-leave status (enabled/disabled).  
<b>/autoleave enable</b> — Enable auto-leave mode.  
<b>/autoleave disable</b> — Disable auto-leave mode.

<b>🧠 Details:</b>
Once enabled, the bot checks all joined groups/channels every day at <b>4:35 AM IST</b> and leaves up to <b>%d chats per cycle</b> that are not in the active room list.

<b>⚠️ Restrictions:</b>
This command can only be used by <b>owners</b> or <b>sudo users</b>.`,
		limit,
	)
}

func autoLeaveHandler(m *tg.NewMessage) error {
	args := strings.Fields(m.Text())
	chatID := m.ChannelID()

	currentState, err := database.AutoLeave()
	if err != nil {
		m.Reply(F(chatID, "autoleave_fetch_fail"))
		return tg.ErrEndGroup
	}

	status := F(chatID, utils.IfElse(currentState, "enabled", "disabled"))

	if len(args) < 2 {
		m.Reply(F(chatID, "autoleave_status", locales.Arg{
			"cmd":    getCommand(m),
			"action": status,
		}))
		return tg.ErrEndGroup
	}

	newState, err := utils.ParseBool(args[1])
	if err != nil {
		m.Reply(F(chatID, "invalid_bool"))
		return tg.ErrEndGroup
	}

	if newState == currentState {
		m.Reply(F(chatID, "autoleave_already", locales.Arg{
			"action": status,
		}))
		return tg.ErrEndGroup
	}

	if err := database.SetAutoLeave(newState); err != nil {
		m.Reply(F(chatID, "autoleave_update_fail"))
		return tg.ErrEndGroup
	}

	newStatus := F(chatID, utils.IfElse(newState, "enabled", "disabled"))
	m.Reply(F(chatID, "autoleave_updated", locales.Arg{
		"action": newStatus,
	}))

	autoLeaveMu.Lock()
	if newState {
		if !autoLeaveRunning {
			go startAutoLeave()
		}
	} else {
		autoLeaveRunning = false
	}
	autoLeaveMu.Unlock()

	return tg.ErrEndGroup
}

func startAutoLeave() {
	autoLeaveMu.Lock()
	if autoLeaveRunning {
		autoLeaveMu.Unlock()
		return
	}
	autoLeaveRunning = true
	autoLeaveMu.Unlock()

	// Load India Standard Time (Asia/Kolkata) timezone
	loc, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		// Fallback to manual IST offset if tzdata is missing from the system (UTC + 5:30)
		loc = time.FixedZone("IST", 5*3600+1800)
	}

	for {
		autoLeaveMu.Lock()
		if !autoLeaveRunning {
			autoLeaveMu.Unlock()
			return
		}
		autoLeaveMu.Unlock()

		// Get current time in IST
		now := time.Now().In(loc)
		
		// Set target time to exactly 4:35 AM IST for today
		next := time.Date(now.Year(), now.Month(), now.Day(), 4, 35, 0, 0, loc)
		
		// If it's already past 4:35 AM IST today, schedule for 4:35 AM IST tomorrow
		if !now.Before(next) {
			next = next.Add(24 * time.Hour)
		}
		
		// Calculate precise duration to sleep relative to actual absolute time
		sleepDuration := next.Sub(time.Now())

		// Sleep until the exact target time
		time.Sleep(sleepDuration)

		// Check again if the feature was disabled while we were sleeping
		autoLeaveMu.Lock()
		if !autoLeaveRunning {
			autoLeaveMu.Unlock()
			return
		}
		autoLeaveMu.Unlock()

		activeRooms := core.GetAllRooms()
		
		// Run cleanup concurrently for all assistants
		core.Assistants.ForEach(func(a *core.Assistant) {
			if a == nil || a.Client == nil {
				return
			}
			go autoLeaveAssistant(a, activeRooms)
		})
		
		// Sleep for an extra minute to ensure we don't accidentally re-trigger 
		// on the exact same minute if processing finishes extremely fast
		time.Sleep(1 * time.Minute)
	}
}

func autoLeaveAssistant(
	ass *core.Assistant,
	activeRooms map[int64]*core.RoomState,
) {
	leaveCount := 0
	err := ass.Client.IterDialogs(func(d *tg.TLDialog) error {
		if d.IsUser() {
			return nil
		}
		chatID := d.GetChannelID()

		if chatID == 0 || chatID == config.LoggerID ||
			d.GetID() == config.LoggerID {
			return nil
		}

		if _, ok := activeRooms[chatID]; ok {
			return nil
		}

		if err := ass.Client.LeaveChannel(chatID); err != nil {
			if wait := tg.GetFloodWait(err); wait > 0 {
				gologging.ErrorF(
					"FloodWait detected (%ds). Sleeping...", wait,
				)
				time.Sleep(time.Duration(wait) * time.Second)
				return nil
			}

			if strings.Contains(err.Error(), "USER_NOT_PARTICIPANT") ||
				strings.Contains(err.Error(), "CHANNEL_PRIVATE") {
				return nil
			}

			gologging.WarnF(
				"AutoLeave (Assistant %d) failed to leave %d: %v",
				ass.Index, chatID, err,
			)
			return nil
		}

		leaveCount++
		gologging.InfoF(
			"AutoLeave: Assistant %d left %d (%d/%d)",
			ass.Index, chatID, leaveCount, limit,
		)

		// 3 second delay between leaving groups to avoid strict floodwaits
		time.Sleep(3 * time.Second)

		// Stop once we hit the daily limit per client
		if leaveCount >= limit {
			return tg.ErrStopIteration
		}

		return nil
	}, &tg.DialogOptions{
		Limit: 0,
	})

	if err != nil && err != tg.ErrStopIteration {
		gologging.WarnF(
			"AutoLeave: IterDialogs error (assistant %d): %v",
			ass.Index, err,
		)
	}
}
