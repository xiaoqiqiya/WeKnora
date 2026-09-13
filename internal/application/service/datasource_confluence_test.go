package service

import (
	"context"
	"encoding/json"
	"errors"
	"mime/multipart"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

type confluenceTestRepo struct {
	interfaces.KnowledgeRepository
	rows       map[string]*types.Knowledge
	promoteErr error
}

func (r *confluenceTestRepo) FindByDataSourceExternalID(_ context.Context, tenant uint64, kb, ds, external string) (*types.Knowledge, error) {
	for _, k := range r.rows {
		if k.TenantID == tenant && k.KnowledgeBaseID == kb && k.GetMetadata()["datasource_id"] == ds && k.GetMetadata()["external_id"] == external {
			return k, nil
		}
	}
	return nil, nil
}
func (r *confluenceTestRepo) HardDeleteKnowledge(_ context.Context, _ uint64, id string) error {
	delete(r.rows, id)
	return nil
}
func (r *confluenceTestRepo) UpdateKnowledgeColumns(_ context.Context, id string, values map[string]interface{}) error {
	if r.promoteErr != nil {
		return r.promoteErr
	}
	if meta, ok := values["metadata"]; ok {
		r.rows[id].Metadata = meta.(types.JSON)
	}
	if enabled, ok := values["enable_status"]; ok {
		r.rows[id].EnableStatus = enabled.(string)
	}
	return nil
}

type confluenceTestKnowledgeService struct {
	interfaces.KnowledgeService
	repo      *confluenceTestRepo
	created   int
	createErr error
	deleted   []string
}

func (s *confluenceTestKnowledgeService) GetRepository() interfaces.KnowledgeRepository {
	return s.repo
}
func (s *confluenceTestKnowledgeService) CreateKnowledgeFromFile(_ context.Context, kb string, _ *multipart.FileHeader, metadata map[string]string, _ *bool, _ string, _ []string, _ string, _ *types.KnowledgeProcessOverrides) (*types.Knowledge, error) {
	if s.createErr != nil {
		return nil, s.createErr
	}
	s.created++
	id := "candidate"
	blob, _ := json.Marshal(metadata)
	k := &types.Knowledge{ID: id, TenantID: 7, KnowledgeBaseID: kb, Metadata: types.JSON(blob), ParseStatus: types.ParseStatusProcessing, EnableStatus: "disabled"}
	s.repo.rows[id] = k
	return k, nil
}
func (s *confluenceTestKnowledgeService) DeleteKnowledge(_ context.Context, id string) error {
	s.deleted = append(s.deleted, id)
	return nil
}

func confluenceTestSetup() (*DataSourceService, *confluenceTestKnowledgeService, *types.DataSource, *types.FetchedItem) {
	meta, _ := json.Marshal(map[string]string{"datasource_id": "source", "external_id": "12", "source_revision": "revision-1", "source_site": "https://confluence.example.com"})
	repo := &confluenceTestRepo{rows: map[string]*types.Knowledge{"old": {ID: "old", TenantID: 7, KnowledgeBaseID: "kb", Metadata: types.JSON(meta), ParseStatus: types.ParseStatusCompleted, EnableStatus: "enabled"}}}
	ks := &confluenceTestKnowledgeService{repo: repo}
	s := &DataSourceService{knowledgeService: ks}
	ds := &types.DataSource{ID: "source", TenantID: 7, KnowledgeBaseID: "kb", Type: types.ConnectorTypeConfluence, ConflictStrategy: types.ConflictStrategyOverwrite}
	item := &types.FetchedItem{ExternalID: "12", Title: "Guide", Content: []byte("# Guide\nupdated"), FileName: "guide.md", Metadata: map[string]string{"source_revision": "revision-2", "source_site": "https://confluence.example.com"}}
	return s, ks, ds, item
}

func TestConfluenceReplacementKeepsOldUntilCompleted(t *testing.T) {
	s, ks, ds, item := confluenceTestSetup()
	ctx := context.Background()
	if update, err := s.ingestItem(ctx, ds, item, nil); err != nil || !update {
		t.Fatalf("stage: update=%v err=%v", update, err)
	}
	if len(ks.deleted) != 0 || ks.repo.rows["old"] == nil {
		t.Fatal("deleted old copy before parsing")
	}
	h := &streamSyncHandler{svc: s, ds: ds}
	if committed, err := h.ItemCommitted(ctx, item); err != nil || committed {
		t.Fatal("submitted revision marked committed")
	}
	_, err := s.ingestItem(ctx, ds, item, nil)
	var duplicate *types.DuplicateKnowledgeError
	if !errors.As(err, &duplicate) || ks.created != 1 {
		t.Fatal("pending candidate was duplicated")
	}
	ks.repo.rows["candidate"].ParseStatus = types.ParseStatusFinalizing
	ks.repo.rows["candidate"].EnableStatus = "enabled"
	_, _ = s.ingestItem(ctx, ds, item, nil)
	if len(ks.deleted) != 0 {
		t.Fatal("finalizing is not completed")
	}
	ks.repo.rows["candidate"].ParseStatus = types.ParseStatusCompleted
	if update, err := s.ingestItem(ctx, ds, item, nil); err != nil || !update {
		t.Fatalf("promote: %v", err)
	}
	if ks.repo.rows["old"] != nil || ks.repo.rows["candidate"].GetMetadata()["external_id"] != "12" {
		t.Fatal("replacement not promoted")
	}
	if committed, err := h.ItemCommitted(ctx, item); err != nil || !committed {
		t.Fatal("completed replacement not committed")
	}
}

func TestConfluenceFetchOrParseFailurePreservesOldAndRetries(t *testing.T) {
	s, ks, ds, item := confluenceTestSetup()
	ctx := context.Background()
	ks.createErr = errors.New("upload unavailable")
	_, err := s.ingestItem(ctx, ds, item, nil)
	if err == nil || len(ks.deleted) != 0 || ks.repo.rows["old"] == nil {
		t.Fatal("upload failure lost old copy")
	}
	ks.createErr = nil
	_, _ = s.ingestItem(ctx, ds, item, nil)
	ks.repo.rows["candidate"].ParseStatus = types.ParseStatusFailed
	if _, err := s.ingestItem(ctx, ds, item, nil); err != nil {
		t.Fatal(err)
	}
	if ks.repo.rows["old"] == nil || ks.created != 2 || len(ks.deleted) != 1 || ks.deleted[0] != "candidate" {
		t.Fatal("failed parse did not retry safely")
	}
}

func TestConfluencePromotionRecoversMetadataFailure(t *testing.T) {
	s, ks, ds, item := confluenceTestSetup()
	ctx := context.Background()
	_, _ = s.ingestItem(ctx, ds, item, nil)
	ks.repo.rows["candidate"].ParseStatus = types.ParseStatusCompleted
	ks.repo.rows["candidate"].EnableStatus = "enabled"
	ks.repo.promoteErr = errors.New("metadata write unavailable")
	if _, err := s.ingestItem(ctx, ds, item, nil); err == nil {
		t.Fatal("expected metadata error")
	}
	if ks.repo.rows["candidate"] == nil || !confluenceReady(ks.repo.rows["candidate"]) {
		t.Fatal("lost ready replacement")
	}
	ks.repo.promoteErr = nil
	if _, err := s.ingestItem(ctx, ds, item, nil); err != nil {
		t.Fatal(err)
	}
	if ks.created != 1 || ks.repo.rows["candidate"].GetMetadata()["external_id"] != "12" {
		t.Fatal("interrupted promotion did not recover")
	}
}

func TestConfluenceStreamFailureStopsCheckpointAndSkipHonored(t *testing.T) {
	s, ks, ds, item := confluenceTestSetup()
	ctx := context.Background()
	ks.createErr = errors.New("index unavailable")
	h := &streamSyncHandler{svc: s, ds: ds, result: &types.SyncResult{}}
	if err := h.Emit(ctx, *item); err == nil || h.result.Failed != 1 {
		t.Fatal("ingest failure did not stop stream")
	}
	ks.createErr = nil
	ds.ConflictStrategy = types.ConflictStrategySkip
	_, err := s.ingestItem(ctx, ds, item, nil)
	var duplicate *types.DuplicateKnowledgeError
	if !errors.As(err, &duplicate) || ks.created != 0 {
		t.Fatal("skip strategy overwrote existing copy")
	}
	if committed, err := h.ItemCommitted(ctx, item); err != nil || !committed {
		t.Fatal("skip strategy did not converge")
	}
	wrong := *ks.repo.rows["old"]
	wrong.KnowledgeBaseID = "other-kb"
	if err := s.removeConfluenceCopy(ctx, ds, &wrong); err == nil {
		t.Fatal("ownership boundary not enforced")
	}
}

func TestConfluenceReplacementIndexesRemainHiddenUntilPromotion(t *testing.T) {
	meta, _ := json.Marshal(map[string]string{"replacement_for": "old"})
	k := &types.Knowledge{Channel: types.ChannelConfluence, Metadata: types.JSON(meta)}
	finalizeIndexedKnowledgeState(k, 100, 1, false, time.Now())
	if k.EnableStatus != "disabled" {
		t.Fatal("unfinished replacement became searchable")
	}
	s, ks, ds, item := confluenceTestSetup()
	_, _ = s.ingestItem(context.Background(), ds, item, nil)
	ks.repo.rows["candidate"].ParseStatus = types.ParseStatusCompleted
	if _, err := s.ingestItem(context.Background(), ds, item, nil); err != nil {
		t.Fatal(err)
	}
	if ks.repo.rows["candidate"].EnableStatus != "enabled" {
		t.Fatal("completed replacement was not enabled")
	}
}
