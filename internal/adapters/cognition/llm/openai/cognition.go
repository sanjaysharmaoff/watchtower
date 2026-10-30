package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/sanjaysharmaoff/watchtower/internal/domain"
	"github.com/sanjaysharmaoff/watchtower/internal/ports"
	"github.com/sanjaysharmaoff/watchtower/internal/wire"
)

func init() {
	wire.Planners.Register("openai", func(e *wire.Env) (ports.Planner, error) {
		c, err := Shared(e)
		if err != nil {
			return nil, err
		}
		return Planner{c: c, model: e.Cfg.LLM.PlannerModel, temp: e.Cfg.LLM.Temperature, maxTokens: e.Cfg.LLM.MaxTokens}, nil
	})
	wire.Critics.Register("openai", func(e *wire.Env) (ports.Critic, error) {
		c, err := Shared(e)
		if err != nil {
			return nil, err
		}
		return Critic{c: c, model: e.Cfg.LLM.CriticModel, maxTokens: e.Cfg.LLM.MaxTokens}, nil
	})
	wire.Narrators.Register("openai", func(e *wire.Env) (ports.Narrator, error) {
		c, err := Shared(e)
		if err != nil {
			return nil, err
		}
		return Narrator{c: c, model: e.Cfg.LLM.PlannerModel, maxTokens: e.Cfg.LLM.MaxTokens}, nil
	})
	wire.Embedders.Register("openai", func(e *wire.Env) (ports.Embedder, error) {
		c, err := Shared(e)
		if err != nil {
			return nil, err
		}
		return Embedder{c: c, model: e.Cfg.LLM.EmbedModel, dims: dimensionsFor(e.Cfg.LLM.EmbedModel, e.Cfg.RAG.VectorDims)}, nil
	})
}

// Planner drafts remediation plans.
type Planner struct {
	c         *Client
	model     string
	temp      float64
	maxTokens int
}

// Name implements ports.Planner.
func (p Planner) Name() string { return "openai:" + p.model }

// Plan implements ports.Planner.
func (p Planner) Plan(ctx context.Context, prompt domain.Prompt) (domain.Plan, error) {
	raw, err := p.c.Chat(ctx, ChatOptions{
		Model:       p.model,
		System:      prompt.System,
		User:        prompt.User,
		Temperature: p.temp,
		MaxTokens:   p.maxTokens,
		JSONOnly:    true,
	})
	if err != nil {
		return domain.Plan{}, err
	}
	plan, err := ParsePlan(raw)
	if err != nil {
		return domain.Plan{}, err
	}
	plan.Model = p.Name()
	return plan, nil
}

// ParsePlan decodes a model response into a Plan, tolerating markdown fences and
// leading prose that models sometimes add despite instructions.
func ParsePlan(raw string) (domain.Plan, error) {
	body := extractJSON(raw)
	if body == "" {
		return domain.Plan{}, fmt.Errorf("planner returned no JSON object: %s", snippet([]byte(raw)))
	}

	var dto struct {
		RootCause    string          `json:"root_cause"`
		Impact       string          `json:"impact"`
		Confidence   float64         `json:"confidence"`
		Verification json.RawMessage `json:"verification"`
		Rollback     json.RawMessage `json:"rollback"`
		Steps        []struct {
			Run     string `json:"run"`
			Explain string `json:"explain"`
			Risk    string `json:"risk"`
		} `json:"steps"`
	}
	if err := json.Unmarshal([]byte(body), &dto); err != nil {
		return domain.Plan{}, fmt.Errorf("decode plan JSON: %w", err)
	}

	plan := domain.Plan{
		RootCause:    strings.TrimSpace(dto.RootCause),
		Impact:       strings.TrimSpace(dto.Impact),
		Verification: flexStrings(dto.Verification),
		Rollback:     flexStrings(dto.Rollback),
		Confidence:   dto.Confidence,
		Raw:          body,
	}
	for _, s := range dto.Steps {
		run := strings.TrimSpace(s.Run)
		if run == "" {
			continue
		}
		plan.Steps = append(plan.Steps, domain.Command{
			Run:     run,
			Explain: strings.TrimSpace(s.Explain),
			Risk:    parseRisk(s.Risk),
		})
	}
	if plan.RootCause == "" && len(plan.Steps) == 0 {
		return domain.Plan{}, fmt.Errorf("plan has neither root cause nor steps")
	}
	return plan, nil
}

// Critic reviews a draft and demands a rewrite when it is wrong.
type Critic struct {
	c         *Client
	model     string
	maxTokens int
}

// Name implements ports.Critic.
func (c Critic) Name() string { return "openai:" + c.model }

const criticSystem = `You review remediation plans written by another engineer before they reach a production on-call SRE.

Reject the plan if any of these hold:
- The root cause is not supported by the evidence in the incident context.
- A command references a resource that does not appear in the context.
- A step is destructive, irreversible, or broader in scope than the incident.
- The plan changes state before establishing what is wrong.
- Verification is missing for a step that changes state.

Reply with a single JSON object: {"approved": true|false, "issues": ["..."], "notes": "..."}. No prose outside the JSON.`

