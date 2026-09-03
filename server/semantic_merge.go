package server

import "sort"

func mergeSearchResults(lexical []searchResult, semantic []semanticSearchResult, config SemanticSearchConfig, want int) []searchResult {
	lexByID := map[string]searchResult{}
	for _, result := range lexical {
		if _, exists := lexByID[result.ID]; !exists {
			lexByID[result.ID] = result
		}
	}
	semByID := map[string]semanticSearchResult{}
	for _, result := range semantic {
		if _, exists := semByID[result.ID]; !exists {
			semByID[result.ID] = result
		}
	}
	type ranked struct {
		id    string
		score float64
	}
	var rankedResults []ranked
	for id := range lexByID {
		lexRank := rankOf(lexical, id)
		semRank := rankOfSemantic(semantic, id)
		score := config.LexicalWeight / (config.RRFK + float64(lexRank))
		if semRank > 0 {
			score += config.SemanticWeight / (config.RRFK + float64(semRank))
		}
		rankedResults = append(rankedResults, ranked{id: id, score: score})
	}
	for id := range semByID {
		if _, exists := lexByID[id]; exists {
			continue
		}
		semRank := rankOfSemantic(semantic, id)
		rankedResults = append(rankedResults, ranked{id: id, score: config.SemanticWeight / (config.RRFK + float64(semRank))})
	}
	sort.Slice(rankedResults, func(i, j int) bool {
		if rankedResults[i].score != rankedResults[j].score {
			return rankedResults[i].score > rankedResults[j].score
		}
		return rankedResults[i].id < rankedResults[j].id
	})
	if len(rankedResults) > want {
		rankedResults = rankedResults[:want]
	}
	out := make([]searchResult, 0, len(rankedResults))
	for _, item := range rankedResults {
		result, hasLex := lexByID[item.id]
		semanticResult, hasSem := semByID[item.id]
		if !hasLex {
			result = semanticResult.searchResult
			result.Provenance = []retrievalProvenance{provenanceOf(result)}
			out = append(out, result)
			continue
		}
		if hasSem && semanticContribution(semantic, item.id, config) > lexicalContribution(lexical, item.id, config) {
			result.Snippet, result.Kind, result.Heading = semanticResult.Snippet, semanticResult.Kind, semanticResult.Heading
		}
		result.Source = "lexical"
		result.Provenance = []retrievalProvenance{provenanceOf(lexByID[item.id])}
		if hasSem {
			result.Source = "hybrid"
			result.Provenance = append(result.Provenance, provenanceOf(semanticResult.searchResult))
		}
		out = append(out, result)
	}
	return out
}

func semanticContribution(results []semanticSearchResult, id string, config SemanticSearchConfig) float64 {
	return config.SemanticWeight / (config.RRFK + float64(rankOfSemantic(results, id)))
}

func lexicalContribution(results []searchResult, id string, config SemanticSearchConfig) float64 {
	return config.LexicalWeight / (config.RRFK + float64(rankOf(results, id)))
}

func provenanceOf(result searchResult) retrievalProvenance {
	return retrievalProvenance{Source: result.Source, Kind: result.Kind, Heading: result.Heading, Snippet: result.Snippet}
}

func rankOf(results []searchResult, id string) int {
	for i, result := range results {
		if result.ID == id {
			return i + 1
		}
	}
	return 0
}

func rankOfSemantic(results []semanticSearchResult, id string) int {
	for i, result := range results {
		if result.ID == id {
			return i + 1
		}
	}
	return 0
}
