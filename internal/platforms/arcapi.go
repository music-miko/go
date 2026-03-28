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
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Laky-64/gologging"
	"github.com/amarnathcjd/gogram/telegram"
	
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"main/internal/config"
	"main/internal/core"
	state "main/internal/core/models"
	"main/internal/utils"
)

var telegramDLRegex = regexp.MustCompile(
	`https:\/\/t\.me\/([a-zA-Z0-9_]{5,})\/(\d+)`,
)

const PlatformArcApi state.PlatformName = "ArcApi"

var (
	mediaDbOnce     sync.Once
	mediaCollection *mongo.Collection
)

type ArcApiPlatform struct {
	name state.PlatformName
}

func init() {
	Register(80, &ArcApiPlatform{
		name: PlatformArcApi,
	})
}

// getMediaCollection safely initializes the MongoDB connection once
func getMediaCollection() *mongo.Collection {
	mediaDbOnce.Do(func() {
		if config.DbURI == "" {
			return
		}
		opts := options.Client().ApplyURI(config.DbURI)
		client, err := mongo.Connect(opts)
		if err != nil {
			gologging.Error("ArcApi: Failed to connect to Media DB: " + err.Error())
			return
		}
		mediaCollection = client.Database("arcapi").Collection("medias")
		gologging.Info("ArcApi: Connected to Media DB successfully")
	})
	return mediaCollection
}

func (f *ArcApiPlatform) Name() state.PlatformName {
	return f.name
}

func (f *ArcApiPlatform) CanGetTracks(query string) bool {
	return false
}

func (f *ArcApiPlatform) GetTracks(_ string, _ bool) ([]*state.Track, error) {
	return nil, errors.New("arcapi is a download-only platform")
}

func (f *ArcApiPlatform) CanDownload(source state.PlatformName) bool {
	if config.ArcAPIURL == "" || config.ArcAPIKey == "" {
		return false
	}
	return source == PlatformYouTube
}

func (f *ArcApiPlatform) Download(
	ctx context.Context,
	track *state.Track,
	statusMsg *telegram.NewMessage,
) (string, error) {

	// 0. Check Local Cache First
	if f := findFile(track); f != "" {
		gologging.Debug("ArcApi: Download -> Local Cached File -> " + f)
		return f, nil
	}

	var pm *telegram.ProgressManager
	if statusMsg != nil {
		pm = utils.GetProgress(statusMsg)
	}

	ext := "mp3"
	if track.Video {
		ext = "mp4"
	}
	path := getPath(track, "."+ext)

	// 1. Try Media DB Cache (Telegram Channel Download)
	if dbPath, err := f.downloadFromMediaDB(ctx, track, path, pm); err == nil && dbPath != "" {
		gologging.Info(fmt.Sprintf("DB-CACHE | %s | Video: %t", track.ID, track.Video))
		return dbPath, nil
	} else if err != nil {
		gologging.DebugF("ArcApi DB check failed or missed: %v", err)
	}

	gologging.Debug("ArcApi: DB Miss -> Falling back to API V2 Download")

	// 2. Try V2 API Polling (Optimized Download)
	dlURL, err := f.v2Download(ctx, track)
	if err != nil {
		gologging.ErrorF("ArcApi: V2 Download failed: %v", err)
		return "", err
	}

	var downloadErr error
	if telegramDLRegex.MatchString(dlURL) {
		gologging.DebugF("ArcApi: Downloading via Telegram URL: %s", dlURL)
		path, downloadErr = f.downloadFromTelegram(ctx, dlURL, path, pm)
	} else {
		gologging.DebugF("ArcApi: Downloading via CDN URL: %s", dlURL)
		downloadErr = f.downloadFromURL(ctx, dlURL, path)
	}

	if downloadErr != nil {
		return "", downloadErr
	}
	if !fileExists(path) {
		return "", errors.New("empty file returned by API")
	}

	gologging.Info(fmt.Sprintf("V2-API | %s | Video: %t", track.ID, track.Video))
	return path, nil
}

