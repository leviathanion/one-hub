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
			"ResponsesWSEventBridgeOpenProviderError",
			"ResponsesWSEventBridgeOpenLocalError",
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

func TestResponsesWSBridgeOpenSettlementRoutingDoesNotBranchRawDetailOrigin(t *testing.T) {
	root := responsesWSTestRepoRoot(t)
	fset := token.NewFileSet()
	filePath := filepath.Join(root, "relay/responses_ws.go")
	file, err := parser.ParseFile(fset, filePath, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", filePath, err)
	}
	targets := map[string]bool{
		"handleEvent":                true,
		"handleBridgeLocalOpenError": true,
	}
	seen := make(map[string]bool, len(targets))
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || !targets[fn.Name.Name] {
			continue
		}
		seen[fn.Name.Name] = true
		ast.Inspect(fn.Body, func(node ast.Node) bool {
			switch stmt := node.(type) {
			case *ast.IfStmt:
				if responsesWSExprBranchesBridgeOpenDetailOrigin(stmt.Cond) {
					t.Fatalf("%s:%d: %s must route bridge open settlement by typed event facts, not raw DetailOrigin", filePath, fset.Position(stmt.Pos()).Line, fn.Name.Name)
				}
			case *ast.SwitchStmt:
				if responsesWSExprContainsDetailOrigin(stmt.Tag) && responsesWSSwitchContainsBridgeOpenDetailOriginCase(stmt) {
					t.Fatalf("%s:%d: %s must route bridge open settlement by typed event facts, not raw DetailOrigin", filePath, fset.Position(stmt.Pos()).Line, fn.Name.Name)
				}
			}
			return true
		})
	}
	for name := range targets {
		if !seen[name] {
			t.Fatalf("%s: missing %s", filePath, name)
		}
	}
}

func TestResponsesWSActorLifecycleCleanupUsesTurnSlotHelpers(t *testing.T) {
	root := responsesWSTestRepoRoot(t)
	fset := token.NewFileSet()
	filePath := filepath.Join(root, "relay/responses_ws.go")
	file, err := parser.ParseFile(fset, filePath, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", filePath, err)
	}
	ast.Inspect(file, func(node ast.Node) bool {
		assign, ok := node.(*ast.AssignStmt)
		if !ok {
			return true
		}
		for i, lhs := range assign.Lhs {
			path := responsesWSSelectorPath(lhs)
			if path == "" {
				continue
			}
			var rhs ast.Expr
			if len(assign.Rhs) == 1 {
				rhs = assign.Rhs[0]
			} else if i < len(assign.Rhs) {
				rhs = assign.Rhs[i]
			}
			if responsesWSForbiddenTurnSlotCleanupAssignment(path, rhs) {
				t.Fatalf("%s:%d: actor lifecycle cleanup must use turn slot helpers, found assignment to %s", filePath, fset.Position(lhs.Pos()).Line, path)
			}
		}
		return true
	})
}

