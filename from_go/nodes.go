// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// This file implements printing of AST nodes; specifically
// expressions, statements, declarations, and files. It uses
// the print functionality implemented in from_go.go.

package from_go

import (
	"bytes"
	iToken "github.com/DAddYE/igo/token"
	"go/ast"
	"go/token"
	"unicode/utf8"
)

// Formatting issues:
// - better comment formatting for /*-style comments at the end of a line (e.g. a declaration)
//   when the comment spans multiple lines; if such a comment is just two lines, formatting is
//   not idempotent
// - formatting of expression lists
// - should use blank instead of tab to separate one-line function bodies from
//   the function header unless there is a group of consecutive one-liners

// ----------------------------------------------------------------------------
// Common AST nodes.

// Print as many newlines as necessary (but at least min newlines) to get to
//   the current line. ws is printed before the first line break. If newSection
//   is set, the first line break is printed as formfeed. Returns true if any
//   line break was printed; returns false otherwise.
//
//  TODO(gri): linebreak may add too many lines if the next statement at "line"
//             is preceded by comments because the computation of n assumes
//             the current position before the comment and the target position
//             after the comment. Thus, after interspersing such comments, the
//             space taken up by them is not considered to reduce the number of
//             linebreaks. At the moment there is no easy way to know about
//             future (not yet interspersed) comments in this function.
//
func (self *printer) linebreak(line, min int, ws whiteSpace, newSection bool) (printedBreak bool) {
	n := nlimit(line - self.pos.Line)
	if n < min {
		n = min

	}
	if n > 0 {
		self.print(ws)
		if newSection {
			self.print(formfeed)
			n--

		}
		for ; n > 0; n-- {
			self.print(newline)

		}
		printedBreak = true

	}
	return
}

// setComment sets g as the next comment if g != nil and if node comments
// are enabled - this mode is used when printing source code fragments such
// as exports only. It assumes that there is no pending comment in p.comments
// and at most one pending comment in the p.comment cache.
func (self *printer) setComment(g *ast.CommentGroup) {
	if g == nil || !self.useNodeComments {
		return

	}
	if self.comments == nil { // initialize p.comments lazily
		self.comments = make([]*ast.CommentGroup, 1)
	} else if self.cindex < len(self.comments) { // for some reason there are pending comments; this
		// should never happen - handle gracefully and flush
		// all comments up to g, ignore anything after that
		self.flush(self.posFor(g.List[0].Pos()), token.ILLEGAL)
		self.comments = self.comments[0:1]
		// in debug mode, report error
		self.internalError("setComment found pending comments")

	}
	self.comments[0] = g
	self.cindex = 0
	// don't overwrite any pending comment in the p.comment cache
	// (there may be a pending comment when a line comment is
	// immediately followed by a lead comment with no other
	// tokens between)
	if self.commentOffset == infinity {
		self.nextComment() // get comment ready for use
	}
}

type exprListMode uint

const (
	commaTerm exprListMode = 1 << iota // list is optionally terminated by a comma
	noIndent  // no extra indentation in multi-line lists
)

// If indent is set, a multi-line identifier list is indented after the
// first linebreak encountered.
func (self *printer) identList(list []*ast.Ident, indent bool) { // convert into an expression list so we can re-use exprList formatting
	xlist := make([]ast.Expr, len(list))
	for i, x := range list {
		xlist[i] = x

	}
	var mode exprListMode
	if !indent {
		mode = noIndent

	}
	self.exprList(token.NoPos, xlist, 1, mode, token.NoPos)
}

