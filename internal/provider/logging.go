package provider

import (
	"context"
	"slices"
	"strings"

	"github.com/hashicorp/terraform-plugin-log/tflog"
)

func logPath(path []string, apiKey string) string {
	p := slices.Clone(path)
	for i, token := range p {
		lower := strings.ToLower(token)
		if strings.Contains(lower, "password") || strings.Contains(lower, "secret") ||
			strings.Contains(lower, "key") || strings.Contains(lower, "token") ||
			lower == "authentication" || lower == "community" {
			for j := i + 1; j < len(p); j++ {
				p[j] = "[REDACTED]"
			}
			break
		}
	}
	out := formatPath(p)
	if apiKey != "" {
		out = strings.ReplaceAll(out, apiKey, "[REDACTED]")
	}
	return out
}

func logBatch(ctx context.Context, batch Batch, phase, change, name, apiKey string) {
	if apiKey != "" {
		ctx = tflog.MaskLogStrings(ctx, apiKey)
	}
	ctx = tflog.SetField(ctx, "phase", phase)
	ctx = tflog.SetField(ctx, "change", change)
	ctx = tflog.SetField(ctx, "resource", name)
	tflog.Debug(ctx, "VyOS operation batch", map[string]any{"operation_count": len(batch.Operations)})
	for _, resolution := range batch.Resolutions {
		tflog.Debug(ctx, "Resolved VyOS delete path", map[string]any{
			"source_set_path": logPath(resolution.Source, apiKey), "delete_path": logPath(resolution.Path, apiKey), "reason": resolution.Reason,
		})
	}
	for i, operation := range batch.Operations {
		verb := strings.ToUpper(operation.Op)
		tflog.Debug(ctx, "VyOS operation", map[string]any{
			"index": i, "operation": verb, "command": verb + " " + logPath(operation.Path, apiKey),
		})
	}
}
