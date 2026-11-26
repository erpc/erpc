package util

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// dslRoundTripFunc lets tests answer the OpenAI client without a network.
type dslRoundTripFunc func(*http.Request) (*http.Response, error)

func (f dslRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// useFakeOpenAI swaps the package OpenAI client's transport for the test.
func useFakeOpenAI(t *testing.T, rt dslRoundTripFunc) {
	t.Helper()
	prev := openAIHTTPClient
	openAIHTTPClient = &http.Client{Transport: rt, Timeout: 5 * time.Second}
	t.Cleanup(func() { openAIHTTPClient = prev })
}

func openAIResponse(status int, body string, header http.Header) *http.Response {
	if header == nil {
		header = http.Header{}
	}
	return &http.Response{
		StatusCode: status,
		Header:     header,
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

// chatCompletion wraps content in the OpenAI chat-completions envelope.
func chatCompletion(t *testing.T, content string) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"choices": []map[string]any{{"message": map[string]string{"content": content}}},
	})
	require.NoError(t, err)
	return string(b)
}

type failingBody struct{}

func (failingBody) Read([]byte) (int, error) { return 0, errors.New("body broke") }
func (failingBody) Close() error             { return nil }

func TestSanitizeTestName(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"simple", "simple"},
		{"Mixed Case Name", "mixed_case_name"},
		{"a, b; c/d.e(f)-g", "a_b_c_d_e_f_g"},
		{"  leading and trailing  ", "leading_and_trailing"},
		{"many   spaces\tand\nnewlines", "many_spaces_and_newlines"},
		{"__already__underscored__", "already_underscored"},
		{"(only-punctuation)", "only_punctuation"},
		{"", ""},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			assert.Equal(t, tc.want, SanitizeTestName(tc.in))
		})
	}
}

func TestSafeHash(t *testing.T) {
	assert.Equal(t, "<none>", safeHash(""))
	assert.Equal(t, "abc", safeHash("abc"))
	assert.Equal(t, "01234567", safeHash("01234567"))
	assert.Equal(t, "01234567", safeHash("0123456789abcdef"))
}

func TestChunkScenarios(t *testing.T) {
	items := []string{"a", "b", "c", "d", "e", "f", "g"}
	cases := []struct {
		name string
		in   []string
		size int
		want [][]string
	}{
		{"even split", items[:6], 3, [][]string{{"a", "b", "c"}, {"d", "e", "f"}}},
		{"remainder in last chunk", items, 3, [][]string{{"a", "b", "c"}, {"d", "e", "f"}, {"g"}}},
		{"size larger than input", items[:2], 10, [][]string{{"a", "b"}}},
		{"zero size defaults to 5", items, 0, [][]string{{"a", "b", "c", "d", "e"}, {"f", "g"}}},
		{"negative size defaults to 5", items[:5], -1, [][]string{{"a", "b", "c", "d", "e"}}},
		{"empty input", nil, 3, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, chunkScenarios(tc.in, tc.size))
		})
	}
}

func TestParseRetryAfterHeader(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
	}{
		{"1", time.Second},
		{" 30 ", 30 * time.Second},
		{"300", 5 * time.Minute},
		{"301", 5 * time.Minute},
		{"86400", 5 * time.Minute},
		{"0", 0},
		{"-5", 0},
		{"1.5", 0},
		{"Wed, 21 Oct 2015 07:28:00 GMT", 0},
		{"", 0},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			assert.Equal(t, tc.want, parseRetryAfterHeader(tc.in))
		})
	}
}

func TestBuildOpenAIUserPrompt(t *testing.T) {
	p := buildOpenAIUserPrompt("SPEC-BODY", []string{"first scenario", "second scenario"})
	assert.Contains(t, p, "\n\nSPEC-BODY\n\n")
	// Scenarios are listed verbatim, one per line, in order, without numbering.
	assert.True(t, strings.HasSuffix(p, "Scenarios:\nfirst scenario\nsecond scenario\n"), p)
	assert.NotContains(t, p, "1. first")
	assert.Less(t, strings.Index(p, "SPEC-BODY"), strings.Index(p, "first scenario"))
}