// Print a list of expressions. If the list spans multiple
// source lines, the original line breaks are respected between
// expressions.
//
// TODO(gri) Consider rewriting this to be independent of []ast.Expr
//           so that we can use the algorithm for any kind of list
//           (e.g., pass list via a channel over which to range).
func (self *printer) exprList(prev0 token.Pos, list []ast.Expr, depth int, mode exprListMode, next0 token.Pos) {
	if len(list) == 0 {
		return

	}
	prev := self.posFor(prev0)
	next := self.posFor(next0)
	line := self.lineFor(list[0].Pos())
	endLine := self.lineFor(list[len(list)-1].End())

	if prev.IsValid() && prev.Line == line && line == endLine { // all list entries on a single line
		for i, x := range list {
			if i > 0 { // use position of expression following the comma as
				// comma position for correct comment placement
				self.print(x.Pos(), token.COMMA, blank)

			}
			self.expr0(x, depth)

		}
		return

	} // list entries span multiple lines;
	// use source code positions to guide line breaks

	// don't add extra indentation if noIndent is set;
	// i.e., pretend that the first line is already indented
	ws := ignore
	if mode&noIndent == 0 {
		ws = indent

	} // the first linebreak is always a formfeed since this section must not
	// depend on any previous formatting
	prevBreak := -1 // index of last expression that was followed by a linebreak
	if prev.IsValid() && prev.Line < line && self.linebreak(line, 0, ws, true) {
		ws = ignore
		prevBreak = 0

	} // initialize expression/key size: a zero value indicates expr/key doesn't fit on a single line
	size := 0

	// print all list elements
	for i, x := range list {
		prevLine := line
		line = self.lineFor(x.Pos())

		// determine if the next linebreak, if any, needs to use formfeed:
		// in general, use the entire node size to make the decision; for
		// key:value expressions, use the key size
		// TODO(gri) for a better result, should probably incorporate both
		//           the key and the node size into the decision process
		useFF := true

		// determine element size: all bets are off if we don't have
		// position information for the previous and next token (likely
		// generated code - simply ignore the size in this case by setting
		// it to 0)
		prevSize := size
		const infinity = 1e6 // larger than any source line
		size = self.nodeSize(x, infinity)
		pair, isPair := x.(*ast.KeyValueExpr)
		if size <= infinity && prev.IsValid() && next.IsValid() { // x fits on a single line
			if isPair {
				size = self.nodeSize(pair.Key, infinity)

			}
		} else /* size <= infinity  */ 

		{ // size too large or we don't have good layout information
			size = 0

		} // if the previous line and the current line had single-
		// line-expressions and the key sizes are small or the
		// the ratio between the key sizes does not exceed a
		// threshold, align columns and do not use formfeed
		if prevSize > 0 && size > 0 {
			const smallSize = 20
			if prevSize <= smallSize && size <= smallSize {
				useFF = false
			} else {
				const r = 4 // threshold
				ratio := float64(size) / float64(prevSize)
				useFF = ratio <= 1.0/r || r <= ratio

			}
		}
		if i > 0 {
			needsLinebreak := prevLine < line && prevLine > 0 && line > 0
			// use position of expression following the comma as
			// comma position for correct comment placement, but
			// only if the expression is on the same line
			if !needsLinebreak {
				self.print(x.Pos())

			}
			self.print(token.COMMA)
			needsBlank := true
			if needsLinebreak { // lines are broken using newlines so comments remain aligned
				// unless forceFF is set or there are multiple expressions on
				// the same line in which case formfeed is used
				if self.linebreak(line, 0, ws, useFF || prevBreak+1 < i) {
					ws = ignore
					prevBreak = i
					needsBlank = false // we got a line break instead

				}
			}
			if needsBlank {
				self.print(blank)

			}
		}
		if isPair && size > 0 && len(list) > 1 { // we have a key:value expression that fits onto one line and
			// is in a list with more then one entry: use a column for the
			// key such that consecutive entries can align if possible
			self.expr(pair.Key)
			self.print(pair.Colon, token.COLON, vtab)
			self.expr(pair.Value)
		} else {
			self.expr0(x, depth)

		}
	}
	if mode&commaTerm != 0 && next.IsValid() && self.pos.Line < next.Line { // print a terminating comma if the next token is on a new line
		self.print(token.COMMA)
		if ws == ignore && mode&noIndent == 0 { // unindent if we indented
			self.print(unindent)

		}
		self.print(formfeed) // terminating comma needs a line break to look good
		return

	}
	if ws == ignore && mode&noIndent == 0 { // unindent if we indented
		self.print(unindent)

	}
}
func (self *printer) parameters(fields *ast.FieldList) {
	self.print(fields.Opening, token.LPAREN)
	if len(fields.List) > 0 {
		prevLine := self.lineFor(fields.Opening)
		ws := indent
		for i, par := range fields.List { // determine par begin and end line (may be different
			// if there are multiple parameter names for this par
			// or the type is on a separate line)
			var parLineBeg int
			if len(par.Names) > 0 {
				parLineBeg = self.lineFor(par.Names[0].Pos())
			} else {
				parLineBeg = self.lineFor(par.Type.Pos())

			}
			var parLineEnd = self.lineFor(par.Type.End())
			// separating "," if needed
			needsLinebreak := 0 < prevLine && prevLine < parLineBeg
			if i > 0 { // use position of parameter following the comma as
				// comma position for correct comma placement, but
				// only if the next parameter is on the same line
				if !needsLinebreak {
					self.print(par.Pos())

				}
				self.print(token.COMMA)

			} // separator if needed (linebreak or blank)
			if needsLinebreak && self.linebreak(parLineBeg, 0, ws, true) { // break line if the opening "(" or previous parameter ended on a different line
				ws = ignore
			} else if i > 0 {
				self.print(blank)

			} // parameter names
			if len(par.Names) > 0 { // Very subtle: If we indented before (ws == ignore), identList
				// won't indent again. If we didn't (ws == indent), identList will
				// indent if the identList spans multiple lines, and it will outdent
				// again at the end (and still ws == indent). Thus, a subsequent indent
				// by a linebreak call after a type, or in the next multi-line identList
				// will do the right thing.
				self.identList(par.Names, ws == indent)
				self.print(blank)

			} // parameter type
			self.expr(stripParensAlways(par.Type))
			prevLine = parLineEnd

		} // if the closing ")" is on a separate line from the last parameter,
		// print an additional "," and line break
		if closing := self.lineFor(fields.Closing); 0 < prevLine && prevLine < closing {
			self.print(token.COMMA)
			self.linebreak(closing, 0, ignore, true)

		} // unindent if we indented
		if ws == ignore {
			self.print(unindent)

		}
	}
	self.print(fields.Closing, token.RPAREN)

}
func (self *printer) signature(params, result *ast.FieldList) {
	if params != nil {
		self.parameters(params)
	} else {
		self.print(token.LPAREN, token.RPAREN)

	}
	n := result.NumFields()
	if n > 0 { // result != nil
		self.print(blank)
		if n == 1 && result.List[0].Names == nil { // single anonymous result; no ()'s
			self.expr(stripParensAlways(result.List[0].Type))
			return

		}
		self.parameters(result)

	}
}
func identListSize(list []*ast.Ident, maxSize int) (size int) {
	for i, x := range list {
		if i > 0 {
			size += len(", ")

		}
		size += utf8.RuneCountInString(x.Name)
		if size >= maxSize {
			break

		}
	}
	return

}
func (self *printer) isOneLineFieldList(list []*ast.Field) bool {
	if len(list) != 1 {
		return false // allow only one field

	}
	f := list[0]
	if f.Tag != nil || f.Comment != nil {
		return false // don't allow tags or comments

	} // only name(s) and type
	const maxSize = 30 // adjust as appropriate, this is an approximate value
	namesSize := identListSize(f.Names, maxSize)
	if namesSize > 0 {
		namesSize = 1 // blank between names and types

	}
	typeSize := self.nodeSize(f.Type, maxSize)
	return namesSize+typeSize <= maxSize

}
func (self *printer) setLineComment(text string) {
	self.setComment(&ast.CommentGroup{List: []*ast.Comment{{Slash: token.NoPos, Text: text}}})

}
func (self *printer) isMultiLine(n ast.Node) bool {
	return self.lineFor(n.End())-self.lineFor(n.Pos()) > 0

}
func (self *printer) fieldList(fields *ast.FieldList, isStruct, isIncomplete bool) {
	lbrace := fields.Opening
	list := fields.List
	rbrace := fields.Closing
	hasComments := isIncomplete || self.commentBefore(self.posFor(rbrace))
	srcIsOneLine := lbrace.IsValid() && rbrace.IsValid() && self.lineFor(lbrace) == self.lineFor(rbrace)

	if !hasComments && srcIsOneLine { // possibly a one-line struct/interface
		if len(list) == 0 {
			return
		} else if isStruct && self.isOneLineFieldList(list) /* for now ignore interfaces  */ { // small enough - print on one line
			// (don't use identList and ignore source line breaks)
			self.print(lbrace, token.COLON, blank)
			f := list[0]
			for i, x := range f.Names {
				if i > 0 { // no comments so no need for comma position
					self.print(token.COMMA, blank)

				}
				self.expr(x)

			}
			if len(f.Names) > 0 {
				self.print(blank)

			}
			self.expr(f.Type)
			return

		} // hasComments || !srcIsOneLine

	}
	if hasComments || len(list) > 0 {
		self.print(formfeed)

	}
	self.print(indent)
	if isStruct {
		sep := vtab
		if len(list) == 1 {
			sep = blank

		}
		newSection := false
		for i, f := range list {
			if i > 0 {
				self.linebreak(self.lineFor(f.Pos()), 1, ignore, newSection)

			}
			extraTabs := 0
			self.setComment(f.Doc)
			if len(f.Names) > 0 { // named fields
				self.identList(f.Names, false)
				self.print(sep)
				self.expr(f.Type)
				extraTabs = 1
			} else { // anonymous field
				self.expr(f.Type)
				extraTabs = 2

			}
			if f.Tag != nil {
				if len(f.Names) > 0 && sep == vtab {
					self.print(sep)

				}
				self.print(sep)
				self.expr(f.Tag)
				extraTabs = 0

			}
			if f.Comment != nil {
				for ; extraTabs > 0; extraTabs-- {
					self.print(sep)

				}
				self.setComment(f.Comment)

			}
			newSection = self.isMultiLine(f)

		}
		if isIncomplete {
			if len(list) > 0 {
				self.print(formfeed)

			} // p.flush(p.posFor(rbrace), token.RBRACE) // make sure we don't lose the last line comment
			self.setLineComment("// contains filtered or unexported fields")

		}
	} else { // interface

		newSection := false
		for i, f := range list {
			if i > 0 {
				self.linebreak(self.lineFor(f.Pos()), 1, ignore, newSection)

			}
			self.setComment(f.Doc)
			if ftyp, isFtyp := f.Type.(*ast.FuncType); isFtyp { // method
				self.expr(f.Names[0])
				self.signature(ftyp.Params, ftyp.Results)
			} else { // embedded interface
				self.expr(f.Type)

			}
			self.setComment(f.Comment)
			newSection = self.isMultiLine(f)

		}
		if isIncomplete {
			if len(list) > 0 {
				self.print(formfeed)

			} // p.flush(p.posFor(rbrace), token.RBRACE) // make sure we don't lose the last line comment
			self.setLineComment("// contains filtered or unexported methods")

		}
	}
	self.print(unindent, formfeed)
}

