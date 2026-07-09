package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"

	"cpa-usage-keeper/internal/config"
	"cpa-usage-keeper/internal/cpa"
	"cpa-usage-keeper/internal/models"
	"cpa-usage-keeper/internal/repository"
	"github.com/sirupsen/logrus"
	"gorm.io/gorm"
)

type MetadataFetcher interface {
	FetchAuthFiles(ctx context.Context) (*cpa.AuthFilesResult, error)
	FetchGeminiAPIKeys(ctx context.Context) (*cpa.ProviderKeyConfigResult, error)
	FetchClaudeAPIKeys(ctx context.Context) (*cpa.ProviderKeyConfigResult, error)
	FetchCodexAPIKeys(ctx context.Context) (*cpa.ProviderKeyConfigResult, error)
	FetchVertexAPIKeys(ctx context.Context) (*cpa.ProviderKeyConfigResult, error)
	FetchOpenAICompatibility(ctx context.Context) (*cpa.OpenAICompatibilityResult, error)
}

type CPAClientFetcher interface {
	MetadataFetcher
}

const redisInboxProcessLimit = 1000

const (
	syncMetadataOptional = false
	syncMetadataRequired = true
)

const (
	authFileRecentAPIGroupKey = "auth-files-recent"
	authFileRecentModel       = "recent-requests"
	authFileRecentEndpoint    = "auth-files/recent_requests"
	authFileRecentAuthType    = "oauth"
)

type SyncService struct {
	db              *gorm.DB
	client          CPAClientFetcher
	redisQueue      RedisQueue
	redisQueueKey   string
	metadataFetcher MetadataFetcher
	baseURL         string
	now             func() time.Time
}

type SyncResult struct {
	Status         string
	InsertedEvents int
	DedupedEvents  int
}

type RedisBatchSyncResult struct {
	Empty          bool
	Status         string
	InsertedEvents int
	DedupedEvents  int
}

type RedisInboxPullResult struct {
	Empty        bool
	Status       string
	InsertedRows int
}

func NewSyncService(db *gorm.DB, cfg config.Config) *SyncService {
	return NewSyncServiceWithOptions(db, SyncServiceOptions{
		BaseURL:       cfg.CPABaseURL,
		Client:        cpa.NewClient(cfg.CPABaseURL, cfg.CPAManagementKey, cfg.RequestTimeout),
		RedisQueue:    cpa.NewRedisQueueClient(cfg.CPABaseURL, cfg.RedisQueueAddr, cfg.CPAManagementKey, cfg.RequestTimeout, cfg.RedisQueueKey, cfg.RedisQueueBatchSize),
		RedisQueueKey: cfg.RedisQueueKey,
	})
}

type SyncServiceOptions struct {
	BaseURL         string
	Client          CPAClientFetcher
	MetadataFetcher MetadataFetcher
	RedisQueue      RedisQueue
	RedisQueueKey   string
	Now             func() time.Time
}

func NewSyncServiceWithOptions(db *gorm.DB, opts SyncServiceOptions) *SyncService {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	metadataFetcher := opts.MetadataFetcher
	if metadataFetcher == nil {
		metadataFetcher = opts.Client
	}
	return &SyncService{
		db:              db,
		client:          opts.Client,
		redisQueue:      opts.RedisQueue,
		redisQueueKey:   redisQueueKey(opts.RedisQueueKey),
		metadataFetcher: metadataFetcher,
		baseURL:         strings.TrimSpace(opts.BaseURL),
		now:             now,
	}
}

func NewSyncServiceWithClient(db *gorm.DB, baseURL string, client CPAClientFetcher) *SyncService {
	return NewSyncServiceWithOptions(db, SyncServiceOptions{
		BaseURL: baseURL,
		Client:  client,
	})
}

func (s *SyncService) SyncOnce(ctx context.Context) error {
	_, err := s.SyncNow(ctx)
	return err
}

func (s *SyncService) SyncNow(ctx context.Context) (*SyncResult, error) {
	if _, err := s.PullRedisUsageInbox(ctx); err != nil {
		return nil, err
	}
	result, err := s.ProcessRedisUsageInbox(ctx)
	return syncResultFromRedisBatch(result), err
}

func syncResultFromRedisBatch(result *RedisBatchSyncResult) *SyncResult {
	if result == nil {
		return nil
	}
	return &SyncResult{
		Status:         result.Status,
		InsertedEvents: result.InsertedEvents,
		DedupedEvents:  result.DedupedEvents,
	}
}

