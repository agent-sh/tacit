package modelrun

import "context"

type jsonSchemaKey struct{}

// WithJSONSchema requests schema-validated output from the Claude adapter.
// Other adapters keep their existing text protocol and caller validation.
func WithJSONSchema(ctx context.Context, schema string) context.Context {
	return context.WithValue(ctx, jsonSchemaKey{}, schema)
}
