package main

// Scoring for each utility provider. System One providers answer one noul
// question per candidate; model providers (Bedrock, OpenAI-compatible) score
// every candidate 0-100 against the same rubric in one reply, scaled to [0,1].
import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"
)

// utilityInputLimit: UTF-8 byte length is a conservative bound on text tokens. Never truncate.
const utilityInputLimit = 32000

var utilitySystemPrompt = "You judge which saved memories an AI coding agent should be shown for the user's current request.\n" +
	"Question for each memory: " + utilityQuestion + "\n" +
	"High score when: " + utilityTrue + "\n" +
	"Low score when: " + utilityFalse + "\n" +
	"Score every memory independently from 0 (certainly not useful) to 100 (certainly useful). " +
	`Reply with only a JSON object mapping each memory id to its integer score, e.g. {"candidate_0": 12, "candidate_1": 85}.`

// scoreUtility returns one score in [0,1] per memory, in order, and the input tokens the provider reported.
func scoreUtility(ctx context.Context, c utilityConfig, key, task string, memories []string) ([]float64, int, error) {
	switch c.Provider {
	case providerJev, providerSystemOne:
		request := utilityRequest{Model: c.Model, State: map[string]string{"task": task}, Questions: map[string]utilityQuestionBody{}}
		for i, memory := range memories {
			request.Questions[fmt.Sprintf("candidate_%d", i)] = utilityQuestionBody{Type: "noul", Instructions: map[string]string{"memory": memory, "question": utilityQuestion}, Criteria: map[string]string{"true": utilityTrue, "false": utilityFalse}}
		}
		body, _ := json.Marshal(request)
		if len(body) > utilityInputLimit {
			return nil, 0, errors.New("input_too_long")
		}
		response, err := requestSystemOne(ctx, c, key, body, len(memories))
		if err != nil {
			return nil, 0, err
		}
		scores := make([]float64, len(memories))
		for i := range memories {
			scores[i] = *response.Answers[fmt.Sprintf("candidate_%d", i)].Score
		}
		return scores, response.Usage.InputTokens, nil
	case providerBedrock, providerOpenAI:
		user, err := memoriesPrompt(task, memories)
		if err != nil {
			return nil, 0, err
		}
		var body []byte
		if c.Provider == providerBedrock {
			body, _ = json.Marshal(map[string]any{
				"system":          []map[string]string{{"text": utilitySystemPrompt}},
				"messages":        []map[string]any{{"role": "user", "content": []map[string]string{{"text": user}}}},
				"inferenceConfig": map[string]int{"maxTokens": 600},
			})
		} else {
			body, _ = json.Marshal(map[string]any{
				"model":    c.Model,
				"messages": []map[string]string{{"role": "system", "content": utilitySystemPrompt}, {"role": "user", "content": user}},
			})
		}
		endpoint := c.Endpoint
		if c.Provider == providerOpenAI {
			endpoint += "/chat/completions"
		}
		raw, err := postJSON(ctx, endpoint, key, body)
		if err != nil {
			return nil, 0, err
		}
		text, tokens, err := modelReply(c.Provider, raw)
		if err != nil {
			return nil, 0, err
		}
		scores, err := parseScores(text, len(memories))
		return scores, tokens, err
	}
	return nil, 0, errors.New("utility_disabled")
}

func memoriesPrompt(task string, memories []string) (string, error) {
	type memory struct {
		ID     string `json:"id"`
		Memory string `json:"memory"`
	}
	list := make([]memory, len(memories))
	for i, m := range memories {
		list[i] = memory{fmt.Sprintf("candidate_%d", i), m}
	}
	b, _ := json.Marshal(map[string]any{"task": task, "memories": list})
	if len(b)+len(utilitySystemPrompt) > utilityInputLimit {
		return "", errors.New("input_too_long")
	}
	return string(b), nil
}

// modelReply extracts the reply text and input-token count from a Converse or chat-completions response.
func modelReply(provider string, raw []byte) (string, int, error) {
	if provider == providerBedrock {
		var r struct {
			Output struct {
				Message struct {
					Content []struct {
						Text string `json:"text"`
					} `json:"content"`
				} `json:"message"`
			} `json:"output"`
			Usage struct {
				InputTokens int `json:"inputTokens"`
			} `json:"usage"`
		}
		if json.Unmarshal(raw, &r) != nil {
			return "", 0, errors.New("response_invalid")
		}
		var b strings.Builder
		for _, part := range r.Output.Message.Content {
			b.WriteString(part.Text)
		}
		return b.String(), r.Usage.InputTokens, nil
	}
	var r struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens int `json:"prompt_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(raw, &r) != nil || len(r.Choices) == 0 {
		return "", 0, errors.New("response_invalid")
	}
	return r.Choices[0].Message.Content, r.Usage.PromptTokens, nil
}

var scoreObject = regexp.MustCompile(`\{[^{}]*\}`)

// parseScores reads the first JSON object in the reply: every candidate_i must
// have a number in [0,100]; anything else is a failed request, not a guess.
func parseScores(text string, count int) ([]float64, error) {
	m := scoreObject.FindString(text)
	if m == "" {
		return nil, errors.New("response_invalid")
	}
	var got map[string]any
	if json.Unmarshal([]byte(m), &got) != nil {
		return nil, errors.New("response_invalid")
	}
	scores := make([]float64, count)
	for i := range scores {
		v, ok := got[fmt.Sprintf("candidate_%d", i)].(float64)
		if !ok || math.IsNaN(v) || v < 0 || v > 100 {
			return nil, errors.New("response_invalid")
		}
		scores[i] = v / 100
	}
	return scores, nil
}