func (s *SyncService) SyncStatus(ctx context.Context) (string, error) {
	result, err := s.SyncNow(ctx)
	if result == nil {
		return "", err
	}
	return result.Status, err
}

func (s *SyncService) SyncMetadata(ctx context.Context) error {
	if err := s.validate(syncMetadataRequired); err != nil {
		return err
	}
	logrus.Debug("metadata sync started")
	fetchedAt := s.now().UTC()
	authFilesResult, authFilesErr := s.metadataFetcher.FetchAuthFiles(ctx)
	providerConfig, fetchedProviderTypes, providerMetadataErr := fetchProviderMetadata(ctx, s.metadataFetcher)
	authSyncErr := syncAuthFiles(ctx, s.db, authFilesResult, authFilesErr, fetchedAt)
	providerSyncErr, providerWarningErr := syncProviderMetadata(ctx, s.db, providerConfig, fetchedProviderTypes, providerMetadataErr, fetchedAt)
	upsertErr := joinErrors(authSyncErr, providerSyncErr)
	var aggregateErr error
	if upsertErr == nil {
		aggregateErr = repository.AggregateUsageIdentityStats(ctx, s.db, fetchedAt)
		if aggregateErr != nil {
			aggregateErr = fmt.Errorf("aggregate usage identity stats: %w", aggregateErr)
		}
	}
	err := joinErrors(upsertErr, aggregateErr, providerWarningErr)
	fields := logrus.Fields{
		"status": "completed",
	}
	if err != nil {
		fields["status"] = "completed_with_warnings"
		fields["error"] = err.Error()
	}
	logrus.WithFields(fields).Debug("metadata sync finished")
	return err
}

// PullRedisUsageInbox 是 Redis 同步的拉取阶段：只 LPOP 队列消息并原样写入 redis_usage_inboxes。
// 这个阶段不解码消息、不写 usage_events，保证 Redis 消费和本地处理职责分离。
func (s *SyncService) PullRedisUsageInbox(ctx context.Context) (*RedisInboxPullResult, error) {
	if err := s.validate(syncMetadataOptional); err != nil {
		return nil, err
	}
	if s.redisQueue == nil {
		return nil, fmt.Errorf("sync service redis queue is nil")
	}

	fetchedAt := s.now().UTC()
	messages, err := s.redisQueue.PopUsage(ctx)
	if err != nil {
		return &RedisInboxPullResult{Status: "failed"}, fmt.Errorf("fetch redis usage: %w", err)
	}
	logrus.WithFields(logrus.Fields{
		"queue_key":     s.redisQueueKey,
		"message_count": len(messages),
	}).Debug("redis usage batch popped")
	if len(messages) == 0 {
		return &RedisInboxPullResult{Empty: true, Status: "empty"}, nil
	}

	inboxRows, err := insertRedisInboxMessages(s.db, s.redisQueueKey, messages, fetchedAt)
	if err != nil {
		return &RedisInboxPullResult{Status: "failed"}, fmt.Errorf("insert redis usage inbox: %w", err)
	}
	logrus.WithFields(logrus.Fields{
		"queue_key": s.redisQueueKey,
		"row_count": len(inboxRows),
	}).Debug("redis usage inbox rows inserted")
	return &RedisInboxPullResult{Status: "completed", InsertedRows: len(inboxRows)}, nil
}

// ProcessRedisUsageInbox 是 Redis 同步的本地处理阶段：只读取 pending/process_failed inbox 行并写入 usage_events。
// 成功处理后仅用 usage_event_key 记录 inbox 与最终事件的关联。
func (s *SyncService) ProcessRedisUsageInbox(ctx context.Context) (*RedisBatchSyncResult, error) {
	if err := s.validate(syncMetadataOptional); err != nil {
		return nil, err
	}
	fetchedAt := s.now().UTC()
	processableRows, err := repository.ListProcessableRedisUsageInbox(s.db, redisInboxProcessLimit)
	if err != nil {
		return &RedisBatchSyncResult{Status: "failed"}, fmt.Errorf("list processable redis usage inbox: %w", err)
	}
	if len(processableRows) == 0 {
		return &RedisBatchSyncResult{Empty: true, Status: "empty"}, nil
	}
	logrus.WithField("row_count", len(processableRows)).Debug("redis usage inbox rows found for processing")
	return s.processRedisInboxRows(processableRows, fetchedAt)
}

