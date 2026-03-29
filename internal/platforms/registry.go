/*
 * ● ArcMusic
 * ○ A high-performance engine for streaming music in Telegram voicechats.
 *
 * Copyright (C) 2026 Team Arc
 */

package platforms

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/Laky-64/gologging"
	"github.com/amarnathcjd/gogram/telegram"
	"resty.dev/v3"

	state "main/internal/core/models"
	"main/internal/database"
	"main/internal/utils"
)

// TODO: NOT TESTED YET

type platformEntry struct {
	platform state.Platform
	priority int
}

type PlatformRegistry struct {
	platforms []platformEntry
	mu        sync.RWMutex
}

var (
	registry = &PlatformRegistry{
		platforms: make([]platformEntry, 0),
	}
	rc = resty.New()
)

// Register adds a platform to the registry with given priority
func Register(priority int, p state.Platform) {
	registry.mu.Lock()
	defer registry.mu.Unlock()

	registry.platforms = append(registry.platforms, platformEntry{p, priority})
	sort.Slice(registry.platforms, func(i, j int) bool {
		return registry.platforms[i].priority > registry.platforms[j].priority
	})
}

// GetOrderedPlatforms returns all platforms sorted by priority
func GetOrderedPlatforms() []state.Platform {
	registry.mu.RLock()
	defer registry.mu.RUnlock()

	res := make([]state.Platform, len(registry.platforms))
	for i, e := range registry.platforms {
		res[i] = e.platform
	}
	return res
}

func findPlatform(url string) state.Platform {
	for _, p := range GetOrderedPlatforms() {
		if p.CanGetTracks(url) {
			return p
		}
	}
	return nil
}

// GetTracks extracts tracks from the given query or message context
func GetTracks(m *telegram.NewMessage, video bool) ([]*state.Track, error) {
	gologging.Debug("GetTracks called | video: " + strconv.FormatBool(video))

	// 1. URL Processing
	if urls, _ := utils.ExtractURLs(m); len(urls) > 0 {
		gologging.Debug("URLs detected in message: " + strconv.Itoa(len(urls)))
		tracks, errs := processURLs(urls, video)
		if len(tracks) > 0 {
			gologging.Info("Returning tracks from URLs")
			return tracks, nil
		}

		if !hasPlayableReply(m) {
			return nil, combineErrors("no supported platform for given URL(s)", errs)
		}
		gologging.Debug("URL extraction failed, falling back to reply media check")
	}

	// 2. Query/Search Processing
	if query := m.Args(); query != "" {
		gologging.Info("Processing search query: " + query)
		tracks, err := processSearchQuery(query, video)
		if err == nil && len(tracks) > 0 {
			return tracks, nil
		}
	}

	// 3. Reply Chain Processing
	if m.IsReply() {
		return processReplyChain(m)
	}

	gologging.Info("No tracks found after checking URLs, Query, and Replies")
	return nil, errors.New("no tracks found")
}

func processURLs(urls []string, video bool) ([]*state.Track, []string) {
	var allTracks []*state.Track
	var errs []string

	for _, url := range urls {
		gologging.Info("Processing URL: " + url)
		p := findPlatform(url)
		if p == nil {
			errMsg := "No platform found for URL: " + url
			gologging.Error(errMsg)
			errs = append(errs, errMsg)
			continue
		}

		gologging.Debug("Matched platform [" + string(p.Name()) + "] for URL: " + url)
		tracks, err := p.GetTracks(url, video)
		if err != nil {
			if strings.Contains(err.Error(), "failed to extract metadata") {
				gologging.Debug("Silent skip: metadata extraction failed for " + url)
				continue
			}
			errMsg := string(p.Name()) + ": " + err.Error()
			gologging.Error(errMsg)
			errs = append(errs, errMsg)
			continue
		}

		gologging.Info("Tracks found: " + strconv.Itoa(len(tracks)))
		allTracks = append(allTracks, tracks...)
	}
	return allTracks, errs
}

func processSearchQuery(query string, video bool) ([]*state.Track, error) {
	if p := findPlatform(query); p != nil && p.Name() != PlatformYouTube {
		gologging.Debug("Query matches specific platform: " + string(p.Name()))
		tracks, err := p.GetTracks(query, video)
		if err == nil && len(tracks) > 0 {
			gologging.Info("Query handled by platform: " + string(p.Name()))
			return tracks, nil
		}
	}

	gologging.Info("Searching YouTube with query: " + query)
	tracks, err := yt.GetTracks(query, video)
	if err != nil {
		gologging.Error("YouTube search failed: " + err.Error())
		return nil, err
	}

	if len(tracks) > 0 {
		gologging.Info("YouTube search successful, returning top result")
		return []*state.Track{tracks[0]}, nil
	}

	gologging.Debug("YouTube search returned 0 results for: " + query)
	return nil, nil
}

func processReplyChain(m *telegram.NewMessage) ([]*state.Track, error) {
	gologging.Debug("Message is a reply, resolving media chain...")
	target, isVideo, err := findMediaInReply(m)
	if err != nil {
		gologging.Info("Reply chain does not contain valid media")
		return nil, err
	}

	tg := &TelegramPlatform{}
	track, err := tg.GetTracksByMessage(target)
	if err != nil {
		gologging.Error("Failed to get track from Telegram reply: " + err.Error())
		return nil, err
	}

	track.Video = isVideo
	if isVideo {
		noThumb, err := database.ThumbnailsDisabled(m.ChannelID())
		if err != nil || !noThumb {
