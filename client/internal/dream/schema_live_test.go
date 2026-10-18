package dream

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/tacit/client/internal/modelrun"
)

// Opt-in native protocol check; it supplies no store, transcript or project data.
func TestInstalledClaudeDreamSchema(t *testing.T) {
	model := os.Getenv("TACIT_SCHEMA_SMOKE_MODEL")
	if model == "" {
		t.Skip("set TACIT_SCHEMA_SMOKE_MODEL for one synthetic live call")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	route := modelrun.Route{Runner: "claude", Model: model}
	ctx = modelrun.WithRoute(modelrun.WithJSONSchema(ctx, outputSchema), route)
	prompt := fmt.Sprintf(Prompt, "synthetic", "synthetic", "E: The synthetic fixture command `sample --mode=read` failed. A: Adding --mode=read fixes the synthetic fixture. Record one machine fact about that fixture.")
	result, err := modelrun.Run(ctx, "dream", model, prompt, NewSessionID())
	if err != nil {
		t.Fatal(err)
	}
	rows, err := ParseChecked(result.Text)
	if err != nil || len(rows) == 0 || !result.Structured || !strings.Contains(rows[0].Description, "sample") {
		t.Fatalf("schema reply: %+v %v", result, err)
	}
	t.Logf("schema-validated synthetic extraction: facts=%d input_tokens=%d reported_cost=$%.6f", len(rows), result.InputTokens, result.CostUSD)
}
