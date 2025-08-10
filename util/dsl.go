package util

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/joho/godotenv"
)

var (
	badNameChars   = regexp.MustCompile(`[\s,/\.;\(\)-]`)
	manyUnderlines = regexp.MustCompile(`_+`)
)

type dslCache struct {
	Hash string `json:"hash"`
	// Legacy support: some caches may only have DSL lines without NL
	DSL []string `json:"dsl,omitempty"`
	// New format: explicit NL+DSL items
	Items []dslItem `json:"items,omitempty"`
}

// dslItem binds a natural language scenario to its DSL line.
type dslItem struct {
	NL  string `json:"nl"`
	DSL string `json:"dsl"`
}

func SanitizeTestName(name string) string {
	return strings.ToLower(strings.Trim(manyUnderlines.ReplaceAllString(badNameChars.ReplaceAllString(name, "_"), "_"), "_"))
}

// generateDslFromScenariosViaLLM converts natural language scenarios to DSL using OpenAI and caches results.
// The cache is stored under testdata/consensus_dsl_scenarios.json with a hash of inputs to detect staleness.
func GenerateDslFromScenariosViaLLM(t *testing.T, namespace string, spec string, scenarios []string) []dslItem {
	t.Helper()

	// Compute a stable hash of the scenarios array
	payload, err := json.Marshal(scenarios)
	if err != nil {
		t.Fatalf("failed to marshal scenarios: %v", err)
	}
	hash := sha256.Sum256(payload)
	hashHex := hex.EncodeToString(hash[:])

	// Cache file path
	cachePath := filepath.Join("testdata", fmt.Sprintf("%s.json", namespace))
	t.Logf("DSL cache path: %s", cachePath)

	// Try to read cache
	if existing := readDslCache(t, cachePath); existing != nil {
		if existing.Hash == hashHex {
			// Prefer items (new format). Validate outcomes under current constraints.
			if len(existing.Items) > 0 {
				t.Logf("DSL cache hit (hash=%s). Using cached %d item(s).", hashHex[:8], len(existing.Items))
				return existing.Items
			}
		}
		t.Logf("DSL cache stale/invalid or empty (cacheHash=%s, newHash=%s). Regenerating via LLM...", safeHash(existing.Hash), hashHex[:8])
	} else {
		t.Logf("DSL cache not found. Generating via LLM...")
	}

	// Regenerate via LLM
	items := callOpenAIToGenerateDSL(t, spec, scenarios)

	// Write cache
	result := &dslCache{Hash: hashHex, Items: items}
	if err := writeDslCache(cachePath, result); err != nil {
		t.Logf("warning: failed to write DSL cache: %v", err)
	}
	t.Logf("DSL cache updated on %s (%d item(s), hash=%s).", cachePath, len(items), hashHex[:8])

	return items
}

func readDslCache(t *testing.T, path string) *dslCache {
	t.Helper()
	f, err := os.Open(path) // #nosec G304
	if err != nil {
		return nil
	}
	defer f.Close()
	var c dslCache
	if err := json.NewDecoder(f).Decode(&c); err != nil {
		t.Logf("warning: failed to decode DSL cache: %v", err)
		return nil
	}
	return &c
}

func writeDslCache(path string, c *dslCache) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil { // #nosec G301
		return err
	}
	tmp := path + ".tmp"
	f, err := os.Create(tmp) // #nosec G304
	if err != nil {
		return err
	}
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	if err := enc.Encode(c); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

// safeHash shortens a hash for logs and tolerates empty strings.
func safeHash(h string) string {
	if len(h) >= 8 {
		return h[:8]
	}
	if h == "" {
		return "<none>"
	}
	return h
}

// callOpenAIToGenerateDSL calls OpenAI to convert natural language scenarios to {nl,dsl} items.
// Requires OPENAI_API_KEY or an up-to-date cache.
func callOpenAIToGenerateDSL(t *testing.T, spec string, scenarios []string) []dslItem {
	t.Helper()

	currentDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("failed to get current directory: %v", err)
	}
	for _, path := range []string{
		filepath.Join(currentDir, "..", "..", ".env"),
		filepath.Join(currentDir, "..", ".env"),
		filepath.Join(currentDir, ".env"),
	} {
		err = godotenv.Load(path)
		if err == nil {
			break
		}
	}
	if err != nil {
		t.Fatalf("failed to load .env file: %v", err)
	}
	apiKey := os.Getenv("OPENAI_API_KEY")
	if apiKey == "" {
		t.Fatalf("OPENAI_API_KEY not set and no up-to-date cache")
	}

	// Generate in small batches to reduce hallucinations
	chunks := chunkScenarios(scenarios, 10)
	t.Logf("Generating DSL via OpenAI in %d chunk(s)...", len(chunks))
	var combined []dslItem
	for i, chunk := range chunks {
		start := time.Now()
		t.Logf("[OpenAI] Chunk %d/%d: %d scenario(s)...", i+1, len(chunks), len(chunk))
		items, err := openAIGenerateChunk(t, apiKey, spec, chunk)
		if err != nil {
			t.Fatalf("OpenAI chunk generation failed: %v", err)
		}
		t.Logf("[OpenAI] Chunk %d/%d done in %s (generated %d item(s)).", i+1, len(chunks), time.Since(start).Round(time.Millisecond), len(items))
		combined = append(combined, items...)
	}

	// Final review of the entire NL+DSL set
	// t.Logf("[OpenAI] Review: validating %d item(s) against NL scenarios...", len(combined))
	// reviewStart := time.Now()
	// reviewed, err := openAIReviewDsl(t, apiKey, spec, combined)
	// if err == nil && len(reviewed) == len(combined) {
	// 	combined = reviewed
	// 	t.Logf("[OpenAI] Review OK in %s.", time.Since(reviewStart).Round(time.Millisecond))
	// } else if err != nil {
	// 	t.Logf("[OpenAI] Review failed after %s, using unreviewed DSL: %v", time.Since(reviewStart).Round(time.Millisecond), err)
	// }
	return combined
}