// CleanupRedisUsageInbox 只清理 Redis inbox 表，供测试和单独维护入口使用；每日任务使用 CleanupStorage 统一执行。
func (s *SyncService) CleanupRedisUsageInbox(ctx context.Context) error {
	if err := s.validate(syncMetadataOptional); err != nil {
		return err
	}
	_, err := repository.CleanupRedisUsageInbox(s.db, s.now())
	return err
}

// CleanupStorage 是每日 03:00 维护任务调用的统一入口：先清 Redis inbox，最后 VACUUM 收缩 SQLite。
func (s *SyncService) CleanupStorage(ctx context.Context) error {
	if err := s.validate(syncMetadataOptional); err != nil {
		return err
	}
	_, err := repository.CleanupStorage(s.db, s.now())
	return err
}

// SyncRedisBatch 保留为兼容入口：先处理本地存量 inbox，空了再拉一次 Redis 并立即处理。
// 后台任务不要调用它，后台必须使用拆分后的 PullRedisUsageInbox、ProcessRedisUsageInbox 和 CleanupStorage。
func (s *SyncService) SyncRedisBatch(ctx context.Context) (*RedisBatchSyncResult, error) {
	if result, err := s.ProcessRedisUsageInbox(ctx); err != nil || result == nil || !result.Empty {
		return result, err
	}
	if _, err := s.PullRedisUsageInbox(ctx); err != nil {
		return &RedisBatchSyncResult{Status: "failed"}, err
	}
	return s.ProcessRedisUsageInbox(ctx)
}

// processRedisInboxRows 只从已落库的原始消息解码和写入事件，坏消息会标记为 decode_failed，不阻塞同批其它数据。
// 可解码但入库失败的消息标记为 process_failed，后续 ProcessRedisUsageInbox 会按 id 顺序重试。
func (s *SyncService) processRedisInboxRows(inboxRows []models.RedisUsageInbox, fetchedAt time.Time) (*RedisBatchSyncResult, error) {
	logrus.WithField("row_count", len(inboxRows)).Debug("redis usage inbox processing started")
	validRows := make([]models.RedisUsageInbox, 0, len(inboxRows))
	events := make([]models.UsageEvent, 0, len(inboxRows))
	decodeErrs := make([]error, 0)
	for _, row := range inboxRows {
		event, _, decodeErr := DecodeRedisUsageMessage(row.RawMessage, fetchedAt)
		if decodeErr != nil {
			if markErr := repository.MarkRedisUsageInboxDecodeFailed(s.db, row.ID, decodeErr); markErr != nil {
				return &RedisBatchSyncResult{Status: "failed"}, fmt.Errorf("mark redis usage inbox decode failed: %w", markErr)
			}
			decodeErrs = append(decodeErrs, decodeErr)
			continue
		}
		validRows = append(validRows, row)
		events = append(events, event)
	}
	decodeErr := joinErrors(decodeErrs...)
	logrus.WithFields(logrus.Fields{
		"row_count":           len(inboxRows),
		"valid_event_count":   len(events),
		"decode_failed_count": len(decodeErrs),
	}).Debug("redis usage inbox rows decoded")
	if len(events) == 0 {
		if decodeErr != nil {
			return &RedisBatchSyncResult{Status: "completed_with_warnings"}, decodeErr
		}
		return &RedisBatchSyncResult{Empty: true, Status: "empty"}, nil
	}

	logrus.WithField("event_count", len(events)).Debug("redis usage events persistence started")
	result, err := s.persistRedisUsageEvents(events)
	if result == nil {
		markRedisInboxRowsProcessFailed(s.db, validRows, err)
		return nil, err
	}
	if err != nil && result.Status == "failed" {
		markRedisInboxRowsProcessFailed(s.db, validRows, err)
		return &RedisBatchSyncResult{Status: result.Status}, err
	}
	for i, row := range validRows {
		if markErr := repository.MarkRedisUsageInboxProcessed(s.db, row.ID, events[i].EventKey, fetchedAt); markErr != nil {
			return &RedisBatchSyncResult{Status: "failed"}, fmt.Errorf("mark redis usage inbox processed: %w", markErr)
		}
	}
	logrus.WithFields(logrus.Fields{
		"processed_rows":  len(validRows),
		"inserted_events": result.InsertedEvents,
		"deduped_events":  result.DedupedEvents,
		"status":          result.Status,
	}).Debug("redis usage inbox rows processed")

	status := result.Status
	returnErr := err
	if decodeErr != nil {
		status = "completed_with_warnings"
		if returnErr != nil {
			returnErr = joinErrors(returnErr, decodeErr)
		} else {
			returnErr = decodeErr
		}
	}
	return &RedisBatchSyncResult{
		Status:         status,
		InsertedEvents: result.InsertedEvents,
		DedupedEvents:  result.DedupedEvents,
	}, returnErr
}

