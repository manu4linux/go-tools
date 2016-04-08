// Package staticcheck contains a linter for Go source code.
package staticcheck // import "honnef.co/go/staticcheck"

import (
	"fmt"
	"go/ast"
	"go/constant"
	"go/types"
	htmltemplate "html/template"
	"regexp"
	"strconv"
	"strings"
	texttemplate "text/template"
	"time"

	"honnef.co/go/lint"
)

var Funcs = []lint.Func{
	CheckRegexps,
	CheckTemplate,
	CheckTimeParse,
	CheckEncodingBinary,
	CheckTimeSleepConstant,
	CheckWaitgroupAdd,
	CheckWaitgroupCopy,
	CheckInfiniteEmptyLoop,
}

func CheckRegexps(f *lint.File) {
	fn := func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		if !lint.IsPkgDot(call.Fun, "regexp", "MustCompile") &&
			!lint.IsPkgDot(call.Fun, "regexp", "Compile") {
			return true
		}
		if len(call.Args) != 1 {
			return true
		}
		typ := f.Pkg.TypesInfo.Types[call.Args[0]]
		if typ.Value == nil {
			return true
		}
		if typ.Value.Kind() != constant.String {
			return true
		}
		s := constant.StringVal(typ.Value)
		_, err := regexp.Compile(s)
		if err != nil {
			f.Errorf(call.Args[0], 1, lint.Category("FIXME"), "%s", err)
		}
		return true
	}
	f.Walk(fn)
}

func CheckTemplate(f *lint.File) {
	fn := func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		if len(call.Args) != 1 {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if sel.Sel.Name != "Parse" {
			return true
		}
		var kind string
		typ := f.Pkg.TypesInfo.TypeOf(sel.X)
		if typ == nil {
			return true
		}
		switch typ.String() {
		case "*text/template.Template":
			kind = "text"
		case "*html/template.Template":
			kind = "html"
		default:
			return true
		}

		val := f.Pkg.TypesInfo.Types[call.Args[0]].Value
		if val == nil {
			return true
		}
		if val.Kind() != constant.String {
			return true
		}
		s := constant.StringVal(val)
		var err error
		switch kind {
		case "text":
			_, err = texttemplate.New("").Parse(s)
		case "html":
			_, err = htmltemplate.New("").Parse(s)
		}
		if err != nil {
			// TODO(dominikh): whitelist other parse errors, if any
			if strings.Contains(err.Error(), "unexpected") {
				f.Errorf(call.Args[0], 1, lint.Category("FIXME"), "%s", err)
			}
		}
		return true
	}
	f.Walk(fn)
}

func CheckTimeParse(f *lint.File) {
	fn := func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		if !lint.IsPkgDot(call.Fun, "time", "Parse") {
			return true
		}
		if len(call.Args) != 2 {
			return true
		}
		typ := f.Pkg.TypesInfo.Types[call.Args[0]]
		if typ.Value == nil {
			return true
		}
		if typ.Value.Kind() != constant.String {
			return true
		}
		s := constant.StringVal(typ.Value)
		s = strings.Replace(s, "_", " ", -1)
		s = strings.Replace(s, "Z", "-", -1)
		_, err := time.Parse(s, s)
		if err != nil {
			f.Errorf(call.Args[0], 1, lint.Category("FIXME"), "%s", err)
		}
		return true
	}
	f.Walk(fn)
}

func CheckEncodingBinary(f *lint.File) {
	// TODO(dominikh): also check binary.Read
	fn := func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		if !lint.IsPkgDot(call.Fun, "binary", "Write") {
			return true
		}
		if len(call.Args) != 3 {
			return true
		}
		dataType := f.Pkg.TypesInfo.TypeOf(call.Args[2]).Underlying()
		if typ, ok := dataType.(*types.Pointer); ok {
			dataType = typ.Elem().Underlying()
		}
		if typ, ok := dataType.(interface {
			Elem() types.Type
		}); ok {
			if _, ok := typ.(*types.Pointer); !ok {
				dataType = typ.Elem()
			}
		}

		if validEncodingBinaryType(dataType) {
			return true
		}
		f.Errorf(call.Args[2], 1, lint.Category("FIXME"), "type %s cannot be used with binary.Write",
			f.Pkg.TypesInfo.TypeOf(call.Args[2]))
		return true
	}
	f.Walk(fn)
}