func (*ArcApiPlatform) CanSearch() bool { return false }

func (*ArcApiPlatform) Search(string, bool) ([]*state.Track, error) {
	return nil, nil
}

// --- Optimization Core: Database & API V2 ---

func (f *ArcApiPlatform) downloadFromMediaDB(
	ctx context.Context,
	track *state.Track,
	path string,
	pm *telegram.ProgressManager,
) (string, error) {
	col := getMediaCollection()
	if col == nil {
		return "", errors.New("db not configured")
	}
	if config.MediachannelId == 0 {
		return "", errors.New("media channel not configured")
	}

	ext := "mp3"
	mediaType := "a"
	if track.Video {
		ext = "mp4"
		mediaType = "v"
	}

	keys := []string{
		fmt.Sprintf("%s.%s", track.ID, ext),
		track.ID,
		fmt.Sprintf("%s_%s", track.ID, mediaType),
		fmt.Sprintf("%s_%s.%s", track.ID, mediaType, ext),
	}

	filter := bson.M{
		"track_id": bson.M{"$in": keys},
		"isVideo":  track.Video, // Dynamically checks audio vs video
	}

	var result struct {
		MessageID int32 `bson:"message_id"`
	}

	err := col.FindOne(ctx, filter).Decode(&result)
	if err != nil {
		if err == mongo.ErrNoDocuments {
			return "", errors.New("track not found in db cache")
		}
		return "", err
	}

	if result.MessageID == 0 {
		return "", errors.New("invalid message_id in db")
	}

	gologging.DebugF("ArcApi: Found MessageID %d in MediaChannel %d", result.MessageID, config.MediachannelId)

	msg, err := core.Bot.GetMessageByID(config.MediachannelId, result.MessageID)
	if err != nil {
		return "", fmt.Errorf("failed to fetch message from channel: %w", err)
	}

	dOpts := &telegram.DownloadOptions{
		FileName: path,
		Ctx:      ctx,
	}
	if pm != nil {
		dOpts.ProgressManager = pm
	}

	_, err = msg.Download(dOpts)
	if err != nil {
		os.Remove(path)
		return "", err
	}

	return path, nil
}

func (f *ArcApiPlatform) v2Download(ctx context.Context, track *state.Track) (string, error) {
	apiURL := config.ArcAPIURL
	apiKey := config.ArcAPIKey

	query := track.ID
	if query == "" {
		query = track.URL
	}

	for cycle := 0; cycle < 3; cycle++ {
		reqURL := fmt.Sprintf("%s/youtube/v2/download?api_key=%s&query=%s&isVideo=%t",
			strings.TrimRight(apiURL, "/"),
			apiKey,
			url.QueryEscape(query),
			track.Video, // Dynamically requests audio or video
		)

		var respData map[string]any
		resp, err := rc.R().
			SetContext(ctx).
			SetResult(&respData).
			Get(reqURL)

		if err != nil || resp.IsError() {
			time.Sleep(1 * time.Second)
			continue
		}

		candidate := f.extractCandidate(respData)
		if candidate == "" || strings.Contains(strings.ToLower(candidate), "processing") || strings.Contains(strings.ToLower(candidate), "queued") {
			jobID := f.extractJobID(respData)
			if jobID != "" {
				gologging.DebugF("ArcApi: Polling Job ID: %s", jobID)
				candidate = f.pollJobStatus(ctx, jobID)
			}
		}

		if candidate != "" {
			return f.normalizeURL(candidate, apiURL), nil
		}
		time.Sleep(2 * time.Second)
	}

	return "", errors.New("failed to extract download url from api after retries")
}

