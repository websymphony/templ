package lspcmd

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"strings"
	"sync"

	"github.com/a-h/templ/generator"
	"github.com/a-h/templ/parser"
	"github.com/sourcegraph/go-lsp"
	"github.com/sourcegraph/jsonrpc2"
	"go.uber.org/zap"
)

type Proxy struct {
	log              *zap.Logger
	gopls            *jsonrpc2.Conn
	client           *jsonrpc2.Conn
	documentContents *documentContents
	sourceMapCache   *sourceMapCache
	toClient         chan toClientRequest
	context          context.Context
}

// NewProxy returns a new proxy to send messages from the client to and from gopls,
// however, init needs to be called before it is usable.
func NewProxy(logger *zap.Logger) (p *Proxy) {
	return &Proxy{
		log:              logger,
		documentContents: newDocumentContents(logger),
		sourceMapCache:   newSourceMapCache(),
		// Prevent trying to send to the client when message handling is taking place.
		// The proxy can place up to 32 requests onto the toClient buffered channel
		// during handling.
		toClient: make(chan toClientRequest, 32),
	}
}

// Init the proxy.
func (p *Proxy) Init(ctx context.Context, client, gopls *jsonrpc2.Conn) {
	p.context = ctx
	p.client = client
	p.gopls = gopls
	go func() {
		p.log.Info("sendToClient: starting up")
		for r := range p.toClient {
			r := r
			p.sendToClient(r)
		}
		p.log.Info("sendToClient: closed")
	}()
}

type toClientRequest struct {
	Method string
	Notif  bool
	Params interface{}
}

// sendToClient should not be called directly. Instead, send a message to the non-blocking
// toClient channel.
func (p *Proxy) sendToClient(r toClientRequest) {
	p.log.Info("sendToClient: sending", zap.String("method", r.Method))
	if r.Notif {
		err := p.client.Notify(p.context, r.Method, r.Params)
		if err != nil {
			p.log.Error("sendToClient: error", zap.String("type", "notification"), zap.String("method", r.Method), zap.Error(err))
			return
		}
	} else {
		var result map[string]interface{}
		err := p.client.Call(p.context, r.Method, r.Params, &result)
		if err != nil {
			p.log.Error("sendToClient: error", zap.String("type", "call"), zap.String("method", r.Method), zap.Error(err))
			return
		}
	}
	p.log.Info("sendToClient: success", zap.String("method", r.Method))
}

func (p *Proxy) proxyFromGoplsToClient(ctx context.Context, conn *jsonrpc2.Conn, r *jsonrpc2.Request) {
	p.log.Info("gopls -> client", zap.String("method", r.Method), zap.Bool("notif", r.Notif))
	if r.Notif {
		var err error
		switch r.Method {
		case "window/showMessage":
			if p.shouldSuppressWindowShowMessage(r) {
				return
			}
		case "textDocument/publishDiagnostics":
			err = p.rewriteGoplsPublishDiagnostics(r)
		}
		if err != nil {
			p.log.Error("gopls -> client: error rewriting notification", zap.Error(err))
			return
		}
		err = p.client.Notify(ctx, r.Method, r.Params)
		if err != nil {
			p.log.Error("gopls -> client: notification: send error", zap.Error(err))
			return
		}
		p.log.Info("gopls -> client: notification: complete")
	} else {
		var result map[string]interface{}
		err := p.client.Call(ctx, r.Method, &r.Params, &result)
		if err != nil {
			p.log.Error("gopls -> client: call: error", zap.Error(err))
		}
		p.log.Info("gopls -> client -> gopls", zap.String("method", r.Method), zap.Any("reply", result))
		err = conn.Reply(ctx, r.ID, result)
		if err != nil {
			p.log.Error("gopls -> client -> gopls: call reply: error", zap.Error(err))
		}
		p.log.Info("gopls -> client: call: complete", zap.String("method", r.Method), zap.Bool("notif", r.Notif))
	}
	p.log.Info("gopls -> client: complete", zap.String("method", r.Method), zap.Bool("notif", r.Notif))
}

