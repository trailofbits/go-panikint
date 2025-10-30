// Copyright 2011 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package types2

import (
	"cmd/compile/internal/syntax"
	"strings"
	"sync"
	"sync/atomic"
)

// Type-checking Named types is subtle, because they may be recursively
// defined, and because their full details may be spread across multiple
// declarations (via methods). For this reason they are type-checked lazily,
// to avoid information being accessed before it is complete.
//
// Conceptually, it is helpful to think of named types as having two distinct
// sets of information:
//  - "LHS" information, defining their identity: Obj() and TypeArgs()
//  - "RHS" information, defining their details: TypeParams(), Underlying(),
//    and methods.
//
// In this taxonomy, LHS information is available immediately, but RHS
// information is lazy. Specifically, a named type N may be constructed in any
// of the following ways:
//  1. type-checked from the source
//  2. loaded eagerly from export data
//  3. loaded lazily from export data (when using unified IR)
//  4. instantiated from a generic type
//
// In cases 1, 3, and 4, it is possible that the underlying type or methods of
// N may not be immediately available.
//  - During type-checking, we allocate N before type-checking its underlying
//    type or methods, so that we can create recursive references.
//  - When loading from export data, we may load its methods and underlying
//    type lazily using a provided load function.
//  - After instantiating, we lazily expand the underlying type and methods
//    (note that instances may be created while still in the process of
//    type-checking the original type declaration).
//
// In cases 3 and 4 this lazy construction may also occur concurrently, due to
// concurrent use of the type checker API (after type checking or importing has
// finished). It is critical that we keep track of state, so that Named types
// are constructed exactly once and so that we do not access their details too
// soon.
//
// We achieve this by tracking state with an atomic state variable, and
// guarding potentially concurrent calculations with a mutex. See [stateMask]
// for details.
//
// GLOSSARY: Here are a few terms used in this file to describe Named types:
//  - We say that a Named type is "instantiated" if it has been constructed by
//    instantiating a generic named type with type arguments.
//  - We say that a Named type is "declared" if it corresponds to a type
//    declaration in the source. Instantiated named types correspond to a type
//    instantiation in the source, not a declaration. But their Origin type is
//    a declared type.
//  - We say that a Named type is "unpacked" if its RHS information has been
//    populated, normalizing its representation for use in type-checking
//    operations and abstracting away how it was created:
//      - For a Named type constructed from unified IR, this involves invoking
//        a lazy loader function to extract details from UIR as needed.
//      - For an instantiated Named type, this involves extracting information
//        from its origin and substituting type arguments into a "synthetic"
//        RHS; this process is called "expanding" the RHS (see below).
//  - We say that a Named type is "expanded" if it is an instantiated type and
//    type parameters in its RHS and methods have been substituted with the type
//    arguments from the instantiation. A type may be partially expanded if some
//    but not all of these details have been substituted. Similarly, we refer to
//    these individual details (RHS or method) as being "expanded".
//
// Some invariants to keep in mind: each declared Named type has a single
// corresponding object, and that object's type is the (possibly generic) Named
// type. Declared Named types are identical if and only if their pointers are
// identical. On the other hand, multiple instantiated Named types may be
// identical even though their pointers are not identical. One has to use
// Identical to compare them. For instantiated named types, their obj is a
// synthetic placeholder that records their position of the corresponding
// instantiation in the source (if they were constructed during type checking).
//
// To prevent infinite expansion of named instances that are created outside of
// type-checking, instances share a Context with other instances created during
// their expansion. Via the pidgeonhole principle, this guarantees that in the
// presence of a cycle of named types, expansion will eventually find an
// existing instance in the Context and short-circuit the expansion.
//
// Once an instance is fully expanded, we can nil out this shared Context to unpin
// memory, though the Context may still be held by other incomplete instances
// in its "lineage".