// ----------------------------------------------------------------------------
// Expressions

func walkBinary(e *ast.BinaryExpr) (has4, has5 bool, maxProblem int) {
	switch e.Op.Precedence() {
	case 4:

		has4 = true

	case 5:

		has5 = true



	}
	switch l := e.X.(type) {
	case *ast.BinaryExpr:

		if l.Op.Precedence() < e.Op.Precedence() { // parens will be inserted.
			// pretend this is an *ast.ParenExpr and do nothing.
			break

		}
		h4, h5, mp := walkBinary(l)
		has4 = has4 || h4
		has5 = has5 || h5
		if maxProblem < mp {
			maxProblem = mp

		}

	}
	switch r := e.Y.(type) {
	case *ast.BinaryExpr:

		if r.Op.Precedence() <= e.Op.Precedence() { // parens will be inserted.
			// pretend this is an *ast.ParenExpr and do nothing.
			break

		}
		h4, h5, mp := walkBinary(r)
		has4 = has4 || h4
		has5 = has5 || h5
		if maxProblem < mp {
			maxProblem = mp

		}

	case *ast.StarExpr:

		if e.Op == token.QUO /* `*\/`  */ {
			maxProblem = 5

		}

	case *ast.UnaryExpr:

		switch e.Op.String() + r.Op.String() {
		case "/*", "&&", "&^":

			maxProblem = 5

		case "++", "--":

			if maxProblem < 4 {
				maxProblem = 4

			}

		}

	}
	return

}
func cutoff(e *ast.BinaryExpr, depth int) int {
	has4, has5, maxProblem := walkBinary(e)
	if maxProblem > 0 {
		return maxProblem + 1

	}
	if has4 && has5 {
		if depth == 1 {
			return 5

		}
		return 4

	}
	if depth == 1 {
		return 6

	}
	return 4

}
func diffPrec(expr ast.Expr, prec int) int {
	x, ok := expr.(*ast.BinaryExpr)
	if !ok || prec != x.Op.Precedence() {
		return 1

	}
	return 0

}
func reduceDepth(depth int) int {
	depth--
	if depth < 1 {
		depth = 1

	}
	return depth
}

