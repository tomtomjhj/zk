package lsp

import (
	"fmt"
	"io/ioutil"
	"path/filepath"
	"strings"

	"github.com/mickael-menu/zk/internal/core"
	"github.com/mickael-menu/zk/internal/util"
	"github.com/mickael-menu/zk/internal/util/errors"
	"github.com/mickael-menu/zk/internal/util/opt"
	strutil "github.com/mickael-menu/zk/internal/util/strings"
	"github.com/tliron/glsp"
	protocol "github.com/tliron/glsp/protocol_3_16"
	glspserv "github.com/tliron/glsp/server"
	"github.com/tliron/kutil/logging"
	_ "github.com/tliron/kutil/logging/simple"
)

// Server holds the state of the Language Server.
type Server struct {
	server    *glspserv.Server
	notebooks *core.NotebookStore
	documents map[protocol.DocumentUri]*document
	fs        core.FileStorage
	logger    util.Logger
}

// ServerOpts holds the options to create a new Server.
type ServerOpts struct {
	Name      string
	Version   string
	LogFile   opt.String
	Logger    *util.ProxyLogger
	Notebooks *core.NotebookStore
	FS        core.FileStorage
}

// NewServer creates a new Server instance.
func NewServer(opts ServerOpts) *Server {
	fs := opts.FS
	debug := !opts.LogFile.IsNull()
	if debug {
		logging.Configure(10, opts.LogFile.Value)
	}

	workspace := newWorkspace()
	handler := protocol.Handler{}
	server := &Server{
		server:    glspserv.NewServer(&handler, opts.Name, debug),
		notebooks: opts.Notebooks,
		documents: map[string]*document{},
		fs:        fs,
	}

	// Redirect zk's logger to GLSP's to avoid breaking the JSON-RPC protocol
	// with unwanted output.
	if opts.Logger != nil {
		opts.Logger.Logger = newGlspLogger(server.server.Log)
		server.logger = opts.Logger
	}

	var clientCapabilities protocol.ClientCapabilities

	handler.Initialize = func(context *glsp.Context, params *protocol.InitializeParams) (interface{}, error) {
		clientCapabilities = params.Capabilities

		if len(params.WorkspaceFolders) > 0 {
			for _, f := range params.WorkspaceFolders {
				workspace.addFolder(f.URI)
			}
		} else if params.RootURI != nil {
			workspace.addFolder(*params.RootURI)
		} else if params.RootPath != nil {
			workspace.addFolder(*params.RootPath)
		}

		// To see the logs with coc.nvim, run :CocCommand workspace.showOutput
		// https://github.com/neoclide/coc.nvim/wiki/Debug-language-server#using-output-channel
		if params.Trace != nil {
			protocol.SetTraceValue(*params.Trace)
		}

		capabilities := handler.CreateServerCapabilities()
		capabilities.HoverProvider = true

		capabilities.TextDocumentSync = protocol.TextDocumentSyncKindIncremental
		capabilities.DocumentLinkProvider = &protocol.DocumentLinkOptions{
			ResolveProvider: boolPtr(true),
		}

		triggerChars := []string{"[", "#", ":"}

		capabilities.CompletionProvider = &protocol.CompletionOptions{
			TriggerCharacters: triggerChars,
		}

		capabilities.DefinitionProvider = boolPtr(true)

		return protocol.InitializeResult{
			Capabilities: capabilities,
			ServerInfo: &protocol.InitializeResultServerInfo{
				Name:    opts.Name,
				Version: &opts.Version,
			},
		}, nil
	}

	handler.Initialized = func(context *glsp.Context, params *protocol.InitializedParams) error {
		return nil
	}

	handler.Shutdown = func(context *glsp.Context) error {
		protocol.SetTraceValue(protocol.TraceValueOff)
		return nil
	}

	handler.SetTrace = func(context *glsp.Context, params *protocol.SetTraceParams) error {
		protocol.SetTraceValue(params.Value)
		return nil
	}

	handler.WorkspaceDidChangeWorkspaceFolders = func(context *glsp.Context, params *protocol.DidChangeWorkspaceFoldersParams) error {
		for _, f := range params.Event.Added {
			workspace.addFolder(f.URI)
		}
		for _, f := range params.Event.Removed {
			workspace.removeFolder(f.URI)
		}
		return nil
	}

	handler.TextDocumentDidOpen = func(context *glsp.Context, params *protocol.DidOpenTextDocumentParams) error {
		langID := params.TextDocument.LanguageID
		if langID != "markdown" && langID != "vimwiki" {
			return nil
		}

		path := fs.Canonical(strings.TrimPrefix(params.TextDocument.URI, "file://"))

		server.documents[params.TextDocument.URI] = &document{
			Path:    path,
			Content: params.TextDocument.Text,
			Log:     server.server.Log,
		}

		return nil
	}

	handler.TextDocumentDidChange = func(context *glsp.Context, params *protocol.DidChangeTextDocumentParams) error {
		doc, ok := server.documents[params.TextDocument.URI]
		if !ok {
			return nil
		}

		doc.ApplyChanges(params.ContentChanges)
		return nil
	}

	handler.TextDocumentDidClose = func(context *glsp.Context, params *protocol.DidCloseTextDocumentParams) error {
		delete(server.documents, params.TextDocument.URI)
		return nil
	}

	handler.TextDocumentDidSave = func(context *glsp.Context, params *protocol.DidSaveTextDocumentParams) error {
		return nil
	}

	handler.TextDocumentCompletion = func(context *glsp.Context, params *protocol.CompletionParams) (interface{}, error) {
		triggerChar := params.Context.TriggerCharacter
		if params.Context.TriggerKind != protocol.CompletionTriggerKindTriggerCharacter || triggerChar == nil {
			return nil, nil
		}

		doc, ok := server.documents[params.TextDocument.URI]
		if !ok {
			return nil, nil
		}

		notebook, err := server.notebookOf(doc)
		if err != nil {
			return nil, err
		}

		switch *triggerChar {
		case "#":
			if notebook.Config.Format.Markdown.Hashtags {
				return server.buildTagCompletionList(notebook, "#")
			}
		case ":":
			if notebook.Config.Format.Markdown.ColonTags {
				return server.buildTagCompletionList(notebook, ":")
			}
		case "[":
			if doc.LookBehind(params.Position, 2) == "[[" {
				return server.buildLinkCompletionList(doc, notebook, params)
			}
		}

		return nil, nil
	}

	handler.TextDocumentHover = func(context *glsp.Context, params *protocol.HoverParams) (*protocol.Hover, error) {
		doc, ok := server.documents[params.TextDocument.URI]
		if !ok {
			return nil, nil
		}

		link, err := doc.DocumentLinkAt(params.Position)
		if link == nil || err != nil {
			return nil, err
		}

		notebook, err := server.notebookOf(doc)
		if err != nil {
			return nil, err
		}

		target, err := server.targetForHref(link.Href, doc, notebook)
		if err != nil || target == "" || strutil.IsURL(target) {
			return nil, err
		}

		target = strings.TrimPrefix(target, "file://")
		contents, err := ioutil.ReadFile(target)
		if err != nil {
			return nil, err
		}

		return &protocol.Hover{
			Contents: protocol.MarkupContent{
				Kind:  protocol.MarkupKindMarkdown,
				Value: string(contents),
			},
		}, nil
	}

	handler.TextDocumentDocumentLink = func(context *glsp.Context, params *protocol.DocumentLinkParams) ([]protocol.DocumentLink, error) {
		doc, ok := server.documents[params.TextDocument.URI]
		if !ok {
			return nil, nil
		}

		links, err := doc.DocumentLinks()
		if err != nil {
			return nil, err
		}

		notebook, err := server.notebookOf(doc)
		if err != nil {
			return nil, err
		}

		documentLinks := []protocol.DocumentLink{}
		for _, link := range links {
			target, err := server.targetForHref(link.Href, doc, notebook)
			if target == "" || err != nil {
				continue
			}

			documentLinks = append(documentLinks, protocol.DocumentLink{
				Range:  link.Range,
				Target: &target,
			})
		}

		return documentLinks, err
	}

	handler.TextDocumentDefinition = func(context *glsp.Context, params *protocol.DefinitionParams) (interface{}, error) {
		doc, ok := server.documents[params.TextDocument.URI]
		if !ok {
			return nil, nil
		}

		link, err := doc.DocumentLinkAt(params.Position)
		if link == nil || err != nil {
			return nil, err
		}

		notebook, err := server.notebookOf(doc)
		if err != nil {
			return nil, err
		}

		target, err := server.targetForHref(link.Href, doc, notebook)
		if link == nil || target == "" || err != nil {
			return nil, err
		}

		// FIXME: Waiting for https://github.com/tliron/glsp/pull/3 to be
		// merged before using LocationLink.
		if false && isTrue(clientCapabilities.TextDocument.Definition.LinkSupport) {
			return protocol.LocationLink{
				OriginSelectionRange: &link.Range,
				TargetURI:            target,
			}, nil
		} else {
			return protocol.Location{
				URI: target,
			}, nil
		}
	}

	return server
}

