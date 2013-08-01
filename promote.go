package ssa2

// This file defines utilities for method-set computation including
// synthesis of wrapper methods.
//
// Wrappers include:
// - indirection/promotion wrappers for methods of embedded fields.
// - interface method wrappers for closures of I.f.
// - bound method wrappers, for uncalled obj.Method closures.

// TODO(adonovan): rename to wrappers.go.

import (
	"fmt"
	"go/token"

	"code.google.com/p/go.tools/go/types"
)

// recvType returns the receiver type of method obj.
func recvType(obj *types.Func) types.Type {
	return obj.Type().(*types.Signature).Recv().Type()
}

// MethodSet returns the method set for type typ, building wrapper
// methods as needed for embedded field promotion, and indirection for
// *T receiver types, etc.
// A nil result indicates an empty set.
//
// This function should only be called when you need to construct the
// entire method set, synthesizing all wrappers, for example during
// the processing of a MakeInterface instruction or when visiting all
// reachable functions.
//
// If you only need to look up a single method (obj), avoid this
// function and use LookupMethod instead:
//
//      meth := types.MethodSet(typ).Lookup(pkg, name)
// 	m := prog.MethodSet(typ)[meth.Id()]   // don't do this
//	m := prog.LookupMethod(meth)          // use this instead
//
// If you only need to enumerate the keys, use types.MethodSet
// instead.
//
// EXCLUSIVE_LOCKS_ACQUIRED(prog.methodsMu)
//
// Thread-safe.
//
func (prog *Program) MethodSet(typ types.Type) MethodSet {
	return prog.populateMethodSet(typ, nil)
}

// populateMethodSet returns the method set for typ, ensuring that it
// contains at least the function for meth, if that is a key.
// If meth is nil, the entire method set is populated.
//
// EXCLUSIVE_LOCKS_ACQUIRED(prog.methodsMu)
//
func (prog *Program) populateMethodSet(typ types.Type, meth *types.Selection) MethodSet {
	tmset := methodSet(typ)
	n := tmset.Len()
	if n == 0 {
		return nil
	}

	if prog.mode&LogSource != 0 {
		defer logStack("populateMethodSet %s meth=%v", typ, meth)()
	}

	prog.methodsMu.Lock()
	defer prog.methodsMu.Unlock()

	mset, _ := prog.methodSets.At(typ).(MethodSet)
	if mset == nil {
		mset = make(MethodSet)
		prog.methodSets.Set(typ, mset)
	}

	if len(mset) < n {
		if meth != nil { // single method
			id := meth.Obj().Id()
			if mset[id] == nil {
				mset[id] = findMethod(prog, meth)
			}
		} else {
			// complete set
			for i := 0; i < n; i++ {
				meth := tmset.At(i)
				if id := meth.Obj().Id(); mset[id] == nil {
					mset[id] = findMethod(prog, meth)
				}
			}
		}
	}

	return mset
}

func methodSet(typ types.Type) *types.MethodSet {
	// TODO(adonovan): temporary workaround.  Inline it away when fixed.
	if _, ok := deref(typ).Underlying().(*types.Interface); ok && isPointer(typ) {
		// TODO(gri): fix: go/types bug: pointer-to-interface
		// has no methods---yet go/types says it has!
		return new(types.MethodSet)
	}
	return typ.MethodSet()
}

// LookupMethod returns the Function for the specified method object,
// building wrapper methods on demand.  It returns nil if the typ has
// no such method.
//
// Thread-safe.
//
// EXCLUSIVE_LOCKS_ACQUIRED(prog.methodsMu)
//
func (prog *Program) LookupMethod(meth *types.Selection) *Function {
	return prog.populateMethodSet(meth.Recv(), meth)[meth.Obj().Id()]
}

// concreteMethod returns the concrete method denoted by obj.
// Panic ensues if there is no such method (e.g. it's a standalone
// function).
//
func (prog *Program) concreteMethod(obj *types.Func) *Function {
	fn := prog.concreteMethods[obj]
	if fn == nil {
		panic("no concrete method: " + obj.String())
	}
	return fn
}

