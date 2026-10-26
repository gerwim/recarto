(class_definition name: (identifier) @name) @definition.class
; a def whose nearest enclosing def/class is a class is a method; ParseFile
; reclassifies it (the blocks between them are out of a pattern's reach)
(function_definition name: (identifier) @name) @definition.function
; PEP 695 aliases: type Alias = ..., type Pair[T] = ...
(type_alias_statement left: (type [(identifier) @name (generic_type (identifier) @name)])) @definition.type

; calls: f() and recv.m(), the receiver being the object expression's text.
; ParseFile renames a super() receiver to the enclosing class's first base.
(call function: (identifier) @name) @reference.call
(call function: (attribute object: (_) @recv attribute: (identifier) @name)) @reference.call

; imports, one edge per name: `import a.b` names the module, `from m import X as Y`
; names X, so the edge resolves to X's definition
(import_statement name: [(dotted_name) @name (aliased_import name: (dotted_name) @name)] @reference.import)
(import_from_statement name: [(dotted_name) @name (aliased_import name: (dotted_name) @name)] @reference.import)

; positional bases of `class C(Base, mod.Base, Generic[T], metaclass=M)`; a keyword
; argument is not a base
(class_definition superclasses: (argument_list [
  (identifier) @name
  (attribute attribute: (identifier) @name)
  (subscript value: [(identifier) @name (attribute attribute: (identifier) @name)])
] @reference.extends))

; decorators: @d, @m.d, @d(...), @m.d(...). They precede the def/class inside a
; decorated_definition; ParseFile widens the symbol's span over them.
(decorator [
  (identifier) @name
  (attribute attribute: (identifier) @name)
  (call function: [(identifier) @name (attribute attribute: (identifier) @name)])
]) @reference.annotation

; type bindings. Typed parameters, typed defaults and annotated assignments are
; the only nodes with a type: field, each naming its target first, so ONE pattern
; covers them and their type shapes cannot drift: T, m.T, "T", T[..], m.T[..],
; Optional[T], m.Optional[T], T | None. `self.x: T` and a class-level `x: T` type
; the attribute self.x for the whole class (ParseFile scopes them).
(_ . [
  (identifier) @bind.name
  (attribute object: (identifier) @_self attribute: (identifier) @bind.attr)
] type: (type [
  (identifier) @bind.type
  (attribute attribute: (identifier) @bind.type)
  (string (string_content) @bind.type)
  (generic_type (identifier) @bind.type)
  (generic_type (identifier) @_optional (type_parameter . (type [
    (identifier) @bind.type
    (attribute attribute: (identifier) @bind.type)
    (string (string_content) @bind.type)]) .))
  (subscript value: [(identifier) @bind.type (attribute attribute: (identifier) @bind.type)])
  (subscript value: (attribute attribute: (identifier) @_optional) subscript: [
    (identifier) @bind.type
    (attribute attribute: (identifier) @bind.type)
    (string (string_content) @bind.type)])
  (binary_operator left: [(identifier) @bind.type (attribute attribute: (identifier) @bind.type)] right: (none))
]) (#eq? @_self "self") (#eq? @_optional "Optional") (#not-eq? @bind.type "Optional"))
; x = T(..) and self.x = T(..) for a CapWords callee (a class, by PEP 8)
(assignment left: [
  (identifier) @bind.name
  (attribute object: (identifier) @_self attribute: (identifier) @bind.attr)
] right: (call function: [(identifier) @bind.type (attribute attribute: (identifier) @bind.type)])
  (#eq? @_self "self") (#match? @bind.type "^[A-Z]"))
; self.x = param: the attribute takes the parameter's annotated type (ParseFile)
(assignment left: (attribute object: (identifier) @_self attribute: (identifier) @bind.attr)
  right: (identifier) @bind.from (#eq? @_self "self"))
; a method's first parameter, self or cls, is its class (scoped to the method)
(class_definition name: (identifier) @bind.type body: (block [
  (function_definition parameters: (parameters . [(identifier) @bind.name (typed_parameter . (identifier) @bind.name)]))
  (decorated_definition definition: (function_definition parameters: (parameters . [(identifier) @bind.name (typed_parameter . (identifier) @bind.name)])))
]) (#any-of? @bind.name "self" "cls"))

; a class's first positional base, which super() and super(C, self) resolve to
(class_definition name: (identifier) @super.class superclasses: (argument_list . [
  (identifier) @super.base
  (attribute attribute: (identifier) @super.base)
  (subscript value: [(identifier) @super.base (attribute attribute: (identifier) @super.base)])
]))