func (s *Server) notebookOf(doc *document) (*core.Notebook, error) {
	return s.notebooks.Open(doc.Path)
}

// targetForHref returns the LSP documentUri for the note at the given HREF.
func (s *Server) targetForHref(href string, doc *document, notebook *core.Notebook) (string, error) {
	if strutil.IsURL(href) {
		return href, nil
	} else {
		path := filepath.Clean(filepath.Join(filepath.Dir(doc.Path), href))
		path, err := filepath.Rel(notebook.Path, path)
		if err != nil {
			return "", errors.Wrapf(err, "failed to resolve href: %s", href)
		}
		note, err := notebook.FindByHref(path)
		if err != nil {
			s.logger.Printf("findByHref(%s): %s", href, err.Error())
			return "", err
		}
		if note == nil {
			return "", nil
		}
		return "file://" + filepath.Join(notebook.Path, note.Path), nil
	}
}

// Run starts the Language Server in stdio mode.
func (s *Server) Run() error {
	return errors.Wrap(s.server.RunStdio(), "lsp")
}

func (s *Server) buildTagCompletionList(notebook *core.Notebook, triggerChar string) ([]protocol.CompletionItem, error) {
	tags, err := notebook.FindCollections(core.CollectionKindTag)
	if err != nil {
		return nil, err
	}

	var items []protocol.CompletionItem
	for _, tag := range tags {
		items = append(items, protocol.CompletionItem{
			Label:      tag.Name,
			InsertText: s.buildInsertForTag(tag.Name, triggerChar, notebook.Config),
			Detail:     stringPtr(fmt.Sprintf("%d %s", tag.NoteCount, strutil.Pluralize("note", tag.NoteCount))),
		})
	}

	return items, nil
}

