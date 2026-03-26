/*
 * ○ A high-performance engine for streaming music in Telegram voicechats.
 *
 * Copyright (C) 2026 Team Arc
 */

package modules

import (
	"context"
	"fmt"
	"html"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Laky-64/gologging"
	tg "github.com/amarnathcjd/gogram/telegram"

	"main/internal/config"
	"main/internal/core"
	"main/internal/database"
	"main/internal/locales"
)

// BroadcastManager manages the state and lifecycle of a broadcast operation.
type BroadcastManager struct {
	mu     sync.Mutex
	active bool
	cancel context.CancelFunc
	ctx    context.Context
}

var bManager = &BroadcastManager{}

// TryStart attempts to start a new broadcast. It returns true if successful,
// or false if a broadcast is already running.
func (bm *BroadcastManager) TryStart(
	ctx context.Context,
	cancel context.CancelFunc,
) bool {
	bm.mu.Lock()
	defer bm.mu.Unlock()
	if bm.active {
		return false
	}
	bm.active = true
	bm.ctx = ctx
	bm.cancel = cancel
	return true
}

// Stop cancels the ongoing broadcast and resets the manager's state.
func (bm *BroadcastManager) Stop() {
	bm.mu.Lock()
	defer bm.mu.Unlock()
	if bm.cancel != nil {
		bm.cancel()
	}
	bm.active = false
	bm.ctx = nil
	bm.cancel = nil
}

// IsActive returns whether a broadcast is currently running.
func (bm *BroadcastManager) IsActive() bool {
	bm.mu.Lock()
	defer bm.mu.Unlock()
	return bm.active
}

type BroadcastStats struct {
	TotalChats  int32
	TotalUsers  int32
	DoneChats   int32
	DoneUsers   int32
	FailedChats int32
	FailedUsers int32
	StartTime   time.Time
	Finished    atomic.Bool
}

type BroadcastFlags struct {
	Chats   bool
	Users   bool
	All     bool
	Forward bool
}

type broadcastJob struct {
	TargetID int64
	IsChat   bool
}

func init() {
	helpTexts["broadcast"] = `<i>High-performance broadcast to served chats and users.</i>

<u>Usage:</u>
<b>/broadcast [flags] [text] </b> — Broadcast text message.
<b>/broadcast [flags] [reply to message]</b> — Broadcast the replied message.

<blockquote>
<b>📋 Flags:</b>
• <code>-all</code> — Broadcast to both chats and users (Default)
• <code>-chats</code> — Broadcast to groups/chats only
• <code>-users</code> — Broadcast to users only
• <code>-forward</code> — Forward message with author tag (Default is Copy mode)
• <code>-cancel</code> - Cancel an ongoing broadcast
</blockquote>
<blockquote>
<b>📌 Examples:</b>
/broadcast -chats Important announcement
/broadcast -users -forward [reply to message]
</blockquote>
<b>⚠️ Notes:</b>
• Only the <b>owner</b> can use this command
• Optimized to handle 260K+ targets in under 3 hours safely.
• Only one broadcast can run at a time`

	helpTexts["gcast"] = helpTexts["broadcast"]
	helpTexts["bcast"] = helpTexts["broadcast"]
}