// A Named represents a named (defined) type.
//
// A declaration such as:
//
//	type S struct { ... }
//
// creates a defined type whose underlying type is a struct,
// and binds this type to the object S, a [TypeName].
// Use [Named.Underlying] to access the underlying type.
// Use [Named.Obj] to obtain the object S.
//
// Before type aliases (Go 1.9), the spec called defined types "named types".
type Named struct {
	check *Checker  // non-nil during type-checking; nil otherwise
	obj   *TypeName // corresponding declared object for declared types; see above for instantiated types

	// flags indicating temporary violations of the invariants for fromRHS and underlying
	allowNilRHS        bool // same as below, as well as briefly during checking of a type declaration
	allowNilUnderlying bool // may be true from creation via [NewNamed] until [Named.SetUnderlying]

	inst *instance // information for instantiated types; nil otherwise

	mu         sync.Mutex     // guards all fields below
	state_     uint32         // the current state of this type; must only be accessed atomically or when mu is held
	fromRHS    Type           // the declaration RHS this type is derived from
	tparams    *TypeParamList // type parameters, or nil
	underlying Type           // underlying type, or nil

	// methods declared for this type (not the method set of this type)
	// Signatures are type-checked lazily.
	// For non-instantiated types, this is a fully populated list of methods. For
	// instantiated types, methods are individually expanded when they are first
	// accessed.
	methods []*Func

	// loader may be provided to lazily load type parameters, underlying type, methods, and delayed functions
	loader func(*Named) ([]*TypeParam, Type, []*Func, []func())
}

// instance holds information that is only necessary for instantiated named
// types.
type instance struct {
	orig            *Named    // original, uninstantiated type
	targs           *TypeList // type arguments
	expandedMethods int       // number of expanded methods; expandedMethods <= len(orig.methods)
	ctxt            *Context  // local Context; set to nil after full expansion
}

// stateMask represents each state in the lifecycle of a named type.
//
// Each named type begins in the initial state. A named type may transition to a new state
// according to the below diagram:
//
//	initial
//	lazyLoaded
//	unpacked
//	└── hasMethods
//	└── hasUnder
//
// That is, descent down the tree is mostly linear (initial through unpacked), except upon
// reaching the leaves (hasMethods and hasUnder). A type may occupy any combination of the
// leaf states at once (they are independent states).
//
// To represent this independence, the set of active states is represented with a bit set. State
// transitions are monotonic. Once a state bit is set, it remains set.
//
// The above constraints significantly narrow the possible bit sets for a named type. With bits
// set left-to-right, they are:
//
//	0000 | initial
//	1000 | lazyLoaded
//	1100 | unpacked, which implies lazyLoaded
//	1110 | hasMethods, which implies unpacked (which in turn implies lazyLoaded)
//	1101 | hasUnder, which implies unpacked ...
//	1111 | both hasMethods and hasUnder which implies unpacked ...
//
// To read the state of a named type, use [Named.stateHas]; to write, use [Named.setState].
type stateMask uint32

const (
	// initially, type parameters, RHS, underlying, and methods might be unavailable
	lazyLoaded stateMask = 1 << iota // methods are available, but constraints might be unexpanded (for generic types)
	unpacked                         // methods might be unexpanded (for instances)
	hasMethods                       // methods are all expanded (for instances)
	hasUnder                         // underlying type is available
)

// NewNamed returns a new named type for the given type name, underlying type, and associated methods.
// If the given type name obj doesn't have a type yet, its type is set to the returned named type.
// The underlying type must not be a *Named.
func NewNamed(obj *TypeName, underlying Type, methods []*Func) *Named {
	if asNamed(underlying) != nil {
		panic("underlying type must not be *Named")
	}
	n := (*Checker)(nil).newNamed(obj, underlying, methods)
	if underlying == nil {
		n.allowNilRHS = true
		n.allowNilUnderlying = true
	} else {
		n.SetUnderlying(underlying)
	}
	return n

}

