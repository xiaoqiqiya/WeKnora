package repository

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCheckKnowledgeExists_ConfluenceAttachmentOwnership(t *testing.T) {
	db := setupKnowledgeTestDB(t)
	repo := NewKnowledgeRepository(db)
	ctx := context.Background()
	meta, err := json.Marshal(map[string]string{"datasource_id": "source-a", "external_id": "12#attachment#21"})
	require.NoError(t, err)
	k := &types.Knowledge{ID: uuid.NewString(), TenantID: 1, KnowledgeBaseID: "kb", Type: "file", FileType: "pdf", FileHash: "shared-bytes", ParseStatus: types.ParseStatusCompleted, Metadata: types.JSON(meta)}
	require.NoError(t, db.Exec(`INSERT INTO knowledges (id, tenant_id, knowledge_base_id, type, file_type, file_hash, parse_status, metadata) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, k.ID, k.TenantID, k.KnowledgeBaseID, k.Type, k.FileType, k.FileHash, k.ParseStatus, string(meta)).Error)
	for _, tc := range []struct {
		ds, external string
		want         bool
	}{
		{"source-a", "12#attachment#21", true},
		{"source-a", "13#attachment#22", false},
		{"source-b", "12#attachment#21", false},
		{"source-a", "12#attachment#21#pending#replacement", false},
		{"", "", true},
	} {
		exists, _, err := repo.CheckKnowledgeExists(ctx, 1, "kb", &types.KnowledgeCheckParams{Type: "file", FileType: "pdf", FileHash: "shared-bytes", DataSourceID: tc.ds, ExternalID: tc.external})
		require.NoError(t, err)
		assert.Equal(t, tc.want, exists, "%s / %s", tc.ds, tc.external)
	}
}

func TestCheckKnowledgeExists_FileHashIsScopedByFileType(t *testing.T) {
	db := setupKnowledgeTestDB(t)
	repo := NewKnowledgeRepository(db)
	ctx := context.Background()
	tenantID := uint64(1)
	kbID := uuid.NewString()
	const fileHash = "same-content-hash"

	require.NoError(t, db.Exec(`
		INSERT INTO knowledges (id, tenant_id, knowledge_base_id, type, title, file_name, file_type, file_hash, parse_status)
		VALUES (?, ?, ?, 'file', 'document.md', 'document.md', 'md', ?, 'completed')
	`, uuid.NewString(), tenantID, kbID, fileHash).Error)

	t.Run("same content with another file type is allowed", func(t *testing.T) {
		exists, knowledge, err := repo.CheckKnowledgeExists(ctx, tenantID, kbID, &types.KnowledgeCheckParams{
			Type:     "file",
			FileHash: fileHash,
			FileType: "txt",
		})

		require.NoError(t, err)
		assert.False(t, exists)
		assert.Nil(t, knowledge)
	})

	t.Run("same content and file type remains a duplicate", func(t *testing.T) {
		exists, knowledge, err := repo.CheckKnowledgeExists(ctx, tenantID, kbID, &types.KnowledgeCheckParams{
			Type:     "file",
			FileHash: fileHash,
			FileType: "md",
		})

		require.NoError(t, err)
		assert.True(t, exists)
		require.NotNil(t, knowledge)
		assert.Equal(t, "md", knowledge.FileType)
	})

	t.Run("file type matching is case-insensitive", func(t *testing.T) {
		exists, knowledge, err := repo.CheckKnowledgeExists(ctx, tenantID, kbID, &types.KnowledgeCheckParams{
			Type:     "file",
			FileHash: fileHash,
			FileType: "MD",
		})

		require.NoError(t, err)
		assert.True(t, exists)
		require.NotNil(t, knowledge)
		assert.Equal(t, "md", knowledge.FileType)
	})
}
