// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package parser implements a parser for Go source files. Input may be
// provided in a variety of forms (see the various Parse* functions); the
// output is an abstract syntax tree (AST) representing the Go source. The
// parser is invoked through one of the Parse* functions.
//
package parser

import (
	"fmt"
	"github.com/DAddYE/igo/ast"
	"github.com/DAddYE/igo/scanner"
	"github.com/DAddYE/igo/token"
	"strconv"
	"strings"
	"unicode"
)

// The parser structure holds the parser's internal state.
type parser struct {
	file    *token.File
	errors  scanner.ErrorList
	scanner scanner.Scanner

	// Tracing/debugging
	mode   Mode // parsing mode
	trace  bool // == (mode & Trace != 0)
	indent int  // indentation used for tracing output

	// Comments
	comments    []*ast.CommentGroup
	leadComment *ast.CommentGroup // last lead comment
	lineComment *ast.CommentGroup // last line comment

	// Next token
	pos  token.Pos   // token position
	tok  token.Token // one token look-ahead
	ptok token.Token // one token look-back
	lit  string      // token literal

	// Error recovery
	// (used to limit the number of calls to syncXXX functions
	// w/o making scanning progress - avoids potential endless
	// loops across multiple parser functions during error recovery)
	syncPos token.Pos // last synchronization position
	syncCnt int       // number of calls to syncXXX without progress

	// Non-syntactic parser control
	exprLev         int           // < 0: in control clause, >= 0: in expression
	inRhs           bool          // if set, the parser is parsing a rhs expression
	call            *ast.CallExpr // control if we are in a call
	inDo            bool          // control if we are in a do literal
	allowEmptyBlock bool          // control if we allowe empty blocks

	// Ordinary identifier scopes
	pkgScope   *ast.Scope        // pkgScope.Outer == nil
	topScope   *ast.Scope        // top-most scope; may be pkgScope
	unresolved []*ast.Ident      // unresolved identifiers
	imports    []*ast.ImportSpec // list of imports

	// Label scopes
	// (maintained by open/close LabelScope)
	labelScope  *ast.Scope     // label scope for current function
	targetStack [][]*ast.Ident // stack of unresolved labels
}

func (self *parser) init(fset *token.FileSet, filename string, src []byte, mode Mode) {
	self.file = fset.AddFile(filename, fset.Base(), len(src))
	var m scanner.Mode
	if mode&ParseComments != 0 {
		m = scanner.ScanComments

	}
	eh := func(pos token.Position, msg string) {
		self.errors.Add(pos, msg)

	}
	self.scanner.Init(self.file, src, eh, m)

	self.mode = mode
	self.trace = mode&Trace != 0 // for convenience (p.trace is used frequently)

	self.next()
}

// ----------------------------------------------------------------------------
// Scoping support

func (self *parser) openScope() {
	self.topScope = ast.NewScope(self.topScope)

}
func (self *parser) closeScope() {
	self.topScope = self.topScope.Outer

}
func (self *parser) openLabelScope() {
	self.labelScope = ast.NewScope(self.labelScope)
	self.targetStack = append(self.targetStack, nil)

}
func (self *parser) closeLabelScope() { // resolve labels
	n := len(self.targetStack) - 1
	scope := self.labelScope
	for _, ident := range self.targetStack[n] {
		ident.Obj = scope.Lookup(ident.Name)
		if ident.Obj == nil && self.mode&DeclarationErrors != 0 {
			self.error(ident.Pos(), fmt.Sprintf("label %s undefined", ident.Name))

		} // pop label scope
	}
	self.targetStack = self.targetStack[0:n]
	self.labelScope = self.labelScope.Outer

}
func (self *parser) declare(decl, data interface{}, scope *ast.Scope, kind ast.ObjKind, idents ...*ast.Ident) {
	for _, ident := range idents {
		assert(ident.Obj == nil, "identifier already declared or resolved")
		obj := ast.NewObj(kind, ident.Name)
		// remember the corresponding declaration for redeclaration
		// errors and global variable resolution/typechecking phase
		obj.Decl = decl
		obj.Data = data
		ident.Obj = obj
		if ident.Name != "_" {
			if alt := scope.Insert(obj); alt != nil && self.mode&DeclarationErrors != 0 {
				prevDecl := ""
				if pos := alt.Pos(); pos.IsValid() {
					prevDecl = fmt.Sprintf("\n\tprevious declaration at %s", self.file.Position(pos))

				}
				self.error(ident.Pos(), fmt.Sprintf("%s redeclared in this block%s", ident.Name, prevDecl))

			}
		}
	}
}
func (self *parser) isIndent() bool {
	return self.tok == token.SEMICOLON && self.lit == "\n"

}
func (self *parser) shortVarDecl(decl *ast.AssignStmt, list []ast.Expr) { // Go spec: A short variable declaration may redeclare variables
	// provided they were originally declared in the same block with
	// the same type, and at least one of the non-blank variables is new.
	n := 0 // number of new variables
	for _, x := range list {
		if ident, isIdent := x.(*ast.Ident); isIdent {
			assert(ident.Obj == nil, "identifier already declared or resolved")
			obj := ast.NewObj(ast.Var, ident.Name)
			// remember corresponding assignment for other tools
			obj.Decl = decl
			ident.Obj = obj
			if ident.Name != "_" {
				if alt := self.topScope.Insert(obj); alt != nil {
					ident.Obj = alt
				} else /* redeclaration  */ 

				{
					n++

				}
			}
		} else /* new declaration  */ 

		{
			self.errorExpected(x.Pos(), "identifier on left side of :=")

		}
	}
	if n == 0 && self.mode&DeclarationErrors != 0 {
		self.error(list[0].Pos(), "no new variables on left side of :=")

	} // The unresolved object is a sentinel to mark identifiers that have been added
	// to the list of unresolved identifiers. The sentinel is only used for verifying
	// internal consistency.
}

var unresolved = new(ast.Object)

// If x is an identifier, tryResolve attempts to resolve x by looking up
// the object it denotes. If no object is found and collectUnresolved is
// set, x is marked as unresolved and collected in the list of unresolved
// identifiers.
//
func (self *parser) tryResolve(x ast.Expr, collectUnresolved bool) { // nothing to do if x is not an identifier or the blank identifier
	ident, _ := x.(*ast.Ident)
	if ident == nil {
		return

	}
	assert(ident.Obj == nil, "identifier '"+ident.Name+"' already declared or resolved")
	if ident.Name == "_" {
		return

	} // try to resolve the identifier
	for s := self.topScope; s != nil; s = s.Outer {
		if obj := s.Lookup(ident.Name); obj != nil {
			ident.Obj = obj
			return

		} // all local scopes are known, so any unresolved identifier
		// must be found either in the file scope, package scope
		// (perhaps in another file), or universe scope --- collect
		// them so that they can be resolved later
	}
	if collectUnresolved {
		ident.Obj = unresolved
		self.unresolved = append(self.unresolved, ident)

	}
}
func (self *parser) resolve(x ast.Expr) {
	self.tryResolve(x, true)
}

// ----------------------------------------------------------------------------
// Parsing support

func (self *parser) printTrace(a ...interface{}) {
	const dots = ". . . . . . . . . . . . . . . . . . . . . . . . . . . . . . . . "
	const n = len(dots)
	pos := self.file.Position(self.pos)
	fmt.Printf("%5d:%3d: ", pos.Line, pos.Column)
	i := 2 * self.indent
	for i > n {
		fmt.Print(dots)
		i -= n

	} // i <= n
	fmt.Print(dots[0:i])
	fmt.Println(a...)

}
func trace(p *parser, msg string) *parser {
	p.printTrace(msg, "(")
	p.indent++
	return p
}

// Usage pattern: defer un(trace(p, "..."))
func un(p *parser) {
	p.indent--
	p.printTrace(")")
}

// Advance to the next token.
func (self *parser) next0() { // Because of one-token look-ahead, print the previous token
	// when tracing as it provides a more readable output. The
	// very first token (!p.pos.IsValid()) is not initialized
	// (it is token.ILLEGAL), so don't print it .
	if self.trace && self.pos.IsValid() {
		s := self.tok.String()
		switch {
		case self.tok.IsLiteral():

			self.printTrace(s, self.lit)

		case self.tok.IsOperator(), self.tok.IsKeyword():

			self.printTrace("\"" + s + "\"")

		default:

			self.printTrace(s)



		}
	}
	self.ptok = self.tok
	self.pos, self.tok, self.lit = self.scanner.Scan()
}

// Consume a comment and return it and the line on which it ends.
func (self *parser) consumeComment() (comment *ast.Comment, endline int) {
	endline = self.file.Line(self.pos)
	comment = &ast.Comment{Slash: self.pos, Text: self.lit}
	self.next0()
	return
}

// Consume a group of adjacent comments, add it to the parser's
// comments list, and return it together with the line at which
// the last comment in the group ends. A non-comment token or n
// empty lines terminate a comment group.
//
func (self *parser) consumeCommentGroup(n int) (comments *ast.CommentGroup, endline int) {
	var list []*ast.Comment
	endline = self.file.Line(self.pos)
	for self.tok == token.COMMENT && self.file.Line(self.pos) <= endline+n {
		var comment *ast.Comment
		comment, endline = self.consumeComment()
		list = append(list, comment)

	} // add comment group to the comments list
	comments = &ast.CommentGroup{List: list}
	self.comments = append(self.comments, comments)

	return
}