// Format the binary expression: decide the cutoff and then format.
// Let's call depth == 1 Normal mode, and depth > 1 Compact mode.
// (Algorithm suggestion by Russ Cox.)
//
// The precedences are:
//	5             *  /  %  <<  >>  &  &^
//	4             +  -  |  ^
//	3             ==  !=  <  <=  >  >=
//	2             &&
//	1             ||
//
// The only decision is whether there will be spaces around levels 4 and 5.
// There are never spaces at level 6 (unary), and always spaces at levels 3 and below.
//
// To choose the cutoff, look at the whole expression but excluding primary
// expressions (function calls, parenthesized exprs), and apply these rules:
//
//	1) If there is a binary operator with a right side unary operand
//	   that would clash without a space, the cutoff must be (in order):
//
//		/*	6
//		&&	6
//		&^	6
//		++	5
//		--	5
//
//         (Comparison operators always have spaces around them.)
//
//	2) If there is a mix of level 5 and level 4 operators, then the cutoff
//	   is 5 (use spaces to distinguish precedence) in Normal mode
//	   and 4 (never use spaces) in Compact mode.
//
//	3) If there are no level 4 operators or no level 5 operators, then the
//	   cutoff is 6 (always use spaces) in Normal mode
//	   and 4 (never use spaces) in Compact mode.
//
func (self *printer) binaryExpr(x *ast.BinaryExpr, prec1, cutoff, depth int) {
	prec := x.Op.Precedence()
	if prec < prec1 { // parenthesis needed
		// Note: The parser inserts an ast.ParenExpr node; thus this case
		//       can only occur if the AST is created in a different way.
		self.print(token.LPAREN)
		self.expr0(x, reduceDepth(depth)) // parentheses undo one level of depth
		self.print(token.RPAREN)
		return

	}
	printBlank := prec < cutoff

	ws := indent
	self.expr1(x.X, prec, depth+diffPrec(x.X, prec))
	if printBlank {
		self.print(blank)

	}
	xline := self.pos.Line // before the operator (it may be on the next line!)
	yline := self.lineFor(x.Y.Pos())
	self.print(x.OpPos, x.Op)
	if xline != yline && xline > 0 && yline > 0 { // at least one line break, but respect an extra empty line
		// in the source
		if self.linebreak(yline, 1, ws, true) {
			ws = ignore
			printBlank = false // no blank after line break

		}
	}
	if printBlank {
		self.print(blank)

	}
	self.expr1(x.Y, prec+1, depth+1)
	if ws == ignore {
		self.print(unindent)

	}
}
func isBinary(expr ast.Expr) bool {
	_, ok := expr.(*ast.BinaryExpr)
	return ok

}
func (self *printer) expr1(expr ast.Expr, prec1, depth int) {
	self.print(expr.Pos())

	switch x := expr.(type) {
	case *ast.BadExpr:

		self.print("BadExpr")



	case *ast.Ident:

		if self.rcvName != nil && self.rcvName.Name == x.Name {
			self.print(&ast.Ident{NamePos: x.NamePos, Name: "self", Obj: x.Obj})
		} else {
			self.print(x)

		}

	case *ast.BinaryExpr:

		if depth < 1 {
			self.internalError("depth < 1:", depth)
			depth = 1

		}
		self.binaryExpr(x, prec1, cutoff(x, depth), depth)



	case *ast.KeyValueExpr:

		self.expr(x.Key)
		self.print(x.Colon, token.COLON, blank)
		self.expr(x.Value)



	case *ast.StarExpr:

		const prec = token.UnaryPrec
		if prec < prec1 { // parenthesis needed
			self.print(token.LPAREN)
			self.print(token.MUL)
			self.expr(x.X)
			self.print(token.RPAREN)
		} else { // no parenthesis needed
			self.print(token.MUL)
			self.expr(x.X)

		}

	case *ast.UnaryExpr:

		const prec = token.UnaryPrec
		if prec < prec1 { // parenthesis needed
			self.print(token.LPAREN)
			self.expr(x)
			self.print(token.RPAREN)
		} else { // no parenthesis needed
			self.print(x.Op)
			if x.Op == token.RANGE { // TODO(gri) Remove this code if it cannot be reached.
				self.print(blank)

			}
			self.expr1(x.X, prec, depth)

		}

	case *ast.BasicLit:

		self.print(x)



	case *ast.FuncLit:

		self.expr(x.Type)
		self.adjBlock(x.Body)



	case *ast.ParenExpr:

		if _, hasParens := x.X.(*ast.ParenExpr); hasParens { // don't print parentheses around an already parenthesized expression
			// TODO(gri) consider making this more general and incorporate precedence levels
			self.expr0(x.X, reduceDepth(depth))
		} else /* parentheses undo one level of depth  */ 

		{
			self.print(token.LPAREN)
			self.expr0(x.X, reduceDepth(depth)) // parentheses undo one level of depth
			self.print(x.Rparen, token.RPAREN)

		}

	case *ast.SelectorExpr:

		self.expr1(x.X, token.HighestPrec, depth)
		self.print(token.PERIOD)
		if line := self.lineFor(x.Sel.Pos()); self.pos.IsValid() && self.pos.Line < line {
			self.print(indent, newline, x.Sel.Pos(), x.Sel, unindent)
		} else {
			self.print(x.Sel.Pos(), x.Sel)

		}

	case *ast.TypeAssertExpr:

		self.expr1(x.X, token.HighestPrec, depth)
		self.print(token.PERIOD, token.LPAREN)
		if x.Type != nil {
			self.expr(x.Type)
		} else {
			self.print(token.TYPE)

		}
		self.print(token.RPAREN)



	case *ast.IndexExpr:
		// TODO(gri): should treat[] like parentheses and undo one level of depth
		self.expr1(x.X, token.HighestPrec, 1)
		self.print(x.Lbrack, token.LBRACK)
		self.expr0(x.Index, depth+1)
		self.print(x.Rbrack, token.RBRACK)



	case *ast.SliceExpr:
		// TODO(gri): should treat[] like parentheses and undo one level of depth
		self.expr1(x.X, token.HighestPrec, 1)
		self.print(x.Lbrack, token.LBRACK)
		if x.Low != nil {
			self.expr0(x.Low, depth+1)

		} // blanks around ":" if both sides exist and either side is a binary expression
		if depth <= 1 && x.Low != nil && x.High != nil && (isBinary(x.Low) || isBinary(x.High)) {
			self.print(blank, token.COLON, blank)
		} else {
			self.print(token.COLON)

		}
		if x.High != nil {
			self.expr0(x.High, depth+1)

		}
		self.print(x.Rbrack, token.RBRACK)



	case *ast.CallExpr:

		if len(x.Args) > 1 {
			depth++

		}
		if _, ok := x.Fun.(*ast.FuncType); ok { // conversions to literal function types require parentheses around the type
			self.print(token.LPAREN)
			self.expr1(x.Fun, token.HighestPrec, depth)
			self.print(token.RPAREN)
		} else {
			self.expr1(x.Fun, token.HighestPrec, depth)

		}
		self.print(x.Lparen, token.LPAREN)
		if x.Ellipsis.IsValid() {
			self.exprList(x.Lparen, x.Args, depth, 0, x.Ellipsis)
			self.print(x.Ellipsis, token.ELLIPSIS)
			if x.Rparen.IsValid() && self.lineFor(x.Ellipsis) < self.lineFor(x.Rparen) {
				self.print(token.COMMA, formfeed)

			}
			self.print(x.Rparen, token.RPAREN)
		} else {
			if len(x.Args) > 0 {
				last := x.Args[len(x.Args)-1]
				if fn, ok := last.(*ast.FuncLit); ok {
					args := x.Args[:len(x.Args)-1]
					self.exprList(x.Lparen, args, depth, commaTerm, x.Rparen)
					self.print(x.Rparen, token.RPAREN)
					self.print(blank, iToken.DO)
					self.signature(fn.Type.Params, fn.Type.Results)
					self.adjBlock(fn.Body)
				} else {
					self.exprList(x.Lparen, x.Args, depth, commaTerm, x.Rparen)
					self.print(x.Rparen, token.RPAREN)

				}
			} else {
				self.print(x.Rparen, token.RPAREN)

			}
		}

	case *ast.CompositeLit:
		// composite literal elements that are composite literals themselves may have the type omitted
		if x.Type != nil {
			self.expr1(x.Type, token.HighestPrec, depth)

		}
		self.print(x.Lbrace, token.LBRACE)
		self.exprList(x.Lbrace, x.Elts, 1, commaTerm, x.Rbrace)
		// do not insert extra line breaks because of comments before
		// the closing '}' as it might break the code if there is no
		// trailing ','
		self.print(noExtraLinebreak, x.Rbrace, token.RBRACE, noExtraLinebreak)



	case *ast.Ellipsis:

		self.print(token.ELLIPSIS)
		if x.Elt != nil {
			self.expr(x.Elt)

		}

	case *ast.ArrayType:

		self.print(token.LBRACK)
		if x.Len != nil {
			self.expr(x.Len)

		}
		self.print(token.RBRACK)
		self.expr(x.Elt)



	case *ast.StructType:

		self.print(token.STRUCT)
		self.fieldList(x.Fields, true, x.Incomplete)



	case *ast.FuncType:

		self.print(token.FUNC)
		self.signature(x.Params, x.Results)



	case *ast.InterfaceType:

		self.print(token.INTERFACE)
		self.fieldList(x.Methods, false, x.Incomplete)



	case *ast.MapType:

		self.print(token.MAP, token.LBRACK)
		self.expr(x.Key)
		self.print(token.RBRACK)
		self.expr(x.Value)



	case *ast.ChanType:

		switch x.Dir {
		case ast.SEND | ast.RECV:

			self.print(token.CHAN)

		case ast.RECV:

			self.print(token.ARROW, token.CHAN) // x.Arrow and x.Pos() are the same
		case ast.SEND:

			self.print(token.CHAN, x.Arrow, token.ARROW)



		}
		self.print(blank)
		self.expr(x.Value)



	default:

		panic("unreachable")



	}
	return

}
func (self *printer) expr0(x ast.Expr, depth int) {
	self.expr1(x, token.LowestPrec, depth)

}
func (self *printer) expr(x ast.Expr) {
	const depth = 1
	self.expr1(x, token.LowestPrec, depth)
}

