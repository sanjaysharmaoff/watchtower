package stages

import (
	"context"
	"fmt"
	"strings"

	"github.com/sanjaysharmaoff/watchtower/internal/domain"
	"github.com/sanjaysharmaoff/watchtower/internal/ports"
)

// RetrieveOptions configures the RAG stage.
type RetrieveOptions struct {
	TopK     int
	TopN     int
	MinScore float64
	QueryOf  func(*domain.Incident) string
}

// Retrieve embeds a query built from the incident, pulls the top K historical
// runbooks from the vector store and hands the shortlist to the reranker.
//
// Every dependency is optional: with no embedder or no vector store the stage
// simply records that retrieval was skipped and the planner works cold.
func Retrieve(emb ports.Embedder, store ports.VectorStore, rr ports.Reranker, opts RetrieveOptions) Stage {
	if opts.TopK <= 0 {
		opts.TopK = 10
	}
	if opts.TopN <= 0 {
		opts.TopN = 2
	}
	if opts.QueryOf == nil {
		opts.QueryOf = DefaultQuery
	}

	return mutator("retrieve", func(ctx context.Context, inc *domain.Incident) error {
		if emb == nil || store == nil {
			inc.Note("retrieval disabled")
			return nil
		}
		query := opts.QueryOf(inc)
		if strings.TrimSpace(query) == "" {
			inc.Note("retrieval skipped: empty query")
			return nil
		}
		inc.SetMeta("rag_query", truncate(query, 240))

		vec, err := emb.Embed(ctx, query)
		if err != nil {
			inc.Note("embedding failed, continuing without runbooks: %v", err)
			return nil
		}
		matches, err := store.Search(ctx, vec, opts.TopK)
		if err != nil {
			inc.Note("vector search failed, continuing without runbooks: %v", err)
			return nil
		}
		if len(matches) == 0 {
			inc.Note("no historical runbooks matched")
			return nil
		}
		if rr != nil {
			reranked, err := rr.Rerank(ctx, query, matches, opts.TopN)
			if err != nil {
				inc.Note("rerank failed, using raw similarity order: %v", err)
			} else {
				matches = reranked
			}
		}
		kept := make([]domain.RunbookMatch, 0, opts.TopN)
		for _, m := range matches {
			if len(kept) >= opts.TopN {
				break
			}
			if m.Score < opts.MinScore {
				continue
			}
			kept = append(kept, m)
		}
		inc.Matches = kept
		inc.Note("retrieved %d runbook(s) above score %.2f", len(kept), opts.MinScore)
		return nil
	})
}

// DefaultQuery derives retrieval text from the alert names, the target and the
// most severe log lines. Cheap, deterministic, and good enough to hit the right
// neighbourhood of the corpus.
func DefaultQuery(inc *domain.Incident) string {
	var b strings.Builder
	primary := inc.PrimaryAlert()
	b.WriteString(primary.Name)
	if primary.Summary != "" {
		b.WriteString(": ")
		b.WriteString(primary.Summary)
	}
	if t := primary.Target(); t != "unknown" {
		b.WriteString(" [")
		b.WriteString(t)
		b.WriteString("]")
	}
	for _, a := range inc.Alerts[1:min(len(inc.Alerts), 4)] {
		b.WriteString("; ")
		b.WriteString(a.Name)
	}
	for _, l := range TopLines(inc.Logs, 3) {
		b.WriteString("\n")
		b.WriteString(truncate(l.Message, 200))
	}
	for _, e := range inc.Events[:min(len(inc.Events), 2)] {
		b.WriteString("\n")
		b.WriteString(fmt.Sprintf("%s %s", e.Reason, truncate(e.Message, 160)))
	}
	return strings.TrimSpace(b.String())
}

// TopLines returns the n highest-severity log lines, most severe first.
func TopLines(lines []domain.LogLine, n int) []domain.LogLine {
	if len(lines) == 0 || n <= 0 {
		return nil
	}
	sorted := append([]domain.LogLine(nil), lines...)
	for i := 1; i < len(sorted); i++ {
		for j := i; j > 0 && sorted[j].LevelRank() > sorted[j-1].LevelRank(); j-- {
			sorted[j], sorted[j-1] = sorted[j-1], sorted[j]
		}
	}
	return sorted[:min(n, len(sorted))]
}

func truncate(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