// Advance to the next non-comment token. In the process, collect
// any comment groups encountered, and remember the last lead and
// and line comments.
//
// A lead comment is a comment group that starts and ends in a
// line without any other tokens and that is followed by a non-comment
// token on the line immediately after the comment group.
//
// A line comment is a comment group that follows a non-comment
// token on the same line, and that has no tokens after it on the line
// where it ends.
//
// Lead and line comments may be considered documentation that is
// stored in the AST.
//
func (self *parser) next() {
	self.leadComment = nil
	self.lineComment = nil
	prev := self.pos
	self.next0()

	if self.tok == token.COMMENT {
		var comment *ast.CommentGroup
		var endline int

		if self.file.Line(self.pos) == self.file.Line(prev) { // The comment is on same line as the previous token; it
			// cannot be a lead comment but may be a line comment.
			comment, endline = self.consumeCommentGroup(0)
			if self.file.Line(self.pos) != endline { // The next token is on a different line, thus
				// the last comment group is a line comment.
				self.lineComment = comment

			} // consume successor comments, if any
		}
		endline = -1
		for self.tok == token.COMMENT {
			comment, endline = self.consumeCommentGroup(1)

		}
		if endline+1 == self.file.Line(self.pos) { // The next token is following on the line immediately after the
			// comment group, thus the last comment group is a lead comment.
			self.leadComment = comment

		} // A bailout panic is raised to indicate early termination.
	}
}

type bailout struct{}

func (self *parser) error(pos token.Pos, msg string) {
	epos := self.file.Position(pos)

	// If AllErrors is not set, discard errors reported on the same line
	// as the last recorded error and stop parsing if there are more than
	// 10 errors.
	if self.mode&AllErrors == 0 {
		n := len(self.errors)
		if n > 0 && self.errors[n-1].Pos.Line == epos.Line {
			return // discard - likely a spurious error

		}
		if n > 10 {
			panic(bailout{})

		}
	}
	self.errors.Add(epos, msg)

}
func (self *parser) errorExpected(pos token.Pos, msg string) {
	msg = "expected " + msg
	if pos == self.pos { // the error happened at the current position;
		// make the error message more specific
		if self.isIndent() {
			msg += ", found newline"
		} else {
			msg += ", found '" + self.tok.String() + "'"
			if self.tok.IsLiteral() {
				msg += " " + self.lit

			}
		}
	}
	panic(fmt.Sprintf("%s %s", self.file.Position(pos), msg))
	// p.error(pos, msg)
}
func (self *parser) expect(tok token.Token) token.Pos {
	pos := self.pos
	if self.tok != tok {
		self.errorExpected(pos, "'"+tok.String()+"'")

	}
	self.next() // make progress
	return pos
}

// expectClosing is like expect but provides a better error message
// for the common case of a missing comma before a newline.
//
func (self *parser) expectClosing(tok token.Token, context string) token.Pos {
	if self.tok != tok && self.isIndent() {
		self.error(self.pos, "missing ',' before newline in "+context)
		self.next()

	}
	return self.expect(tok)

}
func (self *parser) expectSemi() { // semicolon is optional before:
	if self.tok != token.RPAREN && self.tok != token.RBRACE && self.tok != token.DEDENT {
		switch {
		case self.tok == token.SEMICOLON:

			self.next()

		case self.ptok == token.SEMICOLON:
			// semicolon doesn't need to be consecutive ';;'
			// nothing. Already consumed.
		case self.ptok == token.COMMENT:
			// Comment has not semi after
		case self.ptok == token.DEDENT:
			// dedent is semi
		default:

			self.errorExpected(self.pos, "';'")
			syncStmt(self)



		}
	}
}
func (self *parser) atComma(context string) bool {
	if self.tok == token.COMMA {
		return true

	}
	if self.isIndent() {
		self.error(self.pos, "missing ',' before newline in "+context)
		return true // "insert" the comma and continue

	}
	return false

}
func assert(cond bool, msg string) {
	if !cond {
		panic("go/parser internal error: " + msg)

	} // syncStmt advances to the next statement.
	// Used for synchronization after an error.
	//
}
func syncStmt(p *parser) {
	for {
		switch p.tok {
		case token.BREAK, token.CONST, token.CONTINUE, token.DEFER,
			token.FALLTHROUGH, token.FOR, token.GO, token.GOTO,
			token.IF, token.RETURN, token.SELECT, token.SWITCH,
			token.TYPE, token.VAR:
			// Return only if parser made some progress since last
			// sync or if it has not reached 10 sync calls without
			// progress. Otherwise consume at least one token to
			// avoid an endless parser loop (it is possible that
			// both parseOperand and parseStmt call syncStmt and
			// correctly do not advance, thus the need for the
			// invocation limit p.syncCnt).
			if p.pos == p.syncPos && p.syncCnt < 10 {
				p.syncCnt++
				return

			}
			if p.pos > p.syncPos {
				p.syncPos = p.pos
				p.syncCnt = 0
				return

				// Reaching here indicates a parser bug, likely an
				// incorrect token list in this function, but it only
				// leads to skipping of possibly correct code if a
				// previous error is present, and thus is preferred
				// over a non-terminating parse.
			}

		case token.EOF:

			return



		}
		p.next()

	} // syncDecl advances to the next declaration.
	// Used for synchronization after an error.
	//
}
func syncDecl(p *parser) {
	for {
		switch p.tok {
		case token.CONST, token.TYPE, token.VAR:
			// see comments in syncStmt
			if p.pos == p.syncPos && p.syncCnt < 10 {
				p.syncCnt++
				return

			}
			if p.pos > p.syncPos {
				p.syncPos = p.pos
				p.syncCnt = 0
				return

			}

		case token.EOF:

			return



		}
		p.next()

	} // ----------------------------------------------------------------------------
	// Identifiers
}
func (self *parser) parseIdent() *ast.Ident {
	pos := self.pos
	name := "_"
	if self.tok == token.IDENT {
		name = self.lit
		self.next()
	} else {
		self.expect(token.IDENT) // use expect() error handling

	}
	return &ast.Ident{NamePos: pos, Name: name}

}
func (self *parser) parseIdentList() (list []*ast.Ident) {
	if self.trace {
		defer un(trace(self, "IdentList"))

	}
	list = append(list, self.parseIdent())
	for self.tok == token.COMMA {
		self.next()
		list = append(list, self.parseIdent())

	}
	return
}

// ----------------------------------------------------------------------------
// Common productions

// If lhs is set, result list elements which are identifiers are not resolved.
func (self *parser) parseExprList(lhs bool) (list []ast.Expr) {
	if self.trace {
		defer un(trace(self, "ExpressionList"))

	}
	list = append(list, self.checkExpr(self.parseExpr(lhs)))
	for self.tok == token.COMMA {
		self.next()
		list = append(list, self.checkExpr(self.parseExpr(lhs)))

	}
	return

}
func (self *parser) parseLhsList() []ast.Expr {
	old := self.inRhs
	self.inRhs = false
	list := self.parseExprList(true)
	switch self.tok {
	case token.DEFINE:
		// lhs of a short variable declaration
		// but doesn't enter scope until later:
		// caller must call p.shortVarDecl(p.makeIdentList(list))
		// at appropriate time.
	case token.COLON:
		// lhs of a label declaration or a communication clause of a select
		// statement (parseLhsList is not called when parsing the case clause
		// of a switch statement):
		// - labels are declared by the caller of parseLhsList
		// - for communication clauses, if there is a stand-alone identifier
		//   followed by a colon, we have a syntax error; there is no need
		//   to resolve the identifier in that case
	default:
		// identifiers must be declared elsewhere
		for _, x := range list {
			self.resolve(x)

		}

	}
	self.inRhs = old
	return list

}
func (self *parser) parseRhsList() []ast.Expr {
	old := self.inRhs
	self.inRhs = true
	list := self.parseExprList(false)
	self.inRhs = old
	return list
}

// ----------------------------------------------------------------------------
// Types

func (self *parser) parseType() ast.Expr {
	if self.trace {
		defer un(trace(self, "Type"))

	}
	typ := self.tryType()

	if typ == nil {
		pos := self.pos
		self.errorExpected(pos, "type")
		self.next() // make progress
		return &ast.BadExpr{From: pos, To: self.pos}

	}
	return typ
}