// unpack populates the type parameters, methods, and RHS of n.
//
// For the purposes of unpacking, there are three categories of named types:
//  1. Lazy loaded types
//  2. Instantiated types
//  3. All others
//
// Note that the above form a partition.
//
// Lazy loaded types:
// Type parameters, methods, and RHS of n become accessible and are fully
// expanded.
//
// Instantiated types:
// Type parameters, methods, and RHS of n become accessible, though methods
// are lazily populated as needed.
//
// All others:
// Effectively, nothing happens.
func (n *Named) unpack() *Named {
	if n.stateHas(lazyLoaded | unpacked) { // avoid locking below
		return n
	}

	// TODO(rfindley): if n.check is non-nil we can avoid locking here, since
	// type-checking is not concurrent. Evaluate if this is worth doing.
	n.mu.Lock()
	defer n.mu.Unlock()

	// only atomic for consistency; we are holding the mutex
	if n.stateHas(lazyLoaded | unpacked) {
		return n
	}

	// underlying comes after unpacking, do not set it
	defer (func() { assert(!n.stateHas(hasUnder)) })()

	if n.inst != nil {
		assert(n.fromRHS == nil) // instantiated types are not declared types
		assert(n.loader == nil)  // cannot import an instantiation

		orig := n.inst.orig
		orig.unpack()

		n.fromRHS = n.expandRHS()
		n.tparams = orig.tparams

		if len(orig.methods) == 0 {
			n.setState(lazyLoaded | unpacked | hasMethods) // nothing further to do
			n.inst.ctxt = nil
		} else {
			n.setState(lazyLoaded | unpacked)
		}
		return n
	}

	// TODO(mdempsky): Since we're passing n to the loader anyway
	// (necessary because types2 expects the receiver type for methods
	// on defined interface types to be the Named rather than the
	// underlying Interface), maybe it should just handle calling
	// SetTypeParams, SetUnderlying, and AddMethod instead?  Those
	// methods would need to support reentrant calls though. It would
	// also make the API more future-proof towards further extensions.
	if n.loader != nil {
		assert(n.fromRHS == nil) // not loaded yet
		assert(n.inst == nil)    // cannot import an instantiation

		tparams, underlying, methods, delayed := n.loader(n)
		n.loader = nil

		n.tparams = bindTParams(tparams)
		n.fromRHS = underlying // for cycle detection
		n.methods = methods

		n.setState(lazyLoaded) // avoid deadlock calling delayed functions
		for _, f := range delayed {
			f()
		}
	}

	n.setState(lazyLoaded | unpacked | hasMethods)
	return n
}

// stateHas atomically determines whether the current state includes any active bit in sm.
func (n *Named) stateHas(m stateMask) bool {
	return stateMask(atomic.LoadUint32(&n.state_))&m != 0
}

// setState atomically sets the current state to include each active bit in sm.
// Must only be called while holding n.mu.
func (n *Named) setState(m stateMask) {
	atomic.OrUint32(&n.state_, uint32(m))
	// verify state transitions
	if debug {
		m := stateMask(atomic.LoadUint32(&n.state_))
		u := m&unpacked != 0
		// unpacked => lazyLoaded
		if u {
			assert(m&lazyLoaded != 0)
		}
		// hasMethods => unpacked
		if m&hasMethods != 0 {
			assert(u)
		}
		// hasUnder => unpacked
		if m&hasUnder != 0 {
			assert(u)
		}
	}
}

// newNamed is like NewNamed but with a *Checker receiver.
func (check *Checker) newNamed(obj *TypeName, fromRHS Type, methods []*Func) *Named {
	typ := &Named{check: check, obj: obj, fromRHS: fromRHS, methods: methods}
	if obj.typ == nil {
		obj.typ = typ
	}
	// Ensure that typ is always sanity-checked.
	if check != nil {
		check.needsCleanup(typ)
	}
	return typ
}

// newNamedInstance creates a new named instance for the given origin and type
// arguments, recording pos as the position of its synthetic object (for error
// reporting).
//
// If set, expanding is the named type instance currently being expanded, that
// led to the creation of this instance.
func (check *Checker) newNamedInstance(pos syntax.Pos, orig *Named, targs []Type, expanding *Named) *Named {
	assert(len(targs) > 0)

	obj := NewTypeName(pos, orig.obj.pkg, orig.obj.name, nil)
	inst := &instance{orig: orig, targs: newTypeList(targs)}

	// Only pass the expanding context to the new instance if their packages
	// match. Since type reference cycles are only possible within a single
	// package, this is sufficient for the purposes of short-circuiting cycles.
	// Avoiding passing the context in other cases prevents unnecessary coupling
	// of types across packages.
	if expanding != nil && expanding.Obj().pkg == obj.pkg {
		inst.ctxt = expanding.inst.ctxt
	}
	typ := &Named{check: check, obj: obj, inst: inst}
	obj.typ = typ
	// Ensure that typ is always sanity-checked.
	if check != nil {
		check.needsCleanup(typ)
	}
	return typ
}

func (n *Named) cleanup() {
	// Instances can have a nil underlying at the end of type checking — they
	// will lazily expand it as needed. All other types must have one.
	if n.inst == nil {
		n.Underlying()
	}
	n.check = nil
}