func TestParseOpenAIItems(t *testing.T) {
	t.Run("items are sanitized and trimmed", func(t *testing.T) {
		got, err := parseOpenAIItems(`{"items":[{"nl":"Two Upstreams, One Fails","dsl":"  a => b \n"},{"nl":"x","dsl":"y"}]}`)
		require.NoError(t, err)
		assert.Equal(t, []dslItem{
			{NL: "two_upstreams_one_fails", DSL: "a => b"},
			{NL: "x", DSL: "y"},
		}, got)
	})
	for name, content := range map[string]string{
		"empty items":       `{"items":[]}`,
		"dsl-only shape":    `{"dsl":["a => b"]}`,
		"not json":          `items: nope`,
		"wrong items type":  `{"items":"nope"}`,
		"empty json object": `{}`,
	} {
		t.Run(name, func(t *testing.T) {
			got, err := parseOpenAIItems(content)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "unrecognized OpenAI content shape")
			assert.Nil(t, got)
		})
	}
}

func TestDslCache_WriteReadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "dir", "cache.json")
	want := &dslCache{Hash: "deadbeef", Items: []dslItem{{NL: "n1", DSL: "d1"}, {NL: "n2", DSL: "d2"}}}
	require.NoError(t, writeDslCache(path, want))

	_, err := os.Stat(path + ".tmp")
	assert.True(t, os.IsNotExist(err), "temp file must be renamed away")

	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(raw), "\n  \"hash\": \"deadbeef\"", "cache is written indented")

	got := readDslCache(t, path)
	require.NotNil(t, got)
	assert.Equal(t, want, got)
}

func TestDslCache_ReadMissingOrCorrupt(t *testing.T) {
	dir := t.TempDir()
	assert.Nil(t, readDslCache(t, filepath.Join(dir, "missing.json")))

	corrupt := filepath.Join(dir, "corrupt.json")
	require.NoError(t, os.WriteFile(corrupt, []byte("{not json"), 0o600))
	assert.Nil(t, readDslCache(t, corrupt))

	legacy := filepath.Join(dir, "legacy.json")
	require.NoError(t, os.WriteFile(legacy, []byte(`{"hash":"h","dsl":["a => b"]}`), 0o600))
	got := readDslCache(t, legacy)
	require.NotNil(t, got)
	assert.Equal(t, []string{"a => b"}, got.DSL)
	assert.Empty(t, got.Items)
}

func TestDslCache_WriteErrors(t *testing.T) {
	c := &dslCache{Hash: "h"}

	t.Run("parent is a file", func(t *testing.T) {
		dir := t.TempDir()
		blocker := filepath.Join(dir, "file")
		require.NoError(t, os.WriteFile(blocker, nil, 0o600))
		require.Error(t, writeDslCache(filepath.Join(blocker, "cache.json"), c))
	})

	t.Run("temp path is a directory", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "cache.json")
		require.NoError(t, os.Mkdir(path+".tmp", 0o750))
		require.Error(t, writeDslCache(path, c))
	})

	t.Run("target is a non-empty directory", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "cache.json")
		require.NoError(t, os.MkdirAll(filepath.Join(path, "child"), 0o750))
		require.Error(t, writeDslCache(path, c))
	})
}