func TestResponsesWSAccountingPathDoesNotBranchRawDetailOrigin(t *testing.T) {
	root := responsesWSTestRepoRoot(t)
	targets := map[string][]string{
		filepath.Join(root, "relay/responses_ws_settlement.go"): {
			"decideResponsesWSSettlement",
		},
		filepath.Join(root, "relay/responses_ws_actor_settlement.go"): {
			"buildPendingSettlementInput",
			"buildActiveSettlementInput",
			"buildSettlementInputFromAttempt",
		},
		filepath.Join(root, "relay/responses_ws_settlement_projection.go"): {
			"ProjectResponsesWSProviderEvidence",
		},
	}
	for filePath, names := range targets {
		responsesWSAssertFunctionsDoNotBranchRawDetailOrigin(t, filePath, names...)
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

func TestResponsesWSDocsUseCurrentProviderContract(t *testing.T) {
	root := responsesWSTestRepoRoot(t)
	files := []string{
		filepath.Join(root, "docs/dev/responses-ws-architecture.md"),
		filepath.Join(root, "docs/dev/responses-ws-provider-contract.md"),
		filepath.Join(root, "docs/dev/responses-ws-settlement-core-actor-v2.md"),
		filepath.Join(root, "docs/dev/responses-ws-transport-boundary.md"),
	}
	responsesWSAssertDocsDoNotContain(t, files,
		"ResponsesWSSendResult",
		"ResponsesWSSendStatus",
		"ResponsesWSSendReason",
		"ResponsesWSSendOutcome",
		"runtimesession.Frame",
		"runtimesession.Recv",
		"runtime/session.Responses",
		"runtime/session.responsesws",
		"SendClientWithResult(ctx, runtime/session.Frame)",
		"SendClient(ctx, responsesws.Frame) error",
	)
	responsesWSAssertDocTokenOnlyWhenLineContains(t, files, "runtime/session.Frame", "/v1/realtime")
	responsesWSAssertDocTokenOnlyWhenLineContains(t, files, "RecvEvent", "/v1/realtime")
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

func responsesWSForbiddenTurnSlotCleanupAssignment(path string, rhs ast.Expr) bool {
	switch {
	case strings.HasSuffix(path, ".turns.pending.attempt"):
		return responsesWSExprIsNil(rhs)
	case strings.HasSuffix(path, ".turns.pending.provider"):
		return true
	case strings.HasSuffix(path, ".turns.active.attempt"):
		return responsesWSExprIsNil(rhs)
	case strings.HasSuffix(path, ".turns.active.evidence"):
		return true
	case strings.HasSuffix(path, ".turns.active.affinity"):
		return responsesWSExprIsNil(rhs)
	case strings.HasSuffix(path, ".turns.active.channelID"):
		return responsesWSExprIsIntegerLiteral(rhs, "0")
	case strings.HasSuffix(path, ".turns.active.bridgeCancelPendingAttemptID"):
		return responsesWSExprIsStringLiteral(rhs, "")
	default:
		return false
	}
}

func responsesWSExprIsNil(expr ast.Expr) bool {
	ident, ok := expr.(*ast.Ident)
	return ok && ident.Name == "nil"
}

func responsesWSExprIsIntegerLiteral(expr ast.Expr, value string) bool {
	lit, ok := expr.(*ast.BasicLit)
	return ok && lit.Kind == token.INT && lit.Value == value
}

func responsesWSExprIsStringLiteral(expr ast.Expr, value string) bool {
	lit, ok := expr.(*ast.BasicLit)
	return ok && lit.Kind == token.STRING && lit.Value == `"`+value+`"`
}

func responsesWSAssertDocsDoNotContain(t *testing.T, files []string, tokens ...string) {
	t.Helper()
	for _, filePath := range files {
		content, err := os.ReadFile(filePath)
		if err != nil {
			t.Fatalf("read %s: %v", filePath, err)
		}
		text := string(content)
		for _, token := range tokens {
			if strings.Contains(text, token) {
				t.Fatalf("%s must not contain legacy ResponsesWS contract token %q", filePath, token)
			}
		}
	}
}

func responsesWSAssertDocTokenOnlyWhenLineContains(t *testing.T, files []string, token, required string) {
	t.Helper()
	for _, filePath := range files {
		content, err := os.ReadFile(filePath)
		if err != nil {
			t.Fatalf("read %s: %v", filePath, err)
		}
		for i, line := range strings.Split(string(content), "\n") {
			if strings.Contains(line, token) && !strings.Contains(line, required) {
				t.Fatalf("%s:%d contains %q without %q: %s", filePath, i+1, token, required, line)
			}
		}
	}
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

func responsesWSExprBranchesBridgeOpenDetailOrigin(expr ast.Expr) bool {
	return responsesWSExprContainsDetailOrigin(expr) && responsesWSExprContainsBridgeOpenDetailOrigin(expr)
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

func responsesWSExprContainsBridgeOpenDetailOrigin(expr ast.Expr) bool {
	if expr == nil {
		return false
	}
	found := false
	ast.Inspect(expr, func(node ast.Node) bool {
		sel, ok := node.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		switch sel.Sel.Name {
		case "RecvDetailOriginBridgeOpenProviderError", "RecvDetailOriginBridgeStreamError":
			found = true
			return false
		default:
			return true
		}
	})
	return found
}

func responsesWSSwitchContainsBridgeOpenDetailOriginCase(stmt *ast.SwitchStmt) bool {
	if stmt == nil || stmt.Body == nil {
		return false
	}
	for _, bodyStmt := range stmt.Body.List {
		clause, ok := bodyStmt.(*ast.CaseClause)
		if !ok {
			continue
		}
		for _, expr := range clause.List {
			if responsesWSExprContainsBridgeOpenDetailOrigin(expr) {
				return true
			}
		}
	}
	return false
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