func buildOpenAIUserPrompt(spec string, scenarios []string) string {
	var b strings.Builder
	b.WriteString("You will convert natural language test scenarios into the project's DSL.\n")
	b.WriteString("Only output a JSON object with shape {\"items\":[{\"nl\":string,\"dsl\":string}, ...]}.\n")
	b.WriteString("On the right side of each DSL line (after =>), you MUST output only one of response type tokens.\n")
	b.WriteString("Do not add any numbering or bullets to items.nl or items.dsl; keep them clean and unprefixed.\n")
	b.WriteString("Here is the DSL specification you MUST follow exactly:\n\n")
	b.WriteString(spec)
	b.WriteString("\n\nScenarios to convert (preserve exact NL in items.nl):\n")
	b.WriteString("Scenarios:\n")
	for i, s := range scenarios {
		// Write each scenario on its own line without numbering or bullets
		_ = i // index intentionally unused; no numbering
		fmt.Fprintf(&b, "%s\n", s)
	}
	return b.String()
}

func parseOpenAIItems(content string) ([]dslItem, error) {
	// Shape: {"items":[{"nl":"...","dsl":"..."}, ...]}
	var obj struct {
		Items []dslItem `json:"items"`
		DSL   []string  `json:"dsl"`
	}
	if err := json.Unmarshal([]byte(content), &obj); err == nil {
		if len(obj.Items) > 0 {
			for i := range obj.Items {
				obj.Items[i].NL = SanitizeTestName(obj.Items[i].NL)
				obj.Items[i].DSL = strings.TrimSpace(obj.Items[i].DSL)
			}
			return obj.Items, nil
		}
	}
	return nil, fmt.Errorf("unrecognized OpenAI content shape")
}

func openAIGenerateChunk(t *testing.T, apiKey, spec string, scenarios []string) ([]dslItem, error) {
	sys := "You are a precise DSL generator for a consensus testing framework."
	user := buildOpenAIUserPrompt(spec, scenarios)
	reqBody := map[string]any{
		"model": "gpt-5",
		// "temperature": 0.0,
		"messages": []map[string]string{
			{"role": "system", "content": sys},
			{"role": "user", "content": user},
		},
		"response_format": map[string]string{"type": "json_object"},
	}
	bodyBytes, _ := json.Marshal(reqBody)
	responseBody, err := doOpenAIRequestWithRetries(t, apiKey, bodyBytes)
	if err != nil {
		return nil, err
	}
	var raw struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(responseBody, &raw); err != nil {
		return nil, err
	}
	if len(raw.Choices) == 0 {
		return nil, errors.New("no choices")
	}
	return parseOpenAIItems(raw.Choices[0].Message.Content)
}

