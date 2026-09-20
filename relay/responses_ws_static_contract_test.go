package relay

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestResponsesWSProviderHelpersDoNotCallTurnObserverAccounting(t *testing.T) {
	root := responsesWSTestRepoRoot(t)
	files := []string{
		filepath.Join(root, "common/responsesws"),
		filepath.Join(root, "providers/openai/responses_ws_upstream.go"),
		filepath.Join(root, "providers/codex/responses_ws_upstream.go"),
	}
	for _, path := range files {
		responsesWSAssertNoSourceToken(t, path,
			"AdmitTurn(",
			"RollbackTurnAdmission(",
			"FinalizeTurn(",
		)
	}
}

func TestResponsesWSFirstTurnSetupIsQueuedBeforeClientReadPump(t *testing.T) {
	root := responsesWSTestRepoRoot(t)
	source, err := os.ReadFile(filepath.Join(root, "relay/responses_ws.go"))
	if err != nil {
		t.Fatal(err)
	}
	setup := strings.Index(string(source), "actor.PostReliable(ResponsesWSEventFirstTurnSetup")
	pump := strings.Index(string(source), "go clientPump.Run(c.Request.Context())")
	if setup < 0 || pump < 0 || setup > pump {
		t.Fatalf("first-turn setup must be enqueued before the client read pump starts: setup=%d pump=%d", setup, pump)
	}
}

func TestRuntimeSessionDoesNotDeclareProtocolSpecificTypes(t *testing.T) {
	root := responsesWSTestRepoRoot(t)
	responsesWSAssertNoTypeDeclarations(t, filepath.Join(root, "runtime/session"),
		"FrameKind",
		"Frame",
		"RecvEvent",
		"RealtimeSession",
		"RealtimeOpenOptions",
		"RealtimePayloadOrigin",
		"ClientPayloadError",
		"RecvDetailOrigin",
		"RecvDetailPhase",
		"ResponsesWSTransportSendResult",
		"ResponsesWSTransportSendStatus",
		"ResponsesWSTransportSendReason",
	)
	responsesWSAssertNoSourceToken(t, filepath.Join(root, "runtime/session"),
		"ErrInvalidFrame",
		"ErrInvalidResponsesWSTransportSendResult",
		"ExpectedPayloadOriginForRecvDetailOrigin",
	)
}

func TestResponsesWSProxyLocalEventsDoNotStoreDuplicateKind(t *testing.T) {
	root := responsesWSTestRepoRoot(t)
	responsesWSAssertStructsDoNotDeclareFields(t, filepath.Join(root, "relay/responses_ws_events.go"),
		[]string{
			"ResponsesWSEventProxyLocalError",
		},
		"Kind",
	)
}

func TestResponsesWSEvidenceEventsDoNotStoreDuplicateCoarseOrigin(t *testing.T) {
	root := responsesWSTestRepoRoot(t)
	targets := map[string][]string{
		filepath.Join(root, "common/responsesws/upstream.go"): {
			"UpstreamEvent",
		},
		filepath.Join(root, "relay/responses_ws_events.go"): {
			"ResponsesWSEventProviderDownstream",
			"ResponsesWSEventProviderUsageObserved",
			"ResponsesWSEventProviderRecvFailed",
		},
	}
	for filePath, typeNames := range targets {
		responsesWSAssertStructsDoNotDeclareFields(t, filePath, typeNames, "Origin")
	}
}

func TestResponsesWSAccountingPathDoesNotBranchRawDetailOrigin(t *testing.T) {
	root := responsesWSTestRepoRoot(t)
	targets := map[string][]string{
		filepath.Join(root, "relay/responses_ws_actor_settlement.go"): {
			"projectResponsesWSSharedDecision",
		},
		filepath.Join(root, "relay/responses_ws_observation.go"): {"finishObservedWork", "finishAllObservedWorks"},
	}
	for filePath, names := range targets {
		responsesWSAssertFunctionsDoNotBranchRawDetailOrigin(t, filePath, names...)
	}
}

func TestResponsesWSStreamEvidenceIsObservedOnlyAtProviderIngress(t *testing.T) {
	root := responsesWSTestRepoRoot(t)
	fset := token.NewFileSet()
	filePath := filepath.Join(root, "relay/responses_ws_observation.go")
	file, err := parser.ParseFile(fset, filePath, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var count int
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		calls := responsesWSMethodCallPositions(fn.Body, "ObserveResponsesStreamPayload")
		if len(calls) > 0 && fn.Name.Name != "observeParallelDownstream" {
			t.Fatalf("evidence observed outside provider ingress: %s", fn.Name.Name)
		}
		count += len(calls)
	}
	if count != 1 {
		t.Fatalf("provider ingress must observe evidence once: %d", count)
	}
}