// If the result is an identifier, it is not resolved.
func (self *parser) parseTypeName() ast.Expr {
	if self.trace {
		defer un(trace(self, "TypeName"))

	}
	ident := self.parseIdent()
	// don't resolve ident yet - it may be a parameter or field name

	if self.tok == token.PERIOD { // ident is a package name
		self.next()
		self.resolve(ident)
		sel := self.parseIdent()
		return &ast.SelectorExpr{X: ident, Sel: sel}

	}
	return ident

}
func (self *parser) parseArrayType() ast.Expr {
	if self.trace {
		defer un(trace(self, "ArrayType"))

	}
	lbrack := self.expect(token.LBRACK)
	var len ast.Expr
	// always permit ellipsis for more fault-tolerant parsing
	if self.tok == token.ELLIPSIS {
		len = &ast.Ellipsis{Ellipsis: self.pos}
		self.next()
	} else if self.tok != token.RBRACK {
		len = self.parseRhs()

	}
	self.expect(token.RBRACK)
	elt := self.parseType()

	return &ast.ArrayType{Lbrack: lbrack, Len: len, Elt: elt}

}
func (self *parser) makeIdentList(list []ast.Expr) []*ast.Ident {
	idents := make([]*ast.Ident, len(list))
	for i, x := range list {
		ident, isIdent := x.(*ast.Ident)
		if !isIdent {
			if _, isBad := x.(*ast.BadExpr); !isBad { // only report error if it's a new one
				self.errorExpected(x.Pos(), "identifier")

			}
			ident = &ast.Ident{NamePos: x.Pos(), Name: "_"}

		}
		idents[i] = ident

	}
	return idents

}
func (self *parser) parseFieldDecl(scope *ast.Scope) *ast.Field {
	if self.trace {
		defer un(trace(self, "FieldDecl"))

	}
	doc := self.leadComment

	// FieldDecl
	list, typ := self.parseVarList(false)

	// Tag
	var tag *ast.BasicLit
	if self.tok == token.STRING {
		tag = &ast.BasicLit{ValuePos: self.pos, Kind: self.tok, Value: self.lit}
		self.next()

	} // analyze case
	var idents []*ast.Ident
	if typ != nil { // IdentifierList Type
		idents = self.makeIdentList(list)
	} else { // ["*"] TypeName (AnonymousField)
		typ = list[0] // we always have at least one element
		if n := len(list); n > 1 || !isTypeName(deref(typ)) {
			pos := typ.Pos()
			self.errorExpected(pos, "anonymous field")
			typ = &ast.BadExpr{From: pos, To: list[n-1].End()}

		} // Allow multiple types on the same line
	}
	if self.tok == token.SEMICOLON {
		self.expectSemi() // call before accessing p.linecomment

	}
	field := &ast.Field{Doc: doc, Names: idents, Type: typ, Tag: tag, Comment: self.lineComment}
	self.declare(field, nil, scope, ast.Var, idents...)
	self.resolve(typ)

	return field

}
func (self *parser) parseStructType() *ast.StructType {
	if self.trace {
		defer un(trace(self, "StructType"))

	}
	pos := self.expect(token.STRUCT)

	scope := ast.NewScope(nil) // interface scope

	var start, end token.Pos
	var list []*ast.Field

	switch self.tok {
	case token.COLON:

		start = self.expect(token.COLON)
		if self.tok == token.IDENT || self.tok == token.MUL || self.tok == token.LPAREN {
			list = append(list, self.parseFieldDecl(scope))
		} else {
			self.expect(token.IDENT)

		}

	case token.SEMICOLON:

		self.expectSemi()
		if self.tok == token.INDENT {
			start = self.expect(token.INDENT)
			for self.tok == token.IDENT {
				list = append(list, self.parseFieldDecl(scope))

			}
			end = self.expect(token.DEDENT)
		} else {
			start, end = pos, pos

		}

	default:
		// Allow unbraced types https://gist.github.com/DAddYE/d7d11c0879188dd3fb86
		start, end = pos, pos



	}
	return &ast.StructType{
		Struct: pos,
		Fields: &ast.FieldList{
			Opening: start,
			List:    list,
			Closing: end,
		},
	}

}
func (self *parser) parsePointerType() *ast.StarExpr {
	if self.trace {
		defer un(trace(self, "PointerType"))

	}
	star := self.expect(token.MUL)
	base := self.parseType()

	return &ast.StarExpr{Star: star, X: base}
}

// If the result is an identifier, it is not resolved.
func (self *parser) tryVarType(isParam bool) ast.Expr {
	if isParam && self.tok == token.ELLIPSIS {
		pos := self.pos
		self.next()
		typ := self.tryIdentOrType() // don't use parseType so we can provide better error message
		if typ != nil {
			self.resolve(typ)
		} else {
			self.error(pos, "'...' parameter is missing type")
			typ = &ast.BadExpr{From: pos, To: self.pos}

		}
		return &ast.Ellipsis{Ellipsis: pos, Elt: typ}

	}
	return self.tryIdentOrType()
}

// If the result is an identifier, it is not resolved.
func (self *parser) parseVarType(isParam bool) ast.Expr {
	typ := self.tryVarType(isParam)
	if typ == nil {
		pos := self.pos
		self.errorExpected(pos, "type")
		self.next() // make progress
		typ = &ast.BadExpr{From: pos, To: self.pos}

	}
	return typ
}

// If any of the results are identifiers, they are not resolved.
func (self *parser) parseVarList(isParam bool) (list []ast.Expr, typ ast.Expr) {
	if self.trace {
		defer un(trace(self, "VarList"))

	} // a list of identifiers looks like a list of type names
	//
	// parse/tryVarType accepts any type (including parenthesized
	// ones) even though the syntax does not permit them here: we
	// accept them all for more robust parsing and complain later
	for typ := self.parseVarType(isParam); typ != nil; {
		list = append(list, typ)
		if self.tok != token.COMMA {
			break

		}
		self.next()
		typ = self.tryVarType(isParam) // maybe nil as in: func f(int,) {}

	} // if we had a list of identifiers, it must be followed by a type
	typ = self.tryVarType(isParam)

	return

}
func (self *parser) parseParameterList(scope *ast.Scope, ellipsisOk bool) (params []*ast.Field) {
	if self.trace {
		defer un(trace(self, "ParameterList"))

	} // ParameterDecl
	list, typ := self.parseVarList(ellipsisOk)

	// analyze case
	if typ != nil { // IdentifierList Type
		idents := self.makeIdentList(list)
		field := &ast.Field{Names: idents, Type: typ}
		params = append(params, field)
		// Go spec: The scope of an identifier denoting a function
		// parameter or result variable is the function body.
		self.declare(field, nil, scope, ast.Var, idents...)
		self.resolve(typ)
		if self.tok == token.COMMA {
			self.next()

		}
		for self.tok != token.RPAREN && self.tok != token.EOF {
			idents := self.parseIdentList()
			typ := self.parseVarType(ellipsisOk)
			field := &ast.Field{Names: idents, Type: typ}
			params = append(params, field)
			// Go spec: The scope of an identifier denoting a function
			// parameter or result variable is the function body.
			self.declare(field, nil, scope, ast.Var, idents...)
			self.resolve(typ)
			if !self.atComma("parameter list") {
				break

			}
			self.next()

		}
	} else { // Type { "," Type } (anonymous parameters)
		params = make([]*ast.Field, len(list))
		for i, typ := range list {
			self.resolve(typ)
			params[i] = &ast.Field{Type: typ}

		}
	}
	return

}
func (self *parser) parseParameters(scope *ast.Scope, ellipsisOk bool) *ast.FieldList {
	if self.trace {
		defer un(trace(self, "Parameters"))

	}
	var params []*ast.Field
	lparen := self.expect(token.LPAREN)
	if self.tok != token.RPAREN {
		params = self.parseParameterList(scope, ellipsisOk)

	}
	rparen := self.expect(token.RPAREN)

	return &ast.FieldList{Opening: lparen, List: params, Closing: rparen}

}
func (self *parser) parseResult(scope *ast.Scope) *ast.FieldList {
	if self.trace {
		defer un(trace(self, "Result"))

	}
	if self.tok == token.LPAREN {
		return self.parseParameters(scope, false)

	}
	typ := self.tryType()
	if typ != nil {
		list := make([]*ast.Field, 1)
		list[0] = &ast.Field{Type: typ}
		return &ast.FieldList{List: list}

	}
	return nil

}
func (self *parser) parseSignature(scope *ast.Scope) (params, results *ast.FieldList) {
	if self.trace {
		defer un(trace(self, "Signature"))

	}
	params = self.parseParameters(scope, true)
	results = self.parseResult(scope)

	return

}
func (self *parser) parseFuncType() (*ast.FuncType, *ast.Scope) {
	if self.trace {
		defer un(trace(self, "FuncType"))

	}
	pos := self.expect(token.FUNC)
	scope := ast.NewScope(self.topScope) // function scope
	params, results := self.parseSignature(scope)

	return &ast.FuncType{Func: pos, Params: params, Results: results}, scope

}
func (self *parser) parseMethodSpec(scope *ast.Scope) *ast.Field {
	if self.trace {
		defer un(trace(self, "MethodSpec"))

	}
	doc := self.leadComment
	var idents []*ast.Ident
	var typ ast.Expr
	x := self.parseTypeName()
	if ident, isIdent := x.(*ast.Ident); isIdent && self.tok == token.LPAREN { // method
		idents = []*ast.Ident{ident}
		scope := ast.NewScope(nil) // method scope
		params, results := self.parseSignature(scope)
		typ = &ast.FuncType{Func: token.NoPos, Params: params, Results: results}
	} else { // embedded interface
		typ = x
		self.resolve(typ)

	} // We can allow it on the same line
	if self.tok == token.SEMICOLON {
		self.expectSemi() // call before accessing p.linecomment

	}
	spec := &ast.Field{Doc: doc, Names: idents, Type: typ, Comment: self.lineComment}
	self.declare(spec, nil, scope, ast.Fun, idents...)

	return spec

}
func (self *parser) parseInterfaceType() *ast.InterfaceType {
	if self.trace {
		defer un(trace(self, "InterfaceType"))

	}
	pos := self.expect(token.INTERFACE)
	scope := ast.NewScope(nil) // interface scope

	var start, end token.Pos
	var list []*ast.Field

	switch self.tok {
	case token.COLON:

		start = self.expect(token.COLON)
		if self.tok == token.IDENT {
			list = append(list, self.parseMethodSpec(scope))
		} else {
			self.expect(token.IDENT)

		}

	case token.SEMICOLON:

		self.expectSemi()
		if self.tok == token.INDENT {
			start = self.expect(token.INDENT)
			for self.tok == token.IDENT {
				list = append(list, self.parseMethodSpec(scope))

			}
			end = self.expect(token.DEDENT)
		} else {
			start, end = pos, pos

		}

	default:
		// Allow unbraced types https://gist.github.com/DAddYE/d7d11c0879188dd3fb86
		start, end = pos, pos



	}
	return &ast.InterfaceType{
		Interface: pos,
		Methods: &ast.FieldList{
			Opening: start,
			List:    list,
			Closing: end,
		},
	}

}
func (self *parser) parseMapType() *ast.MapType {
	if self.trace {
		defer un(trace(self, "MapType"))

	}
	pos := self.expect(token.MAP)
	self.expect(token.LBRACK)
	key := self.parseType()
	self.expect(token.RBRACK)
	value := self.parseType()

	return &ast.MapType{Map: pos, Key: key, Value: value}

}
func (self *parser) parseChanType() *ast.ChanType {
	if self.trace {
		defer un(trace(self, "ChanType"))

	}
	pos := self.pos
	dir := ast.SEND | ast.RECV
	var arrow token.Pos
	if self.tok == token.CHAN {
		self.next()
		if self.tok == token.ARROW {
			arrow = self.pos
			self.next()
			dir = ast.SEND

		}
	} else {
		arrow = self.expect(token.ARROW)
		self.expect(token.CHAN)
		dir = ast.RECV

	}
	value := self.parseType()

	return &ast.ChanType{Begin: pos, Arrow: arrow, Dir: dir, Value: value}
}