// Obj returns the type name for the declaration defining the named type t. For
// instantiated types, this is same as the type name of the origin type.
func (t *Named) Obj() *TypeName {
	if t.inst == nil {
		return t.obj
	}
	return t.inst.orig.obj
}

// Origin returns the generic type from which the named type t is
// instantiated. If t is not an instantiated type, the result is t.
func (t *Named) Origin() *Named {
	if t.inst == nil {
		return t
	}
	return t.inst.orig
}

// TypeParams returns the type parameters of the named type t, or nil.
// The result is non-nil for an (originally) generic type even if it is instantiated.
func (t *Named) TypeParams() *TypeParamList { return t.unpack().tparams }

// SetTypeParams sets the type parameters of the named type t.
// t must not have type arguments.
func (t *Named) SetTypeParams(tparams []*TypeParam) {
	assert(t.inst == nil)
	t.unpack().tparams = bindTParams(tparams)
}

// TypeArgs returns the type arguments used to instantiate the named type t.
func (t *Named) TypeArgs() *TypeList {
	if t.inst == nil {
		return nil
	}
	return t.inst.targs
}

// NumMethods returns the number of explicit methods defined for t.
func (t *Named) NumMethods() int {
	return len(t.Origin().unpack().methods)
}

// Method returns the i'th method of named type t for 0 <= i < t.NumMethods().
//
// For an ordinary or instantiated type t, the receiver base type of this
// method is the named type t. For an uninstantiated generic type t, each
// method receiver is instantiated with its receiver type parameters.
//
// Methods are numbered deterministically: given the same list of source files
// presented to the type checker, or the same sequence of NewMethod and AddMethod
// calls, the mapping from method index to corresponding method remains the same.
// But the specific ordering is not specified and must not be relied on as it may
// change in the future.
func (t *Named) Method(i int) *Func {
	t.unpack()

	if t.stateHas(hasMethods) {
		return t.methods[i]
	}

	assert(t.inst != nil) // only instances should have unexpanded methods
	orig := t.inst.orig

	t.mu.Lock()
	defer t.mu.Unlock()

	if len(t.methods) != len(orig.methods) {
		assert(len(t.methods) == 0)
		t.methods = make([]*Func, len(orig.methods))
	}

	if t.methods[i] == nil {
		assert(t.inst.ctxt != nil) // we should still have a context remaining from the resolution phase
		t.methods[i] = t.expandMethod(i)
		t.inst.expandedMethods++

		// Check if we've created all methods at this point. If we have, mark the
		// type as having all of its methods.
		if t.inst.expandedMethods == len(orig.methods) {
			t.setState(hasMethods)
			t.inst.ctxt = nil // no need for a context anymore
		}
	}

	return t.methods[i]
}

// expandMethod substitutes type arguments in the i'th method for an
// instantiated receiver.
func (t *Named) expandMethod(i int) *Func {
	// t.orig.methods is not lazy. origm is the method instantiated with its
	// receiver type parameters (the "origin" method).
	origm := t.inst.orig.Method(i)
	assert(origm != nil)

	check := t.check
	// Ensure that the original method is type-checked.
	if check != nil {
		check.objDecl(origm, nil)
	}

	origSig := origm.typ.(*Signature)
	rbase, _ := deref(origSig.Recv().Type())

	// If rbase is t, then origm is already the instantiated method we're looking
	// for. In this case, we return origm to preserve the invariant that
	// traversing Method->Receiver Type->Method should get back to the same
	// method.
	//
	// This occurs if t is instantiated with the receiver type parameters, as in
	// the use of m in func (r T[_]) m() { r.m() }.
	if rbase == t {
		return origm
	}

	sig := origSig
	// We can only substitute if we have a correspondence between type arguments
	// and type parameters. This check is necessary in the presence of invalid
	// code.
	if origSig.RecvTypeParams().Len() == t.inst.targs.Len() {
		smap := makeSubstMap(origSig.RecvTypeParams().list(), t.inst.targs.list())
		var ctxt *Context
		if check != nil {
			ctxt = check.context()
		}
		sig = check.subst(origm.pos, origSig, smap, t, ctxt).(*Signature)
	}

	if sig == origSig {
		// No substitution occurred, but we still need to create a new signature to
		// hold the instantiated receiver.
		copy := *origSig
		sig = &copy
	}

	var rtyp Type
	if origm.hasPtrRecv() {
		rtyp = NewPointer(t)
	} else {
		rtyp = t
	}

	sig.recv = cloneVar(origSig.recv, rtyp)
	return cloneFunc(origm, sig)
}

