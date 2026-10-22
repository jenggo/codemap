package mcp

import (
	"fmt"
	"path/filepath"
	"strings"

	"codemap/query"
	"codemap/render"
	"codemap/store"
	"codemap/workspace"
)

const (
	toolReplaceSymbolBody = "replace_symbol_body"
	toolInsertBefore      = "insert_before_symbol"
	toolInsertAfter       = "insert_after_symbol"
	toolSafeDelete        = "safe_delete_symbol"
	toolRenameSymbol      = "rename_symbol"
)

var writeTools = map[string]bool{
	toolReplaceSymbolBody: true,
	toolInsertBefore:      true,
	toolInsertAfter:       true,
	toolSafeDelete:        true,
	toolRenameSymbol:      true,
}

// writeRefusal is the single refusal every write tool returns while write mode
// is off, so the message names the flag that turns it on.
func writeRefusal(tool string) string {
	return fmt.Sprintf("Error: %s modifies files but write mode is off; start the server with --allow-write to enable it", tool)
}

// writeHandlers binds the mutating tools to this server, since they need the
// served roots and the reindex path that plain query handlers do not.
func (s *Server) writeHandlers() map[string]toolHandler {
	return map[string]toolHandler{
		toolReplaceSymbolBody: s.handleReplaceSymbolBody,
		toolInsertBefore:      s.insertHandler(false),
		toolInsertAfter:       s.insertHandler(true),
		toolSafeDelete:        s.handleSafeDeleteSymbol,
		toolRenameSymbol:      s.handleRenameSymbol,
	}
}

// allowedWriteTarget requires path, after symlink resolution, to live inside
// one of the allowed index roots, so a write can never escape the served tree.
func (s *Server) allowedWriteTarget(path string) error {
	realPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		return err
	}
	for _, root := range s.allowedRoots() {
		if realPath == root || strings.HasPrefix(realPath, root+string(filepath.Separator)) {
			return nil
		}
	}
	return fmt.Errorf("path %s is outside the allowed index roots (%s)",
		path, strings.Join(s.allowedRoots(), ", "))
}

// writeGuard resolves the symbol a write names and confines its file to the
// allowed roots before anything is read or written. It returns a refusal
// message when the file escapes containment, and no error for an unresolvable
// symbol, which the write itself reports with name suggestions.
func (s *Server) writeGuard(st *store.Store, qualifiedName string) (*store.Symbol, string) {
	sym, err := st.SymbolByName(qualifiedName)
	if err != nil {
		return nil, ""
	}
	if err := s.allowedWriteTarget(sym.PosFile); err != nil {
		return nil, fmt.Sprintf("Error: %v", err)
	}
	return sym, ""
}

// refreshAfterWrite reindexes the repo the written symbol belongs to. A reindex
// failure is recorded on the result rather than returned, because the write
// itself already succeeded.
func (s *Server) refreshAfterWrite(st *store.Store, sym *store.Symbol, result *query.WriteResult) {
	if sym == nil {
		return
	}
	if err := s.reindexRepo(st, sym.Repo); err != nil {
		result.ReindexError = err.Error()
	}
}

// reindexRepo rebuilds one repo's rows. An empty module path means a
// single-repo index, which is rebuilt from the served repo root.
func (s *Server) reindexRepo(st *store.Store, modulePath string) error {
	if modulePath == "" {
		return s.autoIndexMemoized(s.repoPath)
	}
	repos, err := st.ListRepos()
	if err != nil {
		return err
	}
	for _, r := range repos {
		if r.ModulePath == modulePath {
			_, err := workspace.ReindexRepo(s.resolveDBPath(), modulePath, r.Dir)
			return err
		}
	}
	return fmt.Errorf("repo %s is not registered in this index", modulePath)
}

func (s *Server) handleReplaceSymbolBody(st *store.Store, _ []query.Option, renderOpts []render.Option, args map[string]any) (string, any, bool) {
	qn := requiredString(args, keyQualifiedName)
	if qn == "" {
		return errQualifiedNameRequired, nil, true
	}
	sym, refusal := s.writeGuard(st, qn)
	if refusal != "" {
		return refusal, nil, true
	}
	includeDoc, _ := args[keyIncludeDoc].(bool)
	result, err := query.ReplaceSymbolBody(st, qn, requiredString(args, keyBody), includeDoc)
	if err != nil {
		return fmt.Sprintf("Error: %v", err), nil, true
	}
	s.refreshAfterWrite(st, sym, result)
	return render.RenderWriteResult(result, renderOpts...), result, false
}

// insertHandler builds the before/after insert handler: both share one write
// path and differ only in which boundary they target.
func (s *Server) insertHandler(after bool) toolHandler {
	return func(st *store.Store, _ []query.Option, renderOpts []render.Option, args map[string]any) (string, any, bool) {
		qn := requiredString(args, keyQualifiedName)
		if qn == "" {
			return errQualifiedNameRequired, nil, true
		}
		sym, refusal := s.writeGuard(st, qn)
		if refusal != "" {
			return refusal, nil, true
		}
		result, err := query.InsertSymbol(st, qn, requiredString(args, keyText), after)
		if err != nil {
			return fmt.Sprintf("Error: %v", err), nil, true
		}
		s.refreshAfterWrite(st, sym, result)
		return render.RenderWriteResult(result, renderOpts...), result, false
	}
}