// persistRedisUsageEvents 写入 Redis inbox 解码出的 usage_events。
func (s *SyncService) persistRedisUsageEvents(events []models.UsageEvent) (*SyncResult, error) {
	var err error
	events, err = alignUsageEventKeysWithExistingCanonicalEvents(s.db, events)
	if err != nil {
		return &SyncResult{Status: "failed"}, fmt.Errorf("align usage events: %w", err)
	}
	logrus.WithField("event_count", len(events)).Debug("usage events insert started")
	inserted, deduped, err := repository.InsertUsageEvents(s.db, events)
	if err != nil {
		return &SyncResult{Status: "failed"}, fmt.Errorf("insert usage events: %w", err)
	}
	logrus.WithFields(logrus.Fields{
		"inserted_events": inserted,
		"deduped_events":  deduped,
	}).Debug("usage events insert finished")
	return &SyncResult{Status: "completed", InsertedEvents: inserted, DedupedEvents: deduped}, nil
}

func alignUsageEventKeysWithExistingCanonicalEvents(db *gorm.DB, events []models.UsageEvent) ([]models.UsageEvent, error) {
	if len(events) == 0 {
		return events, nil
	}
	canonicalEventKeys := make(map[string]string, len(events))
	consumedCanonicalKeys := make(map[string]struct{}, len(events))
	for i := range events {
		events[i].Timestamp = events[i].Timestamp.UTC()
		canonicalKey := canonicalUsageEventKey(events[i])
		incomingKey := strings.TrimSpace(events[i].EventKey)
		if strings.TrimSpace(events[i].RequestID) != "" {
			canonicalEventKeys[canonicalKey] = incomingKey
			continue
		}
		if existingKey := canonicalEventKeys[canonicalKey]; existingKey != "" {
			if incomingKey == canonicalKey {
				events[i].EventKey = existingKey
			} else if existingKey == canonicalKey {
				if _, consumed := consumedCanonicalKeys[canonicalKey]; !consumed {
					events[i].EventKey = existingKey
					consumedCanonicalKeys[canonicalKey] = struct{}{}
				}
			}
			continue
		}

		var existing models.UsageEvent
		result := db.Select("event_key").Where(
			"TRIM(api_group_key) = ? AND TRIM(model) = ? AND timestamp = ? AND TRIM(source) = ? AND TRIM(auth_index) = ? AND failed = ? AND input_tokens = ? AND output_tokens = ? AND reasoning_tokens = ? AND cached_tokens = ? AND total_tokens = ?",
			strings.TrimSpace(events[i].APIGroupKey),
			strings.TrimSpace(events[i].Model),
			events[i].Timestamp,
			strings.TrimSpace(events[i].Source),
			strings.TrimSpace(events[i].AuthIndex),
			events[i].Failed,
			events[i].InputTokens,
			events[i].OutputTokens,
			events[i].ReasoningTokens,
			events[i].CachedTokens,
			events[i].TotalTokens,
		).Order("id ASC").Limit(1).Find(&existing)
		if result.Error != nil {
			return nil, fmt.Errorf("find equivalent usage event: %w", result.Error)
		}
		if result.RowsAffected == 0 {
			canonicalEventKeys[canonicalKey] = incomingKey
			continue
		}
		existingKey := strings.TrimSpace(existing.EventKey)
		if existingKey != "" {
			if incomingKey == canonicalKey {
				events[i].EventKey = existingKey
			} else if existingKey == canonicalKey {
				alreadyConsumed, err := redisInboxAlreadyReferencesEventKey(db, canonicalKey)
				if err != nil {
					return nil, err
				}
				if !alreadyConsumed {
					events[i].EventKey = existingKey
					consumedCanonicalKeys[canonicalKey] = struct{}{}
				}
			}
			canonicalEventKeys[canonicalKey] = existingKey
		} else {
			canonicalEventKeys[canonicalKey] = incomingKey
		}
	}
	return events, nil
}

