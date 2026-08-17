package guard

import (
	"go/ast"
	"go/token"
	"go/types"
	"strings"
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

// Money never enters the system through a float. NewFromFloat and its siblings
// are the only constructors that can carry binary rounding error into a decimal,
// so they are banned outright; values come from strings, integers, or the
// database.
func TestNoDecimalConstructedFromFloat(t *testing.T) {
	t.Parallel()

	inspectModule(t, func(pkg *packages.Package, node ast.Node) {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return
		}
		fn, ok := pkg.TypesInfo.Uses[sel.Sel].(*types.Func)
		if !ok || fn.Pkg() == nil || fn.Pkg().Path() != decimalPkg {
			return
		}
		if !strings.HasPrefix(fn.Name(), "NewFromFloat") {
			return
		}
		t.Errorf("%s: decimal.%s carries float rounding error — build decimals from strings or integers",
			pkg.Fset.Position(call.Lparen), fn.Name())
	})
}