func (f *ArcApiPlatform) extractCandidate(data map[string]any) string {
	if job, ok := data["job"].(map[string]any); ok {
		if res, ok := job["result"].(map[string]any); ok {
			for _, k := range []string{"public_url", "cdnurl", "download_url", "url"} {
				if v, ok := res[k].(string); ok && strings.TrimSpace(v) != "" {
					return strings.TrimSpace(v)
				}
			}
		}
	}
	if res, ok := data["result"].(map[string]any); ok {
		for _, k := range []string{"public_url", "cdnurl", "download_url", "url"} {
			if v, ok := res[k].(string); ok && strings.TrimSpace(v) != "" {
				return strings.TrimSpace(v)
			}
		}
	}
	for _, k := range []string{"public_url", "cdnurl", "download_url", "url", "tg_link"} {
		if v, ok := data[k].(string); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func (f *ArcApiPlatform) extractJobID(data map[string]any) string {
	if job, ok := data["job"].(map[string]any); ok {
		if id, ok := job["id"].(string); ok {
			return id
		}
	}
	if id, ok := data["job_id"].(string); ok {
		return id
	}
	return ""
}

func (f *ArcApiPlatform) pollJobStatus(ctx context.Context, jobID string) string {
	apiURL := config.ArcAPIURL
	apiKey := config.ArcAPIKey
	interval := 2.0 // seconds

	for attempt := 0; attempt < 10; attempt++ {
		time.Sleep(time.Duration(interval * float64(time.Second)))

		reqURL := fmt.Sprintf("%s/youtube/jobStatus?api_key=%s&job_id=%s",
			strings.TrimRight(apiURL, "/"), apiKey, jobID)

		var respData map[string]any
		resp, err := rc.R().SetContext(ctx).SetResult(&respData).Get(reqURL)

		if err == nil && !resp.IsError() {
			cand := f.extractCandidate(respData)
			if cand != "" && !strings.Contains(strings.ToLower(cand), "processing") && !strings.Contains(strings.ToLower(cand), "queued") {
				return cand
			}
		}
		interval *= 1.2 // Exponential backoff
	}
	return ""
}

func (f *ArcApiPlatform) normalizeURL(candidate, apiURL string) string {
	if strings.HasPrefix(candidate, "http://") || strings.HasPrefix(candidate, "https://") {
		return candidate
	}
	if strings.HasPrefix(candidate, "/") {
		return strings.TrimRight(apiURL, "/") + candidate
	}
	return strings.TrimRight(apiURL, "/") + "/" + candidate
}

// --- Standard HTTP & Telegram Downloader Base ---

func (f *ArcApiPlatform) downloadFromURL(
	ctx context.Context,
	dlURL, path string,
) error {
	resp, err := rc.R().
		SetContext(ctx).
		SetOutputFileName(path).
		Get(dlURL)
	if err != nil {
		os.Remove(path)
		if errors.Is(err, context.Canceled) ||
			errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		return fmt.Errorf("http download failed: %w", err)
	}

	if resp.IsError() {
		return fmt.Errorf("download failed with status: %d", resp.StatusCode())
	}

	return nil
}

func (f *ArcApiPlatform) downloadFromTelegram(
	ctx context.Context,
	dlURL, path string,
	pm *telegram.ProgressManager,
) (string, error) {
	matches := telegramDLRegex.FindStringSubmatch(dlURL)
	if len(matches) < 3 {
		return "", fmt.Errorf("invalid telegram download url: %s", dlURL)
	}

	username := matches[1]
	messageID, err := strconv.Atoi(matches[2])
	if err != nil {
		return "", fmt.Errorf("invalid message ID: %v", err)
	}

	msg, err := core.Bot.GetMessageByID(username, int32(messageID))
	if err != nil {
		return "", fmt.Errorf("failed to fetch Telegram message: %w", err)
	}

	dOpts := &telegram.DownloadOptions{
		FileName: path,
		Ctx:      ctx,
	}
	if pm != nil {
		dOpts.ProgressManager = pm
	}
	_, err = msg.Download(dOpts)
	if err != nil {
		os.Remove(path)
		return "", err
	}
	return path, nil
}
