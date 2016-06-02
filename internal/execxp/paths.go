package execxp

import (
	"encoding/xml"
	"fmt"
	"strconv"

	"github.com/ChrisTrenkamp/goxpath/internal/parser"
	"github.com/ChrisTrenkamp/goxpath/internal/parser/findutil"
	"github.com/ChrisTrenkamp/goxpath/internal/parser/intfns"
	"github.com/ChrisTrenkamp/goxpath/internal/xconst"
	"github.com/ChrisTrenkamp/goxpath/internal/xsort"
	"github.com/ChrisTrenkamp/goxpath/xfn"
	"github.com/ChrisTrenkamp/goxpath/xtypes"

	"github.com/ChrisTrenkamp/goxpath/internal/lexer"
	"github.com/ChrisTrenkamp/goxpath/internal/parser/pathexpr"
	"github.com/ChrisTrenkamp/goxpath/tree"
)

type xpFilt struct {
	t       tree.Node
	ctx     xtypes.Result
	expr    pathexpr.PathExpr
	ns      map[string]string
	ctxPos  int
	ctxSize int
	proxPos map[int]int
	fns     map[xml.Name]xfn.Wrap
}

type xpExecFn func(*xpFilt, string)

var xpFns = map[lexer.XItemType]xpExecFn{
	lexer.XItemAbsLocPath:     xfAbsLocPath,
	lexer.XItemAbbrAbsLocPath: xfAbbrAbsLocPath,
	lexer.XItemRelLocPath:     xfRelLocPath,
	lexer.XItemAbbrRelLocPath: xfAbbrRelLocPath,
	lexer.XItemAxis:           xfAxis,
	lexer.XItemAbbrAxis:       xfAbbrAxis,
	lexer.XItemNCName:         xfNCName,
	lexer.XItemQName:          xfQName,
	lexer.XItemNodeType:       xfNodeType,
	lexer.XItemProcLit:        xfProcInstLit,
	lexer.XItemStrLit:         xfStrLit,
	lexer.XItemNumLit:         xfNumLit,
}

func xfExec(f *xpFilt, n *parser.Node) (err error) {
	for n != nil {
		if fn, ok := xpFns[n.Val.Typ]; ok {
			fn(f, n.Val.Val)
			n = n.Left
		} else if n.Val.Typ == lexer.XItemPredicate {
			if err = xfPredicate(f, n.Left); err != nil {
				return
			}

			n = n.Right
		} else if n.Val.Typ == lexer.XItemFunction {
			if err = xfFunction(f, n); err != nil {
				return
			}

			n = n.Right
		} else if n.Val.Typ == lexer.XItemOperator {
			lf := xpFilt{
				t:       f.t,
				ns:      f.ns,
				ctx:     f.ctx,
				ctxPos:  f.ctxPos,
				ctxSize: f.ctxSize,
				proxPos: f.proxPos,
				fns:     f.fns,
			}
			left, err := exec(&lf, n.Left)
			if err != nil {
				return err
			}

			rf := xpFilt{
				t:   f.t,
				ns:  f.ns,
				ctx: f.ctx,
				fns: f.fns,
			}
			right, err := exec(&rf, n.Right)
			if err != nil {
				return err
			}

			return xfOperator(left, right, f, n.Val.Val)
		} else if string(n.Val.Typ) == "" {
			n = n.Left
			//} else {
			//	return fmt.Errorf("Cannot process " + string(n.Val.Typ))
		}
	}

	return
}

func xfPredicate(f *xpFilt, n *parser.Node) (err error) {
	res := f.ctx.(xtypes.NodeSet)
	newRes := make(xtypes.NodeSet, 0, len(res))

	for i := range res {
		pf := xpFilt{
			t:       f.t,
			ns:      f.ns,
			ctxPos:  i,
			ctxSize: f.ctxSize,
			ctx:     xtypes.NodeSet{res[i]},
			fns:     f.fns,
		}

		predRes, err := exec(&pf, n)
		if err != nil {
			return err
		}

		ok, err := checkPredRes(predRes, f, res[i])
		if err != nil {
			return err
		}

		if ok {
			newRes = append(newRes, res[i])
		}
	}

	f.ctx = newRes

	return
}

func checkPredRes(ret xtypes.Result, f *xpFilt, node tree.Node) (bool, error) {
	if num, ok := ret.(xtypes.Num); ok {
		if float64(f.proxPos[node.Pos()]) == float64(num) {
			return true, nil
		}
		return false, nil
	}

	if b, ok := ret.(xtypes.IsBool); ok {
		return bool(b.Bool()), nil
	}

	return false, fmt.Errorf("Cannot convert argument to boolean")
}

func xfFunction(f *xpFilt, n *parser.Node) error {
	fn, ok := intfns.BuiltIn[n.Val.Val]
	if !ok {
		fn, ok = f.fns[xml.Name{Local: n.Val.Val}]
	}

	if ok {
		args := []xtypes.Result{}
		param := n.Left

		for param != nil {
			pf := xpFilt{
				t:       f.t,
				ctx:     f.ctx,
				ns:      f.ns,
				ctxPos:  f.ctxPos,
				ctxSize: f.ctxSize,
				fns:     f.fns,
			}
			res, err := exec(&pf, param.Left)
			if err != nil {
				return err
			}

			args = append(args, res)
			param = param.Right
		}

		filt, err := fn.Call(xfn.Ctx{NodeSet: f.ctx.(xtypes.NodeSet), Size: f.ctxSize, Pos: f.ctxPos + 1}, args...)
		f.ctx = filt
		return err
	}

	return fmt.Errorf("Unknown function: %s", n.Val.Val)
}