func TestDoOpenAIRequestWithRetries(t *testing.T) {
	t.Run("success sends auth and body", func(t *testing.T) {
		var gotAuth, gotCT, gotBody, gotURL, gotMethod string
		useFakeOpenAI(t, func(r *http.Request) (*http.Response, error) {
			gotAuth = r.Header.Get("Authorization")
			gotCT = r.Header.Get("Content-Type")
			gotURL = r.URL.String()
			gotMethod = r.Method
			b, _ := io.ReadAll(r.Body)
			gotBody = string(b)
			return openAIResponse(200, `{"ok":true}`, nil), nil
		})
		body, err := doOpenAIRequestWithRetries(t, "sk-test", []byte(`{"q":1}`))
		require.NoError(t, err)
		assert.Equal(t, `{"ok":true}`, string(body))
		assert.Equal(t, "Bearer sk-test", gotAuth)
		assert.Equal(t, "application/json", gotCT)
		assert.Equal(t, `{"q":1}`, gotBody)
		assert.Equal(t, "POST", gotMethod)
		assert.Equal(t, "https://api.openai.com/v1/chat/completions", gotURL)
	})

	for _, status := range []int{400, 401, 404} {
		t.Run(fmt.Sprintf("status %d is not retried", status), func(t *testing.T) {
			calls := 0
			useFakeOpenAI(t, func(r *http.Request) (*http.Response, error) {
				calls++
				return openAIResponse(status, "denied", nil), nil
			})
			_, err := doOpenAIRequestWithRetries(t, "k", []byte(`{}`))
			require.Error(t, err)
			assert.Equal(t, fmt.Sprintf("status %d: denied", status), err.Error())
			assert.Equal(t, 1, calls)
		})
	}

	t.Run("body read error on non-retryable status", func(t *testing.T) {
		calls := 0
		useFakeOpenAI(t, func(r *http.Request) (*http.Response, error) {
			calls++
			return &http.Response{StatusCode: 403, Header: http.Header{}, Body: failingBody{}}, nil
		})
		_, err := doOpenAIRequestWithRetries(t, "k", []byte(`{}`))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "read body error (status=403)")
		assert.Contains(t, err.Error(), "body broke")
		assert.Equal(t, 1, calls)
	})

	t.Run("body read error on 2xx is not success", func(t *testing.T) {
		calls := 0
		useFakeOpenAI(t, func(r *http.Request) (*http.Response, error) {
			calls++
			if calls == 1 {
				return &http.Response{StatusCode: 200, Header: http.Header{}, Body: failingBody{}}, nil
			}
			return openAIResponse(400, "stop", nil), nil
		})
		_, err := doOpenAIRequestWithRetries(t, "k", []byte(`{}`))
		require.Error(t, err)
		// A 200 with an unreadable body is not retryable by status, so the
		// read error is returned immediately.
		assert.Contains(t, err.Error(), "read body error (status=200)")
		assert.Equal(t, 1, calls)
	})

	t.Run("transport error then retry-after then success", func(t *testing.T) {
		var calls int
		var mu sync.Mutex
		useFakeOpenAI(t, func(r *http.Request) (*http.Response, error) {
			mu.Lock()
			defer mu.Unlock()
			calls++
			switch calls {
			case 1:
				return nil, errors.New("connection reset")
			case 2:
				return openAIResponse(429, "slow down", http.Header{"Retry-After": []string{"1"}}), nil
			default:
				return openAIResponse(200, "done", nil), nil
			}
		})
		body, err := doOpenAIRequestWithRetries(t, "k", []byte(`{}`))
		require.NoError(t, err)
		assert.Equal(t, "done", string(body))
		assert.Equal(t, 3, calls)
	})

	t.Run("5xx with unusable retry-after backs off then succeeds", func(t *testing.T) {
		calls := 0
		useFakeOpenAI(t, func(r *http.Request) (*http.Response, error) {
			calls++
			if calls == 1 {
				return openAIResponse(503, "busy", http.Header{"Retry-After": []string{"later"}}), nil
			}
			return openAIResponse(201, "created", nil), nil
		})
		body, err := doOpenAIRequestWithRetries(t, "k", []byte(`{}`))
		require.NoError(t, err)
		assert.Equal(t, "created", string(body))
		assert.Equal(t, 2, calls)
	})
}

func TestOpenAIGenerateChunk(t *testing.T) {
	cases := []struct {
		name    string
		resp    func(t *testing.T) *http.Response
		want    []dslItem
		wantErr string
	}{
		{
			name: "items parsed",
			resp: func(t *testing.T) *http.Response {
				return openAIResponse(200, chatCompletion(t, `{"items":[{"nl":"A B","dsl":" x => y "}]}`), nil)
			},
			want: []dslItem{{NL: "a_b", DSL: "x => y"}},
		},
		{
			name:    "http error",
			resp:    func(t *testing.T) *http.Response { return openAIResponse(400, "bad", nil) },
			wantErr: "status 400: bad",
		},
		{
			name:    "envelope not json",
			resp:    func(t *testing.T) *http.Response { return openAIResponse(200, "<html>", nil) },
			wantErr: "invalid character",
		},
		{
			name:    "no choices",
			resp:    func(t *testing.T) *http.Response { return openAIResponse(200, `{"choices":[]}`, nil) },
			wantErr: "no choices",
		},
		{
			name: "content in wrong shape",
			resp: func(t *testing.T) *http.Response {
				return openAIResponse(200, chatCompletion(t, `{"dsl":["x"]}`), nil)
			},
			wantErr: "unrecognized OpenAI content shape",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var sent map[string]any
			useFakeOpenAI(t, func(r *http.Request) (*http.Response, error) {
				require.NoError(t, json.NewDecoder(r.Body).Decode(&sent))
				return tc.resp(t), nil
			})
			got, err := openAIGenerateChunk(t, "k", "SPEC", []string{"scenario one"})
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
				assert.Nil(t, got)
			} else {
				require.NoError(t, err)
				assert.Equal(t, tc.want, got)
			}
			assert.Equal(t, "gpt-5", sent["model"])
			assert.Equal(t, map[string]any{"type": "json_object"}, sent["response_format"])
			msgs := sent["messages"].([]any)
			require.Len(t, msgs, 2)
			user := msgs[1].(map[string]any)
			assert.Equal(t, "user", user["role"])
			assert.Contains(t, user["content"], "scenario one")
			assert.Contains(t, user["content"], "SPEC")
		})
	}
}