// If the result is an identifier, it is not resolved.
func (self *parser) tryIdentOrType() ast.Expr {
	switch self.tok {
	case token.IDENT:

		return self.parseTypeName()

	case token.LBRACK:

		return self.parseArrayType()

	case token.STRUCT:

		return self.parseStructType()

	case token.MUL:

		return self.parsePointerType()

	case token.FUNC:

		typ, _ := self.parseFuncType()
		return typ

	case token.INTERFACE:

		return self.parseInterfaceType()

	case token.MAP:

		return self.parseMapType()

	case token.CHAN, token.ARROW:

		return self.parseChanType()

	case token.LPAREN:

		lparen := self.pos
		self.next()
		typ := self.parseType()
		rparen := self.expect(token.RPAREN)
		return &ast.ParenExpr{Lparen: lparen, X: typ, Rparen: rparen}

		// no type found
	}
	return nil

}
func (self *parser) tryType() ast.Expr {
	typ := self.tryIdentOrType()
	if typ != nil {
		self.resolve(typ)

	}
	return typ
}

// ----------------------------------------------------------------------------
// Blocks

func (self *parser) parseStmtList() (list []ast.Stmt) {
	if self.trace {
		defer un(trace(self, "StatementList"))

	}
	for self.tok != token.CASE &&
		self.tok != token.DEFAULT &&
		self.tok != token.DEDENT &&
		self.tok != token.EOF {
		list = append(list, self.parseStmt())

	}
	return

}
func (self *parser) parseBody(scope *ast.Scope) *ast.BlockStmt {
	if self.trace {
		defer un(trace(self, "Body"))

	}
	if self.tok == token.COLON {
		colon := self.expect(token.COLON)
		self.topScope = scope // open function scope
		// Allow empty body
		var list []ast.Stmt
		if self.tok == token.SEMICOLON {
			self.expectSemi()
		} else {
			list = []ast.Stmt{self.parseSmallStmt()}

		}
		self.closeScope()

		return &ast.BlockStmt{Opening: colon, List: list, Closing: self.pos, Small: true}
	} else {
		self.expectSemi()

		switch {
		case self.tok == token.INDENT:

			indent := self.expect(token.INDENT)
			self.openScope()
			self.openLabelScope()
			list := self.parseStmtList()
			self.closeLabelScope()
			self.closeScope()
			dedent := self.expect(token.DEDENT)
			return &ast.BlockStmt{Opening: indent, List: list, Closing: dedent}

		case self.allowEmptyBlock:

			return &ast.BlockStmt{Opening: self.pos, Closing: self.pos}



		}
		self.errorExpected(self.pos, "block")
		return &ast.BlockStmt{Opening: self.pos, Closing: self.pos}

	}
}
func (self *parser) parseBlockStmt() *ast.BlockStmt {
	if self.trace {
		defer un(trace(self, "BlockStmt"))

	}
	if self.tok == token.COLON {
		colon := self.expect(token.COLON)
		self.openScope()
		list := []ast.Stmt{self.parseSmallStmt()}
		self.closeScope()
		pos := self.pos
		self.expectSemi()
		return &ast.BlockStmt{Opening: colon, List: list, Closing: pos, Small: true}
	} else {
		self.expectSemi()

		switch {
		case self.tok == token.INDENT:

			indent := self.expect(token.INDENT)
			self.openScope()
			list := self.parseStmtList()
			self.closeScope()
			dedent := self.expect(token.DEDENT)
			return &ast.BlockStmt{Opening: indent, List: list, Closing: dedent}

		case self.allowEmptyBlock:

			return &ast.BlockStmt{Opening: self.pos, Closing: self.pos}



		}
		self.errorExpected(self.pos, "block")
		return &ast.BlockStmt{Opening: self.pos, Closing: self.pos}

	} // ----------------------------------------------------------------------------
	// Expressions
}
func (self *parser) parseFuncTypeOrLit() ast.Expr {
	if self.trace {
		defer un(trace(self, "FuncTypeOrLit"))

	}
	typ, scope := self.parseFuncType()

	self.exprLev++
	body := self.parseBody(scope)
	self.exprLev--

	// Function type is when there is no comma and empty body
	// func a(): // empty
	// a := func() // type
	// Remember that 'small' implies ':'
	if !body.Small && body.List == nil { // function type only
		return typ

	}
	return &ast.FuncLit{Type: typ, Body: body}
}

// parseOperand may return an expression or a raw type (incl. array
// types of the form [...]T. Callers must verify the result.
// If lhs is set and the result is an identifier, it is not resolved.
//
func (self *parser) parseOperand(lhs bool) ast.Expr {
	if self.trace {
		defer un(trace(self, "Operand"))

	}
again:

	switch self.tok {
	case token.SEMICOLON:

		if self.lit == "\n" {
			self.next()
			goto again

		}

	case token.IDENT:

		x := self.parseIdent()
		if !lhs {
			self.resolve(x)

		}
		return x



	case token.INT, token.FLOAT, token.IMAG, token.CHAR, token.STRING:

		x := &ast.BasicLit{ValuePos: self.pos, Kind: self.tok, Value: self.lit}
		self.next()
		return x



	case token.LPAREN:

		lparen := self.pos
		self.next()
		self.exprLev++
		x := self.parseRhsOrType() // types may be parenthesized: (some type)
		self.exprLev--
		rparen := self.expect(token.RPAREN)
		return &ast.ParenExpr{Lparen: lparen, X: x, Rparen: rparen}



	case token.FUNC:

		return self.parseFuncTypeOrLit()



	}
	if typ := self.tryIdentOrType(); typ != nil { // could be type for composite literal or conversion
		_, isIdent := typ.(*ast.Ident)
		assert(!isIdent, "type cannot be identifier")
		return typ

	} // we have an error
	pos := self.pos
	self.errorExpected(pos, "operand")
	syncStmt(self)
	return &ast.BadExpr{From: pos, To: self.pos}



}
func (self *parser) parseSelector(x ast.Expr) ast.Expr {
	if self.trace {
		defer un(trace(self, "Selector"))

	}
	sel := self.parseIdent()

	return &ast.SelectorExpr{X: x, Sel: sel}

}
func (self *parser) parseTypeAssertion(x ast.Expr) ast.Expr {
	if self.trace {
		defer un(trace(self, "TypeAssertion"))

	}
	self.expect(token.LPAREN)
	var typ ast.Expr
	if self.tok == token.TYPE { // type switch: typ == nil
		self.next()
	} else {
		typ = self.parseType()

	}
	self.expect(token.RPAREN)

	return &ast.TypeAssertExpr{X: x, Type: typ}

}
func (self *parser) parseIndexOrSlice(x ast.Expr) ast.Expr {
	if self.trace {
		defer un(trace(self, "IndexOrSlice"))

	}
	lbrack := self.expect(token.LBRACK)
	self.exprLev++
	var low, high ast.Expr
	isSlice := false
	if self.tok != token.COLON {
		low = self.parseRhs()

	}
	if self.tok == token.COLON {
		isSlice = true
		self.next()
		if self.tok != token.RBRACK {
			high = self.parseRhs()

		}
	}
	self.exprLev--
	rbrack := self.expect(token.RBRACK)

	if isSlice {
		return &ast.SliceExpr{X: x, Lbrack: lbrack, Low: low, High: high, Rbrack: rbrack}

	}
	return &ast.IndexExpr{X: x, Lbrack: lbrack, Index: low, Rbrack: rbrack}

}
func (self *parser) parseCallOrConversion(fun ast.Expr) *ast.CallExpr {
	if self.trace {
		defer un(trace(self, "CallOrConversion"))

	}
	lparen := self.expect(token.LPAREN)
	self.exprLev++
	var list []ast.Expr
	var ellipsis token.Pos
	for self.tok != token.RPAREN && self.tok != token.EOF && !ellipsis.IsValid() {
		list = append(list, self.parseRhsOrType()) // builtins may expect a type: make(some type, ...)
		if self.tok == token.ELLIPSIS {
			ellipsis = self.pos
			self.next()

		}
		if !self.atComma("argument list") {
			break

		}
		self.next()

	}
	self.exprLev--
	rparen := self.expectClosing(token.RPAREN, "argument list")

	if self.tok == token.DO {
		pos := self.expect(token.DO)

		scope := ast.NewScope(self.topScope) // function scope
		params, results := self.parseSignature(scope)

		typ := &ast.FuncType{Func: pos, Params: params, Results: results}

		self.exprLev++
		body := self.parseBody(scope)
		self.exprLev--

		list = append(list, &ast.FuncLit{Type: typ, Body: body})

	}
	return &ast.CallExpr{Fun: fun, Lparen: lparen, Args: list, Ellipsis: ellipsis, Rparen: rparen}

}
func (self *parser) parseElement(keyOk bool) ast.Expr {
	if self.trace {
		defer un(trace(self, "Element"))

	}
	if self.tok == token.LBRACE {
		return self.parseLiteralValue(nil)

	} // Because the parser doesn't know the composite literal type, it cannot
	// know if a key that's an identifier is a struct field name or a name
	// denoting a value. The former is not resolved by the parser or the
	// resolver.
	//
	// Instead, _try_ to resolve such a key if possible. If it resolves,
	// it a) has correctly resolved, or b) incorrectly resolved because
	// the key is a struct field with a name matching another identifier.
	// In the former case we are done, and in the latter case we don't
	// care because the type checker will do a separate field lookup.
	//
	// If the key does not resolve, it a) must be defined at the top
	// level in another file of the same package, the universe scope, or be
	// undeclared; or b) it is a struct field. In the former case, the type
	// checker can do a top-level lookup, and in the latter case it will do
	// a separate field lookup.
	x := self.checkExpr(self.parseExpr(keyOk))
	if keyOk {
		if self.tok == token.COLON {
			colon := self.pos
			self.next()
			// Try to resolve the key but don't collect it
			// as unresolved identifier if it fails so that
			// we don't get (possibly false) errors about
			// undeclared names.
			self.tryResolve(x, false)
			return &ast.KeyValueExpr{Key: x, Colon: colon, Value: self.parseElement(false)}

		}
		self.resolve(x) // not a key

	}
	return x

}
func (self *parser) parseElementList() (list []ast.Expr) {
	if self.trace {
		defer un(trace(self, "ElementList"))

	}
	for self.tok != token.RBRACE && self.tok != token.EOF {
		list = append(list, self.parseElement(true))
		if !self.atComma("composite literal") {
			break

		}
		self.next()

	}
	return

}
func (self *parser) parseLiteralValue(typ ast.Expr) ast.Expr {
	if self.trace {
		defer un(trace(self, "LiteralValue"))

	}
	lbrace := self.expect(token.LBRACE)
	var elts []ast.Expr
	self.exprLev++
	if self.tok != token.RBRACE {
		elts = self.parseElementList()

	}
	self.exprLev--
	rbrace := self.expectClosing(token.RBRACE, "composite literal")
	return &ast.CompositeLit{Type: typ, Lbrace: lbrace, Elts: elts, Rbrace: rbrace}
}