// SetUnderlying sets the underlying type and marks t as complete.
// t must not have type arguments.
func (t *Named) SetUnderlying(u Type) {
	assert(t.inst == nil)
	if u == nil {
		panic("underlying type must not be nil")
	}
	if asNamed(u) != nil {
		panic("underlying type must not be *Named")
	}
	// be careful to uphold the state invariants
	t.mu.Lock()
	defer t.mu.Unlock()

	t.fromRHS = u
	t.allowNilRHS = false
	t.setState(lazyLoaded | unpacked | hasMethods) // TODO(markfreeman): Why hasMethods?

	t.underlying = u
	t.allowNilUnderlying = false
	t.setState(hasUnder)
}

// AddMethod adds method m unless it is already in the method list.
// The method must be in the same package as t, and t must not have
// type arguments.
func (t *Named) AddMethod(m *Func) {
	assert(samePkg(t.obj.pkg, m.pkg))
	assert(t.inst == nil)
	t.unpack()
	if t.methodIndex(m.name, false) < 0 {
		t.methods = append(t.methods, m)
	}
}

// methodIndex returns the index of the method with the given name.
// If foldCase is set, capitalization in the name is ignored.
// The result is negative if no such method exists.
func (t *Named) methodIndex(name string, foldCase bool) int {
	if name == "_" {
		return -1
	}
	if foldCase {
		for i, m := range t.methods {
			if strings.EqualFold(m.name, name) {
				return i
			}
		}
	} else {
		for i, m := range t.methods {
			if m.name == name {
				return i
			}
		}
	}
	return -1
}

// rhs returns [Named.fromRHS].
//
// In debug mode, it also asserts that n is in an appropriate state.
func (n *Named) rhs() Type {
	if debug {
		assert(n.stateHas(lazyLoaded | unpacked))
	}
	return n.fromRHS
}

// Underlying returns the [underlying type] of the named type t, resolving all
// forwarding declarations. Underlying types are never Named, TypeParam, or
// Alias types.
//
// [underlying type]: https://go.dev/ref/spec#Underlying_types.
func (n *Named) Underlying() Type {
	n.unpack()

	// The gccimporter depends on writing a nil underlying via NewNamed and
	// immediately reading it back. Rather than putting that in Named.under
	// and complicating things there, we just check for that special case here.
	if n.rhs() == nil {
		assert(n.allowNilRHS)
		if n.allowNilUnderlying {
			return nil
		}
	}

	if !n.stateHas(hasUnder) { // minor performance optimization
		n.resolveUnderlying()
	}

	return n.underlying
}

func (t *Named) String() string { return TypeString(t, nil) }

// ----------------------------------------------------------------------------
// Implementation
//
// TODO(rfindley): reorganize the loading and expansion methods under this
// heading.

// resolveUnderlying computes the underlying type of n. If n already has an
// underlying type, nothing happens.
//
// It does so by following RHS type chains for alias and named types. If any
// other type T is found, each named type in the chain has its underlying
// type set to T. Aliases are skipped because their underlying type is
// not memoized.
//
// This method also checks for cycles among alias and named types, which will
// yield no underlying type. If such a cycle is found, the underlying type is
// set to Typ[Invalid] and a cycle is reported.
func (n *Named) resolveUnderlying() {
	assert(n.stateHas(unpacked))

	var seen map[*Named]int // allocated lazily
	var u Type
	for rhs := Type(n); u == nil; {
		switch t := rhs.(type) {
		case nil:
			u = Typ[Invalid]

		case *Alias:
			rhs = unalias(t)

		case *Named:
			if i, ok := seen[t]; ok {
				// compute cycle path
				path := make([]Object, len(seen))
				for t, j := range seen {
					path[j] = t.obj
				}
				path = path[i:]
				// only called during type checking, hence n.check != nil
				n.check.cycleError(path, firstInSrc(path))
				u = Typ[Invalid]
				break
			}

			// don't recalculate the underlying
			if t.stateHas(hasUnder) {
				u = t.underlying
				break
			}

			if seen == nil {
				seen = make(map[*Named]int)
			}
			seen[t] = len(seen)

			t.unpack()
			assert(t.rhs() != nil || t.allowNilRHS)
			rhs = t.rhs()

		default:
			u = rhs // any type literal or predeclared type works
		}
	}

	for t := range seen {
		func() {
			t.mu.Lock()
			defer t.mu.Unlock()
			// Careful, t.underlying has lock-free readers. Since we might be racing
			// another call to resolveUnderlying, we have to avoid overwriting
			// t.underlying. Otherwise, the race detector will be tripped.
			if !t.stateHas(hasUnder) {
				t.underlying = u
				t.setState(hasUnder)
			}
		}()
	}
}