// findMethod returns the concrete Function for the method meth,
// synthesizing wrappers as needed.
//
// EXCLUSIVE_LOCKS_REQUIRED(prog.methodsMu)
//
func findMethod(prog *Program, meth *types.Selection) *Function {
	needsPromotion := len(meth.Index()) > 1
	mfunc := meth.Obj().(*types.Func)
	needsIndirection := !isPointer(recvType(mfunc)) && isPointer(meth.Recv())

	if needsPromotion || needsIndirection {
		return makeWrapper(prog, meth.Recv(), meth)
	}

	if _, ok := meth.Recv().Underlying().(*types.Interface); ok {
		return interfaceMethodWrapper(prog, meth.Recv(), mfunc)
	}

	return prog.concreteMethod(mfunc)
}

// makeWrapper returns a synthetic wrapper Function that optionally
// performs receiver indirection, implicit field selections and then a
// tailcall of a "promoted" method.  For example, given these decls:
//
//    type A struct {B}
//    type B struct {*C}
//    type C ...
//    func (*C) f()
//
// then makeWrapper(typ=A, obj={Func:(*C).f, Indices=[B,C,f]})
// synthesize this wrapper method:
//
//    func (a A) f() { return a.B.C->f() }
//
// prog is the program to which the synthesized method will belong.
// typ is the receiver type of the wrapper method.  obj is the
// type-checker's object for the promoted method; its Func may be a
// concrete or an interface method.
//
// EXCLUSIVE_LOCKS_REQUIRED(prog.methodsMu)
//
func makeWrapper(prog *Program, typ types.Type, meth *types.Selection) *Function {
	mfunc := meth.Obj().(*types.Func)
	old := mfunc.Type().(*types.Signature)
	sig := types.NewSignature(nil, types.NewVar(token.NoPos, nil, "recv", typ), old.Params(), old.Results(), old.IsVariadic())

	description := fmt.Sprintf("wrapper for %s", mfunc)
	if prog.mode&LogSource != 0 {
		defer logStack("make %s to (%s)", description, typ)()
	}
	fn := &Function{
		name:      mfunc.Name(),
		method:    meth,
		Signature: sig,
		Synthetic: description,
		Breakpoint: false,
		Scope      : nil,
		LocalsByName: make(map[string]int),
		Prog:      prog,
		pos:       mfunc.Pos(),
	}
	fn.startBody(nil)
	fn.addSpilledParam(sig.Recv())
	createParams(fn)

	var v Value = fn.Locals[0] // spilled receiver
	if isPointer(typ) {
		// TODO(adonovan): consider emitting a nil-pointer check here
		// with a nice error message, like gc does.
		v = emitLoad(fn, v)
	}

	// Invariant: v is a pointer, either
	//   value of *A receiver param, or
	// address of  A spilled receiver.

	// We use pointer arithmetic (FieldAddr possibly followed by
	// Load) in preference to value extraction (Field possibly
	// preceded by Load).

	indices := meth.Index()
	v = emitImplicitSelections(fn, v, indices[:len(indices)-1])

	// Invariant: v is a pointer, either
	//   value of implicit *C field, or
	// address of implicit  C field.

	var c Call
	if _, ok := old.Recv().Type().Underlying().(*types.Interface); !ok { // concrete method
		if !isPointer(old.Recv().Type()) {
			v = emitLoad(fn, v)
		}
		c.Call.Value = prog.concreteMethod(mfunc)
		c.Call.Args = append(c.Call.Args, v)
	} else {
		c.Call.Method = mfunc
		c.Call.Value = emitLoad(fn, v)
	}
	for _, arg := range fn.Params[1:] {
		c.Call.Args = append(c.Call.Args, arg)
	}
	emitTailCall(fn, &c)
	fn.finishBody()
	return fn
}

// createParams creates parameters for wrapper method fn based on its
// Signature.Params, which do not include the receiver.
//
func createParams(fn *Function) {
	var last *Parameter
	tparams := fn.Signature.Params()
	for i, n := 0, tparams.Len(); i < n; i++ {
		last = fn.addParamObj(tparams.At(i))
	}
	if fn.Signature.IsVariadic() {
		last.typ = types.NewSlice(last.typ)
	}
}

// Wrappers for standalone interface methods ----------------------------------