var eqOps = map[string]bool{
	"=":  true,
	"!=": true,
}

var booleanOps = map[string]bool{
	"=":  true,
	"!=": true,
	"<":  true,
	"<=": true,
	">":  true,
	">=": true,
}

var numOps = map[string]bool{
	"*":   true,
	"div": true,
	"mod": true,
	"+":   true,
	"-":   true,
	"=":   true,
	"!=":  true,
	"<":   true,
	"<=":  true,
	">":   true,
	">=":  true,
}

var andOrOps = map[string]bool{
	"and": true,
	"or":  true,
}

func xfOperator(left, right xtypes.Result, f *xpFilt, op string) error {
	if booleanOps[op] {
		lNode, lOK := left.(xtypes.NodeSet)
		rNode, rOK := right.(xtypes.NodeSet)
		if lOK && rOK {
			return bothNodeOperator(lNode, rNode, f, op)
		}

		if lOK {
			return leftNodeOperator(lNode, right, f, op)
		}

		if rOK {
			return rightNodeOperator(left, rNode, f, op)
		}

		if eqOps[op] {
			return equalsOperator(left, right, f, op)
		}
	}

	if numOps[op] {
		return numberOperator(left, right, f, op)
	}

	if andOrOps[op] {
		return andOrOperator(left, right, f, op)
	}

	//if op == "|" {
	return unionOperator(left, right, f, op)
	//}

	//return fmt.Errorf("Unknown operator " + op)
}

func xfAbsLocPath(f *xpFilt, val string) {
	i := f.t
	for i.GetNodeType() != tree.NtRoot {
		i = i.GetParent()
	}
	f.ctx = xtypes.NodeSet{i}
}

func xfAbbrAbsLocPath(f *xpFilt, val string) {
	i := f.t
	for i.GetNodeType() != tree.NtRoot {
		i = i.GetParent()
	}
	f.ctx = xtypes.NodeSet{i}
	f.expr = abbrPathExpr()
	find(f)
}

func xfRelLocPath(f *xpFilt, val string) {
}

func xfAbbrRelLocPath(f *xpFilt, val string) {
	f.expr = abbrPathExpr()
	find(f)
}

func xfAxis(f *xpFilt, val string) {
	f.expr.Axis = val
}

func xfAbbrAxis(f *xpFilt, val string) {
	f.expr.Axis = xconst.AxisAttribute
}

func xfNCName(f *xpFilt, val string) {
	f.expr.Name.Space = val
}

func xfQName(f *xpFilt, val string) {
	f.expr.Name.Local = val
	find(f)
}

func xfNodeType(f *xpFilt, val string) {
	f.expr.NodeType = val
	find(f)
}

func xfProcInstLit(f *xpFilt, val string) {
	filt := xtypes.NodeSet{}
	for _, i := range f.ctx.(xtypes.NodeSet) {
		if i.GetToken().(xml.ProcInst).Target == val {
			filt = append(filt, i)
		}
	}
	f.ctx = filt
}

func xfStrLit(f *xpFilt, val string) {
	f.ctx = xtypes.String(val)
}

func xfNumLit(f *xpFilt, val string) {
	num, _ := strconv.ParseFloat(val, 64)
	f.ctx = xtypes.Num(num)
}

func abbrPathExpr() pathexpr.PathExpr {
	return pathexpr.PathExpr{
		Name:     xml.Name{},
		Axis:     xconst.AxisDescendentOrSelf,
		NodeType: xconst.NodeTypeNode,
	}
}

func find(f *xpFilt) {
	dupFilt := make(map[int]tree.Node)
	f.proxPos = make(map[int]int)

	if f.expr.Axis == "" && f.expr.NodeType == "" && f.expr.Name.Space == "" {
		if f.expr.Name.Local == "." {
			f.expr = pathexpr.PathExpr{
				Name:     xml.Name{},
				Axis:     xconst.AxisSelf,
				NodeType: xconst.NodeTypeNode,
			}
		}

		if f.expr.Name.Local == ".." {
			f.expr = pathexpr.PathExpr{
				Name:     xml.Name{},
				Axis:     xconst.AxisParent,
				NodeType: xconst.NodeTypeNode,
			}
		}
	}

	f.expr.NS = f.ns

	for _, i := range f.ctx.(xtypes.NodeSet) {
		for pos, j := range findutil.Find(i, f.expr) {
			dupFilt[j.Pos()] = j
			f.proxPos[j.Pos()] = pos + 1
		}
	}

	res := make(xtypes.NodeSet, 0, len(dupFilt))
	for _, i := range dupFilt {
		res = append(res, i)
	}

	xsort.SortNodes(res)

	f.expr = pathexpr.PathExpr{}
	f.ctxSize = len(res)
	f.ctx = res
}