func validEncodingBinaryType(typ types.Type) bool {
	typ = typ.Underlying()
	switch typ := typ.(type) {
	case *types.Basic:
		switch typ.Kind() {
		case types.Uint8, types.Uint16, types.Uint32, types.Uint64,
			types.Int8, types.Int16, types.Int32, types.Int64,
			types.Float32, types.Float64, types.Complex64, types.Complex128, types.Invalid:
			return true
		}
		return false
	case *types.Struct:
		n := typ.NumFields()
		for i := 0; i < n; i++ {
			if !validEncodingBinaryType(typ.Field(i).Type()) {
				return false
			}
		}
		return true
	case *types.Array:
		return validEncodingBinaryType(typ.Elem())
	case *types.Interface:
		// we can't determine if it's a valid type or not
		return true
	}
	return false
}

func CheckTimeSleepConstant(f *lint.File) {
	fn := func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		if !lint.IsPkgDot(call.Fun, "time", "Sleep") {
			return true
		}
		if len(call.Args) != 1 {
			return true
		}
		lit, ok := call.Args[0].(*ast.BasicLit)
		if !ok {
			return true
		}
		n, err := strconv.Atoi(lit.Value)
		if err != nil {
			return true
		}
		if n == 0 || n > 120 {
			// time.Sleep(0) is a seldomly used pattern in concurrency
			// tests. >120 might be intentional. 120 was chosen
			// because the user could've meant 2 minutes.
			return true
		}
		recommendation := "time.Sleep(time.Nanosecond)"
		if n != 1 {
			recommendation = fmt.Sprintf("time.Sleep(%d * time.Nanosecond)", n)
		}
		f.Errorf(call.Args[0], 1, lint.Category("FIXME"), "sleeping for %d nanoseconds is probably a bug. Be explicit if it isn't: %s", n, recommendation)
		return true
	}
	f.Walk(fn)
}

func CheckWaitgroupAdd(f *lint.File) {
	fn := func(node ast.Node) bool {
		g, ok := node.(*ast.GoStmt)
		if !ok {
			return true
		}
		fun, ok := g.Call.Fun.(*ast.FuncLit)
		if !ok {
			return true
		}
		if len(fun.Body.List) == 0 {
			return true
		}
		stmt, ok := fun.Body.List[0].(*ast.ExprStmt)
		if !ok {
			return true
		}
		call, ok := stmt.X.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		fn, ok := f.Pkg.TypesInfo.ObjectOf(sel.Sel).(*types.Func)
		if !ok {
			return true
		}
		if fn.FullName() == "(*sync.WaitGroup).Add" {
			f.Errorf(sel, 1, "should call %s before starting the goroutine to avoid a race",
				f.Render(stmt))
		}
		return true
	}
	f.Walk(fn)
}

func CheckWaitgroupCopy(f *lint.File) {
	fn := func(node ast.Node) bool {
		fn, ok := node.(*ast.FuncType)
		if !ok {
			return true
		}
		for _, arg := range fn.Params.List {
			typ := f.Pkg.TypesInfo.TypeOf(arg.Type)
			if typ != nil && typ.String() == "sync.WaitGroup" {
				f.Errorf(arg, 1, "should pass sync.WaitGroup by pointer")
			}
		}
		return true
	}
	f.Walk(fn)
}

func CheckInfiniteEmptyLoop(f *lint.File) {
	fn := func(node ast.Node) bool {
		loop, ok := node.(*ast.ForStmt)
		if !ok || len(loop.Body.List) != 0 || loop.Cond != nil || loop.Init != nil {
			return true
		}
		f.Errorf(loop, 1, "should not use an infinite empty loop. It will spin. Consider select{} instead.")
		return true
	}
	f.Walk(fn)
}
