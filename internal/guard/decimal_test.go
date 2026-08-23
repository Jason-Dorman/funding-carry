package guard

import (
	"go/ast"
	"go/token"
	"go/types"
	"sync"
	"testing"

	"golang.org/x/tools/go/packages"
)

const (
	decimalPkg  = "github.com/shopspring/decimal"
	decimalType = decimalPkg + ".Decimal"
)

// loadModule type-checks every package in the module once and shares the result
// with each guard test.
var loadModule = sync.OnceValues(func() ([]*packages.Package, error) {
	cfg := &packages.Config{
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedSyntax |
			packages.NeedTypes | packages.NeedTypesInfo,
		// The guard package sits two levels below the module root.
		Dir:   "../..",
		Tests: true,
		// Without the tags, packages.Load type-checks only what builds in the
		// default configuration, and every file behind a build tag is invisible
		// to these rules. That silently excluded the two places the decimal
		// contract matters most: internal/db's integration suite, which is where
		// the numeric round trip is asserted, and internal/ingest's live suite.
		// api-spec section 3.5 calls this a check "over the whole module", and it
		// was not one.
		BuildFlags: []string{"-tags=integration,live"},
	}
	return packages.Load(cfg, "./...")
})

// inspectModule walks every syntax tree in the module, calling visit with the
// package that owns each node so its type information is available.
func inspectModule(t *testing.T, visit func(pkg *packages.Package, node ast.Node)) {
	t.Helper()

	pkgs, err := loadModule()
	if err != nil {
		t.Fatalf("load module: %v", err)
	}
	if len(pkgs) == 0 {
		t.Fatal("no packages loaded; the guard cannot prove anything")
	}

	for _, pkg := range pkgs {
		for _, err := range pkg.Errors {
			t.Errorf("%s: %v", pkg.PkgPath, err)
		}
		for _, file := range pkg.Syntax {
			ast.Inspect(file, func(node ast.Node) bool {
				if node != nil {
					visit(pkg, node)
				}
				return true
			})
		}
	}
}

// isDecimal reports whether typ is decimal.Decimal, looking through pointers.
func isDecimal(typ types.Type) bool {
	if typ == nil {
		return false
	}
	if ptr, ok := types.Unalias(typ).(*types.Pointer); ok {
		typ = ptr.Elem()
	}
	named, ok := types.Unalias(typ).(*types.Named)
	if !ok {
		return false
	}
	obj := named.Obj()
	if obj == nil || obj.Pkg() == nil {
		return false
	}
	return obj.Pkg().Path()+"."+obj.Name() == decimalType
}

// decimal.Decimal is a struct holding a *big.Int, so == compiles but compares
// pointers and exponents rather than values. It is false even for two decimals
// parsed from the same string, which makes `x == decimal.Zero` a check that can
// never fire. There is no linter for this, so the module enforces it here.
func TestNoEqualityOperatorOnDecimal(t *testing.T) {
	t.Parallel()

	inspectModule(t, func(pkg *packages.Package, node ast.Node) {
		expr, ok := node.(*ast.BinaryExpr)
		if !ok || (expr.Op != token.EQL && expr.Op != token.NEQ) {
			return
		}
		if !isDecimal(pkg.TypesInfo.TypeOf(expr.X)) && !isDecimal(pkg.TypesInfo.TypeOf(expr.Y)) {
			return
		}
		t.Errorf("%s: %s on decimal.Decimal compares representation, not value — use Equal",
			pkg.Fset.Position(expr.OpPos), expr.Op)
	})
}

// Money never enters the system through a float, and never leaves through one
// either.
//
// The rule is expressed against the *signature* rather than against a list of
// names. A name list only ever bans what someone thought of: it caught
// NewFromFloat and missed Float64, InexactFloat64, and anything the library adds
// later, so a value could be taken out to float64, arithmetic done on it there,
// and a decimal rebuilt from the result with the guard silent throughout. Any
// decimal function that mentions a float in its parameters or its results is a
// door in or out, and all of them are shut.
//
// It also matches references, not just calls: `f := decimal.NewFromFloat` and a
// call through f is the same door with a longer handle.
func TestNoFloatCrossesTheDecimalBoundary(t *testing.T) {
	t.Parallel()

	inspectModule(t, func(pkg *packages.Package, node ast.Node) {
		ident, ok := node.(*ast.Ident)
		if !ok {
			return
		}
		fn, ok := pkg.TypesInfo.Uses[ident].(*types.Func)
		if !ok || fn.Pkg() == nil || fn.Pkg().Path() != decimalPkg {
			return
		}
		sig, ok := fn.Type().(*types.Signature)
		if !ok {
			return
		}
		if where := floatInSignature(sig); where != "" {
			t.Errorf("%s: decimal.%s has a float64 in its %s — money does not cross that "+
				"boundary in either direction; build decimals from strings or integers, "+
				"and compare or format them as decimals",
				pkg.Fset.Position(ident.Pos()), fn.Name(), where)
		}
	})
}

// floatInSignature reports where a float appears in a signature, if it does.
func floatInSignature(sig *types.Signature) string {
	if tupleHasFloat(sig.Params()) {
		return "parameters"
	}
	if tupleHasFloat(sig.Results()) {
		return "results"
	}
	return ""
}

func tupleHasFloat(tuple *types.Tuple) bool {
	for i := range tuple.Len() {
		basic, ok := types.Unalias(tuple.At(i).Type()).(*types.Basic)
		if ok && (basic.Kind() == types.Float64 || basic.Kind() == types.Float32) {
			return true
		}
	}
	return false
}

// The equality rule has two more doors than the binary-expression check covers.
// A switch on a decimal compares its tag against each case with ==, and a map
// keyed by a decimal compares keys the same way. Both compile, both are silently
// always-false, and neither is a BinaryExpr.
func TestNoImplicitDecimalEquality(t *testing.T) {
	t.Parallel()

	inspectModule(t, func(pkg *packages.Package, node ast.Node) {
		switch n := node.(type) {
		case *ast.SwitchStmt:
			// A tagless switch compares nothing; its cases are booleans.
			if n.Tag == nil || !isDecimal(pkg.TypesInfo.TypeOf(n.Tag)) {
				return
			}
			t.Errorf("%s: switch on a decimal.Decimal compares each case with ==, which is "+
				"never true — use Equal or Cmp in an if chain",
				pkg.Fset.Position(n.Switch))
		case *ast.MapType:
			if !isDecimal(pkg.TypesInfo.TypeOf(n.Key)) {
				return
			}
			t.Errorf("%s: a map keyed by decimal.Decimal compares keys with ==, so two "+
				"decimals parsed from the same string are different keys — key by String()",
				pkg.Fset.Position(n.Pos()))
		}
	})
}