// Review implements ports.Critic.
func (c Critic) Review(ctx context.Context, prompt domain.Prompt, plan domain.Plan) (domain.Verdict, error) {
	draft, err := json.Marshal(plan)
	if err != nil {
		return domain.Verdict{}, err
	}
	user := prompt.User + "\n\n## Plan under review\n" + string(draft)

	raw, err := c.c.Chat(ctx, ChatOptions{
		Model:     c.model,
		System:    criticSystem,
		User:      user,
		MaxTokens: c.maxTokens,
		JSONOnly:  true,
	})
	if err != nil {
		return domain.Verdict{}, err
	}

	body := extractJSON(raw)
	if body == "" {
		return domain.Verdict{}, fmt.Errorf("critic returned no JSON object")
	}
	var v struct {
		Approved bool            `json:"approved"`
		Issues   json.RawMessage `json:"issues"`
		Notes    string          `json:"notes"`
	}
	if err := json.Unmarshal([]byte(body), &v); err != nil {
		return domain.Verdict{}, fmt.Errorf("decode verdict JSON: %w", err)
	}
	return domain.Verdict{
		Approved: v.Approved,
		Issues:   flexStrings(v.Issues),
		Notes:    strings.TrimSpace(v.Notes),
	}, nil
}

// flexStrings coerces whatever shape a model chose for a list of sentences into a
// list of sentences. Real models return a bare string, an array of strings, an array
// of objects, or a keyed object, and losing an entire critique to a type mismatch is
// not an acceptable failure mode.
func flexStrings(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}

	var asStrings []string
	if err := json.Unmarshal(raw, &asStrings); err == nil {
		return compact(asStrings)
	}

	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		return compact(strings.Split(asString, "\n"))
	}

	var asAny any
	if err := json.Unmarshal(raw, &asAny); err != nil {
		return nil
	}
	return compact(flatten(asAny))
}

func flatten(v any) []string {
	switch typed := v.(type) {
	case nil:
		return nil
	case string:
		return []string{typed}
	case float64, bool:
		return []string{fmt.Sprint(typed)}
	case []any:
		out := make([]string, 0, len(typed))
		for _, item := range typed {
			out = append(out, flatten(item)...)
		}
		return out
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for k := range typed {
			keys = append(keys, k)
		}
		sort.Strings(keys) // stable output for the same input

		out := make([]string, 0, len(typed))
		for _, k := range keys {
			parts := flatten(typed[k])
			// A numeric key is an array index in disguise; a real key is a label.
			if _, err := strconv.Atoi(k); err == nil || len(parts) == 0 {
				out = append(out, parts...)
				continue
			}
			for _, p := range parts {
				out = append(out, k+": "+p)
			}
		}
		return out
	default:
		return nil
	}
}

// Narrator writes the daily digest prose.
type Narrator struct {
	c         *Client
	model     string
	maxTokens int
}

// Name implements ports.Narrator.
func (n Narrator) Name() string { return "openai:" + n.model }

// Narrate implements ports.Narrator.
func (n Narrator) Narrate(ctx context.Context, p domain.Prompt) (string, error) {
	return n.c.Chat(ctx, ChatOptions{Model: n.model, System: p.System, User: p.User, MaxTokens: n.maxTokens})
}

// Embedder produces vectors for retrieval.
type Embedder struct {
	c     *Client
	model string
	dims  int
}

// Name implements ports.Embedder.
func (e Embedder) Name() string { return "openai:" + e.model }

// Dimensions implements ports.Embedder.
func (e Embedder) Dimensions() int { return e.dims }

// Embed implements ports.Embedder.
func (e Embedder) Embed(ctx context.Context, text string) ([]float32, error) {
	return e.c.Embeddings(ctx, e.model, text)
}

// dimensionsFor covers the models whose width is known, and otherwise trusts the
// configured value, so any OpenAI-compatible server (Ollama, vLLM, a gateway) works
// without a code change.
func dimensionsFor(model string, configured int) int {
	switch {
	case strings.Contains(model, "3-large"):
		return 3072
	case strings.Contains(model, "3-small"), strings.Contains(model, "ada-002"):
		return 1536
	case strings.Contains(model, "nomic-embed"):
		return 768
	case strings.Contains(model, "mxbai-embed"), strings.Contains(model, "bge-m3"):
		return 1024
	case strings.Contains(model, "all-minilm"):
		return 384
	case configured > 0:
		return configured
	default:
		return 1536
	}
}

// extractJSON pulls the outermost JSON object out of a model response.
func extractJSON(raw string) string {
	s := strings.TrimSpace(raw)
	if fence := strings.Index(s, "```"); fence >= 0 {
		s = s[fence+3:]
		if nl := strings.IndexByte(s, '\n'); nl >= 0 && !strings.HasPrefix(strings.TrimSpace(s), "{") {
			s = s[nl+1:]
		}
		if end := strings.Index(s, "```"); end >= 0 {
			s = s[:end]
		}
	}
	start := strings.IndexByte(s, '{')
	end := strings.LastIndexByte(s, '}')
	if start < 0 || end <= start {
		return ""
	}
	return strings.TrimSpace(s[start : end+1])
}

func parseRisk(v string) domain.RiskLevel {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "dangerous", "destructive", "high":
		return domain.RiskDangerous
	case "mutate", "write", "change", "medium":
		return domain.RiskMutate
	default:
		return domain.RiskRead
	}
}

func compact(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}