func (p *Proxy) shouldSuppressWindowShowMessage(r *jsonrpc2.Request) (shouldIgnore bool) {
	var params lsp.ShowMessageRequestParams
	if err := json.Unmarshal(*r.Params, &params); err != nil {
		return false
	}
	return strings.HasPrefix(params.Message, "Do not edit this file!")
}

func (p *Proxy) rewriteGoplsPublishDiagnostics(r *jsonrpc2.Request) (err error) {
	// Unmarshal the params.
	var params lsp.PublishDiagnosticsParams
	if err = json.Unmarshal(*r.Params, &params); err != nil {
		return err
	}
	// Get the sourcemap from the cache.
	uri := strings.TrimSuffix(string(params.URI), "_templ.go") + ".templ"
	sourceMap, ok := p.sourceMapCache.Get(uri)
	if !ok {
		return fmt.Errorf("unable to complete because the sourcemap for %q doesn't exist in the cache, has the didOpen notification been sent yet?", uri)
	}
	params.URI = lsp.DocumentURI(uri)
	// Rewrite the positions.
	for i := 0; i < len(params.Diagnostics); i++ {
		item := params.Diagnostics[i]
		start, _, ok := sourceMap.SourcePositionFromTarget(item.Range.Start.Line+1, item.Range.Start.Character)
		if ok {
			item.Range.Start.Line = start.Line - 1
			item.Range.Start.Character = start.Col + 1
		}
		end, _, ok := sourceMap.SourcePositionFromTarget(item.Range.End.Line+1, item.Range.End.Character)
		if ok {
			item.Range.End = templatePositionToLSPPosition(end)
		}
		params.Diagnostics[i] = item
	}
	// Marshal the params back.
	jsonMessage, err := json.Marshal(params)
	if err != nil {
		return
	}
	err = r.Params.UnmarshalJSON(jsonMessage)
	// Done.
	return err
}

// Handle implements jsonrpc2.Handler. This function receives from the text editor client, and calls the proxy function
// to determine how to play it back to the client.
func (p *Proxy) Handle(ctx context.Context, conn *jsonrpc2.Conn, r *jsonrpc2.Request) {
	p.log.Info("client -> gopls", zap.String("method", r.Method), zap.Bool("notif", r.Notif))
	if r.Notif {
		var err error
		switch r.Method {
		case "textDocument/didOpen":
			err = p.rewriteDidOpenRequest(r)
		case "textDocument/didChange":
			err = p.rewriteDidChangeRequest(ctx, r)
		case "textDocument/didSave":
			err = p.rewriteDidSaveRequest(r)
		case "textDocument/didClose":
			err = p.rewriteDidCloseRequest(r)
		}
		if err != nil {
			p.log.Error("client -> gopls: error rewriting notification", zap.Error(err))
			return
		}
		err = p.gopls.Notify(ctx, r.Method, &r.Params)
		if err != nil {
			p.log.Error("client -> gopls: error proxying notification to gopls", zap.Error(err))
			return
		}
		p.log.Info("client -> gopls: notification complete", zap.String("method", r.Method))
	} else {
		switch r.Method {
		case "initialize":
			p.proxyInitialize(ctx, conn, r)
		case "textDocument/completion":
			p.proxyCompletion(ctx, conn, r)
			return
		case "textDocument/formatting":
			p.handleFormatting(ctx, conn, r)
			return
		default:
			p.proxyCall(ctx, conn, r)
			return
		}
	}
}

