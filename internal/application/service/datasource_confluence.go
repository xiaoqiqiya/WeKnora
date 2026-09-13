package service

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/Tencent/WeKnora/internal/types"
)

// ItemCommitted keeps Confluence cursor revisions retryable until their indexes
// are ready. In particular, enqueue success is not indexing success.
func (h *streamSyncHandler) ItemCommitted(ctx context.Context, item *types.FetchedItem) (bool, error) {
	if h.ds.Type != types.ConnectorTypeConfluence {
		return true, nil
	}
	k, err := h.svc.knowledgeService.GetRepository().FindByDataSourceExternalID(ctx, h.ds.TenantID, h.ds.KnowledgeBaseID, h.ds.ID, item.ExternalID)
	if err != nil || k == nil {
		return false, err
	}
	if h.ds.ConflictStrategy == types.ConflictStrategySkip && confluenceReady(k) {
		return true, nil
	}
	return confluenceReady(k) && sameConfluenceRevision(k, item), nil
}

func confluenceReady(k *types.Knowledge) bool {
	return k.EnableStatus == "enabled" && k.ParseStatus == types.ParseStatusCompleted
}

func sameConfluenceRevision(k *types.Knowledge, item *types.FetchedItem) bool {
	metadata := k.GetMetadata()
	return metadata["source_revision"] == item.Metadata["source_revision"] && metadata["source_site"] == item.Metadata["source_site"]
}

// ingestConfluenceItem stages a replacement under a separate external_id. Old
// content survives fetch, enqueue and parse failures; only a completed candidate
// can replace it. A later sync also recovers an interrupted promotion.
func (s *DataSourceService) ingestConfluenceItem(ctx context.Context, ds *types.DataSource, item *types.FetchedItem, tagIDs []string) (bool, error) {
	if len(item.Content) == 0 || item.ExternalID == "" || item.Metadata["source_revision"] == "" {
		return false, fmt.Errorf("confluence content, page ID and revision are required")
	}
	repo := s.knowledgeService.GetRepository()
	existing, err := repo.FindByDataSourceExternalID(ctx, ds.TenantID, ds.KnowledgeBaseID, ds.ID, item.ExternalID)
	if err != nil {
		return false, err
	}
	if existing != nil && confluenceReady(existing) && (sameConfluenceRevision(existing, item) || ds.ConflictStrategy == types.ConflictStrategySkip) {
		return false, types.NewDuplicateFileError(existing)
	}
	stagingID := types.SubtreeChildID(item.ExternalID, "pending", "replacement")
	candidate, err := repo.FindByDataSourceExternalID(ctx, ds.TenantID, ds.KnowledgeBaseID, ds.ID, stagingID)
	if err != nil {
		return false, err
	}
	if candidate != nil {
		if sameConfluenceRevision(candidate, item) && candidate.ParseStatus == types.ParseStatusCompleted {
			// Enable the fully indexed candidate before retiring the old copy.
			// If any following operation fails, at least one good copy stays searchable.
			if candidate.EnableStatus != "enabled" {
				if err := repo.UpdateKnowledgeColumns(ctx, candidate.ID, map[string]interface{}{"enable_status": "enabled"}); err != nil {
					return true, err
				}
				candidate.EnableStatus = "enabled"
			}
			if existing != nil {
				if err := s.removeConfluenceCopy(ctx, ds, existing); err != nil {
					return true, err
				}
			}
			meta := candidate.GetMetadata()
			meta["external_id"] = item.ExternalID
			delete(meta, "replacement_for")
			blob, err := json.Marshal(meta)
			if err != nil {
				return true, err
			}
			// Update only metadata: do not overwrite concurrently updated parse state.
			if err := repo.UpdateKnowledgeColumns(ctx, candidate.ID, map[string]interface{}{"metadata": types.JSON(blob)}); err != nil {
				return true, err
			}
			return true, nil
		}
		if sameConfluenceRevision(candidate, item) && candidate.ParseStatus != types.ParseStatusFailed && candidate.ParseStatus != types.ParseStatusCancelled {
			return false, types.NewDuplicateFileError(candidate)
		}
		// Only the unsuccessful/superseded candidate is removed, never the old good copy.
		if err := s.removeConfluenceCopy(ctx, ds, candidate); err != nil {
			return false, err
		}
	}
	if existing != nil && sameConfluenceRevision(existing, item) && existing.ParseStatus != types.ParseStatusFailed && existing.ParseStatus != types.ParseStatusCancelled {
		return false, types.NewDuplicateFileError(existing)
	}
	metadata := map[string]string{"external_id": item.ExternalID, "datasource_id": ds.ID, "source_resource_id": item.SourceResourceID}
	for k, v := range item.Metadata {
		metadata[k] = v
	}
	metadata["source_title"] = item.Title
	if !item.UpdatedAt.IsZero() {
		metadata["source_updated_at"] = item.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z07:00")
	}
	if existing != nil {
		metadata["external_id"] = stagingID
		metadata["replacement_for"] = existing.ID
	}
	fh, err := bytesToFileHeader(item.Content, item.FileName)
	if err != nil {
		return false, err
	}
	created, err := s.knowledgeService.CreateKnowledgeFromFile(ctx, ds.KnowledgeBaseID, fh, metadata, nil, item.FileName, tagIDs, "confluence", nil)
	if err != nil {
		return false, err
	}
	if created == nil {
		return false, fmt.Errorf("confluence ingestion returned no knowledge")
	}
	if created.ParseStatus == types.ParseStatusFailed || created.ParseStatus == types.ParseStatusCancelled {
		return false, fmt.Errorf("confluence indexing was not queued successfully")
	}
	return existing != nil, nil
}

func (s *DataSourceService) removeConfluenceCopy(ctx context.Context, ds *types.DataSource, k *types.Knowledge) error {
	if k.TenantID != ds.TenantID || k.KnowledgeBaseID != ds.KnowledgeBaseID || k.GetMetadata()["datasource_id"] != ds.ID {
		return fmt.Errorf("confluence replacement ownership mismatch")
	}
	if err := s.knowledgeService.DeleteKnowledge(ctx, k.ID); err != nil {
		return err
	}
	return s.knowledgeService.GetRepository().HardDeleteKnowledge(ctx, ds.TenantID, k.ID)
}