func redisInboxAlreadyReferencesEventKey(db *gorm.DB, eventKey string) (bool, error) {
	var count int64
	if err := db.Model(&models.RedisUsageInbox{}).Where("status = ? AND usage_event_key = ?", repository.RedisUsageInboxStatusProcessed, eventKey).Count(&count).Error; err != nil {
		return false, fmt.Errorf("count redis inbox references: %w", err)
	}
	return count > 0, nil
}

func canonicalUsageEventKey(event models.UsageEvent) string {
	return BuildEventKey(
		event.APIGroupKey,
		event.Model,
		event.Timestamp,
		event.Source,
		event.AuthIndex,
		event.Failed,
		cpa.TokenStats{
			InputTokens:     event.InputTokens,
			OutputTokens:    event.OutputTokens,
			ReasoningTokens: event.ReasoningTokens,
			CachedTokens:    event.CachedTokens,
			TotalTokens:     event.TotalTokens,
		},
	)
}

func (s *SyncService) validate(syncMetadata bool) error {
	if s == nil {
		return fmt.Errorf("sync service is nil")
	}
	if s.db == nil {
		return fmt.Errorf("sync service database is nil")
	}
	if syncMetadata {
		if s.metadataFetcher == nil && s.client != nil {
			s.metadataFetcher = s.client
		}
		if s.metadataFetcher == nil {
			return fmt.Errorf("sync service metadata fetcher is nil")
		}
	}
	return nil
}

// insertRedisInboxMessages 在解码前先把 Redis 原始消息落库，降低 LPOP 后本地处理失败导致的数据丢失风险。
func insertRedisInboxMessages(db *gorm.DB, queueKey string, messages []string, poppedAt time.Time) ([]models.RedisUsageInbox, error) {
	inputs := make([]repository.RedisInboxInsert, 0, len(messages))
	for _, message := range messages {
		inputs = append(inputs, repository.RedisInboxInsert{
			QueueKey:   queueKey,
			RawMessage: message,
			PoppedAt:   poppedAt,
		})
	}
	return repository.InsertRedisUsageInboxMessages(db, inputs)
}

func markRedisInboxRowsProcessFailed(db *gorm.DB, rows []models.RedisUsageInbox, err error) {
	if err == nil {
		return
	}
	for _, row := range rows {
		if markErr := repository.MarkRedisUsageInboxProcessFailed(db, row.ID, err); markErr != nil {
			logrus.WithError(markErr).WithField("inbox_id", row.ID).Warn("failed to mark redis usage inbox process failure")
			continue
		}
		var stored models.RedisUsageInbox
		if loadErr := db.First(&stored, row.ID).Error; loadErr != nil {
			logrus.WithError(loadErr).WithField("inbox_id", row.ID).Warn("failed to load redis usage inbox after process failure")
			continue
		}
		if stored.Status == repository.RedisUsageInboxStatusDiscarded {
			logrus.WithFields(logrus.Fields{
				"inbox_id":      stored.ID,
				"queue_key":     stored.QueueKey,
				"message_hash":  stored.MessageHash,
				"attempt_count": stored.AttemptCount,
				"last_error":    stored.LastError,
				"popped_at":     stored.PoppedAt,
			}).Warn("discarded redis usage inbox row after repeated process failures")
		}
	}
}

func redisQueueKey(value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return cpa.ManagementUsageQueueKey
	}
	return trimmed
}

func errorMessage(err error) string {
	if err == nil {
		return ""
	}
	return strings.TrimSpace(err.Error())
}

func syncAuthFiles(ctx context.Context, db *gorm.DB, result *cpa.AuthFilesResult, fetchErr error, now time.Time) error {
	if fetchErr != nil {
		return fmt.Errorf("fetch auth files: %w", fetchErr)
	}
	if db == nil {
		return fmt.Errorf("database is nil")
	}
	if result == nil {
		return fmt.Errorf("fetch auth files: empty response")
	}

	identities := make([]models.UsageIdentity, 0, len(result.Payload.Files))
	for _, file := range result.Payload.Files {
		identities = append(identities, models.UsageIdentity{
			Name:         firstNonEmpty(file.Email, file.Label, file.Name, file.AuthIndex),
			AuthType:     models.UsageIdentityAuthTypeAuthFile,
			AuthTypeName: "oauth",
			Identity:     file.AuthIndex,
			Type:         file.Type,
			Provider:     file.Provider,
		})
	}
	if err := repository.ReplaceUsageIdentitiesForAuthType(ctx, db, identities, models.UsageIdentityAuthTypeAuthFile, now); err != nil {
		return fmt.Errorf("sync auth file usage identities: %w", err)
	}
	if err := syncAuthFileRecentUsageEvents(ctx, db, result.Payload.Files, now); err != nil {
		return fmt.Errorf("sync auth file recent usage events: %w", err)
	}
	return nil
}