func openAIReviewDsl(t *testing.T, apiKey, spec string, items []dslItem) ([]dslItem, error) {
	if len(items) == 0 {
		return nil, errors.New("no items to review")
	}
	// Build review content
	var b strings.Builder
	b.WriteString("Critically review the DSL generated for the following natural language scenarios.\n")
	b.WriteString("Ensure strict conformance to the DSL spec and that each DSL line faithfully represents its NL scenario.\n")
	b.WriteString("If any line needs correction, return the corrected full list.\n")
	b.WriteString("Respond ONLY with a JSON object: {\"approved\":boolean, \"items\":[{\"nl\":string,\"dsl\":string},...], \"notes\":string}.\n\n")
	b.WriteString("DSL Specification:\n")
	b.WriteString(spec)
	b.WriteString("\n\nItems:\n")
	for i := range items {
		_ = i // avoid numbering in review
		fmt.Fprintf(&b, "NL: %s\n", items[i].NL)
		fmt.Fprintf(&b, "DSL: %s\n", items[i].DSL)
	}
	sys := "You are a rigorous reviewer that ensures DSL lines match their NL descriptions exactly."
	user := b.String()
	reqBody := map[string]any{
		"model": "gpt-5",
		// "temperature": 0.0,
		"messages": []map[string]string{
			{"role": "system", "content": sys},
			{"role": "user", "content": user},
		},
		"response_format": map[string]string{"type": "json_object"},
	}
	bodyBytes, _ := json.Marshal(reqBody)
	responseBody, err := doOpenAIRequestWithRetries(t, apiKey, bodyBytes)
	if err != nil {
		return nil, err
	}
	var raw struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(responseBody, &raw); err != nil {
		return nil, err
	}
	if len(raw.Choices) == 0 {
		return nil, errors.New("no choices in review")
	}
	var obj struct {
		Approved bool      `json:"approved"`
		Items    []dslItem `json:"items"`
		Notes    string    `json:"notes"`
		DSL      []string  `json:"dsl"`
	}
	if err := json.Unmarshal([]byte(raw.Choices[0].Message.Content), &obj); err == nil {
		if len(obj.Items) == len(items) && (!obj.Approved || obj.Approved) {
			return obj.Items, nil
		}
		if len(obj.DSL) == len(items) {
			// Map DSL back onto original NLs
			out := make([]dslItem, len(items))
			for i := range items {
				out[i] = dslItem{NL: items[i].NL, DSL: strings.TrimSpace(obj.DSL[i])}
			}
			return out, nil
		}
	}
	// Last resort: try to parse using the generic items parser
	parsed, err := parseOpenAIItems(raw.Choices[0].Message.Content)
	if err == nil && len(parsed) == len(items) {
		// Preserve NLs if model omitted them
		for i := range parsed {
			if strings.TrimSpace(parsed[i].NL) == "" {
				parsed[i].NL = items[i].NL
			}
		}
		return parsed, nil
	}
	return nil, errors.New("review returned unexpected shape")
}

func chunkScenarios(items []string, size int) [][]string {
	if size <= 0 {
		size = 5
	}
	var chunks [][]string
	for i := 0; i < len(items); i += size {
		j := i + size
		if j > len(items) {
			j = len(items)
		}
		chunks = append(chunks, items[i:j])
	}
	return chunks
}

// --- OpenAI HTTP helpers with retries and a 5-minute timeout ---

var openAIHTTPClient = &http.Client{Timeout: 5 * time.Minute}

func doOpenAIRequestWithRetries(t *testing.T, apiKey string, jsonBody []byte) ([]byte, error) {
	t.Helper()

	const maxAttempts = 6
	var lastErr error

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		// Fresh request and context for each attempt
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		req, err := http.NewRequestWithContext(ctx, "POST", "https://api.openai.com/v1/chat/completions", strings.NewReader(string(jsonBody)))
		if err != nil {
			cancel()
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+apiKey)
		req.Header.Set("Content-Type", "application/json")

		resp, err := openAIHTTPClient.Do(req)
		if err != nil {
			lastErr = err
		} else {
			// Ensure body is closed on all paths
			bodyBytes, readErr := io.ReadAll(resp.Body)
			_ = resp.Body.Close()

			// Success
			if readErr == nil && resp.StatusCode >= 200 && resp.StatusCode < 300 {
				cancel()
				return bodyBytes, nil
			}

			// Build error for logging / retry decision
			if readErr != nil {
				lastErr = fmt.Errorf("read body error (status=%d): %w", resp.StatusCode, readErr)
			} else {
				lastErr = fmt.Errorf("status %d: %s", resp.StatusCode, string(bodyBytes))
			}

			// Retry on 429, 408, and 5xx
			if !(resp.StatusCode == 429 || resp.StatusCode == 408 || (resp.StatusCode >= 500 && resp.StatusCode <= 599)) {
				// Non-retryable status
				cancel()
				return nil, lastErr
			}

			// Honor Retry-After if present
			if ra := resp.Header.Get("Retry-After"); ra != "" {
				if d := parseRetryAfterHeader(ra); d > 0 {
					cancel()
					time.Sleep(d)
					continue
				}
			}
		}

		cancel()

		if attempt == maxAttempts {
			break
		}

		// Exponential backoff with light jitter, capped
		backoff := time.Second * time.Duration(1<<uint(attempt-1)) // #nosec G115
		if backoff > 30*time.Second {
			backoff = 30 * time.Second
		}
		// Jitter up to ~500ms based on time
		jitter := time.Duration((time.Now().UnixNano() % 500)) * time.Millisecond
		time.Sleep(backoff + jitter)
	}
	return nil, lastErr
}

func parseRetryAfterHeader(v string) time.Duration {
	// Only support delta-seconds form
	if secs, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && secs > 0 {
		// Clamp to a reasonable bound
		d := time.Duration(secs) * time.Second
		if d > 5*time.Minute {
			return 5 * time.Minute
		}
		return d
	}
	return 0
}