func TestResponsesWSRelayDoesNotConvertProviderFramesThroughRuntimeFrame(t *testing.T) {
	root := responsesWSTestRepoRoot(t)
	responsesWSAssertNoSourceToken(t, filepath.Join(root, "relay/responses_ws.go"),
		`runtimeRecvEventFromProvider`,
		`responsesws.RuntimeFrame(`,
		`responsesws.FrameFromRuntime(`,
		`responsesWSWireMessageFromFrame`,
		`responsesWSProviderDownstreamMessageType`,
	)
}

func TestRuntimeSessionDoesNotImportCommonResponsesWS(t *testing.T) {
	root := responsesWSTestRepoRoot(t)
	responsesWSAssertNoSourceToken(t, filepath.Join(root, "runtime/session"),
		`common/responsesws`,
		`common/responsesws"`,
	)
}

func responsesWSTestRepoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve caller")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), ".."))
}

func responsesWSSelectorPath(expr ast.Expr) string {
	switch typed := expr.(type) {
	case *ast.Ident:
		return typed.Name
	case *ast.SelectorExpr:
		base := responsesWSSelectorPath(typed.X)
		if base == "" {
			return typed.Sel.Name
		}
		return base + "." + typed.Sel.Name
	case *ast.ParenExpr:
		return responsesWSSelectorPath(typed.X)
	case *ast.StarExpr:
		return responsesWSSelectorPath(typed.X)
	default:
		return ""
	}
}

func responsesWSMethodCallPositions(node ast.Node, method string) []token.Pos {
	var positions []token.Pos
	ast.Inspect(node, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if ok && selector.Sel.Name == method {
			positions = append(positions, call.Pos())
		}
		return true
	})
	return positions
}

func responsesWSExprContainsIdent(expr ast.Expr, name string) bool {
	found := false
	ast.Inspect(expr, func(node ast.Node) bool {
		identifier, ok := node.(*ast.Ident)
		if ok && identifier.Name == name {
			found = true
			return false
		}
		return !found
	})
	return found
}

func responsesWSAssertStructsDoNotDeclareFields(t *testing.T, filePath string, typeNames []string, forbiddenFields ...string) {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filePath, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", filePath, err)
	}
	targets := make(map[string]bool, len(typeNames))
	for _, name := range typeNames {
		targets[name] = false
	}
	forbidden := make(map[string]bool, len(forbiddenFields))
	for _, field := range forbiddenFields {
		forbidden[field] = true
	}
	ast.Inspect(file, func(node ast.Node) bool {
		spec, ok := node.(*ast.TypeSpec)
		if !ok {
			return true
		}
		if _, ok := targets[spec.Name.Name]; !ok {
			return true
		}
		targets[spec.Name.Name] = true
		st, ok := spec.Type.(*ast.StructType)
		if !ok {
			t.Fatalf("%s:%d: %s must remain a struct", filePath, fset.Position(spec.Pos()).Line, spec.Name.Name)
		}
		for _, field := range st.Fields.List {
			for _, name := range field.Names {
				if forbidden[name.Name] {
					t.Fatalf("%s:%d: %s must derive %s instead of storing it", filePath, fset.Position(name.Pos()).Line, spec.Name.Name, name.Name)
				}
			}
		}
		return true
	})
	for name, seen := range targets {
		if !seen {
			t.Fatalf("%s: missing %s", filePath, name)
		}
	}
}

func responsesWSAssertNoTypeDeclarations(t *testing.T, path string, typeNames ...string) {
	t.Helper()
	forbidden := make(map[string]bool, len(typeNames))
	for _, name := range typeNames {
		forbidden[name] = true
	}
	responsesWSWalkGoFiles(t, path, func(filePath string) {
		if strings.HasSuffix(filePath, "_test.go") {
			return
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, filePath, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", filePath, err)
		}
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.TYPE {
				continue
			}
			for _, spec := range gen.Specs {
				typeSpec, ok := spec.(*ast.TypeSpec)
				if !ok {
					continue
				}
				if forbidden[typeSpec.Name.Name] {
					t.Fatalf("%s:%d: forbidden type declaration %s", filePath, fset.Position(typeSpec.Pos()).Line, typeSpec.Name.Name)
				}
			}
		}
	})
}