func (p *Proxy) proxyCall(ctx context.Context, conn *jsonrpc2.Conn, r *jsonrpc2.Request) {
	var resp interface{}
	err := p.gopls.Call(ctx, r.Method, &r.Params, &resp)
	if err != nil {
		p.log.Error("client -> gopls -> client: error", zap.String("method", r.Method), zap.Bool("notif", r.Notif), zap.Error(err))
	}
	p.log.Info("client -> gopls -> client: reply", zap.String("method", r.Method), zap.Bool("notif", r.Notif))
	err = conn.Reply(ctx, r.ID, resp)
	if err != nil {
		p.log.Info("client -> gopls -> client: error sending response", zap.String("method", r.Method), zap.Bool("notif", r.Notif), zap.Error(err))
	}
	p.log.Info("client -> gopls -> client: complete", zap.String("method", r.Method), zap.Bool("notif", r.Notif))
}

func (p *Proxy) proxyInitialize(ctx context.Context, conn *jsonrpc2.Conn, req *jsonrpc2.Request) {
	// Unmarshal the params.
	var params lsp.CompletionParams
	err := json.Unmarshal(*req.Params, &params)
	if err != nil {
		p.log.Error("proxyInitialize: failed to unmarshal request params", zap.Error(err))
	}
	// Call gopls and get the response.
	var resp lsp.InitializeResult
	err = p.gopls.Call(ctx, req.Method, &params, &resp)
	if err != nil {
		p.log.Error("proxyInitialize: client -> gopls: error sending request", zap.Error(err))
	}
	// Add the '<' and '{' trigger so that we can do snippets for tags.
	if resp.Capabilities.CompletionProvider == nil {
		resp.Capabilities.CompletionProvider = &lsp.CompletionOptions{}
	}
	resp.Capabilities.CompletionProvider.TriggerCharacters = append(resp.Capabilities.CompletionProvider.TriggerCharacters, "{", "<")
	// Remove all the gopls commands.
	if resp.Capabilities.ExecuteCommandProvider == nil {
		resp.Capabilities.ExecuteCommandProvider = &lsp.ExecuteCommandOptions{}
	}
	resp.Capabilities.ExecuteCommandProvider.Commands = []string{}
	resp.Capabilities.DocumentFormattingProvider = true
	// Reply to the client.
	err = conn.Reply(ctx, req.ID, &resp)
	if err != nil {
		p.log.Error("proxyInitialize: error sending response", zap.Error(err))
	}
	p.log.Info("proxyInitialize: client -> gopls -> client: complete", zap.Any("resp", resp))
}

func (p *Proxy) proxyCompletion(ctx context.Context, conn *jsonrpc2.Conn, req *jsonrpc2.Request) {
	// Unmarshal the params.
	var params lsp.CompletionParams
	err := json.Unmarshal(*req.Params, &params)
	if err != nil {
		p.log.Error("proxyCompletion: failed to unmarshal request params", zap.Error(err))
	}
	var resp lsp.CompletionList
	switch params.Context.TriggerCharacter {
	case "<":
		resp.Items = htmlSnippets
	case "{":
		resp.Items = templateSnippets
	default:
		// Rewrite the request.
		err = p.rewriteCompletionRequest(&params)
		if err != nil {
			p.log.Error("proxyCompletion: error rewriting request", zap.Error(err))
		}
		// Call gopls and get the response.
		err = p.gopls.Call(ctx, req.Method, &params, &resp)
		if err != nil {
			p.log.Error("proxyCompletion: client -> gopls: error sending request", zap.Error(err))
		}
		// Rewrite the response.
		err = p.rewriteCompletionResponse(string(params.TextDocument.URI), &resp)
		if err != nil {
			p.log.Error("proxyCompletion: error rewriting response", zap.Error(err))
		}
	}
	// Reply to the client.
	err = conn.Reply(ctx, req.ID, &resp)
	if err != nil {
		p.log.Error("proxyCompletion: error sending response", zap.Error(err))
	}
	p.log.Info("proxyCompletion: client -> gopls -> client: complete", zap.Any("resp", resp))
}