func TestOpenAIReviewDsl(t *testing.T) {
	original := []dslItem{{NL: "n1", DSL: "d1"}, {NL: "n2", DSL: "d2"}}

	t.Run("no items", func(t *testing.T) {
		useFakeOpenAI(t, func(r *http.Request) (*http.Response, error) {
			t.Error("must not call OpenAI without items")
			return nil, errors.New("unexpected")
		})
		_, err := openAIReviewDsl(t, "k", "SPEC", nil)
		require.EqualError(t, err, "no items to review")
	})

	cases := []struct {
		name    string
		status  int
		body    string
		content string
		want    []dslItem
		wantErr string
	}{
		{
			name:    "corrected items returned as-is",
			content: `{"approved":false,"items":[{"nl":"n1","dsl":"D1"},{"nl":"n2","dsl":"D2"}],"notes":"fixed"}`,
			want:    []dslItem{{NL: "n1", DSL: "D1"}, {NL: "n2", DSL: "D2"}},
		},
		{
			name:    "dsl-only list mapped onto original NLs",
			content: `{"approved":true,"dsl":[" X1 ","X2"]}`,
			want:    []dslItem{{NL: "n1", DSL: "X1"}, {NL: "n2", DSL: "X2"}},
		},
		{
			name: "fallback parser keeps original NL when omitted",
			// approved is not a bool, so the strict decode fails and the
			// generic items parser takes over.
			content: `{"approved":"yes","items":[{"nl":"","dsl":"Y1"},{"nl":"Given NL","dsl":"Y2"}]}`,
			want:    []dslItem{{NL: "n1", DSL: "Y1"}, {NL: "given_nl", DSL: "Y2"}},
		},
		{
			name:    "item count mismatch",
			content: `{"approved":true,"items":[{"nl":"n1","dsl":"d1"}]}`,
			wantErr: "review returned unexpected shape",
		},
		{
			name:    "content not json",
			content: `looks good to me`,
			wantErr: "review returned unexpected shape",
		},
		{
			name:    "no choices",
			body:    `{"choices":[]}`,
			wantErr: "no choices in review",
		},
		{
			name:    "envelope not json",
			body:    `nope`,
			wantErr: "invalid character",
		},
		{
			name:    "http error",
			status:  422,
			body:    "unprocessable",
			wantErr: "status 422: unprocessable",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var userPrompt string
			useFakeOpenAI(t, func(r *http.Request) (*http.Response, error) {
				var sent struct {
					Messages []map[string]string `json:"messages"`
				}
				require.NoError(t, json.NewDecoder(r.Body).Decode(&sent))
				userPrompt = sent.Messages[1]["content"]
				status := tc.status
				if status == 0 {
					status = 200
				}
				body := tc.body
				if body == "" {
					body = chatCompletion(t, tc.content)
				}
				return openAIResponse(status, body, nil), nil
			})
			in := append([]dslItem(nil), original...)
			got, err := openAIReviewDsl(t, "k", "SPEC", in)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
				assert.Nil(t, got)
			} else {
				require.NoError(t, err)
				assert.Equal(t, tc.want, got)
			}
			assert.Contains(t, userPrompt, "NL: n1\nDSL: d1\nNL: n2\nDSL: d2\n")
			assert.Contains(t, userPrompt, "DSL Specification:\nSPEC")
		})
	}
}