// interfaceMethodWrapper returns a synthetic wrapper function
// permitting an abstract method obj to be called like a standalone
// function, e.g.:
//
//   type I interface { f(x int) R }
//   m := I.f  // wrapper
//   var i I
//   m(i, 0)
//
// The wrapper is defined as if by:
//
//   func I.f(i I, x int, ...) R {
//     return i.f(x, ...)
//   }
//
// typ is the type of the receiver (I here).  It isn't necessarily
// equal to the recvType(obj) because one interface may embed another.
// TODO(adonovan): more tests.
//
// TODO(adonovan): opt: currently the stub is created even when used
// in call position: I.f(i, 0).  Clearly this is suboptimal.
//
// EXCLUSIVE_LOCKS_REQUIRED(prog.methodsMu)
//
func interfaceMethodWrapper(prog *Program, typ types.Type, obj *types.Func) *Function {
	// If one interface embeds another they'll share the same
	// wrappers for common methods.  This is safe, but it might
	// confuse some tools because of the implicit interface
	// conversion applied to the first argument.  If this becomes
	// a problem, we should include 'typ' in the memoization key.
	fn, ok := prog.ifaceMethodWrappers[obj]
	if !ok {
		description := fmt.Sprintf("interface method wrapper for %s.%s", typ, obj)
		if prog.mode&LogSource != 0 {
			defer logStack("%s", description)()
		}
		fn = &Function{
			name:      obj.Name(),
			object:    obj,
			Signature: obj.Type().(*types.Signature),
			Synthetic: description,
			pos:       obj.Pos(),
			Prog:      prog,
			Breakpoint: false,
			Scope     : nil,
			LocalsByName: make(map[string]int),
		}
		fn.startBody(nil)
		fn.addParam("recv", typ, token.NoPos)
		createParams(fn)
		var c Call

		c.Call.Method = obj
		c.Call.Value = fn.Params[0]
		for _, arg := range fn.Params[1:] {
			c.Call.Args = append(c.Call.Args, arg)
		}
		emitTailCall(fn, &c)
		fn.finishBody()

		prog.ifaceMethodWrappers[obj] = fn
	}
	return fn
}

// Wrappers for bound methods -------------------------------------------------

// boundMethodWrapper returns a synthetic wrapper function that
// delegates to a concrete or interface method.
// The wrapper has one free variable, the method's receiver.
// Use MakeClosure with such a wrapper to construct a bound-method
// closure.  e.g.:
//
//   type T int          or:  type T interface { meth() }
//   func (t T) meth()
//   var t T
//   f := t.meth
//   f() // calls t.meth()
//
// f is a closure of a synthetic wrapper defined as if by:
//
//   f := func() { return t.meth() }
//
// EXCLUSIVE_LOCKS_ACQUIRED(meth.Prog.methodsMu)
//
func boundMethodWrapper(prog *Program, obj *types.Func) *Function {
	prog.methodsMu.Lock()
	defer prog.methodsMu.Unlock()
	fn, ok := prog.boundMethodWrappers[obj]
	if !ok {
		description := fmt.Sprintf("bound method wrapper for %s", obj)
		if prog.mode&LogSource != 0 {
			defer logStack("%s", description)()
		}
		s := obj.Type().(*types.Signature)
		fn = &Function{
			name:      "bound$" + obj.FullName(),
			Signature: types.NewSignature(nil, nil, s.Params(), s.Results(), s.IsVariadic()),
			Synthetic: description,
			Prog:      prog,
			Breakpoint: false,
			LocalsByName: make(map[string]int),
			Scope       : nil,
			pos:       obj.Pos(),
		}

		cap := &Capture{name: "recv", typ: recvType(obj), parent: fn}
		fn.FreeVars = []*Capture{cap}
		fn.startBody(nil)
		createParams(fn)
		var c Call

		if _, ok := recvType(obj).Underlying().(*types.Interface); !ok { // concrete
			c.Call.Value = prog.concreteMethod(obj)
			c.Call.Args = []Value{cap}
		} else {
			c.Call.Value = cap
			c.Call.Method = obj
		}
		for _, arg := range fn.Params {
			c.Call.Args = append(c.Call.Args, arg)
		}
		emitTailCall(fn, &c)
		fn.finishBody()

		prog.boundMethodWrappers[obj] = fn
	}
	return fn
}