// checkExpr checks that x is an expression (and not a type).
func (self *parser) checkExpr(x ast.Expr) ast.Expr {
	switch unparen(x).(type) {
	case *ast.BadExpr:


	case *ast.Ident:


	case *ast.BasicLit:


	case *ast.FuncLit:


	case *ast.CompositeLit:


	case *ast.ParenExpr:

		panic("unreachable")

	case *ast.SelectorExpr:


	case *ast.IndexExpr:


	case *ast.SliceExpr:


	case *ast.TypeAssertExpr:
		// If t.Type == nil we have a type assertion of the form
		// y.(type), which is only allowed in type switch expressions.
		// It's hard to exclude those but for the case where we are in
		// a type switch. Instead be lenient and test this in the type
		// checker.
	case *ast.CallExpr:


	case *ast.StarExpr:


	case *ast.UnaryExpr:


	case *ast.BinaryExpr:


	default:
		// all other nodes are not proper expressions
		self.errorExpected(x.Pos(), "expression")
		x = &ast.BadExpr{From: x.Pos(), To: x.End()}



	}
	return x
}

// isTypeName returns true iff x is a (qualified) TypeName.
func isTypeName(x ast.Expr) bool {
	switch t := x.(type) {
	case *ast.BadExpr:


	case *ast.Ident:


	case *ast.SelectorExpr:

		_, isIdent := t.X.(*ast.Ident)
		return isIdent

	default:

		return false // all other nodes are not type names

	}
	return true
}

// isLiteralType returns true iff x is a legal composite literal type.
func isLiteralType(x ast.Expr) bool {
	switch t := x.(type) {
	case *ast.BadExpr:


	case *ast.Ident:


	case *ast.SelectorExpr:

		_, isIdent := t.X.(*ast.Ident)
		return isIdent

	case *ast.ArrayType:


	case *ast.StructType:


	case *ast.MapType:


	default:

		return false // all other nodes are not legal composite literal types

	}
	return true
}

// If x is of the form *T, deref returns T, otherwise it returns x.
func deref(x ast.Expr) ast.Expr {
	if p, isPtr := x.(*ast.StarExpr); isPtr {
		x = p.X

	}
	return x
}

// If x is of the form (T), unparen returns unparen(T), otherwise it returns x.
func unparen(x ast.Expr) ast.Expr {
	if p, isParen := x.(*ast.ParenExpr); isParen {
		x = unparen(p.X)

	}
	return x
}

// checkExprOrType checks that x is an expression or a type
// (and not a raw type such as [...]T).
//
func (self *parser) checkExprOrType(x ast.Expr) ast.Expr {
	switch t := unparen(x).(type) {
	case *ast.ParenExpr:

		panic("unreachable")

	case *ast.UnaryExpr:


	case *ast.ArrayType:

		if len, isEllipsis := t.Len.(*ast.Ellipsis); isEllipsis {
			self.error(len.Pos(), "expected array length, found '...'")
			x = &ast.BadExpr{From: x.Pos(), To: x.End()}

		} // all other nodes are expressions or types
	}
	return x
}

// If lhs is set and the result is an identifier, it is not resolved.
func (self *parser) parsePrimaryExpr(lhs bool) ast.Expr {
	if self.trace {
		defer un(trace(self, "PrimaryExpr"))

	}
	x := self.parseOperand(lhs)
L:

	for {
		switch self.tok {
		case token.PERIOD:

			self.next()
			if lhs {
				self.resolve(x)

			}
			switch self.tok {
			case token.IDENT:

				x = self.parseSelector(self.checkExpr(x))

			case token.LPAREN:

				x = self.parseTypeAssertion(self.checkExpr(x))

			default:

				pos := self.pos
				self.errorExpected(pos, "selector or type assertion")
				self.next() // make progress
				x = &ast.BadExpr{From: pos, To: self.pos}



			}

		case token.LBRACK:

			if lhs {
				self.resolve(x)

			}
			x = self.parseIndexOrSlice(self.checkExpr(x))

		case token.LPAREN:

			if lhs {
				self.resolve(x)

			}
			x = self.parseCallOrConversion(self.checkExprOrType(x))

		case token.LBRACE:

			if isLiteralType(x) && (self.exprLev >= 0 || !isTypeName(x)) {
				if lhs {
					self.resolve(x)

				}
				x = self.parseLiteralValue(x)
			} else {
				break L

			}

		default:

			break L



		}
		lhs = false // no need to try to resolve again

	}
	return x

	// If lhs is set and the result is an identifier, it is not resolved.
}
func (self *parser) parseUnaryExpr(lhs bool) ast.Expr {
	if self.trace {
		defer un(trace(self, "UnaryExpr"))

	}
	switch self.tok {
	case token.ADD, token.SUB, token.NOT, token.XOR, token.AND:

		pos, op := self.pos, self.tok
		self.next()
		x := self.parseUnaryExpr(false)
		return &ast.UnaryExpr{OpPos: pos, Op: op, X: self.checkExpr(x)}



	case token.ARROW:
		// channel type or receive expression
		arrow := self.pos
		self.next()

		// If the next token is token.CHAN we still don't know if it
		// is a channel type or a receive operation - we only know
		// once we have found the end of the unary expression. There
		// are two cases:
		//
		//   <- type  => (<-type) must be channel type
		//   <- expr  => <-(expr) is a receive from an expression
		//
		// In the first case, the arrow must be re-associated with
		// the channel type parsed already:
		//
		//   <- (chan type)    =>  (<-chan type)
		//   <- (chan<- type)  =>  (<-chan (<-type))

		x := self.parseUnaryExpr(false)

		// determine which case we have
		if typ, ok := x.(*ast.ChanType); ok { // (<-type)

			// re-associate position info and <-
			dir := ast.SEND
			for ok && dir == ast.SEND {
				if typ.Dir == ast.RECV { // error: (<-type) is (<-(<-chan T))
					self.errorExpected(typ.Arrow, "'chan'")

				}
				arrow, typ.Begin, typ.Arrow = typ.Arrow, arrow, arrow
				dir, typ.Dir = typ.Dir, ast.RECV
				typ, ok = typ.Value.(*ast.ChanType)

			}
			if dir == ast.SEND {
				self.errorExpected(arrow, "channel type")

			}
			return x

		} // <-(expr)
		return &ast.UnaryExpr{OpPos: arrow, Op: token.ARROW, X: self.checkExpr(x)}



	case token.MUL:
		// pointer type or unary "*" expression
		pos := self.pos
		self.next()
		x := self.parseUnaryExpr(false)
		return &ast.StarExpr{Star: pos, X: self.checkExprOrType(x)}



	}
	return self.parsePrimaryExpr(lhs)

}
func (self *parser) tokPrec() (token.Token, int) {
	tok := self.tok
	if self.inRhs && tok == token.ASSIGN {
		tok = token.EQL

	}
	return tok, tok.Precedence()
}

// If lhs is set and the result is an identifier, it is not resolved.
func (self *parser) parseBinaryExpr(lhs bool, prec1 int) ast.Expr {
	if self.trace {
		defer un(trace(self, "BinaryExpr"))

	}
	x := self.parseUnaryExpr(lhs)
	for _, prec := self.tokPrec(); prec >= prec1; prec-- {
		for {
			op, oprec := self.tokPrec()
			if oprec != prec {
				break

			}
			pos := self.expect(op)
			if lhs {
				self.resolve(x)
				lhs = false

			}
			y := self.parseBinaryExpr(false, prec+1)
			x = &ast.BinaryExpr{X: self.checkExpr(x), OpPos: pos, Op: op, Y: self.checkExpr(y)}

		}
	}
	return x
}