// scenarioHash mirrors the cache key: sha256 of the JSON-encoded scenarios.
func scenarioHash(t *testing.T, scenarios []string) string {
	t.Helper()
	b, err := json.Marshal(scenarios)
	require.NoError(t, err)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func TestGenerateDslFromScenariosViaLLM_CacheHit(t *testing.T) {
	t.Chdir(t.TempDir())
	scenarios := []string{"one", "two"}
	cached := []dslItem{{NL: "one", DSL: "a => ok"}, {NL: "two", DSL: "b => ok"}}
	require.NoError(t, writeDslCache(filepath.Join("testdata", "ns.json"), &dslCache{Hash: scenarioHash(t, scenarios), Items: cached}))

	useFakeOpenAI(t, func(r *http.Request) (*http.Response, error) {
		t.Error("cache hit must not call OpenAI")
		return nil, errors.New("unexpected")
	})
	assert.Equal(t, cached, GenerateDslFromScenariosViaLLM(t, "ns", "SPEC", scenarios))
}

func TestGenerateDslFromScenariosViaLLM_RegeneratesAndCaches(t *testing.T) {
	scenarios := make([]string, 12)
	for i := range scenarios {
		scenarios[i] = fmt.Sprintf("Scenario %02d", i)
	}

	cases := []struct {
		name  string
		setup func(t *testing.T)
	}{
		{"no cache", func(t *testing.T) {}},
		{"stale hash", func(t *testing.T) {
			require.NoError(t, writeDslCache(filepath.Join("testdata", "ns.json"),
				&dslCache{Hash: "0000", Items: []dslItem{{NL: "old", DSL: "old"}}}))
		}},
		{"matching hash but legacy dsl-only cache", func(t *testing.T) {
			require.NoError(t, writeDslCache(filepath.Join("testdata", "ns.json"),
				&dslCache{Hash: scenarioHash(t, scenarios), DSL: []string{"legacy"}}))
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Chdir(t.TempDir())
			require.NoError(t, os.WriteFile(".env", []byte("UNRELATED_DSL_TEST_VAR=1\n"), 0o600))
			t.Setenv("OPENAI_API_KEY", "sk-from-env")
			tc.setup(t)

			var mu sync.Mutex
			var chunkSizes []int
			useFakeOpenAI(t, func(r *http.Request) (*http.Response, error) {
				assert.Equal(t, "Bearer sk-from-env", r.Header.Get("Authorization"))
				var sent struct {
					Messages []map[string]string `json:"messages"`
				}
				require.NoError(t, json.NewDecoder(r.Body).Decode(&sent))
				user := sent.Messages[1]["content"]
				var items []map[string]string
				for _, s := range scenarios {
					if strings.Contains(user, s+"\n") {
						items = append(items, map[string]string{"nl": s, "dsl": "dsl for " + s})
					}
				}
				mu.Lock()
				chunkSizes = append(chunkSizes, len(items))
				mu.Unlock()
				content, err := json.Marshal(map[string]any{"items": items})
				require.NoError(t, err)
				return openAIResponse(200, chatCompletion(t, string(content)), nil), nil
			})

			got := GenerateDslFromScenariosViaLLM(t, "ns", "SPEC", scenarios)
			require.Len(t, got, 12)
			assert.Equal(t, []int{10, 2}, chunkSizes, "scenarios are generated in chunks of 10")
			for i, s := range scenarios {
				assert.Equal(t, dslItem{NL: SanitizeTestName(s), DSL: "dsl for " + s}, got[i])
			}

			cache := readDslCache(t, filepath.Join("testdata", "ns.json"))
			require.NotNil(t, cache)
			assert.Equal(t, scenarioHash(t, scenarios), cache.Hash)
			assert.Equal(t, got, cache.Items)

			// Second call is served from the freshly written cache.
			useFakeOpenAI(t, func(r *http.Request) (*http.Response, error) {
				t.Error("fresh cache must not call OpenAI")
				return nil, errors.New("unexpected")
			})
			assert.Equal(t, got, GenerateDslFromScenariosViaLLM(t, "ns", "SPEC", scenarios))
		})
	}
}

func TestGenerateDslFromScenariosViaLLM_CacheWriteFailureStillReturnsItems(t *testing.T) {
	t.Chdir(t.TempDir())
	require.NoError(t, os.WriteFile(".env", nil, 0o600))
	// A regular file named testdata makes the cache directory uncreatable.
	require.NoError(t, os.WriteFile("testdata", nil, 0o600))
	t.Setenv("OPENAI_API_KEY", "k")
	useFakeOpenAI(t, func(r *http.Request) (*http.Response, error) {
		return openAIResponse(200, chatCompletion(t, `{"items":[{"nl":"only","dsl":"x => y"}]}`), nil), nil
	})
	got := GenerateDslFromScenariosViaLLM(t, "ns", "SPEC", []string{"only"})
	assert.Equal(t, []dslItem{{NL: "only", DSL: "x => y"}}, got)
	fi, err := os.Stat("testdata")
	require.NoError(t, err)
	assert.False(t, fi.IsDir())
}

func TestDoOpenAIRequestWithRetries_ExhaustsAttempts(t *testing.T) {
	if testing.Short() {
		t.Skip("sleeps ~5s honoring Retry-After")
	}
	t.Run("transport error on final attempt", func(t *testing.T) {
		calls := 0
		useFakeOpenAI(t, func(r *http.Request) (*http.Response, error) {
			calls++
			if calls < 6 {
				return openAIResponse(429, "busy", http.Header{"Retry-After": []string{"1"}}), nil
			}
			return nil, errors.New("final transport failure")
		})
		_, err := doOpenAIRequestWithRetries(t, "k", []byte(`{}`))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "final transport failure")
		assert.Equal(t, 6, calls, "gives up after 6 attempts")
	})
}