func (p *Proxy) rewriteCompletionResponse(uri string, resp *lsp.CompletionList) (err error) {
	// Get the sourcemap from the cache.
	uri = strings.TrimSuffix(uri, "_templ.go") + ".templ"
	sourceMap, ok := p.sourceMapCache.Get(uri)
	if !ok {
		return fmt.Errorf("unable to complete because the sourcemap for %q doesn't exist in the cache, has the didOpen notification been sent yet?", uri)
	}
	// Rewrite the positions.
	for i := 0; i < len(resp.Items); i++ {
		item := resp.Items[i]
		if item.TextEdit != nil {
			start, _, ok := sourceMap.SourcePositionFromTarget(item.TextEdit.Range.Start.Line+1, item.TextEdit.Range.Start.Character)
			if ok {
				p.log.Info("rewriteCompletionResponse: found new start position", zap.Any("from", item.TextEdit.Range.Start), zap.Any("start", start))
				item.TextEdit.Range.Start.Line = start.Line - 1
				item.TextEdit.Range.Start.Character = start.Col + 1
			}
			end, _, ok := sourceMap.SourcePositionFromTarget(item.TextEdit.Range.End.Line+1, item.TextEdit.Range.End.Character)
			if ok {
				p.log.Info("rewriteCompletionResponse: found new end position", zap.Any("from", item.TextEdit.Range.End), zap.Any("end", end))
				item.TextEdit.Range.End.Line = end.Line - 1
				item.TextEdit.Range.End.Character = end.Col + 1
			}
		}
		resp.Items[i] = item
	}
	return nil
}

func (p *Proxy) rewriteCompletionRequest(params *lsp.CompletionParams) (err error) {
	base, fileName := path.Split(string(params.TextDocument.URI))
	if !strings.HasSuffix(fileName, ".templ") {
		return
	}
	// Get the sourcemap from the cache.
	sourceMap, ok := p.sourceMapCache.Get(string(params.TextDocument.URI))
	if !ok {
		return fmt.Errorf("unable to complete because the sourcemap for %q doesn't exist in the cache, has the didOpen notification been sent yet?", params.TextDocument.URI)
	}
	// Map from the source position to target Go position.
	to, mapping, ok := sourceMap.TargetPositionFromSource(params.Position.Line+1, params.Position.Character)
	if ok {
		p.log.Info("rewriteCompletionRequest: found position", zap.Int("fromLine", params.Position.Line+1), zap.Int("fromCol", params.Position.Character), zap.Any("to", to), zap.Any("mapping", mapping))
		params.Position.Line = to.Line - 1
		params.Position.Character = to.Col - 1
		params.TextDocumentPositionParams.Position.Line = params.Position.Line
		params.TextDocumentPositionParams.Position.Character = params.Position.Character
	}
	// Update the URI to make gopls look at the Go code instead.
	params.TextDocument.URI = lsp.DocumentURI(base + (strings.TrimSuffix(fileName, ".templ") + "_templ.go"))
	// Done.
	return err
}

func (p *Proxy) handleFormatting(ctx context.Context, conn *jsonrpc2.Conn, req *jsonrpc2.Request) {
	// Unmarshal the params.
	var params lsp.DocumentFormattingParams
	err := json.Unmarshal(*req.Params, &params)
	if err != nil {
		p.log.Error("handleFormatting: failed to unmarshal request params", zap.Error(err))
	}
	var resp []lsp.TextEdit
	defer func() {
		// Reply to the client, even if there's been a failure.
		err = conn.Reply(ctx, req.ID, &resp)
		if err != nil {
			p.log.Error("handleFormatting: error sending response", zap.Error(err))
		}
		p.log.Info("handleFormatting: client -> textDocument.formatting -> client: complete", zap.Any("resp", resp))
	}()
	// Format the current document.
	contents, _ := p.documentContents.Get(string(params.TextDocument.URI))
	var lines int
	for _, c := range contents {
		if c == '\n' {
			lines++
		}
	}
	template, err := parser.ParseString(string(contents))
	if err != nil {
		p.sendParseErrorDiagnosticNotifications(params.TextDocument.URI, err)
		return
	}
	p.sendDiagnosticClearNotification(params.TextDocument.URI)
	w := new(strings.Builder)
	err = template.Write(w)
	if err != nil {
		p.log.Error("handleFormatting: faled to write template", zap.Error(err))
		return
	}
	// Replace everything.
	resp = append(resp, lsp.TextEdit{
		Range: lsp.Range{
			Start: lsp.Position{},
			End:   lsp.Position{Line: lines + 1, Character: 0},
		},
		NewText: w.String(),
	})
}

