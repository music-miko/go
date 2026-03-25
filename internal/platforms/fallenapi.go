/*
 * ● YukkiMusic
 * ○ A high-performance engine for streaming music in Telegram voicechats.
 *
 * Copyright (C) 2026 TheTeamVivek
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
	
	// Updated to use the v2 MongoDB driver to match database.go
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

const PlatformFallenApi state.PlatformName = "FallenApi"

var (
	mediaDbOnce     sync.Once
	mediaCollection *mongo.Collection
)

type FallenApiPlatform struct {
	name state.PlatformName
}

func init() {
	Register(80, &FallenApiPlatform{
		name: PlatformFallenApi,
	})
}

// getMediaCollection safely initializes the MongoDB connection once
func getMediaCollection() *mongo.Collection {
	mediaDbOnce.Do(func() {
		if config.DbURI == "" {
			return
		}
		// v2 Connection Syntax
		opts := options.Client().ApplyURI(config.DbURI)
		client, err := mongo.Connect(opts)
		if err != nil {
			gologging.Error("FallenApi: Failed to connect to Media DB: " + err.Error())
			return
		}
		mediaCollection = client.Database("arcapi").Collection("medias")
		gologging.Info("FallenApi: Connected to Media DB successfully")
	})
	return mediaCollection
}

func (f *FallenApiPlatform) Name() state.PlatformName {
	return f.name
}

func (f *FallenApiPlatform) CanGetTracks(query string) bool {
	return false
}

func (f *FallenApiPlatform) GetTracks(_ string, _ bool) ([]*state.Track, error) {
	return nil, errors.New("fallenapi is a download-only platform")
}

func (f *FallenApiPlatform) CanDownload(source state.PlatformName) bool {
	if config.FallenAPIURL == "" || config.FallenAPIKey == "" {
		return false
	}
	return source == PlatformYouTube
}

func (f *FallenApiPlatform) Download(
	ctx context.Context,
	track *state.Track,
	statusMsg *telegram.NewMessage,
) (string, error) {

	var pm *telegram.ProgressManager
	if statusMsg != nil {
		pm = utils.GetProgress(statusMsg)
	}

	// 0. Check Local Cache
	if f := findFile(track); f != "" {
		gologging.Debug("FallenApi: Download -> Local Cached File -> " + f)
		return f, nil
	}

	ext := ".mp3"
	if track.Video {
		ext = ".mp4"
	}
	path := getPath(track, ext)

	// 1. Try Media DB Cache (Telegram Channel Download)
	if dbPath, err := f.downloadFromMediaDB(ctx, track, path, pm); err == nil && dbPath != "" {
		gologging.Info(fmt.Sprintf("✅ DB-CACHE | %s", track.ID))
		return dbPath, nil
	}

	gologging.Debug("FallenApi: DB Miss -> Falling back to API V2 Download")

	// 2. Try V2 API Polling (Optimized Download)
	dlURL, err := f.v2Download(ctx, track)
	if err != nil {
		return "", err
	}

	var downloadErr error
	if telegramDLRegex.MatchString(dlURL) {
		path, downloadErr = f.downloadFromTelegram(ctx, dlURL, path, pm)
	} else {
		downloadErr = f.downloadFromURL(ctx, dlURL, path)
	}

	if downloadErr != nil {
		return "", downloadErr
	}
	if !fileExists(path) {
		return "", errors.New("empty file returned by API")
	}

	gologging.Info(fmt.Sprintf("✅ V2-API | %s", track.ID))
	return path, nil
}

func (*FallenApiPlatform) CanSearch() bool { return false }

func (*FallenApiPlatform) Search(string, bool) ([]*state.Track, error) {
	return nil, nil
}

// --- Optimization Core: Database & API V2 ---

func (f *FallenApiPlatform) downloadFromMediaDB(
	ctx context.Context,
	track *state.Track,
	path string,
	pm *telegram.ProgressManager,
) (string, error) {
	col := getMediaCollection()
	if col == nil || config.MediachannelId == 0 {
		return "", errors.New("db or media channel not configured")
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
		"isVideo":  track.Video,
	}

	var result struct {
		MessageID int32 `bson:"message_id"`
	}

	err := col.FindOne(ctx, filter).Decode(&result)
	if err != nil {
		return "", err
	}

	if result.MessageID == 0 {
		return "", errors.New("invalid message_id in db")
	}

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

func (f *FallenApiPlatform) v2Download(ctx context.Context, track *state.Track) (string, error) {
	apiURL := config.FallenAPIURL
	apiKey := config.FallenAPIKey

	query := track.ID
	if query == "" {
		query = track.URL
	}

	for cycle := 0; cycle < 5; cycle++ {
		reqURL := fmt.Sprintf("%s/youtube/v2/download?api_key=%s&query=%s&isVideo=%t",
			strings.TrimRight(apiURL, "/"),
			apiKey,
			url.QueryEscape(query),
			track.Video,
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

func (f *FallenApiPlatform) extractCandidate(data map[string]any) string {
	if res, ok := data["result"].(map[string]any); ok {
		for _, k := range []string{"cdnurl", "public_url", "download_url", "url"} {
			if v, ok := res[k].(string); ok && strings.TrimSpace(v) != "" {
				return strings.TrimSpace(v)
			}
		}
	}
	for _, k := range []string{"cdnurl", "public_url", "download_url", "url", "tg_link"} {
		if v, ok := data[k].(string); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func (f *FallenApiPlatform) extractJobID(data map[string]any) string {
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

func (f *FallenApiPlatform) pollJobStatus(ctx context.Context, jobID string) string {
	apiURL := config.FallenAPIURL
	apiKey := config.FallenAPIKey
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

func (f *FallenApiPlatform) normalizeURL(candidate, apiURL string) string {
	if strings.HasPrefix(candidate, "http://") || strings.HasPrefix(candidate, "https://") {
		return candidate
	}
	if strings.HasPrefix(candidate, "/") {
		return strings.TrimRight(apiURL, "/") + candidate
	}
	return strings.TrimRight(apiURL, "/") + "/" + candidate
}

// --- Standard HTTP & Telegram Downloader Base ---

func (f *FallenApiPlatform) downloadFromURL(
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

func (f *FallenApiPlatform) downloadFromTelegram(
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
