package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	freeAgentsSourceURL  = "https://raw.githubusercontent.com/CodebuffAI/codebuff/main/common/src/constants/free-agents.ts"
	modelRefreshInterval = 6 * time.Hour
)

// hardcodedFallback is used when the remote fetch fails on startup.
var hardcodedFallback = map[string][]string{
	"base2-free":         {"minimax/minimax-m2.7", "z-ai/glm-5.1"},
	"file-picker":        {"google/gemini-2.5-flash-lite"},
	"file-picker-max":    {"google/gemini-3.1-flash-lite-preview"},
	"file-lister":        {"google/gemini-3.1-flash-lite-preview"},
	"researcher-web":     {"google/gemini-3.1-flash-lite-preview"},
	"researcher-docs":    {"google/gemini-3.1-flash-lite-preview"},
	"basher":             {"google/gemini-3.1-flash-lite-preview"},
	"editor-lite":        {"minimax/minimax-m2.7", "z-ai/glm-5.1"},
	"code-reviewer-lite": {"minimax/minimax-m2.7", "z-ai/glm-5.1"},
}

// ModelRegistry fetches and caches the agent→model mapping for all free agents
// from the upstream free-agents.ts source file.
type ModelRegistry struct {
	client *http.Client
	logger *log.Logger

	mu           sync.RWMutex
	agentModels  map[string][]string // agentID → []model
	modelToAgent map[string]string   // model → chosen agentID
	allModels    []string            // deduplicated, sorted
	lastOK       time.Time

	stopCh chan struct{}
	wg     sync.WaitGroup
}

func NewModelRegistry(client *http.Client, logger *log.Logger) *ModelRegistry {
	return &ModelRegistry{
		client:       client,
		logger:       logger,
		agentModels:  make(map[string][]string),
		modelToAgent: make(map[string]string),
		stopCh:       make(chan struct{}),
	}
}

func (r *ModelRegistry) Start(ctx context.Context) {
	if err := r.refresh(ctx); err != nil {
		r.logger.Printf("model registry: initial fetch failed, loading hardcoded fallback: %v", err)
		r.loadFallback()
	}

	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		ticker := time.NewTicker(modelRefreshInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				if err := r.refresh(ctx); err != nil {
					r.logger.Printf("model registry: refresh failed: %v", err)
				}
				cancel()
			case <-r.stopCh:
				return
			}
		}
	}()
}

func (r *ModelRegistry) Stop() {
	close(r.stopCh)
	r.wg.Wait()
}

// Models returns the deduplicated list of all available model names.
func (r *ModelRegistry) Models() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, len(r.allModels))
	copy(out, r.allModels)
	return out
}

// HasModel checks if the given model is available.
func (r *ModelRegistry) HasModel(model string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.modelToAgent[model]
	return ok
}

// AgentForModel returns the agent ID that should serve the given model.
func (r *ModelRegistry) AgentForModel(model string) (string, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	agent, ok := r.modelToAgent[model]
	return agent, ok
}

// AgentIDs returns the list of all known agent IDs.
func (r *ModelRegistry) AgentIDs() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	ids := make([]string, 0, len(r.agentModels))
	for id := range r.agentModels {
		ids = append(ids, id)
	}
	return ids
}

func (r *ModelRegistry) refresh(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, freeAgentsSourceURL, nil)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Accept", "text/plain")

	resp, err := r.client.Do(req)
	if err != nil {
		return fmt.Errorf("fetch free-agents source: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("unexpected status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}

	all := parseAllFreeModels(string(body))
	if len(all) == 0 {
		return fmt.Errorf("no free agents found in source")
	}

	modelToAgent, allModels := buildModelMapping(all)

	r.mu.Lock()
	r.agentModels = all
	r.modelToAgent = modelToAgent
	r.allModels = allModels
	r.lastOK = time.Now()
	r.mu.Unlock()

	r.logger.Printf("model registry: updated %d agents, %d models: %v", len(all), len(allModels), allModels)
	return nil
}

func (r *ModelRegistry) loadFallback() {
	modelToAgent, allModels := buildModelMapping(hardcodedFallback)

	r.mu.Lock()
	r.agentModels = hardcodedFallback
	r.modelToAgent = modelToAgent
	r.allModels = allModels
	r.mu.Unlock()

	r.logger.Printf("model registry: loaded fallback models: %v", allModels)
}

// parseAllFreeModels extracts ALL agent→models mappings from the free-agents.ts source.
func parseAllFreeModels(source string) map[string][]string {
	blockPattern := regexp.MustCompile(`'([^']+)':\s*new\s+Set\(\[([^\]]*)\]\)`)
	modelPattern := regexp.MustCompile(`'([^']+)'`)

	result := make(map[string][]string)
	for _, match := range blockPattern.FindAllStringSubmatch(source, -1) {
		agentID := match[1]
		modelsStr := match[2]

		var models []string
		for _, modelMatch := range modelPattern.FindAllStringSubmatch(modelsStr, -1) {
			model := strings.TrimSpace(modelMatch[1])
			if model != "" {
				models = append(models, model)
			}
		}
		if len(models) > 0 {
			result[agentID] = models
		}
	}
	return result
}

// buildModelMapping creates the model→agent reverse mapping and deduplicated model list.
// When a model appears in multiple agents, one is chosen at random.
func buildModelMapping(agentModels map[string][]string) (map[string]string, []string) {
	modelAgents := make(map[string][]string)
	for agentID, models := range agentModels {
		for _, model := range models {
			modelAgents[model] = append(modelAgents[model], agentID)
		}
	}

	modelToAgent := make(map[string]string, len(modelAgents))
	allModels := make([]string, 0, len(modelAgents))
	for model, agents := range modelAgents {
		modelToAgent[model] = agents[rand.Intn(len(agents))]
		allModels = append(allModels, model)
	}
	sort.Strings(allModels)
	return modelToAgent, allModels
}