func broadcastHandler(m *tg.NewMessage) error {
	text := strings.ToLower(m.Text())
	chatID := m.ChannelID()

	// Check for cancel flag
	if strings.Contains(text, "-cancel") || strings.Contains(text, "--cancel") {
		return handleBroadcastCancel(m)
	}

	// Check if broadcast is already running
	if bManager.IsActive() {
		m.Reply(F(chatID, "broadcast_already_running"))
		return tg.ErrEndGroup
	}

	// Parse flags and content
	flags, content, err := parseBroadcastCommand(m)
	if err != nil {
		m.Reply(F(chatID, "broadcast_parse_failed", locales.Arg{
			"error": html.EscapeString(err.Error()),
		}))
		return tg.ErrEndGroup
	}

	if content == "" && !m.IsReply() {
		m.Reply(F(chatID, "broadcast_no_content", locales.Arg{
			"cmd": getCommand(m),
		}))
		return tg.ErrEndGroup
	}

	// If no specific target flag is set, default to All
	if !flags.Chats && !flags.Users && !flags.All {
		flags.All = true
	}

	var servedChats, servedUsers []int64
	var servedChatErr, servedUserErr error

	if flags.All || flags.Chats {
		servedChats, servedChatErr = database.ServedChats()
		if servedChatErr != nil {
			m.Reply(F(chatID, "broadcast_fetch_chats_failed", locales.Arg{
				"error": html.EscapeString(servedChatErr.Error()),
			}))
			return tg.ErrEndGroup
		}
	}

	if flags.All || flags.Users {
		servedUsers, servedUserErr = database.ServedUsers()
		if servedUserErr != nil {
			m.Reply(F(chatID, "broadcast_fetch_users_failed", locales.Arg{
				"error": html.EscapeString(servedUserErr.Error()),
			}))
			return tg.ErrEndGroup
		}
	}

	if len(servedChats) == 0 && len(servedUsers) == 0 {
		m.Reply(F(chatID, "broadcast_no_targets"))
		return tg.ErrEndGroup
	}

	stats := &BroadcastStats{
		TotalChats: int32(len(servedChats)),
		TotalUsers: int32(len(servedUsers)),
		StartTime:  time.Now(),
	}
	stats.Finished.Store(false)

	ctx, cancel := context.WithCancel(context.Background())
	if !bManager.TryStart(ctx, cancel) {
		m.Reply(F(chatID, "broadcast_already_running"))
		return tg.ErrEndGroup
	}

	progressMsg, err := m.Reply(F(chatID, "broadcast_initializing"),
		&tg.SendOptions{
			ReplyMarkup: core.GetBroadcastCancelKeyboard(chatID),
		})
	if err != nil {
		bManager.Stop()
		gologging.ErrorF("Failed to send broadcast progress message: %v", err)
		return tg.ErrEndGroup
	}

	// Start progress updater
	go bManager.updateProgress(ctx, progressMsg, stats)

	// Start highly concurrent broadcast process
	go bManager.start(
		ctx,
		m,
		progressMsg,
		flags,
		content,
		servedChats,
		servedUsers,
		stats,
	)

	return tg.ErrEndGroup
}

func parseBroadcastCommand(m *tg.NewMessage) (*BroadcastFlags, string, error) {
	flags := &BroadcastFlags{}

	text := strings.TrimSpace(m.Text())
	text = strings.TrimPrefix(text, m.GetCommand())
	text = strings.TrimSpace(text)

	lines := strings.Split(text, "\n")
	if len(lines) == 0 {
		return flags, "", nil
	}

	words := strings.Fields(lines[0])
	var contentWords []string

	for i := 0; i < len(words); i++ {
		word := strings.ToLower(words[i])
		switch word {
		case "-chats", "--chats":
			flags.Chats = true
		case "-users", "--users":
			flags.Users = true
		case "-all", "--all":
			flags.All = true
		case "-forward", "--forward":
			flags.Forward = true
		case "-cancel", "--cancel":
			continue // Handled earlier
		default:
			contentWords = append(contentWords, words[i])
		}
	}

	var content strings.Builder
	content.WriteString(strings.Join(contentWords, " "))
	if len(lines) > 1 {
		if content.Len() > 0 {
			content.WriteByte('\n')
		}
		content.WriteString(strings.Join(lines[1:], "\n"))
	}

	return flags, strings.TrimSpace(content.String()), nil
}

func (bm *BroadcastManager) start(
	ctx context.Context,
	m, progressMsg *tg.NewMessage,
	flags *BroadcastFlags,
	content string,
	chats, users []int64,
	stats *BroadcastStats,
) {
	defer bm.Stop()
	defer func() {
		if r := recover(); r != nil {
			gologging.ErrorF("Broadcast panic recovered: %v", r)
			bm.finalize(progressMsg, stats)
		}
	}()

	// Buffered channel for jobs
	jobs := make(chan broadcastJob, 5000)
	
	// Max workers: 25 (Telegram broadcast limit is ~30/s, so 25 is safe)
	const numWorkers = 25
	var wg sync.WaitGroup

	// Global Rate Limiter: strictly caps throughput to 25 msgs/s to ensure stability.
	// This prevents massive flood waits and memory spikes.
	rateLimiter := time.NewTicker(time.Second / numWorkers)
	defer rateLimiter.Stop()

	// Spawn workers
	for i := 0; i < numWorkers; i++ {
		wg.Add(1)
		go bm.worker(ctx, &wg, jobs, rateLimiter.C, m, content, flags, stats)
	}

	// Feed jobs
	go func() {
		defer close(jobs)
		for _, chatID := range chats {
			select {
			case <-ctx.Done():
				return
			case jobs <- broadcastJob{TargetID: chatID, IsChat: true}:
			}
		}
		for _, userID := range users {
			select {
			case <-ctx.Done():
				return
			case jobs <- broadcastJob{TargetID: userID, IsChat: false}:
			}
		}
	}()

	// Wait for all workers to finish their jobs
	wg.Wait()
	bm.finalize(progressMsg, stats)
}