// If lhs is set and the result is an identifier, it is not resolved.
// The result may be a type or even a raw type ([...]int). Callers must
// check the result (using checkExpr or checkExprOrType), depending on
// context.
func (self *parser) parseExpr(lhs bool) ast.Expr {
	if self.trace {
		defer un(trace(self, "Expression"))

	}
	return self.parseBinaryExpr(lhs, token.LowestPrec+1)

}
func (self *parser) parseRhs() ast.Expr {
	old := self.inRhs
	self.inRhs = true
	x := self.checkExpr(self.parseExpr(false))
	self.inRhs = old
	return x

}
func (self *parser) parseRhsOrType() ast.Expr {
	old := self.inRhs
	self.inRhs = true
	x := self.checkExprOrType(self.parseExpr(false))
	self.inRhs = old
	return x
}

// ----------------------------------------------------------------------------
// Statements

// Parsing modes for parseSimpleStmt.
const (
	basic = iota
	labelOk
	rangeOk
)

// parseSimpleStmt returns true as 2nd result if it parsed the assignment
// of a range clause (with mode == rangeOk). The returned statement is an
// assignment with a right-hand side that is a single unary expression of
// the form "range x". No guarantees are given for the left-hand side.
func (self *parser) parseSimpleStmt(mode int) (ast.Stmt, bool) {
	if self.trace {
		defer un(trace(self, "SimpleStmt"))

	}
	x := self.parseLhsList()

	switch self.tok {
	case
		token.DEFINE, token.ASSIGN, token.ADD_ASSIGN,
		token.SUB_ASSIGN, token.MUL_ASSIGN, token.QUO_ASSIGN,
		token.REM_ASSIGN, token.AND_ASSIGN, token.OR_ASSIGN,
		token.XOR_ASSIGN, token.SHL_ASSIGN, token.SHR_ASSIGN, token.AND_NOT_ASSIGN:
		// assignment statement, possibly part of a range clause
		pos, tok := self.pos, self.tok
		self.next()
		var y []ast.Expr
		isRange := false
		if mode == rangeOk && self.tok == token.RANGE && (tok == token.DEFINE || tok == token.ASSIGN) {
			pos := self.pos
			self.next()
			y = []ast.Expr{&ast.UnaryExpr{OpPos: pos, Op: token.RANGE, X: self.parseRhs()}}
			isRange = true
		} else {
			y = self.parseRhsList()

		}
		as := &ast.AssignStmt{Lhs: x, TokPos: pos, Tok: tok, Rhs: y}
		if tok == token.DEFINE {
			self.shortVarDecl(as, x)

		}
		return as, isRange



	}
	if len(x) > 1 {
		self.errorExpected(x[0].Pos(), "1 expression")
		// continue with first expression

	}
	switch self.tok {
	case token.COLON:
		// labeled statement
		if label, isIdent := x[0].(*ast.Ident); mode == labelOk && isIdent { // Go spec: The scope of a label is the body of the function
			// in which it is declared and excludes the body of any nested
			// function.
			colon := self.pos
			self.next()
			stmt := &ast.LabeledStmt{Label: label, Colon: colon, Stmt: self.parseStmt()}
			self.declare(stmt, nil, self.labelScope, ast.Lbl, label)
			return stmt, false

			// The label declaration typically starts at x[0].Pos(), but the label
			// declaration may be erroneous due to a token after that position (and
			// before the ':'). If SpuriousErrors is not set, the (only) error re-
			// ported for the line is the illegal label error instead of the token
			// before the ':' that caused the problem. Thus, use the (latest) colon
			// position for error reporting.
			// p.error(colon, "illegal label declaration")
			// return &ast.BadStmt{From: x[0].Pos(), To: colon + 1}, false

		}

	case token.ARROW:
		// send statement
		arrow := self.pos
		self.next()
		y := self.parseRhs()
		return &ast.SendStmt{Chan: x[0], Arrow: arrow, Value: y}, false



	case token.INC, token.DEC:
		// increment or decrement
		s := &ast.IncDecStmt{X: x[0], TokPos: self.pos, Tok: self.tok}
		self.next()
		return s, false

		// expression
	}
	return &ast.ExprStmt{X: x[0]}, false

}
func (self *parser) parseCallExpr() *ast.CallExpr {
	x := self.parseRhsOrType() // could be a conversion: (some type)(x)
	if call, isCall := x.(*ast.CallExpr); isCall {
		return call

	}
	if _, isBad := x.(*ast.BadExpr); !isBad { // only report error if it's a new one
		self.errorExpected(x.Pos(), "function/method call")

	}
	return nil

}
func (self *parser) parseGoStmt() ast.Stmt {
	if self.trace {
		defer un(trace(self, "GoStmt"))

	}
	pos := self.expect(token.GO)
	call := self.parseCallExpr()
	self.expectSemi()
	if call == nil {
		return &ast.BadStmt{From: pos, To: pos + 2} // len("go")

	}
	return &ast.GoStmt{Go: pos, Call: call}

}
func (self *parser) parseDeferStmt() ast.Stmt {
	if self.trace {
		defer un(trace(self, "DeferStmt"))

	}
	pos := self.expect(token.DEFER)
	call := self.parseCallExpr()
	self.expectSemi()
	if call == nil {
		return &ast.BadStmt{From: pos, To: pos + 5} // len("defer")

	}
	return &ast.DeferStmt{Defer: pos, Call: call}

}
func (self *parser) parseReturnStmt() *ast.ReturnStmt {
	if self.trace {
		defer un(trace(self, "ReturnStmt"))

	}
	pos := self.pos
	self.expect(token.RETURN)
	var x []ast.Expr
	if self.tok != token.SEMICOLON && self.tok != token.DEDENT {
		x = self.parseRhsList()

	}
	self.expectSemi()

	return &ast.ReturnStmt{Return: pos, Results: x}

}
func (self *parser) parseBranchStmt(tok token.Token) *ast.BranchStmt {
	if self.trace {
		defer un(trace(self, "BranchStmt"))

	}
	pos := self.expect(tok)
	var label *ast.Ident
	if tok != token.FALLTHROUGH && self.tok == token.IDENT {
		label = self.parseIdent()
		// add to list of unresolved targets
		n := len(self.targetStack) - 1
		self.targetStack[n] = append(self.targetStack[n], label)

	}
	self.expectSemi()

	return &ast.BranchStmt{TokPos: pos, Tok: tok, Label: label}

}
func (self *parser) makeExpr(s ast.Stmt) ast.Expr {
	if s == nil {
		return nil

	}
	if es, isExpr := s.(*ast.ExprStmt); isExpr {
		return self.checkExpr(es.X)

	}
	self.error(s.Pos(), "expected condition, found simple statement")
	return &ast.BadExpr{From: s.Pos(), To: s.End()}

}
func (self *parser) parseIfStmt() *ast.IfStmt {
	if self.trace {
		defer un(trace(self, "IfStmt"))

	}
	pos := self.expect(token.IF)
	self.openScope()
	defer self.closeScope()

	var s ast.Stmt
	var x ast.Expr

	{
		prevLev := self.exprLev
		self.exprLev = -1
		if self.tok == token.SEMICOLON && !self.isIndent() {
			x = self.parseRhs()
		} else {
			s, _ = self.parseSimpleStmt(basic)
			if self.tok == token.SEMICOLON && !self.isIndent() {
				self.next()
				x = self.parseRhs()
			} else {
				x = self.makeExpr(s)
				s = nil

			}
		}
		self.exprLev = prevLev

	}
	body := self.parseBlockStmt()

	var else_ ast.Stmt
	if self.tok == token.ELSE {
		self.next()
		else_ = self.parseStmt()

	}
	return &ast.IfStmt{If: pos, Init: s, Cond: x, Body: body, Else: else_}

}
func (self *parser) parseTypeList() (list []ast.Expr) {
	if self.trace {
		defer un(trace(self, "TypeList"))

	}
	list = append(list, self.parseType())
	for self.tok == token.COMMA {
		self.next()
		list = append(list, self.parseType())

	}
	return

}
func (self *parser) parseCaseClause(typeSwitch bool) *ast.CaseClause {
	if self.trace {
		defer un(trace(self, "CaseClause"))

	}
	pos := self.pos
	var list []ast.Expr
	if self.tok == token.CASE {
		self.next()
		if typeSwitch {
			list = self.parseTypeList()
		} else {
			list = self.parseRhsList()

		}
	} else {
		self.expect(token.DEFAULT)

	}
	colon := self.expect(token.COLON)
	self.openScope()
	self.allowEmptyBlock = true
	body := self.parseStmtList()
	self.allowEmptyBlock = false
	self.closeScope()

	return &ast.CaseClause{Case: pos, List: list, Colon: colon, Body: body}

}
func isTypeSwitchAssert(x ast.Expr) bool {
	a, ok := x.(*ast.TypeAssertExpr)
	return ok && a.Type == nil

}
func isTypeSwitchGuard(s ast.Stmt) bool {
	switch t := s.(type) {
	case *ast.ExprStmt:
		// x.(nil)
		return isTypeSwitchAssert(t.X)

	case *ast.AssignStmt:
		// v := x.(nil)
		return len(t.Lhs) == 1 && t.Tok == token.DEFINE && len(t.Rhs) == 1 && isTypeSwitchAssert(t.Rhs[0])



	}
	return false

}
func (self *parser) parseSwitchStmt() ast.Stmt {
	if self.trace {
		defer un(trace(self, "SwitchStmt"))

	}
	pos := self.expect(token.SWITCH)
	self.openScope()
	defer self.closeScope()

	var s1, s2 ast.Stmt

	// if is not yet a block, process declaration in ';'
	if !self.isIndent() {
		prevLev := self.exprLev
		self.exprLev = -1
		if self.tok != token.SEMICOLON {
			s2, _ = self.parseSimpleStmt(basic)

		} // here we need p.lit != "\n" because the prvious statement
		// may have advanced the scanner, so we could be at eol.
		if self.tok == token.SEMICOLON && !self.isIndent() {
			self.next()
			s1 = s2
			s2 = nil
			// if we are not introducing a block (indent/dedent)
			if self.tok != token.SEMICOLON && !self.isIndent() { // A TypeSwitchGuard may declare a variable in addition
				// to the variable declared in the initial SimpleStmt.
				// Introduce extra scope to avoid redeclaration errors:
				//
				//	switch t := 0; t := x.(T) { ... }
				//
				// (this code is not valid Go because the first t
				// cannot be accessed and thus is never used, the extra
				// scope is needed for the correct error message).
				//
				// If we don't have a type switch, s2 must be an expression.
				// Having the extra nested but empty scope won't affect it.
				self.openScope()
				defer self.closeScope()
				s2, _ = self.parseSimpleStmt(basic)

			}
		}
		self.exprLev = prevLev

	}
	typeSwitch := isTypeSwitchGuard(s2)
	self.expectSemi()
	indent := self.expect(token.INDENT)
	var list []ast.Stmt
	for self.tok == token.CASE || self.tok == token.DEFAULT {
		list = append(list, self.parseCaseClause(typeSwitch))

	}
	dedent := self.expect(token.DEDENT)
	// p.expectSemi()
	body := &ast.BlockStmt{Opening: indent, List: list, Closing: dedent}

	if typeSwitch {
		return &ast.TypeSwitchStmt{Switch: pos, Init: s1, Assign: s2, Body: body}

	}
	return &ast.SwitchStmt{Switch: pos, Init: s1, Tag: self.makeExpr(s2), Body: body}

}
func (self *parser) parseCommClause() *ast.CommClause {
	if self.trace {
		defer un(trace(self, "CommClause"))

	}
	self.openScope()
	pos := self.pos
	var comm ast.Stmt
	if self.tok == token.CASE {
		self.next()
		lhs := self.parseLhsList()
		if self.tok == token.ARROW { // SendStmt
			if len(lhs) > 1 {
				self.errorExpected(lhs[0].Pos(), "1 expression")
				// continue with first expression

			}
			arrow := self.pos
			self.next()
			rhs := self.parseRhs()
			comm = &ast.SendStmt{Chan: lhs[0], Arrow: arrow, Value: rhs}
		} else { // RecvStmt
			if tok := self.tok; tok == token.ASSIGN || tok == token.DEFINE { // RecvStmt with assignment
				if len(lhs) > 2 {
					self.errorExpected(lhs[0].Pos(), "1 or 2 expressions")
					// continue with first two expressions
					lhs = lhs[0:2]

				}
				pos := self.pos
				self.next()
				rhs := self.parseRhs()
				as := &ast.AssignStmt{Lhs: lhs, TokPos: pos, Tok: tok, Rhs: []ast.Expr{rhs}}
				if tok == token.DEFINE {
					self.shortVarDecl(as, lhs)

				}
				comm = as
			} else { // lhs must be single receive operation
				if len(lhs) > 1 {
					self.errorExpected(lhs[0].Pos(), "1 expression")
					// continue with first expression

				}
				comm = &ast.ExprStmt{X: lhs[0]}

			}
		}
	} else {
		self.expect(token.DEFAULT)

	}
	colon := self.expect(token.COLON)
	self.allowEmptyBlock = true
	body := self.parseStmtList()
	self.allowEmptyBlock = false
	self.closeScope()

	return &ast.CommClause{Case: pos, Comm: comm, Colon: colon, Body: body}

}
func (self *parser) parseSelectStmt() *ast.SelectStmt {
	if self.trace {
		defer un(trace(self, "SelectStmt"))

	}
	pos := self.expect(token.SELECT)
	self.expectSemi()
	indent := self.expect(token.INDENT)
	var list []ast.Stmt
	for self.tok == token.CASE || self.tok == token.DEFAULT {
		list = append(list, self.parseCommClause())

	}
	dedent := self.expect(token.DEDENT)
	self.expectSemi()
	body := &ast.BlockStmt{Opening: indent, List: list, Closing: dedent}

	return &ast.SelectStmt{Select: pos, Body: body}

}
func (self *parser) parseForStmt() ast.Stmt {
	if self.trace {
		defer un(trace(self, "ForStmt"))

	}
	pos := self.expect(token.FOR)
	self.openScope()
	defer self.closeScope()

	var s1, s2, s3 ast.Stmt
	var isRange bool
	if !self.isIndent() && self.tok != token.COLON {
		prevLev := self.exprLev
		self.exprLev = -1
		if self.tok != token.SEMICOLON {
			s2, isRange = self.parseSimpleStmt(rangeOk)

		}
		if !isRange && self.tok == token.SEMICOLON && !self.isIndent() {
			self.next()
			s1 = s2
			s2 = nil
			if self.tok != token.SEMICOLON && !self.isIndent() {
				s2, _ = self.parseSimpleStmt(basic)

			}
			self.expectSemi()
			if !self.isIndent() {
				s3, _ = self.parseSimpleStmt(basic)

			}
		}
		self.exprLev = prevLev

	}
	body := self.parseBlockStmt()
	// p.expectSemi()

	if isRange {
		as := s2.(*ast.AssignStmt)
		// check lhs
		var key, value ast.Expr
		switch len(as.Lhs) {
		case 2:

			key, value = as.Lhs[0], as.Lhs[1]

		case 1:

			key = as.Lhs[0]

		default:

			self.errorExpected(as.Lhs[0].Pos(), "1 or 2 expressions")
			return &ast.BadStmt{From: pos, To: body.End()}

			// parseSimpleStmt returned a right-hand side that
			// is a single unary expression of the form "range x"
		}
		x := as.Rhs[0].(*ast.UnaryExpr).X
		return &ast.RangeStmt{
			For:    pos,
			Key:    key,
			Value:  value,
			TokPos: as.TokPos,
			Tok:    as.Tok,
			X:      x,
			Body:   body,
		}

	} // regular for statement
	return &ast.ForStmt{
		For:  pos,
		Init: s1,
		Cond: self.makeExpr(s2),
		Post: s3,
		Body: body,
	}

}
func (self *parser) parseSmallStmt() (s ast.Stmt) {
	if self.trace {
		defer un(trace(self, "SmallStatement"))

	}
	switch self.tok {
	case token.CONST, token.TYPE, token.VAR:

		s = &ast.DeclStmt{Decl: self.parseDecl(syncStmt)}

	case
		// tokens that may start an expression
		token.IDENT, token.INT, token.FLOAT, token.IMAG, token.CHAR, token.STRING, token.FUNC, token.LPAREN, // operands
		token.LBRACK, token.STRUCT, // composite types
		token.ADD, token.SUB, token.MUL, token.AND, token.XOR, token.ARROW, token.NOT: // unary operators
		s, _ = self.parseSimpleStmt(basic)

	case token.RETURN:

		s = self.parseReturnStmt()

	case token.BREAK, token.CONTINUE, token.GOTO, token.FALLTHROUGH:

		s = self.parseBranchStmt(self.tok)

	case token.SEMICOLON:
		// Allow empty statements
		s = &ast.EmptyStmt{Semicolon: self.pos}

	default:
		// no statement found
		pos := self.pos
		self.errorExpected(pos, "small statement")
		syncStmt(self)
		s = &ast.BadStmt{From: pos, To: self.pos}



	}
	return

}
func (self *parser) parseStmt() (s ast.Stmt) {
	if self.trace {
		defer un(trace(self, "Statement"))

	}
	switch self.tok {
	case token.CONST, token.TYPE, token.VAR:

		s = &ast.DeclStmt{Decl: self.parseDecl(syncStmt)}

	case
		// tokens that may start an expression
		token.IDENT, token.INT, token.FLOAT, token.IMAG, token.CHAR, token.STRING, token.FUNC, token.LPAREN, // operands
		token.LBRACK, token.STRUCT, // composite types
		token.ADD, token.SUB, token.MUL, token.AND, token.XOR, token.ARROW, token.NOT: // unary operators
		s, _ = self.parseSimpleStmt(labelOk)
		// because of the required look-ahead, labeled statements are
		// parsed by parseSimpleStmt - don't expect a semicolon after
		// them
		if _, isLabeledStmt := s.(*ast.LabeledStmt); !isLabeledStmt {
			self.expectSemi()

		}

	case token.GO:

		s = self.parseGoStmt()

	case token.DEFER:

		s = self.parseDeferStmt()

	case token.RETURN:

		s = self.parseReturnStmt()

	case token.BREAK, token.CONTINUE, token.GOTO, token.FALLTHROUGH:

		s = self.parseBranchStmt(self.tok)

	case token.IF:

		s = self.parseIfStmt()

	case token.SWITCH:

		s = self.parseSwitchStmt()

	case token.SELECT:

		s = self.parseSelectStmt()

	case token.FOR:

		s = self.parseForStmt()

	case token.DO:

		self.next()
		s = self.parseBlockStmt()

	case token.SEMICOLON:

		if self.lit == "\n" {
			s = self.parseBlockStmt()
		} else {
			s = &ast.EmptyStmt{Semicolon: self.pos}
			self.next()

		}

	case token.COLON:

		s = self.parseBlockStmt()

	case token.DEDENT:
		// a semicolon may be omitted before a closing "DEDENT"
		s = &ast.EmptyStmt{Semicolon: self.pos}

	default:
		// no statement found
		pos := self.pos
		self.errorExpected(pos, "statement")
		syncStmt(self)
		s = &ast.BadStmt{From: pos, To: self.pos}



	}
	return
}