func (p *Proxy) rewriteDidOpenRequest(r *jsonrpc2.Request) (err error) {
	// Unmarshal the params.
	var params lsp.DidOpenTextDocumentParams
	if err = json.Unmarshal(*r.Params, &params); err != nil {
		return err
	}
	base, fileName := path.Split(string(params.TextDocument.URI))
	if !strings.HasSuffix(fileName, ".templ") {
		return
	}
	// Cache the template doc.
	p.documentContents.Set(string(params.TextDocument.URI), []byte(params.TextDocument.Text))
	// Parse the template.
	template, err := parser.ParseString(params.TextDocument.Text)
	if err != nil {
		p.sendParseErrorDiagnosticNotifications(params.TextDocument.URI, err)
		return
	}
	p.sendDiagnosticClearNotification(params.TextDocument.URI)
	// Generate the output code and cache the source map and Go contents to use during completion
	// requests.
	w := new(strings.Builder)
	sm, err := generator.Generate(template, w)
	if err != nil {
		return
	}
	p.sourceMapCache.Set(string(params.TextDocument.URI), sm)
	// Set the Go contents.
	params.TextDocument.Text = w.String()
	// Change the path.
	params.TextDocument.URI = lsp.DocumentURI(base + (strings.TrimSuffix(fileName, ".templ") + "_templ.go"))
	// Marshal the params back.
	jsonMessage, err := json.Marshal(params)
	if err != nil {
		return
	}
	err = r.Params.UnmarshalJSON(jsonMessage)
	// Done.
	return err
}

func (p *Proxy) rewriteDidChangeRequest(ctx context.Context, r *jsonrpc2.Request) (err error) {
	// Unmarshal the params.
	var params lsp.DidChangeTextDocumentParams
	if err = json.Unmarshal(*r.Params, &params); err != nil {
		return
	}
	base, fileName := path.Split(string(params.TextDocument.URI))
	if !strings.HasSuffix(fileName, ".templ") {
		return
	}
	// Apply content changes to the cached template.
	templateText, err := p.documentContents.Apply(string(params.TextDocument.URI), params.ContentChanges)
	if err != nil {
		return
	}
	// Update the Go code.
	template, err := parser.ParseString(string(templateText))
	if err != nil {
		p.sendParseErrorDiagnosticNotifications(params.TextDocument.URI, err)
		return
	}
	p.sendDiagnosticClearNotification(params.TextDocument.URI)
	w := new(strings.Builder)
	sm, err := generator.Generate(template, w)
	if err != nil {
		return
	}
	// Cache the sourcemap.
	p.sourceMapCache.Set(string(params.TextDocument.URI), sm)
	// Overwrite all the Go contents.
	params.ContentChanges = []lsp.TextDocumentContentChangeEvent{{
		Range:       nil,
		RangeLength: 0,
		Text:        w.String(),
	}}
	// Change the path.
	params.TextDocument.URI = lsp.DocumentURI(base + (strings.TrimSuffix(fileName, ".templ") + "_templ.go"))
	// Marshal the params back.
	jsonMessage, err := json.Marshal(params)
	if err != nil {
		return
	}
	err = r.Params.UnmarshalJSON(jsonMessage)
	// Done.
	return
}