func (bm *BroadcastManager) worker(
	ctx context.Context,
	wg *sync.WaitGroup,
	jobs <-chan broadcastJob,
	tick <-chan time.Time,
	m *tg.NewMessage,
	content string,
	flags *BroadcastFlags,
	stats *BroadcastStats,
) {
	defer wg.Done()

	for job := range jobs {
		// Wait for the rate limiter token
		select {
		case <-ctx.Done():
			return
		case <-tick:
		}

		success := bm.sendMessage(ctx, m, job.TargetID, content, flags)

		if job.IsChat {
			atomic.AddInt32(&stats.DoneChats, 1)
			if !success {
				atomic.AddInt32(&stats.FailedChats, 1)
			}
		} else {
			atomic.AddInt32(&stats.DoneUsers, 1)
			if !success {
				atomic.AddInt32(&stats.FailedUsers, 1)
			}
		}
	}
}

func (bm *BroadcastManager) sendMessage(
	ctx context.Context,
	m *tg.NewMessage,
	targetID int64,
	content string,
	flags *BroadcastFlags,
) bool {
	var err error

	try := func() error {
		if m.IsReply() {
			fOpts := &tg.ForwardOptions{
				HideAuthor: !flags.Forward, // True copies, False actually forwards
			}
			_, ferr := m.Client.Forward(
				targetID,
				m.Peer,
				[]int32{m.ReplyID()},
				fOpts,
			)
			return ferr
		}

		_, ferr := m.Client.SendMessage(targetID, content)
		return ferr
	}

	maxAttempts := 3
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		err = try()
		if err == nil {
			return true
		}

		if wait := tg.GetFloodWait(err); wait > 0 {
			// If flood wait occurs, pause just this worker
			if !bm.sleepCtx(ctx, time.Duration(wait)*time.Second) {
				return false
			}
			continue
		} else {
			break
		}
	}

	if err != nil {
		if !tg.MatchError(err, "USER_IS_BLOCKED") &&
			!tg.MatchError(err, "CHAT_WRITE_FORBIDDEN") &&
			!tg.MatchError(err, "USER_IS_DEACTIVATED") {
			gologging.ErrorF("Broadcast failed for %d: %v", targetID, err)
		}
		return false
	}
	return true
}

func (bm *BroadcastManager) updateProgress(
	ctx context.Context,
	progressMsg *tg.NewMessage,
	stats *BroadcastStats,
) {
	ticker := time.NewTicker(7 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if stats.Finished.Load() {
				return
			}
			text := formatBroadcastProgress(stats, false, progressMsg.ChannelID())
			progressMsg.Edit(text, &tg.SendOptions{
				ReplyMarkup: core.GetBroadcastCancelKeyboard(progressMsg.ChannelID()),
			})
		}
	}
}

func (bm *BroadcastManager) finalize(
	progressMsg *tg.NewMessage,
	stats *BroadcastStats,
) {
	stats.Finished.Store(true)
	text := formatBroadcastProgress(stats, true, progressMsg.ChannelID())
	progressMsg.Edit(text)
}