// ----------------------------------------------------------------------------
// Declarations

type parseSpecFunction func(doc *ast.CommentGroup, keyword token.Token, iota int) ast.Spec

func isValidImport(lit string) bool {
	const illegalChars = `!"#$%&'()*,:;<=>?[\]^{|}` + "`\uFFFD"
	s, _ := strconv.Unquote(lit) // go/scanner returns a legal string literal
	for _, r := range s {
		if !unicode.IsGraphic(r) || unicode.IsSpace(r) || strings.ContainsRune(illegalChars, r) {
			return false

		}
	}
	return s != ""

}
func (self *parser) parseImportSpec(doc *ast.CommentGroup, _ token.Token, _ int) ast.Spec {
	if self.trace {
		defer un(trace(self, "ImportSpec"))

	}
	var ident *ast.Ident
	switch self.tok {
	case token.PERIOD:

		ident = &ast.Ident{NamePos: self.pos, Name: "."}
		self.next()

	case token.IDENT:

		ident = self.parseIdent()



	}
	var path *ast.BasicLit
	if self.tok == token.STRING {
		if !isValidImport(self.lit) {
			self.error(self.pos, "invalid import path: "+self.lit)

		}
		path = &ast.BasicLit{ValuePos: self.pos, Kind: self.tok, Value: self.lit}
		self.next()
	} else {
		self.expect(token.STRING) // use expect() error handling

	}
	self.expectSemi() // call before accessing p.linecomment

	// collect imports
	spec := &ast.ImportSpec{
		Doc:     doc,
		Name:    ident,
		Path:    path,
		Comment: self.lineComment,
	}
	self.imports = append(self.imports, spec)

	return spec

}
func (self *parser) parseValueSpec(doc *ast.CommentGroup, keyword token.Token, iota int) ast.Spec {
	if self.trace {
		defer un(trace(self, keyword.String()+"Spec"))

	}
	idents := self.parseIdentList()
	typ := self.tryType()
	var values []ast.Expr
	if self.tok == token.ASSIGN || keyword == token.CONST && (typ != nil || iota == 0) || keyword == token.VAR && typ == nil {
		self.expect(token.ASSIGN)
		values = self.parseRhsList()

	}
	if self.tok == token.SEMICOLON {
		self.expectSemi() // call before accessing p.linecomment

	} // Go spec: The scope of a constant or variable identifier declared inside
	// a function begins at the end of the ConstSpec or VarSpec and ends at
	// the end of the innermost containing block.
	// (Global identifiers are resolved in a separate phase after parsing.)
	spec := &ast.ValueSpec{
		Doc:     doc,
		Names:   idents,
		Type:    typ,
		Values:  values,
		Comment: self.lineComment,
	}
	kind := ast.Con
	if keyword == token.VAR {
		kind = ast.Var

	}
	self.declare(spec, iota, self.topScope, kind, idents...)

	return spec

}
func (self *parser) parseTypeSpec(doc *ast.CommentGroup, _ token.Token, _ int) ast.Spec {
	if self.trace {
		defer un(trace(self, "TypeSpec"))

	}
	ident := self.parseIdent()

	// Go spec: The scope of a type identifier declared inside a function begins
	// at the identifier in the TypeSpec and ends at the end of the innermost
	// containing block.
	// (Global identifiers are resolved in a separate phase after parsing.)
	spec := &ast.TypeSpec{Doc: doc, Name: ident}
	self.declare(spec, nil, self.topScope, ast.Typ, ident)

	spec.Type = self.parseType()
	self.expectSemi() // call before accessing p.linecomment
	spec.Comment = self.lineComment

	return spec

}
func (self *parser) parseGenDecl(keyword token.Token, f parseSpecFunction) *ast.GenDecl {
	if self.trace {
		defer un(trace(self, "GenDecl("+keyword.String()+")"))

	}
	doc := self.leadComment
	pos := self.expect(keyword)
	var indent, dedent token.Pos
	var list []ast.Spec
	if self.tok == token.SEMICOLON {
		self.expectSemi()
		indent = self.expect(token.INDENT)
		for iota := 0; self.tok != token.DEDENT && self.tok != token.EOF; iota++ {
			list = append(list, f(self.leadComment, keyword, iota))

		}
		dedent = self.expect(token.DEDENT)
	} else {
		list = append(list, f(nil, keyword, 0))

	}
	return &ast.GenDecl{
		Doc:    doc,
		TokPos: pos,
		Tok:    keyword,
		Indent: indent,
		Specs:  list,
		Dedent: dedent,
	}

}
func (self *parser) parseReceiver(typ ast.Expr, scope *ast.Scope) *ast.Field {
	if self.trace {
		defer un(trace(self, "Receiver"))

	}
	ident := &ast.Ident{Name: "self"}
	field := &ast.Field{Names: []*ast.Ident{ident}, Type: typ}

	self.declare(field, nil, scope, ast.Var, ident)
	if t, ok := typ.(*ast.Ident); ok {
		self.resolve(t)

	}
	return field

}
func (self *parser) parseFuncDecl() *ast.FuncDecl {
	if self.trace {
		defer un(trace(self, "FunctionDecl"))

	}
	doc := self.leadComment
	pos := self.expect(token.FUNC)
	scope := ast.NewScope(self.topScope) // function scope

	var recv *ast.Field
	var ident *ast.Ident
	var recvList *ast.FieldList

	lparen := self.pos

	// *T.ident
	if self.tok == token.MUL {
		star := self.expect(token.MUL)
		typ := self.parseIdent()
		expr := &ast.StarExpr{Star: star, X: typ}
		recv = self.parseReceiver(expr, scope)
		self.expect(token.PERIOD)
		ident = self.parseIdent()
	} else {
		ident = self.parseIdent()
		// T.ident
		if self.tok == token.PERIOD {
			recv = self.parseReceiver(ident, scope) // ident is T here
			self.next()
			ident = self.parseIdent()

		}
	}
	if recv != nil {
		recvList = &ast.FieldList{
			Opening: lparen,
			Closing: self.pos,
			List:    []*ast.Field{recv},
		}

	}
	params, results := self.parseSignature(scope)

	body := self.parseBody(scope)

	decl := &ast.FuncDecl{
		Doc:  doc,
		Recv: recvList,
		Name: ident,
		Type: &ast.FuncType{
			Func:    pos,
			Params:  params,
			Results: results,
		},
		Body: body,
	}
	if recv == nil { // Go spec: The scope of an identifier denoting a constant, type,
		// variable, or function (but not method) declared at top level
		// (outside any function) is the package block.
		//
		// init() functions cannot be referred to and there may
		// be more than one - don't put them in the pkgScope
		if ident.Name != "init" {
			self.declare(decl, nil, self.pkgScope, ast.Fun, ident)

		}
	}
	return decl

}
func (self *parser) parseDecl(sync func(*parser)) ast.Decl {
	if self.trace {
		defer un(trace(self, "Declaration"))

	}
	var f parseSpecFunction
	switch self.tok {
	case token.CONST, token.VAR:

		f = self.parseValueSpec



	case token.TYPE:

		f = self.parseTypeSpec



	case token.FUNC:

		return self.parseFuncDecl()



	default:

		pos := self.pos
		self.errorExpected(pos, "declaration")
		sync(self)
		return &ast.BadDecl{From: pos, To: self.pos}



	}
	return self.parseGenDecl(self.tok, f)
}