func (n *Named) lookupMethod(pkg *Package, name string, foldCase bool) (int, *Func) {
	n.unpack()
	if samePkg(n.obj.pkg, pkg) || isExported(name) || foldCase {
		// If n is an instance, we may not have yet instantiated all of its methods.
		// Look up the method index in orig, and only instantiate method at the
		// matching index (if any).
		if i := n.Origin().methodIndex(name, foldCase); i >= 0 {
			// For instances, m.Method(i) will be different from the orig method.
			return i, n.Method(i)
		}
	}
	return -1, nil
}

// context returns the type-checker context.
func (check *Checker) context() *Context {
	if check.ctxt == nil {
		check.ctxt = NewContext()
	}
	return check.ctxt
}

// expandRHS crafts a synthetic RHS for an instantiated type using the RHS of
// its origin type (which must be a generic type).
//
// Suppose that we had:
//
//	type T[P any] struct {
//	  f P
//	}
//
//	type U T[int]
//
// When we go to U, we observe T[int]. Since T[int] is an instantiation, it has no
// declaration. Here, we craft a synthetic RHS for T[int] as if it were declared,
// somewhat similar to:
//
//	type T[int] struct {
//	  f int
//	}
//
// And note that the synthetic RHS here is the same as the underlying for U. Now,
// consider:
//
//	type T[_ any] U
//	type U int
//	type V T[U]
//
// The synthetic RHS for T[U] becomes:
//
//	type T[U] U
//
// Whereas the underlying of V is int, not U.
func (n *Named) expandRHS() (rhs Type) {
	check := n.check
	if check != nil && check.conf.Trace {
		check.trace(n.obj.pos, "-- Named.expandRHS %s", n)
		check.indent++
		defer func() {
			check.indent--
			check.trace(n.obj.pos, "=> %s (rhs = %s)", n, rhs)
		}()
	}

	assert(!n.stateHas(unpacked))
	assert(n.inst.orig.stateHas(lazyLoaded | unpacked))

	if n.inst.ctxt == nil {
		n.inst.ctxt = NewContext()
	}

	ctxt := n.inst.ctxt
	orig := n.inst.orig

	targs := n.inst.targs
	tpars := orig.tparams

	if targs.Len() != tpars.Len() {
		return Typ[Invalid]
	}

	h := ctxt.instanceHash(orig, targs.list())
	u := ctxt.update(h, orig, targs.list(), n) // block fixed point infinite instantiation
	assert(n == u)

	m := makeSubstMap(tpars.list(), targs.list())
	if check != nil {
		ctxt = check.context()
	}

	rhs = check.subst(n.obj.pos, orig.rhs(), m, n, ctxt)

	// TODO(markfreeman): Can we handle this in substitution?
	// If the RHS is an interface, we must set the receiver of interface methods
	// to the named type.
	if iface, _ := rhs.(*Interface); iface != nil {
		if methods, copied := replaceRecvType(iface.methods, orig, n); copied {
			// If the RHS doesn't use type parameters, it may not have been
			// substituted; we need to craft a new interface first.
			if iface == orig.rhs() {
				assert(iface.complete) // otherwise we are copying incomplete data

				crafted := check.newInterface()
				crafted.complete = true
				crafted.implicit = false
				crafted.embeddeds = iface.embeddeds

				iface = crafted
			}
			iface.methods = methods
			iface.tset = nil // recompute type set with new methods

			// go.dev/issue/61561: We have to complete the interface even without a checker.
			if check == nil {
				iface.typeSet()
			}

			return iface
		}
	}

	return rhs
}

// safeUnderlying returns the underlying type of typ without expanding
// instances, to avoid infinite recursion.
//
// TODO(rfindley): eliminate this function or give it a better name.
func safeUnderlying(typ Type) Type {
	if t := asNamed(typ); t != nil {
		return t.underlying
	}
	return typ.Underlying()
}
