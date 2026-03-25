/*
 * ○ A high-performance engine for streaming music in Telegram voicechats.
 *
 * Copyright (C) 2026 Team Arc
 */

package modules

import (
	"fmt"
	"time"

	tg "github.com/amarnathcjd/gogram/telegram"
	"github.com/shirou/gopsutil/v3/cpu"
	"github.com/shirou/gopsutil/v3/disk"
	"github.com/shirou/gopsutil/v3/mem"

	"main/internal/config"
	"main/internal/core"
	"main/internal/database"
	"main/internal/locales"
	"main/internal/utils"
)

func init() {
	helpTexts["/ping"] = `<i>Check bot responsiveness and system stats.</i>

<u>Usage:</u>
<b>/ping</b> — Get bot status

<b>📊 Information Shown:</b>
• Response latency (ms)
• Uptime
• RAM usage
• CPU usage
• Disk usage

<b>💡 Use Case:</b>
Check if the bot is alive and how the server is performing.`
}

func formatUptime(d time.Duration) string {
	days := int(d.Hours() / 24)
	hours := int(d.Hours()) % 24
	minutes := int(d.Minutes()) % 60
	seconds := int(d.Seconds()) % 60

	result := ""
	if days > 0 {
		result += fmt.Sprintf("%dd ", days)
	}
	if hours > 0 {
		result += fmt.Sprintf("%dh ", hours)
	}
	if minutes > 0 {
		result += fmt.Sprintf("%dm ", minutes)
	}
	result += fmt.Sprintf("%ds", seconds)
	return result
}

func pingHandler(m *tg.NewMessage) error {
	if m.IsPrivate() {
		m.Delete()
		database.AddServedUser(m.ChannelID())
	} else {
		database.AddServedChat(m.ChannelID())
	}

	start := time.Now()
	reply, err := m.Respond(F(m.ChannelID(), "ping_start"))
	if err != nil {
		return err
	}

	latency := time.Since(start).Milliseconds()
	uptime := time.Since(config.StartTime)
	uptimeStr := formatUptime(uptime)
	ramInfo := "N/A"
	cpuUsage := "N/A"
	diskUsage := "N/A"

	opt := &tg.SendOptions{
		ReplyMarkup: core.SuppMarkup(m.ChannelID()),
	}
	if config.PingImage != "" {
		opt.Media = config.PingImage
	}

	v, err := mem.VirtualMemory()
	if err == nil {
		ramInfo = fmt.Sprintf("%.2f%%", v.UsedPercent)
	}

	if percentages, err := cpu.Percent(time.Second, false); err == nil &&
		len(percentages) > 0 {
		cpuUsage = fmt.Sprintf("%.2f%%", percentages[0])
	}

	d, err := disk.Usage("/")
	if err == nil {
		diskUsage = fmt.Sprintf("%.2f%%", d.UsedPercent)
	}

	pingText := F(m.ChannelID(), "ping_details", locales.Arg{
		"latency": utils.IntToStr(int(latency)),
		"uptime":  uptimeStr,
		"cpu":     cpuUsage,
		"ram":     ramInfo,
		"disk":    diskUsage,
	})

	utils.EOR(reply, pingText, opt)
	return tg.ErrEndGroup
}
