(struct_item name: (type_identifier) @name) @definition.struct
(union_item name: (type_identifier) @name) @definition.struct
(enum_item name: (type_identifier) @name) @definition.enum
(enum_variant name: (identifier) @name) @definition.enum_member
(type_item name: (type_identifier) @name) @definition.type
(trait_item name: (type_identifier) @name) @definition.trait
(macro_definition name: (identifier) @name) @definition.macro
; only inline modules: `mod m;` just points at another file and encloses nothing
(mod_item name: (identifier) @name body: (declaration_list)) @definition.module

; an impl block is a symbol named after the implementing type, so its methods
; nest under it (qualified name Type.method) and the `impl Trait for Type`
; extends edge below has a source node for impls and interface dispatch
(impl_item type: [
  (type_identifier) @name
  (generic_type type: (type_identifier) @name)
  (scoped_type_identifier name: (type_identifier) @name)
  (generic_type type: (scoped_type_identifier name: (type_identifier) @name))
  (reference_type type: [(type_identifier) @name (generic_type type: (type_identifier) @name)])
]) @definition.impl

; a function_item is a method only inside an impl or trait body. Each context is
; listed explicitly (instead of one catch-all function_item pattern, as upstream
; does) so no item is captured twice.
(source_file (function_item name: (identifier) @name) @definition.function)
(mod_item body: (declaration_list (function_item name: (identifier) @name) @definition.function))
(block (function_item name: (identifier) @name) @definition.function)
(foreign_mod_item body: (declaration_list (function_signature_item name: (identifier) @name) @definition.function))
(impl_item body: (declaration_list (function_item name: (identifier) @name) @definition.method))
(trait_item body: (declaration_list [
  (function_item name: (identifier) @name)
  (function_signature_item name: (identifier) @name)
] @definition.method))

; calls: f(), x.m(), Type::f() / Self::f() / module::f() (receiver = last path
; segment, so Circle::new binds to Circle.new), and their turbofish forms
(call_expression function: (identifier) @name) @reference.call
(call_expression function: (field_expression value: (_) @recv field: (field_identifier) @name)) @reference.call
(call_expression function: (scoped_identifier
  path: [(identifier) @recv (scoped_identifier name: (identifier) @recv) (generic_type type: (type_identifier) @recv)]
  name: (identifier) @name)) @reference.call
(call_expression function: (scoped_identifier path: [(self) (super) (crate) (bracketed_type)] name: (identifier) @name)) @reference.call
(call_expression function: (generic_function function: (identifier) @name)) @reference.call
(call_expression function: (generic_function function: (field_expression value: (_) @recv field: (field_identifier) @name))) @reference.call
(call_expression function: (generic_function function: (scoped_identifier
  path: [(identifier) @recv (scoped_identifier name: (identifier) @recv) (generic_type type: (type_identifier) @recv)]
  name: (identifier) @name))) @reference.call
(call_expression function: (generic_function function: (scoped_identifier path: [(self) (super) (crate) (bracketed_type)] name: (identifier) @name))) @reference.call

; a macro invocation is a reference to the macro (it resolves to the
; macro_rules! definition), not a call: no callable target, nothing to trace
(macro_invocation macro: (identifier) @name) @reference.macro
(macro_invocation macro: (scoped_identifier name: (identifier) @name)) @reference.macro

; calls inside macro arguments: tree-sitter-rust keeps them as raw token trees,
; so `assert_eq!(add(1, 2), 3)` holds no call_expression. ParseFile's macroCalls
; scans the argument tree (and the trees nested one level in it) in one linear
; pass: an identifier followed directly, or after a `::<..>` turbofish, by a
; parenthesized tree is a call. A query anchor cannot express that (it skips
; punctuation, so `x, (1, 2)` would match).
(macro_invocation (token_tree) @macro.args)

; use declarations emit the imported item's name, so the edge resolves to it
(use_declaration argument: (identifier) @name) @reference.import
(use_declaration argument: (scoped_identifier name: (identifier) @name)) @reference.import
(use_as_clause path: [(identifier) @name (scoped_identifier name: (identifier) @name)]) @reference.import
(use_list [(identifier) @name (scoped_identifier name: (identifier) @name)] @reference.import)

; `impl Trait for Type` and supertraits `trait A: B` are extends edges: the
; edge impls and interface dispatch read
(impl_item trait: [
  (type_identifier) @name
  (scoped_type_identifier name: (type_identifier) @name)
  (generic_type type: [(type_identifier) @name (scoped_type_identifier name: (type_identifier) @name)])
]) @reference.extends
(trait_item bounds: (trait_bounds [
  (type_identifier) @name
  (scoped_type_identifier name: (type_identifier) @name)
  (generic_type type: [(type_identifier) @name (scoped_type_identifier name: (type_identifier) @name)])
])) @reference.extends

; attributes: #[test], #[tokio::main], and each derive in #[derive(Debug, Clone)].
; They precede the item as siblings; ParseFile widens the item's span over them.
(attribute_item (attribute [(identifier) @name (scoped_identifier name: (identifier) @name)])) @reference.annotation
(attribute_item (attribute (identifier) @_derive arguments: (token_tree (identifier) @name)) (#eq? @_derive "derive")) @reference.annotation

; type bindings. Parameter and typed let share ONE pattern (they are the only
; nodes with both pattern: and type: fields), so their type shapes cannot drift:
; T, a::T, T<..>, a::T<..>, each behind & / &mut, plus dyn and impl traits
(_ pattern: (identifier) @bind.name type: [
  (type_identifier) @bind.type
  (scoped_type_identifier name: (type_identifier) @bind.type)
  (generic_type type: [(type_identifier) @bind.type (scoped_type_identifier name: (type_identifier) @bind.type)])
  (reference_type type: [
    (type_identifier) @bind.type
    (scoped_type_identifier name: (type_identifier) @bind.type)
    (generic_type type: [(type_identifier) @bind.type (scoped_type_identifier name: (type_identifier) @bind.type)])
    (dynamic_type trait: [
      (type_identifier) @bind.type
      (scoped_type_identifier name: (type_identifier) @bind.type)
      (generic_type type: [(type_identifier) @bind.type (scoped_type_identifier name: (type_identifier) @bind.type)])])])
  (abstract_type trait: [
    (type_identifier) @bind.type
    (scoped_type_identifier name: (type_identifier) @bind.type)
    (generic_type type: [(type_identifier) @bind.type (scoped_type_identifier name: (type_identifier) @bind.type)])])
])
; let x = T { .. } / T::new() / T::default()
(let_declaration pattern: (identifier) @bind.name value: (struct_expression name: [
  (type_identifier) @bind.type
  (scoped_type_identifier name: (type_identifier) @bind.type)
]))
(let_declaration pattern: (identifier) @bind.name value: (call_expression function: (scoped_identifier
  path: [(identifier) @bind.type (scoped_identifier name: (identifier) @bind.type) (generic_type type: (type_identifier) @bind.type)]
  name: (identifier) @_ctor))
  (#any-of? @_ctor "new" "default"))
; `self` and `Self` inside an impl or trait body denote the implementing type
(impl_item type: [
  (type_identifier) @bind.type
  (generic_type type: (type_identifier) @bind.type)
  (scoped_type_identifier name: (type_identifier) @bind.type)
  (generic_type type: (scoped_type_identifier name: (type_identifier) @bind.type))
  (reference_type type: [(type_identifier) @bind.type (generic_type type: (type_identifier) @bind.type)])
] body: (declaration_list) @bind.self)
(trait_item name: (type_identifier) @bind.type body: (declaration_list) @bind.self)