// ----------------------------------------------------------------------------
// Source files

func (self *parser) parseFile() *ast.File {
	if self.trace {
		defer un(trace(self, "File"))

	} // Don't bother parsing the rest if we had errors scanning the first token.
	// Likely not a Go source file at all.
	if self.errors.Len() != 0 {
		return nil

	} // package clause
	doc := self.leadComment
	pos := self.expect(token.PACKAGE)
	// Go spec: The package clause is not a declaration;
	// the package name does not appear in any scope.
	ident := self.parseIdent()
	if ident.Name == "_" {
		self.error(self.pos, "invalid package name _")

	}
	self.expectSemi()

	// Don't bother parsing the rest if we had errors parsing the package clause.
	// Likely not a Go source file at all.
	if self.errors.Len() != 0 {
		return nil

	}
	self.openScope()
	self.pkgScope = self.topScope
	var decls []ast.Decl
	if self.mode&PackageClauseOnly == 0 { // import decls
		for self.tok == token.IMPORT {
			decls = append(decls, self.parseGenDecl(token.IMPORT, self.parseImportSpec))

		}
		if self.mode&ImportsOnly == 0 { // rest of package body
			for self.tok != token.EOF {
				decls = append(decls, self.parseDecl(syncDecl))

			}
		}
	}
	self.closeScope()
	assert(self.topScope == nil, "unbalanced scopes")
	assert(self.labelScope == nil, "unbalanced label scopes")

	// resolve global identifiers within the same file
	i := 0
	for _, ident := range self.unresolved { // i <= index for current ident
		assert(ident.Obj == unresolved, "object already resolved")
		ident.Obj = self.pkgScope.Lookup(ident.Name) // also removes unresolved sentinel
		if ident.Obj == nil {
			self.unresolved[i] = ident
			i++

		}
	}
	return &ast.File{
		Doc:        doc,
		Package:    pos,
		Name:       ident,
		Decls:      decls,
		Scope:      self.pkgScope,
		Imports:    self.imports,
		Unresolved: self.unresolved[0:i],
		Comments:   self.comments,
	}
}