func formatBroadcastProgress(
	stats *BroadcastStats,
	final bool,
	chatID int64,
) string {
	elapsed := time.Since(stats.StartTime)

	doneChats := atomic.LoadInt32(&stats.DoneChats)
	doneUsers := atomic.LoadInt32(&stats.DoneUsers)
	failedChats := atomic.LoadInt32(&stats.FailedChats)
	failedUsers := atomic.LoadInt32(&stats.FailedUsers)

	var sb strings.Builder

	if final {
		// Use your final headers if applicable, else default
		sb.WriteString(F(chatID, "broadcast_completed_header") + "\n\n")
	} else {
		sb.WriteString(F(chatID, "broadcast_progress_header") + "\n\n")
	}

	chatProgress := 0.0
	if stats.TotalChats > 0 {
		chatProgress = float64(doneChats) / float64(stats.TotalChats) * 100
	}

	userProgress := 0.0
	if stats.TotalUsers > 0 {
		userProgress = float64(doneUsers) / float64(stats.TotalUsers) * 100
	}

	sb.WriteString(F(chatID, "broadcast_total_chats", locales.Arg{
		"done":     doneChats,
		"total":    stats.TotalChats,
		"progress": fmt.Sprintf("%.1f", chatProgress),
	}) + "\n")

	sb.WriteString(F(chatID, "broadcast_total_users", locales.Arg{
		"done":     doneUsers,
		"total":    stats.TotalUsers,
		"progress": fmt.Sprintf("%.1f", userProgress),
	}) + "\n\n")

	if failedChats > 0 {
		sb.WriteString(F(chatID, "broadcast_failed_chats", locales.Arg{
			"count": failedChats,
		}) + "\n")
	}
	if failedUsers > 0 {
		sb.WriteString(F(chatID, "broadcast_failed_users", locales.Arg{
			"count": failedUsers,
		}) + "\n")
	}

	if failedChats > 0 || failedUsers > 0 {
		sb.WriteString("\n")
	}

	totalDone := doneChats + doneUsers
	totalTargets := stats.TotalChats + stats.TotalUsers

	avgSpeed := 0.0
	if elapsed.Seconds() > 0 && totalDone > 0 {
		avgSpeed = float64(totalDone) / elapsed.Seconds()
	}

	// We indicate a smart auto delay instead of fixed static delay
	sb.WriteString(F(chatID, "broadcast_delay", locales.Arg{
		"delay": "Auto (Max 25/s)",
	}) + "\n")

	sb.WriteString(F(chatID, "broadcast_elapsed", locales.Arg{
		"elapsed": formatDuration(int(elapsed.Seconds())),
	}) + "\n")

	if !final && avgSpeed > 0 && totalDone < totalTargets {
		remaining := totalTargets - totalDone
		etaSeconds := float64(remaining) / avgSpeed
		sb.WriteString("\n" + F(chatID, "broadcast_eta", locales.Arg{
			"eta": formatDuration(int(etaSeconds)),
		}))
	}

	if final {
		totalSent := doneChats + doneUsers
		totalFailed := failedChats + failedUsers

		successRate := 0.0
		if totalTargets > 0 {
			successRate = float64(totalSent-totalFailed) / float64(totalTargets) * 100
		}

		sb.WriteString("\n\n" + F(chatID, "broadcast_success_rate", locales.Arg{
			"rate":  fmt.Sprintf("%.1f", successRate),
			"sent":  totalSent - totalFailed,
			"total": totalTargets,
		}))
	}

	return sb.String()
}

func handleBroadcastCancel(m *tg.NewMessage) error {
	if !bManager.IsActive() {
		m.Reply(F(m.ChannelID(), "broadcast_not_running"))
		return tg.ErrEndGroup
	}

	bManager.Stop()

	m.Reply(F(m.ChannelID(), "broadcast_cancel_success"))
	return tg.ErrEndGroup
}

func broadcastCancelCB(cb *tg.CallbackQuery) error {
	if cb.SenderID != config.OwnerID {
		cb.Answer(
			F(cb.ChannelID(), "broadcast_cancel_owner_only"),
			&tg.CallbackOptions{Alert: true},
		)
		return tg.ErrEndGroup
	}

	if !bManager.IsActive() {
		cb.Answer(
			F(cb.ChannelID(), "broadcast_cancel_none_running"),
			&tg.CallbackOptions{Alert: true},
		)
		return tg.ErrEndGroup
	}

	bManager.Stop()

	cb.Answer(
		F(cb.ChannelID(), "broadcast_cancel_done"),
		&tg.CallbackOptions{Alert: true},
	)
	
	// Appends the cancelled header so the progress message reflects the cancellation visually
	cb.Edit(F(cb.ChannelID(), "broadcast_cancelled_header"))
	return tg.ErrEndGroup
}

func (bm *BroadcastManager) sleepCtx(
	ctx context.Context,
	d time.Duration,
) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}