func (p *Proxy) sendDiagnosticClearNotification(uri lsp.DocumentURI) {
	p.toClient <- toClientRequest{
		Method: "textDocument/publishDiagnostics",
		Notif:  true,
		Params: lsp.PublishDiagnosticsParams{
			URI:         uri,
			Diagnostics: []lsp.Diagnostic{},
		},
	}
}

func (p *Proxy) sendParseErrorDiagnosticNotifications(uri lsp.DocumentURI, err error) {
	pe, ok := err.(parser.ParseError)
	if !ok {
		return
	}
	p.toClient <- toClientRequest{
		Method: "textDocument/publishDiagnostics",
		Notif:  true,
		Params: lsp.PublishDiagnosticsParams{
			URI: uri,
			Diagnostics: []lsp.Diagnostic{
				{
					Range: lsp.Range{
						Start: templatePositionToLSPPosition(pe.From),
						End:   templatePositionToLSPPosition(pe.To),
					},
					Severity: lsp.Error,
					Code:     "",
					Source:   "templ",
					Message:  pe.Message,
				},
			},
		},
	}
}

func templatePositionToLSPPosition(p parser.Position) lsp.Position {
	return lsp.Position{Line: p.Line - 1, Character: p.Col + 1}
}

func (p *Proxy) rewriteDidSaveRequest(r *jsonrpc2.Request) (err error) {
	// Unmarshal the params.
	var params lsp.DidSaveTextDocumentParams
	if err = json.Unmarshal(*r.Params, &params); err != nil {
		return err
	}
	base, fileName := path.Split(string(params.TextDocument.URI))
	if !strings.HasSuffix(fileName, ".templ") {
		return
	}
	// Update the path.
	params.TextDocument.URI = lsp.DocumentURI(base + (strings.TrimSuffix(fileName, ".templ") + "_templ.go"))
	// Marshal the params back.
	jsonMessage, err := json.Marshal(params)
	if err != nil {
		return
	}
	err = r.Params.UnmarshalJSON(jsonMessage)
	// Done.
	return err
}

func (p *Proxy) rewriteDidCloseRequest(r *jsonrpc2.Request) (err error) {
	// Unmarshal the params.
	var params lsp.DidCloseTextDocumentParams
	if err = json.Unmarshal(*r.Params, &params); err != nil {
		return err
	}
	base, fileName := path.Split(string(params.TextDocument.URI))
	if !strings.HasSuffix(fileName, ".templ") {
		return
	}
	// Delete the template and sourcemaps from caches.
	p.documentContents.Delete(string(params.TextDocument.URI))
	p.sourceMapCache.Delete(string(params.TextDocument.URI))
	// Get gopls to delete the Go file from its cache.
	params.TextDocument.URI = lsp.DocumentURI(base + (strings.TrimSuffix(fileName, ".templ") + "_templ.go"))
	// Marshal the params back.
	jsonMessage, err := json.Marshal(params)
	if err != nil {
		return
	}
	err = r.Params.UnmarshalJSON(jsonMessage)
	// Done.
	return err
}

// Cache of .templ file URIs to the source map.
func newSourceMapCache() *sourceMapCache {
	return &sourceMapCache{
		m:              new(sync.Mutex),
		uriToSourceMap: make(map[string]*parser.SourceMap),
	}
}

type sourceMapCache struct {
	m              *sync.Mutex
	uriToSourceMap map[string]*parser.SourceMap
}

func (fc *sourceMapCache) Set(uri string, m *parser.SourceMap) {
	fc.m.Lock()
	defer fc.m.Unlock()
	fc.uriToSourceMap[uri] = m
}

func (fc *sourceMapCache) Get(uri string) (m *parser.SourceMap, ok bool) {
	fc.m.Lock()
	defer fc.m.Unlock()
	m, ok = fc.uriToSourceMap[uri]
	return
}

func (fc *sourceMapCache) Delete(uri string) {
	fc.m.Lock()
	defer fc.m.Unlock()
	delete(fc.uriToSourceMap, uri)
}