// ----------------------------------------------------------------------------
// Statements

// Print the statement list indented, but without a newline after the last statement.
// Extra line breaks between statements in the source are respected but at most one
// empty line is printed between statements.
func (self *printer) stmtList(list []ast.Stmt, nindent int, nextIsRBrace bool) {
	if nindent > 0 {
		self.print(indent)

	}
	if self.inFunc && self.findent == 0 {
		self.findent = self.indent

	}
	multiLine := false
	i := 0
	for _, s := range list { // ignore empty statements (was issue 3466)
		if _, isEmpty := s.(*ast.EmptyStmt); !isEmpty { // _indent == 0 only for lists of switch/select case clauses;
			// in those cases each clause is a new section
			if len(self.output) > 0 { // only print line break if we are not at the beginning of the output
				// (i.e., we are not printing only a partial program)
				self.linebreak(self.lineFor(s.Pos()), 1, ignore, i == 0 || nindent == 0 || multiLine)

			}
			self.stmt(s, nextIsRBrace && i == len(list)-1)
			multiLine = self.isMultiLine(s)
			i++

		}
	}
	if !self.inFunc {
		self.findent = 0

	}
	if nindent > 0 {
		self.print(unindent)

	} // block prints an *ast.BlockStmt; it always spans at least two lines.
}
func (self *printer) block(b *ast.BlockStmt, nindent int) {
	self.stmtList(b.List, nindent, true)
	self.linebreak(self.lineFor(b.Rbrace), 1, ignore, true)

}
func isTypeName(x ast.Expr) bool {
	switch t := x.(type) {
	case *ast.Ident:

		return true

	case *ast.SelectorExpr:

		return isTypeName(t.X)



	}
	return false

}
func stripParens(x ast.Expr) ast.Expr {
	if px, strip := x.(*ast.ParenExpr); strip { // parentheses must not be stripped if there are any
		// unparenthesized composite literals starting with
		// a type name
		ast.Inspect(px.X, func(node ast.Node) bool {
			switch x := node.(type) {
			case *ast.ParenExpr:
				// parentheses protect enclosed composite literals
				return false

			case *ast.CompositeLit:

				if isTypeName(x.Type) {
					strip = false // do not strip parentheses

				}
				return false

				// in all other cases, keep inspecting
			}
			return true

		})

		if strip {
			return stripParens(px.X)

		}
	}
	return x

}
func stripParensAlways(x ast.Expr) ast.Expr {
	if x, ok := x.(*ast.ParenExpr); ok {
		return stripParensAlways(x.X)

	}
	return x

}
func (self *printer) controlClause(isForStmt bool, init ast.Stmt, expr ast.Expr, post ast.Stmt) {
	self.print(blank)
	needsBlank := false
	if init == nil && post == nil { // no semicolons required
		if expr != nil {
			self.expr(stripParens(expr))
			needsBlank = true

		}
	} else { // all semicolons required
		// (they are not separators, print them explicitly)
		if init != nil {
			self.stmt(init, false)

		}
		self.print(token.SEMICOLON, blank)
		if expr != nil {
			self.expr(stripParens(expr))
			needsBlank = true

		}
		if isForStmt {
			self.print(token.SEMICOLON, blank)
			needsBlank = false
			if post != nil {
				self.stmt(post, false)
				needsBlank = true

			}
		}
	}
	if needsBlank {
		self.print(blank)

	} // indentList reports whether an expression list would look better if it
	// were indented wholesale (starting with the very first element, rather
	// than starting at the first line break).
	//
}
func (self *printer) indentList(list []ast.Expr) bool { // Heuristic: indentList returns true if there are more than one multi-
	// line element in the list, or if there is any element that is not
	// starting on the same line as the previous one ends.
	if len(list) >= 2 {
		var b = self.lineFor(list[0].Pos())
		var e = self.lineFor(list[len(list)-1].End())
		if 0 < b && b < e { // list spans multiple lines
			n := 0 // multi-line element count
			line := b
			for _, x := range list {
				xb := self.lineFor(x.Pos())
				xe := self.lineFor(x.End())
				if line < xb { // x is not starting on the same
					// line as the previous one ended
					return true

				}
				if xb < xe { // x is a multi-line element
					n++

				}
				line = xe

			}
			return n > 1

		}
	}
	return false

}
func (self *printer) alignFuncIndent() {
	self.flush(self.pos, self.lastTok)
	i := self.indent
	for ; i < self.findent; i++ {
		self.print(indent)

	}
	for ; i > self.findent; i-- {
		self.print(unindent)

	}
}
func (self *printer) stmt(stmt ast.Stmt, nextIsRBrace bool) {
	self.print(stmt.Pos())

	switch s := stmt.(type) {
	case *ast.BadStmt:

		self.print("BadStmt")



	case *ast.DeclStmt:

		self.decl(s.Decl)



	case *ast.EmptyStmt:
		// nothing to do

	case *ast.LabeledStmt:

		self.alignFuncIndent()
		self.expr(s.Label)
		self.print(s.Colon, token.COLON, indent)
		if e, isEmpty := s.Stmt.(*ast.EmptyStmt); isEmpty {
			if !nextIsRBrace {
				self.print(newline, e.Pos(), token.SEMICOLON)
				break

			}
		} else {
			self.linebreak(self.lineFor(s.Stmt.Pos()), 1, ignore, true)

		}
		self.stmt(s.Stmt, nextIsRBrace)



	case *ast.ExprStmt:

		const depth = 1
		self.expr0(s.X, depth)



	case *ast.SendStmt:

		const depth = 1
		self.expr0(s.Chan, depth)
		self.print(blank, s.Arrow, token.ARROW, blank)
		self.expr0(s.Value, depth)



	case *ast.IncDecStmt:

		const depth = 1
		self.expr0(s.X, depth+1)
		self.print(s.TokPos, s.Tok)



	case *ast.AssignStmt:

		var depth = 1
		if len(s.Lhs) > 1 && len(s.Rhs) > 1 {
			depth++

		}
		self.exprList(s.Pos(), s.Lhs, depth, 0, s.TokPos)
		self.print(blank, s.TokPos, s.Tok, blank)
		self.exprList(s.TokPos, s.Rhs, depth, 0, token.NoPos)



	case *ast.GoStmt:

		self.print(token.GO, blank)
		self.expr(s.Call)



	case *ast.DeferStmt:

		self.print(token.DEFER, blank)
		self.expr(s.Call)



	case *ast.ReturnStmt:

		self.print(token.RETURN)
		if s.Results != nil {
			self.print(blank)
			// Use indentList heuristic to make corner cases look
			// better (issue 1207). A more systematic approach would
			// always indent, but this would cause significant
			// reformatting of the code base and not necessarily
			// lead to more nicely formatted code in general.
			if self.indentList(s.Results) {
				self.print(indent)
				self.exprList(s.Pos(), s.Results, 1, noIndent, token.NoPos)
				self.print(unindent)
			} else {
				self.exprList(s.Pos(), s.Results, 1, 0, token.NoPos)

			}
		}

	case *ast.BranchStmt:

		self.print(s.Tok)
		if s.Label != nil {
			self.print(blank)
			self.expr(s.Label)

		}

	case *ast.BlockStmt:

		self.block(s, 1)



	case *ast.IfStmt:

		self.print(token.IF)
		self.controlClause(false, s.Init, s.Cond, nil)
		self.block(s.Body, 1)
		if s.Else != nil {
			self.print(token.ELSE)
			switch s.Else.(type) {
			case *ast.BlockStmt, *ast.IfStmt:

				self.print(blank)
				self.stmt(s.Else, nextIsRBrace)

			default:

				self.print(indent, formfeed)
				self.stmt(s.Else, true)
				self.print(unindent, formfeed)



			}
		}

	case *ast.CaseClause:

		if s.List != nil {
			self.print(token.CASE, blank)
			self.exprList(s.Pos(), s.List, 1, 0, s.Colon)
		} else {
			self.print(token.DEFAULT)

		}
		self.print(s.Colon, token.COLON)
		self.stmtList(s.Body, 1, nextIsRBrace)



	case *ast.SwitchStmt:

		self.print(token.SWITCH)
		self.controlClause(false, s.Init, s.Tag, nil)
		self.print(indent)
		self.block(s.Body, 0)
		self.print(unindent)



	case *ast.TypeSwitchStmt:

		self.print(token.SWITCH)
		if s.Init != nil {
			self.print(blank)
			self.stmt(s.Init, false)
			self.print(token.SEMICOLON)

		}
		self.print(blank)
		self.stmt(s.Assign, false)
		self.print(indent)
		self.block(s.Body, 0)
		self.print(unindent)



	case *ast.CommClause:

		if s.Comm != nil {
			self.print(token.CASE, blank)
			self.stmt(s.Comm, false)
		} else {
			self.print(token.DEFAULT)

		}
		self.print(s.Colon, token.COLON)
		self.stmtList(s.Body, 1, nextIsRBrace)



	case *ast.SelectStmt:

		self.print(token.SELECT, blank)
		self.print(indent)
		body := s.Body
		if len(body.List) == 0 && !self.commentBefore(self.posFor(body.Rbrace)) { // print empty select statement w/o comments on one line
			self.internalError("found a select without body")
		} else {
			self.block(body, 0)

		}
		self.print(unindent)



	case *ast.ForStmt:

		self.print(token.FOR)
		self.controlClause(true, s.Init, s.Cond, s.Post)
		self.block(s.Body, 1)



	case *ast.RangeStmt:

		self.print(token.FOR, blank)
		self.expr(s.Key)
		if s.Value != nil { // use position of value following the comma as
			// comma position for correct comment placement
			self.print(s.Value.Pos(), token.COMMA, blank)
			self.expr(s.Value)

		}
		self.print(blank, s.TokPos, s.Tok, blank, token.RANGE, blank)
		self.expr(stripParens(s.X))
		self.print(blank)
		self.block(s.Body, 1)



	default:

		panic("unreachable")



	}
	return
}