func syncAuthFileRecentUsageEvents(ctx context.Context, db *gorm.DB, files []cpa.AuthFile, now time.Time) error {
	if db == nil {
		return fmt.Errorf("database is nil")
	}
	if len(files) == 0 {
		return nil
	}

	location := time.Local
	if location == nil {
		location = time.UTC
	}
	nowLocal := now.In(location)
	events := make([]models.UsageEvent, 0)
	for _, file := range files {
		for _, bucket := range file.RecentRequests {
			startLocal, endLocal, ok := parseAuthFileRecentRequestBucket(bucket.Time, nowLocal, location)
			if !ok {
				continue
			}
			startUTC := startLocal.UTC()
			endUTC := endLocal.UTC()
			if bucket.Success > 0 {
				generated, err := authFileRecentSyntheticEvents(ctx, db, file, startUTC, endUTC, false, bucket.Success)
				if err != nil {
					return err
				}
				events = append(events, generated...)
			}
			if bucket.Failed > 0 {
				generated, err := authFileRecentSyntheticEvents(ctx, db, file, startUTC, endUTC, true, bucket.Failed)
				if err != nil {
					return err
				}
				events = append(events, generated...)
			}
		}
	}
	if len(events) == 0 {
		return nil
	}
	inserted, deduped, err := repository.InsertUsageEvents(db, events)
	if err != nil {
		return err
	}
	logrus.WithFields(logrus.Fields{
		"inserted_events": inserted,
		"deduped_events":  deduped,
	}).Debug("auth file recent usage fallback events synced")
	return nil
}

func authFileRecentSyntheticEvents(ctx context.Context, db *gorm.DB, file cpa.AuthFile, bucketStart, bucketEnd time.Time, failed bool, reported int64) ([]models.UsageEvent, error) {
	if reported <= 0 {
		return nil, nil
	}
	realCount, err := countAuthFileRecentExistingEvents(ctx, db, file, bucketStart, bucketEnd, failed, false)
	if err != nil {
		return nil, err
	}
	syntheticCount, err := countAuthFileRecentExistingEvents(ctx, db, file, bucketStart, bucketEnd, failed, true)
	if err != nil {
		return nil, err
	}
	missing := reported - realCount - syntheticCount
	if missing <= 0 {
		return nil, nil
	}

	duration := bucketEnd.Sub(bucketStart)
	if duration <= 0 {
		duration = 10 * time.Minute
	}
	source := firstNonEmpty(file.Email, file.Label, file.Name, file.AuthIndex)
	authIndex := strings.TrimSpace(file.AuthIndex)
	prefix := authFileRecentEventKeyPrefix(file, bucketStart, failed)
	events := make([]models.UsageEvent, 0, missing)
	for i := int64(0); i < missing; i++ {
		ordinal := syntheticCount + i
		offset := time.Duration((ordinal + 1) * duration.Nanoseconds() / (reported + 1))
		events = append(events, models.UsageEvent{
			EventKey:    fmt.Sprintf("%s%d", prefix, ordinal),
			APIGroupKey: authFileRecentAPIGroupKey,
			Provider:    strings.TrimSpace(file.Provider),
			Endpoint:    authFileRecentEndpoint,
			AuthType:    authFileRecentAuthType,
			Model:       authFileRecentModel,
			Timestamp:   bucketStart.Add(offset).UTC(),
			Source:      source,
			AuthIndex:   authIndex,
			Failed:      failed,
		})
	}
	return events, nil
}

