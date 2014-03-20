// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// This file implements scopes and the objects they contain.

package ast

import (
	"bytes"
	"fmt"
	"github.com/DAddYE/igo/token"
)

// A Scope maintains the set of named language entities declared
// in the scope and a link to the immediately surrounding (outer)
// scope.
//
type Scope struct {
	Outer   *Scope
	Objects map[string]*Object
}

// NewScope creates a new scope nested in the outer scope.
func NewScope(outer *Scope) *Scope {
	const n = 4 // initial scope capacity
	return &Scope{outer, make(map[string]*Object, n)}
}

// Lookup returns the object with the given name if it is
// found in scope s, otherwise it returns nil. Outer scopes
// are ignored.
//
func (self *Scope) Lookup(name string) *Object {
	return self.Objects[name]
}

// Insert attempts to insert a named object obj into the scope s.
// If the scope already contains an object alt with the same name,
// Insert leaves the scope unchanged and returns alt. Otherwise
// it inserts obj and returns nil."
//
func (self *Scope) Insert(obj *Object) (alt *Object) {
	if alt = self.Objects[obj.Name]; alt == nil {
		self.Objects[obj.Name] = obj

	}
	return
}

// Debugging support
func (self *Scope) String() string {
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "scope %p {", self)
	if self != nil && len(self.Objects) > 0 {
		fmt.Fprintln(&buf)
		for _, obj := range self.Objects {
			fmt.Fprintf(&buf, "\t%s %s\n", obj.Kind, obj.Name)

		}
	}
	fmt.Fprintf(&buf, "}\n")
	return buf.String()
}

// ----------------------------------------------------------------------------
// Objects

// An Object describes a named language entity such as a package,
// constant, type, variable, function (incl. methods), or label.
//
// The Data fields contains object-specific data:
//
//	Kind    Data type         Data value
//	Pkg	*types.Package    package scope
//	Con     int               iota for the respective declaration
//	Con     != nil            constant value
//	Typ     *Scope            (used as method scope during type checking - transient)
//
type Object struct {
	Kind ObjKind
	Name string      // declared name
	Decl interface{} // corresponding Field, XxxSpec, FuncDecl, LabeledStmt, AssignStmt, Scope; or nil
	Data interface{} // object-specific data; or nil
	Type interface{} // place holder for type information; may be nil
}

// NewObj creates a new object of a given kind and name.
func NewObj(kind ObjKind, name string) *Object {
	return &Object{Kind: kind, Name: name}
}

// Pos computes the source position of the declaration of an object name.
// The result may be an invalid position if it cannot be computed
// (obj.Decl may be nil or not correct).
func (self *Object) Pos() token.Pos {
	name := self.Name
	switch d := self.Decl.(type) {
	case *Field:

		for _, n := range d.Names {
			if n.Name == name {
				return n.Pos()

			}
		}

	case *ImportSpec:

		if d.Name != nil && d.Name.Name == name {
			return d.Name.Pos()

		}
		return d.Path.Pos()

	case *ValueSpec:

		for _, n := range d.Names {
			if n.Name == name {
				return n.Pos()

			}
		}

	case *TypeSpec:

		if d.Name.Name == name {
			return d.Name.Pos()

		}

	case *FuncDecl:

		if d.Name.Name == name {
			return d.Name.Pos()

		}

	case *LabeledStmt:

		if d.Label.Name == name {
			return d.Label.Pos()

		}

	case *AssignStmt:

		for _, x := range d.Lhs {
			if ident, isIdent := x.(*Ident); isIdent && ident.Name == name {
				return ident.Pos()

			}
		}

	case *Scope:
		// predeclared object - nothing to do for now

	}
	return token.NoPos
}

// ObjKind describes what an object represents.
type ObjKind int

// The list of possible Object kinds.
const (
	Bad ObjKind = iota // for error handling
	Pkg // package
	Con // constant
	Typ // type
	Var // variable
	Fun // function or method
	Lbl // label
)

var objKindStrings = [...]string{
	Bad: "bad",
	Pkg: "package",
	Con: "const",
	Typ: "type",
	Var: "var",
	Fun: "func",
	Lbl: "label",
}

func (self ObjKind) String() string {
	return objKindStrings[self]
}