func (s *Server) handleSafeDeleteSymbol(st *store.Store, _ []query.Option, renderOpts []render.Option, args map[string]any) (string, any, bool) {
	qn := requiredString(args, keyQualifiedName)
	if qn == "" {
		return errQualifiedNameRequired, nil, true
	}
	sym, refusal := s.writeGuard(st, qn)
	if refusal != "" {
		return refusal, nil, true
	}
	allowExported, _ := args["allow_exported"].(bool)
	allowTestOnly, _ := args["allow_test_only"].(bool)
	result, err := query.DeleteSymbol(st, qn, allowExported, allowTestOnly)
	if err != nil {
		return fmt.Sprintf("Error: %v", err), nil, true
	}
	s.refreshAfterWrite(st, sym, result)
	return render.RenderWriteResult(result, renderOpts...), result, false
}

// handleRenameSymbol validates the whole rename, confines every file it would
// rewrite, and only then applies it, so a refusal or a containment failure
// leaves every file untouched.
func (s *Server) handleRenameSymbol(st *store.Store, _ []query.Option, _ []render.Option, args map[string]any) (string, any, bool) {
	qn := requiredString(args, keyQualifiedName)
	if qn == "" {
		return errQualifiedNameRequired, nil, true
	}
	newName := requiredString(args, "new_name")
	if newName == "" {
		return "Error: new_name is required", nil, true
	}
	force, _ := args["force"].(bool)
	plan, err := query.RenameSymbol(st, qn, newName, force)
	if err != nil {
		return fmt.Sprintf("Error: %v", err), nil, true
	}
	for _, file := range plan.Files {
		if err := s.allowedWriteTarget(file.File); err != nil {
			return fmt.Sprintf("Error: %v", err), nil, true
		}
	}
	result, err := query.ApplyRename(plan)
	if err != nil {
		return fmt.Sprintf("Error: %v", err), nil, true
	}
	s.refreshRenamedRepos(st, result)
	return render.RenderRenameResult(result), result, false
}

// refreshRenamedRepos reindexes every repo a rename touched. A failure is
// recorded on the result rather than returned, because the files were written.
func (s *Server) refreshRenamedRepos(st *store.Store, result *query.RenameResult) {
	for _, repo := range result.Repos {
		if err := s.reindexRepo(st, repo); err != nil {
			result.ReindexError = err.Error()
			return
		}
	}
}

// writeToolsList describes the mutating tools. Each description states that the
// tool modifies files, so a client can require confirmation from the catalog
// entry alone.
func writeToolsList() []map[string]any {
	return []map[string]any{
		toolDef(toolReplaceSymbolBody,
			"Replace a symbol's declaration (or its doc comment when include_doc is true) with new Go source. This tool modifies files on disk: both the replacement and the resulting file must parse, otherwise nothing is written.",
			map[string]any{
				keyQualifiedName: stringProp("Fully qualified symbol name"),
				keyBody:          stringProp("Replacement Go source for the whole declaration (for a method, include the receiver clause)"),
				keyIncludeDoc:    boolProp("Replace the doc comment too (default false preserves it)"),
			},
			keyQualifiedName, keyBody),
		toolDef(toolInsertBefore,
			"Insert Go source immediately before a symbol's declaration. This tool modifies files on disk: the text must parse and the file must stay parseable, and exactly one blank line separates the inserted text from its neighbours.",
			map[string]any{
				keyQualifiedName: stringProp("Fully qualified symbol name to insert before"),
				keyText:          stringProp("Go source to insert"),
			},
			keyQualifiedName, keyText),
		toolDef(toolInsertAfter,
			"Insert Go source immediately after a symbol's declaration. This tool modifies files on disk: the text must parse and the file must stay parseable, and exactly one blank line separates the inserted text from its neighbours.",
			map[string]any{
				keyQualifiedName: stringProp("Fully qualified symbol name to insert after"),
				keyText:          stringProp("Go source to insert"),
			},
			keyQualifiedName, keyText),
		toolDef(toolSafeDelete,
			"Delete a symbol's declaration, but only when the index records no incoming calls, references, satisfies, or embeds edges. This tool modifies files on disk: it refuses when the symbol is referenced (listing the referrers), exported, or a member of a grouped var/const block.",
			map[string]any{
				keyQualifiedName:  stringProp("Fully qualified symbol name"),
				"allow_exported":  boolProp("Delete an exported symbol (default false: a consumer outside the index may use it)"),
				"allow_test_only": boolProp("Delete when every reference is test code (default false)"),
			},
			keyQualifiedName),
		toolDef(toolRenameSymbol,
			"Rename a symbol at its declaration and at every indexed reference site, across packages. This tool modifies files on disk: the sites come from the index, so it refuses on a name collision, on a cross-package unexported reference, on a site whose recorded position no longer holds the name, and (unless force) on a site a same-named local could shadow. Every file is validated in memory before any file is written.",
			map[string]any{
				keyQualifiedName: stringProp("Fully qualified name of the symbol to rename"),
				"new_name":       stringProp("New identifier for the symbol"),
				"force":          boolProp("Rename sites that a same-named local could shadow (default false: refuse and list them)"),
			},
			keyQualifiedName, "new_name"),
	}
}