func countAuthFileRecentExistingEvents(ctx context.Context, db *gorm.DB, file cpa.AuthFile, bucketStart, bucketEnd time.Time, failed bool, synthetic bool) (int64, error) {
	if db == nil {
		return 0, fmt.Errorf("database is nil")
	}
	var count int64
	query := db.WithContext(ctx).Model(&models.UsageEvent{}).
		Where("timestamp >= ? AND timestamp < ? AND failed = ?", bucketStart.UTC(), bucketEnd.UTC(), failed)
	if synthetic {
		query = query.Where("api_group_key = ? AND event_key LIKE ?", authFileRecentAPIGroupKey, authFileRecentEventKeyPrefix(file, bucketStart.UTC(), failed)+"%")
	} else {
		query = query.Where("api_group_key <> ?", authFileRecentAPIGroupKey)
		authIndex := strings.TrimSpace(file.AuthIndex)
		if authIndex != "" {
			query = query.Where("auth_index = ?", authIndex)
		} else if source := firstNonEmpty(file.Email, file.Label, file.Name); source != "" {
			query = query.Where("source = ?", source)
		}
	}
	if err := query.Count(&count).Error; err != nil {
		return 0, fmt.Errorf("count auth file recent usage events: %w", err)
	}
	return count, nil
}

func authFileRecentEventKeyPrefix(file cpa.AuthFile, bucketStart time.Time, failed bool) string {
	stableIdentity := firstNonEmpty(file.AuthIndex, file.Email, file.Label, file.Name, "unknown")
	sum := sha256.Sum256([]byte(stableIdentity))
	return fmt.Sprintf("auth-files-recent:%s:%s:%t:", hex.EncodeToString(sum[:8]), bucketStart.UTC().Format("20060102T150405Z"), failed)
}

func parseAuthFileRecentRequestBucket(value string, nowLocal time.Time, location *time.Location) (time.Time, time.Time, bool) {
	parts := strings.Split(strings.TrimSpace(value), "-")
	if len(parts) != 2 {
		return time.Time{}, time.Time{}, false
	}
	startHour, startMinute, ok := parseHourMinute(parts[0])
	if !ok {
		return time.Time{}, time.Time{}, false
	}
	endHour, endMinute, ok := parseHourMinute(parts[1])
	if !ok {
		return time.Time{}, time.Time{}, false
	}
	if location == nil {
		location = time.UTC
	}
	start := time.Date(nowLocal.Year(), nowLocal.Month(), nowLocal.Day(), startHour, startMinute, 0, 0, location)
	end := time.Date(nowLocal.Year(), nowLocal.Month(), nowLocal.Day(), endHour, endMinute, 0, 0, location)
	if !end.After(start) {
		end = end.Add(24 * time.Hour)
	}
	if start.After(nowLocal.Add(time.Minute)) {
		start = start.Add(-24 * time.Hour)
		end = end.Add(-24 * time.Hour)
	}
	return start, end, true
}

func parseHourMinute(value string) (int, int, bool) {
	parts := strings.Split(strings.TrimSpace(value), ":")
	if len(parts) != 2 {
		return 0, 0, false
	}
	hour, err := strconv.Atoi(parts[0])
	if err != nil || hour < 0 || hour > 23 {
		return 0, 0, false
	}
	minute, err := strconv.Atoi(parts[1])
	if err != nil || minute < 0 || minute > 59 {
		return 0, 0, false
	}
	return hour, minute, true
}

func fetchProviderMetadata(ctx context.Context, fetcher MetadataFetcher) (cpa.ProviderMetadataConfig, []string, error) {
	var cfg cpa.ProviderMetadataConfig
	var fetchedProviderTypes []string
	var errs []error

	if result, err := fetcher.FetchGeminiAPIKeys(ctx); err != nil {
		errs = append(errs, fmt.Errorf("fetch gemini api keys: %w", err))
	} else if result == nil {
		errs = append(errs, fmt.Errorf("gemini api keys response is nil"))
	} else {
		fetchedProviderTypes = append(fetchedProviderTypes, "gemini")
		cfg.GeminiAPIKeys = result.Payload
	}
	if result, err := fetcher.FetchClaudeAPIKeys(ctx); err != nil {
		errs = append(errs, fmt.Errorf("fetch claude api keys: %w", err))
	} else if result == nil {
		errs = append(errs, fmt.Errorf("claude api keys response is nil"))
	} else {
		fetchedProviderTypes = append(fetchedProviderTypes, "claude")
		cfg.ClaudeAPIKeys = result.Payload
	}
	if result, err := fetcher.FetchCodexAPIKeys(ctx); err != nil {
		errs = append(errs, fmt.Errorf("fetch codex api keys: %w", err))
	} else if result == nil {
		errs = append(errs, fmt.Errorf("codex api keys response is nil"))
	} else {
		fetchedProviderTypes = append(fetchedProviderTypes, "codex")
		cfg.CodexAPIKeys = result.Payload
	}
	if result, err := fetcher.FetchVertexAPIKeys(ctx); err != nil {
		errs = append(errs, fmt.Errorf("fetch vertex api keys: %w", err))
	} else if result == nil {
		errs = append(errs, fmt.Errorf("vertex api keys response is nil"))
	} else {
		fetchedProviderTypes = append(fetchedProviderTypes, "vertex")
		cfg.VertexAPIKeys = result.Payload
	}
	if result, err := fetcher.FetchOpenAICompatibility(ctx); err != nil {
		errs = append(errs, fmt.Errorf("fetch openai compatibility: %w", err))
	} else if result == nil {
		errs = append(errs, fmt.Errorf("openai compatibility response is nil"))
	} else {
		fetchedProviderTypes = append(fetchedProviderTypes, "openai")
		cfg.OpenAICompatibility = result.Payload
	}

	return cfg, fetchedProviderTypes, joinErrors(errs...)
}