func (s *Server) buildInsertForTag(name string, triggerChar string, config core.Config) *string {
	switch triggerChar {
	case ":":
		name += ":"
	case "#":
		if strings.Contains(name, " ") {
			if config.Format.Markdown.MultiwordTags {
				name += "#"
			} else {
				name = strings.ReplaceAll(name, " ", "\\ ")
			}
		}
	}
	return &name
}

func (s *Server) buildLinkCompletionList(doc *document, notebook *core.Notebook, params *protocol.CompletionParams) ([]protocol.CompletionItem, error) {
	linkFormatter, err := notebook.NewLinkFormatter()
	if err != nil {
		return nil, err
	}

	notes, err := notebook.FindNotes(core.NoteFindOpts{})
	if err != nil {
		return nil, err
	}

	var items []protocol.CompletionItem
	for _, note := range notes {
		textEdit, err := s.buildTextEditForLink(notebook, note, doc, params.Position, linkFormatter)
		if err != nil {
			s.logger.Err(errors.Wrapf(err, "failed to build TextEdit for note at %s", note.Path))
			continue
		}

		label := note.Title
		if label == "" {
			label = note.Path
		}

		items = append(items, protocol.CompletionItem{
			Label:    label,
			TextEdit: textEdit,
			Documentation: protocol.MarkupContent{
				Kind:  protocol.MarkupKindMarkdown,
				Value: note.RawContent,
			},
		})
	}

	return items, nil
}

func (s *Server) buildTextEditForLink(notebook *core.Notebook, note core.ContextualNote, document *document, pos protocol.Position, linkFormatter core.LinkFormatter) (interface{}, error) {
	path := filepath.Join(notebook.Path, note.Path)
	path = s.fs.Canonical(path)
	path, err := filepath.Rel(filepath.Dir(document.Path), path)
	if err != nil {
		path = note.Path
	}

	link, err := linkFormatter(path, note.Title)
	if err != nil {
		return nil, err
	}

	// Overwrite [[ trigger
	start := pos
	start.Character -= 2

	return protocol.TextEdit{
		Range: protocol.Range{
			Start: start,
			End:   pos,
		},
		NewText: link,
	}, nil
}

func positionInRange(content string, rng protocol.Range, pos protocol.Position) bool {
	start, end := rng.IndexesIn(content)
	i := pos.IndexIn(content)
	return i >= start && i <= end
}

func boolPtr(v bool) *bool {
	b := v
	return &b
}

func isTrue(v *bool) bool {
	return v != nil && *v == true
}

func isFalse(v *bool) bool {
	return v == nil || *v == false
}

func stringPtr(v string) *string {
	s := v
	return &s
}