// ----------------------------------------------------------------------------
// Declarations

// The keepTypeColumn function determines if the type column of a series of
// consecutive const or var declarations must be kept, or if initialization
// values (V) can be placed in the type column (T) instead. The i'th entry
// in the result slice is true if the type column in spec[i] must be kept.
//
// For example, the declaration:
//
//	const (
//		foobar int = 42 // comment
//		x          = 7  // comment
//		foo
//              bar = 991
//	)
//
// leads to the type/values matrix below. A run of value columns (V) can
// be moved into the type column if there is no type for any of the values
// in that column (we only move entire columns so that they align properly).
//
//	matrix        formatted     result
//                    matrix
//	T  V    ->    T  V     ->   true      there is a T and so the type
//	-  V          -  V          true      column must be kept
//	-  -          -  -          false
//	-  V          V  -          false     V is moved into T column
//
func keepTypeColumn(specs []ast.Spec) []bool {
	m := make([]bool, len(specs))

	populate := func(i, j int, keepType bool) {
		if keepType {
			for ; i < j; i++ {
				m[i] = true

			}
		}
	}
	i0 := -1 // if i0 >= 0 we are in a run and i0 is the start of the run
	var keepType bool
	for i, s := range specs {
		t := s.(*ast.ValueSpec)
		if t.Values != nil {
			if i0 < 0 { // start of a run of ValueSpecs with non-nil Values
				i0 = i
				keepType = false

			}
		} else {
			if i0 >= 0 { // end of a run
				populate(i0, i, keepType)
				i0 = -1

			}
		}
		if t.Type != nil {
			keepType = true

		}
	}
	if i0 >= 0 { // end of a run
		populate(i0, len(specs), keepType)

	}
	return m

}
func (self *printer) valueSpec(s *ast.ValueSpec, keepType bool) {
	self.setComment(s.Doc)
	self.identList(s.Names, false) // always present
	extraTabs := 3
	if s.Type != nil || keepType {
		self.print(vtab)
		extraTabs--

	}
	if s.Type != nil {
		self.expr(s.Type)

	}
	if s.Values != nil {
		self.print(vtab, token.ASSIGN, blank)
		self.exprList(token.NoPos, s.Values, 1, 0, token.NoPos)
		extraTabs--

	}
	if s.Comment != nil {
		for ; extraTabs > 0; extraTabs-- {
			self.print(vtab)

		}
		self.setComment(s.Comment)

	} // The parameter n is the number of specs in the group. If doIndent is set,
	// multi-line identifier lists in the spec are indented when the first
	// linebreak is encountered.
	//
}
func (self *printer) spec(spec ast.Spec, n int, doIndent bool) {
	switch s := spec.(type) {
	case *ast.ImportSpec:

		self.setComment(s.Doc)
		if s.Name != nil {
			self.expr(s.Name)
			self.print(blank)

		}
		self.expr(s.Path)
		self.setComment(s.Comment)
		self.print(s.EndPos)



	case *ast.ValueSpec:

		if n != 1 {
			self.internalError("expected n = 1; got", n)

		}
		self.setComment(s.Doc)
		self.identList(s.Names, doIndent) // always present
		if s.Type != nil {
			self.print(blank)
			self.expr(s.Type)

		}
		if s.Values != nil {
			self.print(blank, token.ASSIGN, blank)
			self.exprList(token.NoPos, s.Values, 1, 0, token.NoPos)

		}
		self.setComment(s.Comment)



	case *ast.TypeSpec:

		self.setComment(s.Doc)
		self.expr(s.Name)
		if n == 1 {
			self.print(blank)
		} else {
			self.print(vtab)

		}
		self.expr(s.Type)
		self.setComment(s.Comment)



	default:

		panic("unreachable")



	}
}
func (self *printer) genDecl(d *ast.GenDecl) {
	self.setComment(d.Doc)
	self.print(d.Pos(), d.Tok, blank)

	if d.Lparen.IsValid() { // group of parenthesized declarations
		if n := len(d.Specs); n > 0 {
			self.print(indent, formfeed)
			if n > 1 && (d.Tok == token.CONST || d.Tok == token.VAR) { // two or more grouped const/var declarations:
				// determine if the type column must be kept
				keepType := keepTypeColumn(d.Specs)
				newSection := false
				for i, s := range d.Specs {
					if i > 0 {
						self.linebreak(self.lineFor(s.Pos()), 1, ignore, newSection)

					}
					self.valueSpec(s.(*ast.ValueSpec), keepType[i])
					newSection = self.isMultiLine(s)

				}
			} else {
				newSection := false
				for i, s := range d.Specs {
					if i > 0 {
						self.linebreak(self.lineFor(s.Pos()), 1, ignore, newSection)

					}
					self.spec(s, n, false)
					newSection = self.isMultiLine(s)

				}
			}
			self.print(unindent, formfeed)

		}
	} else { // single declaration
		self.spec(d.Specs[0], 1, true)

	} // nodeSize determines the size of n in chars after formatting.
	// The result is <= maxSize if the node fits on one line with at
	// most maxSize chars and the formatted output doesn't contain
	// any control chars. Otherwise, the result is > maxSize.
	//
}
func (self *printer) nodeSize(n ast.Node, maxSize int) (size int) { // nodeSize invokes the printer, which may invoke nodeSize
	// recursively. For deep composite literal nests, this can
	// lead to an exponential algorithm. Remember previous
	// results to prune the recursion (was issue 1628).
	if size, found := self.nodeSizes[n]; found {
		return size

	}
	size = maxSize + 1 // assume n doesn't fit
	self.nodeSizes[n] = size

	// nodeSize computation must be independent of particular
	// style so that we always get the same decision; print
	// in RawFormat
	cfg := Config{Mode: RawFormat}
	var buf bytes.Buffer
	if err := cfg.fprint(&buf, self.fset, n, self.nodeSizes); err != nil {
		return

	}
	if buf.Len() <= maxSize {
		for _, ch := range buf.Bytes() {
			if ch < ' ' {
				return

			}
		}
		size = buf.Len() // n fits
		self.nodeSizes[n] = size

	}
	return
}