func responsesWSExprBranchesRawDetailOrigin(expr ast.Expr) bool {
	found := false
	ast.Inspect(expr, func(node ast.Node) bool {
		binary, ok := node.(*ast.BinaryExpr)
		if !ok {
			return true
		}
		if responsesWSExprContainsDetailOrigin(binary) || responsesWSExprContainsRecvDetailOrigin(binary) {
			found = true
			return false
		}
		return true
	})
	return found
}

func responsesWSExprContainsDetailOrigin(expr ast.Expr) bool {
	if expr == nil {
		return false
	}
	found := false
	ast.Inspect(expr, func(node ast.Node) bool {
		if sel, ok := node.(*ast.SelectorExpr); ok && sel.Sel.Name == "DetailOrigin" {
			found = true
			return false
		}
		return true
	})
	return found
}

func responsesWSExprContainsRecvDetailOrigin(expr ast.Expr) bool {
	if expr == nil {
		return false
	}
	found := false
	ast.Inspect(expr, func(node ast.Node) bool {
		sel, ok := node.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if strings.HasPrefix(sel.Sel.Name, "RecvDetailOrigin") {
			found = true
			return false
		}
		return true
	})
	return found
}

func responsesWSSwitchContainsRecvDetailOriginCase(stmt *ast.SwitchStmt) bool {
	if stmt == nil || stmt.Body == nil {
		return false
	}
	for _, bodyStmt := range stmt.Body.List {
		clause, ok := bodyStmt.(*ast.CaseClause)
		if !ok {
			continue
		}
		for _, expr := range clause.List {
			if responsesWSExprContainsRecvDetailOrigin(expr) {
				return true
			}
		}
	}
	return false
}

func responsesWSAssertFunctionsDoNotBranchRawDetailOrigin(t *testing.T, filePath string, functionNames ...string) {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filePath, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", filePath, err)
	}
	targets := make(map[string]bool, len(functionNames))
	for _, name := range functionNames {
		targets[name] = false
	}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		if _, ok := targets[fn.Name.Name]; !ok {
			continue
		}
		targets[fn.Name.Name] = true
		ast.Inspect(fn.Body, func(node ast.Node) bool {
			switch stmt := node.(type) {
			case *ast.IfStmt:
				if responsesWSExprBranchesRawDetailOrigin(stmt.Cond) {
					t.Fatalf("%s:%d: %s must not branch accounting by raw DetailOrigin", filePath, fset.Position(stmt.Pos()).Line, fn.Name.Name)
				}
			case *ast.SwitchStmt:
				if responsesWSExprContainsDetailOrigin(stmt.Tag) || responsesWSSwitchContainsRecvDetailOriginCase(stmt) {
					t.Fatalf("%s:%d: %s must not switch accounting by raw DetailOrigin", filePath, fset.Position(stmt.Pos()).Line, fn.Name.Name)
				}
			}
			return true
		})
	}
	for name, seen := range targets {
		if !seen {
			t.Fatalf("%s: missing %s", filePath, name)
		}
	}
}

func responsesWSWalkGoFiles(t *testing.T, path string, visit func(string)) {
	t.Helper()
	if err := filepath.WalkDir(path, func(filePath string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if entry.Name() == ".git" || entry.Name() == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(filePath) == ".go" {
			visit(filePath)
		}
		return nil
	}); err != nil {
		t.Fatalf("walk %s: %v", path, err)
	}
}

func responsesWSAssertNoSourceToken(t *testing.T, path string, tokens ...string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	checkFile := func(filePath string) {
		t.Helper()
		content, readErr := os.ReadFile(filePath)
		if readErr != nil {
			t.Fatalf("read %s: %v", filePath, readErr)
		}
		text := string(content)
		for _, token := range tokens {
			if strings.Contains(text, token) {
				t.Fatalf("%s must not contain %q", filePath, token)
			}
		}
	}
	if !info.IsDir() {
		checkFile(path)
		return
	}
	if err := filepath.WalkDir(path, func(filePath string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || filepath.Ext(filePath) != ".go" || strings.HasSuffix(filePath, "_test.go") {
			return nil
		}
		checkFile(filePath)
		return nil
	}); err != nil {
		t.Fatalf("walk %s: %v", path, err)
	}
}