func syncProviderMetadata(ctx context.Context, db *gorm.DB, cfg cpa.ProviderMetadataConfig, fetchedProviderTypes []string, fetchErr error, now time.Time) (error, error) {
	if db == nil {
		return fmt.Errorf("database is nil"), nil
	}

	inputs := flattenProviderMetadata(cfg)
	identities := providerMetadataUsageIdentities(inputs)
	if err := repository.ReplaceUsageIdentitiesForProviderTypes(ctx, db, identities, fetchedProviderTypes, now); err != nil {
		return fmt.Errorf("sync provider usage identities: %w", err), nil
	}
	if fetchErr != nil {
		return nil, fmt.Errorf("fetch provider metadata: %w", fetchErr)
	}
	return nil, nil
}

type providerMetadataInput struct {
	LookupKey    string
	ProviderType string
	DisplayName  string
}

func providerMetadataUsageIdentities(inputs []providerMetadataInput) []models.UsageIdentity {
	identities := make([]models.UsageIdentity, 0, len(inputs))
	for _, input := range inputs {
		identities = append(identities, models.UsageIdentity{
			Name:         input.DisplayName,
			AuthType:     models.UsageIdentityAuthTypeAIProvider,
			AuthTypeName: "apikey",
			Identity:     input.LookupKey,
			Type:         input.ProviderType,
			Provider:     input.DisplayName,
		})
	}
	return identities
}

func flattenProviderMetadata(cfg cpa.ProviderMetadataConfig) []providerMetadataInput {
	items := make([]providerMetadataInput, 0)
	seen := make(map[string]struct{})
	appendItem := func(lookupKey, providerType, displayName string) {
		lookupKey = strings.TrimSpace(lookupKey)
		providerType = strings.TrimSpace(providerType)
		displayName = strings.TrimSpace(displayName)
		if lookupKey == "" || providerType == "" || displayName == "" {
			return
		}
		if _, ok := seen[lookupKey]; ok {
			return
		}
		seen[lookupKey] = struct{}{}
		items = append(items, providerMetadataInput{
			LookupKey:    lookupKey,
			ProviderType: providerType,
			DisplayName:  displayName,
		})
	}
	appendProviderEntries := func(providerType string, configs []cpa.ProviderKeyConfig) {
		for _, cfg := range configs {
			displayName := firstNonEmpty(cfg.Name, providerType)
			appendItem(cfg.APIKey, providerType, displayName)
		}
	}

	appendProviderEntries("gemini", cfg.GeminiAPIKeys)
	appendProviderEntries("claude", cfg.ClaudeAPIKeys)
	appendProviderEntries("codex", cfg.CodexAPIKeys)
	appendProviderEntries("vertex", cfg.VertexAPIKeys)

	for _, provider := range cfg.OpenAICompatibility {
		displayName := firstNonEmpty(provider.Name, "openai")
		for _, entry := range provider.APIKeyEntries {
			appendItem(entry.APIKey, "openai", displayName)
		}
	}

	return items
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		trimmed := strings.TrimSpace(value)
		if trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func joinErrors(errs ...error) error {
	messages := make([]string, 0, len(errs))
	for _, err := range errs {
		if err == nil {
			continue
		}
		messages = append(messages, strings.TrimSpace(err.Error()))
	}
	if len(messages) == 0 {
		return nil
	}
	return fmt.Errorf("%s", strings.Join(messages, "; "))
}