// bodySize is like nodeSize but it is specialized for *ast.BlockStmt's.
func (self *printer) bodySize(b *ast.BlockStmt, maxSize int) int {
	pos1 := b.Pos()
	pos2 := b.Rbrace
	if pos1.IsValid() && pos2.IsValid() && self.lineFor(pos1) != self.lineFor(pos2) { // opening and closing brace are on different lines - don't make it a one-liner
		return maxSize + 1

	}
	if len(b.List) > 5 || self.commentBefore(self.posFor(pos2)) { // too many statements or there is a comment inside - don't make it a one-liner
		return maxSize + 1

	} // otherwise, estimate body size
	bodySize := 0
	for i, s := range b.List {
		if i > 0 {
			bodySize += 2 // space for a semicolon and blank

		}
		bodySize += self.nodeSize(s, maxSize)

	}
	return bodySize
}

// adjBlock prints an "adjacent" block (e.g., a for-loop or function body) following
// a header (e.g., a for-loop control clause or function signature) of given headerSize.
// If the header's and block's size are "small enough" and the block is "simple enough",
// the block is printed on the current line, without line breaks, spaced from the header
// by sep. Otherwise the block's opening "{" is printed on the current line, followed by
// lines for the block's statements and its closing "}".
//
func (self *printer) adjBlock(b *ast.BlockStmt) {
	if b == nil {
		return

	}
	switch len(b.List) {
	case 0:

		self.print(token.COLON)
		return

	case 1:

		if !self.commentNewline {
			switch s := b.List[0]; s.(type) {
			case *ast.ReturnStmt, *ast.BranchStmt, *ast.EmptyStmt, *ast.IncDecStmt:

				self.print(token.COLON, blank)
				self.stmt(s, true)
				return



			}
		}
		fallthrough

	default:

		self.block(b, 1)

		// distanceFrom returns the column difference between from and p.pos (the current
		// estimated position) if both are on the same line; if they are on different lines
		// (or unknown) the result is infinity.
	}
}
func (self *printer) distanceFrom(from token.Pos) int {
	if from.IsValid() && self.pos.IsValid() {
		if f := self.posFor(from); f.Line == self.pos.Line {
			return self.pos.Column - f.Column

		}
	}
	return infinity

}
func (self *printer) funcDecl(d *ast.FuncDecl) {
	self.setComment(d.Doc)
	self.print(d.Pos(), token.FUNC, blank)
	if d.Recv != nil {
		self.expr(d.Recv.List[0].Type) // method: print receiver
		self.print(d.Pos(), ".")
		if names := d.Recv.List[0].Names; len(names) > 0 {
			if name := names[0]; name != nil && name.Name != "_" {
				self.rcvName = name
				defer func() {
					self.rcvName = nil
				}()

			}
		}
	}
	self.expr(d.Name)
	self.signature(d.Type.Params, d.Type.Results)
	self.inFunc = true
	self.adjBlock(d.Body)
	self.inFunc = false
	self.print(unindent)

}
func (self *printer) decl(decl ast.Decl) {
	switch d := decl.(type) {
	case *ast.BadDecl:

		self.print(d.Pos(), "BadDecl")

	case *ast.GenDecl:

		self.genDecl(d)

	case *ast.FuncDecl:

		self.funcDecl(d)

	default:

		panic("unreachable")

		// ----------------------------------------------------------------------------
		// Files
	}
}
func declToken(decl ast.Decl) (tok token.Token) {
	tok = token.ILLEGAL
	switch d := decl.(type) {
	case *ast.GenDecl:

		tok = d.Tok

	case *ast.FuncDecl:

		tok = token.FUNC



	}
	return

}
func (self *printer) declList(list []ast.Decl) {
	tok := token.ILLEGAL
	for _, d := range list {
		prev := tok
		tok = declToken(d)
		// If the declaration token changed (e.g., from CONST to TYPE)
		// or the next declaration has documentation associated with it,
		// print an empty line between top-level declarations.
		// (because p.linebreak is called with the position of d, which
		// is past any documentation, the minimum requirement is satisfied
		// even w/o the extra getDoc(d) nil-check - leave it in case the
		// linebreak logic improves - there's already a TODO).
		if len(self.output) > 0 { // only print line break if we are not at the beginning of the output
			// (i.e., we are not printing only a partial program)
			min := 1
			if prev != tok || getDoc(d) != nil {
				min = 2

			}
			self.linebreak(self.lineFor(d.Pos()), min, ignore, false)

		}
		self.decl(d)

	}
}
func (self *printer) file(src *ast.File) {
	self.setComment(src.Doc)
	self.print(src.Pos(), token.PACKAGE, blank)
	self.expr(src.Name)
	self.declList(src.Decls)
	self.print(newline)
}
