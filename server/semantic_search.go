package server

import (
	"context"
	"fmt"
	"math"
	"sort"
)

type SemanticSearchConfig struct {
	RRFK             float64
	LexicalWeight    float64
	SemanticWeight   float64
	DescriptionBoost float64
	CandidateLimit   int
}

func DefaultSemanticSearchConfig() SemanticSearchConfig {
	return SemanticSearchConfig{RRFK: 60, LexicalWeight: 1, SemanticWeight: 1, DescriptionBoost: 0.05, CandidateLimit: 200}
}

func (c SemanticSearchConfig) validate() error {
	if c.RRFK <= 0 || c.LexicalWeight < 0 || c.SemanticWeight < 0 || c.DescriptionBoost < 0 || c.CandidateLimit <= 0 {
		return fmt.Errorf("invalid semantic search configuration")
	}
	if c.LexicalWeight == 0 && c.SemanticWeight == 0 {
		return fmt.Errorf("at least one semantic search weight is required")
	}
	return nil
}

func (s *Server) SetSemanticSearchConfig(config SemanticSearchConfig) error {
	if err := config.validate(); err != nil {
		return err
	}
	s.semanticConfig = config
	return nil
}

func (s *Server) SetSemanticEmbedder(embedder Embedder) error {
	if embedder == nil {
		return fmt.Errorf("semantic embedder is required")
	}
	signature := embedder.Signature()
	if err := signature.validate(); err != nil {
		return err
	}
	if err := s.semanticCache.DropOtherModels(signature); err != nil {
		return err
	}
	s.semanticEmbedder = embedder
	return s.semanticCache.CleanupStale()
}

type retrievalProvenance struct {
	Source  string `json:"source"`
	Kind    string `json:"kind"`
	Heading string `json:"heading,omitempty"`
	Snippet string `json:"snippet"`
}

type semanticSearchResult struct {
	searchResult
	score float64
}

func (s *Server) searchHybrid(ctx context.Context, userID, query, match string, ws []string, want int) []searchResult {
	lexical := s.searchChunks(userID, match, ws, want)
	if len(lexical) == 0 {
		lexical = s.searchPagesFallback(userID, match, ws, want)
	}
	semantic := s.searchSemantic(ctx, userID, query, ws, s.semanticConfig.CandidateLimit)
	if len(semantic) == 0 {
		return addLexicalProvenance(lexical)
	}
	return mergeSearchResults(lexical, semantic, s.semanticConfig, want)
}

func addLexicalProvenance(results []searchResult) []searchResult {
	for i := range results {
		if len(results[i].Provenance) == 0 {
			results[i].Provenance = []retrievalProvenance{provenanceOf(results[i])}
		}
	}
	return results
}

func (s *Server) searchSemantic(ctx context.Context, userID, query string, ws []string, limit int) []semanticSearchResult {
	if s.semanticCache == nil || s.semanticEmbedder == nil || len(ws) == 0 || limit <= 0 {
		return nil
	}
	queryVector, err := s.semanticEmbedder.Embed(ctx, query)
	if err != nil || len(queryVector) != s.semanticEmbedder.Signature().Dimensions() {
		return nil
	}
	pageIDs := s.readableSemanticPageIDs(userID, ws)
	if len(pageIDs) == 0 {
		return nil
	}
	args := []any{s.semanticEmbedder.Signature().String()}
	args = append(args, stringsAsAny(ws)...)
	args = append(args, stringsAsAny(pageIDs)...)
	rows, err := s.db.Query(`SELECT p.id, p.title, p.icon, c.kind, c.heading, c.text,
			e.vector, e.dimension, e.serialization_format
		FROM chunk_embeddings e
		JOIN page_chunks c ON c.id = e.chunk_id
		JOIN pages p ON p.id = c.page_id
		WHERE e.model_signature = ? AND c.workspace_id IN (`+placeholders(len(ws))+`)
		  AND p.id IN (`+placeholders(len(pageIDs))+`) AND p.trashed_at IS NULL`, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	best := map[string]semanticSearchResult{}
	for rows.Next() {
		var result semanticSearchResult
		var text string
		var encoded []byte
		var dimension int
		var format string
		if err := rows.Scan(&result.ID, &result.Title, &result.Icon, &result.Kind, &result.Heading, &text, &encoded, &dimension, &format); err != nil {
			continue
		}
		if format != embeddingVectorSerializationFormat {
			continue
		}
		vector, err := deserializeEmbeddingVector(encoded, dimension)
		if err != nil {
			continue
		}
		result.Snippet = semanticSnippet(text)
		result.Source = "semantic"
		result.score = cosineSimilarity(queryVector, vector)
		if result.Kind == chunkKindDescription {
			result.score += s.semanticConfig.DescriptionBoost
		}
		current, exists := best[result.ID]
		if !exists || result.score > current.score || (result.score == current.score && result.Kind < current.Kind) {
			best[result.ID] = result
		}
	}
	if err := rows.Err(); err != nil {
		return nil
	}
	results := make([]semanticSearchResult, 0, len(best))
	for _, result := range best {
		if result.score > 0 {
			results = append(results, result)
		}
	}
	sort.Slice(results, func(i, j int) bool {
		if results[i].score != results[j].score {
			return results[i].score > results[j].score
		}
		return results[i].ID < results[j].ID
	})
	if len(results) > limit {
		results = results[:limit]
	}
	return results
}

func (s *Server) readableSemanticPageIDs(userID string, ws []string) []string {
	rows, err := s.db.Query(`SELECT DISTINCT p.id FROM page_chunks c JOIN pages p ON p.id = c.page_id
		WHERE c.workspace_id IN (`+placeholders(len(ws))+`) AND p.trashed_at IS NULL`, stringsAsAny(ws)...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil
	}
	rows.Close()
	readable := make([]string, 0, len(ids))
	for _, id := range ids {
		if s.canRead(userID, id) {
			readable = append(readable, id)
		}
	}
	return readable
}

func stringsAsAny(values []string) []any {
	args := make([]any, len(values))
	for i, value := range values {
		args[i] = value
	}
	return args
}

func cosineSimilarity(left, right []float32) float64 {
	if len(left) != len(right) || len(left) == 0 {
		return 0
	}
	var dot, leftNorm, rightNorm float64
	for i := range left {
		dot += float64(left[i]) * float64(right[i])
		leftNorm += float64(left[i]) * float64(left[i])
		rightNorm += float64(right[i]) * float64(right[i])
	}
	if leftNorm == 0 || rightNorm == 0 {
		return 0
	}
	return dot / (math.Sqrt(leftNorm) * math.Sqrt(rightNorm))
}

func semanticSnippet(text string) string {
	runes := []rune(text)
	if len(runes) > 240 {
		return string(runes[:240]) + "…"
	}
	return text
}
