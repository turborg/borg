package tools

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
)

// rootKey carries the trusted-directory boundary through the tool-call context.
type rootKey struct{}

// WithRoot confines the mutating file tools (write_file, edit_file) to root —
// edits to paths outside it are refused. An empty root disables the boundary
// (no directory trust granted ⇒ unrestricted, e.g. one-shot without a decision).
func WithRoot(ctx context.Context, root string) context.Context {
	if root == "" {
		return ctx
	}
	return context.WithValue(ctx, rootKey{}, root)
}

func rootFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(rootKey{}).(string); ok {
		return v
	}
	return ""
}

// guardPath rejects path when a trusted root is set and path falls outside it.
// The comparison is lexical (after making the path absolute); it doesn't resolve
// symlinks, so it's a guard rail, not a sandbox.
func guardPath(ctx context.Context, path string) error {
	root := rootFromContext(ctx)
	if root == "" {
		return nil
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(root, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("%s is outside the trusted directory %s — borg may only edit files there", path, root)
	}
	return nil
}
