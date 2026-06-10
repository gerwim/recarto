(class_declaration name: (identifier) @name) @definition.class
(interface_declaration name: (identifier) @name) @definition.interface
(struct_declaration name: (identifier) @name) @definition.struct
(record_declaration name: (identifier) @name) @definition.record
(enum_declaration name: (identifier) @name) @definition.enum
(enum_member_declaration name: (identifier) @name) @definition.enum_member
(delegate_declaration name: (identifier) @name) @definition.delegate
(method_declaration name: (identifier) @name) @definition.method
(constructor_declaration name: (identifier) @name) @definition.constructor
(property_declaration name: (identifier) @name) @definition.property
(namespace_declaration name: (_) @name) @definition.namespace
(file_scoped_namespace_declaration name: (_) @name) @definition.namespace
(invocation_expression function: (member_access_expression expression: (_)? @recv name: (identifier) @name)) @reference.call
(invocation_expression function: (identifier) @name) @reference.call

; generic invocations: Swap<int>(x) and geo.Swap<int>(x). The callee is a
; generic_name wrapping the identifier, so the identifier-only patterns above
; never match them — without these, a method only ever called with explicit
; type arguments reports "no callers".
(invocation_expression function: (generic_name (identifier) @name)) @reference.call
(invocation_expression function: (member_access_expression expression: (_)? @recv name: (generic_name (identifier) @name))) @reference.call

; object creation: `new SyncReport(...)`, `new Repo<T>(...)`, `new Ns.Type(...)`.
; A constructor invocation is a call (an LSP counts it among the type's usages);
; without these, a record only ever instantiated reports "no callers" on both
; its constructor and the type. Target-typed `new()` has no name at the site —
; its type is already recorded by the variable-declarator binding below.
(object_creation_expression type: (identifier) @name) @reference.call
(object_creation_expression type: (generic_name (identifier) @name)) @reference.call
(object_creation_expression type: (qualified_name name: (identifier) @name)) @reference.call
(object_creation_expression type: (qualified_name name: (generic_name (identifier) @name))) @reference.call

; base lists: `class C : Base, IFace<T>, Ns.Qualified, Ns.IFace<T>` and the
; record primary-constructor form `record R(...) : Base(args)`. Each base shape
; is a distinct node; a missing shape silently drops the extends edge, so
; impls/hierarchy answers omit real subtypes.
(base_list (identifier) @name) @reference.extends
(base_list (generic_name (identifier) @name)) @reference.extends
(base_list (qualified_name name: (identifier) @name)) @reference.extends
(base_list (qualified_name name: (generic_name (identifier) @name))) @reference.extends
(base_list (primary_constructor_base_type type: (identifier) @name)) @reference.extends
(base_list (primary_constructor_base_type type: (generic_name (identifier) @name))) @reference.extends
(base_list (primary_constructor_base_type type: (qualified_name name: (identifier) @name))) @reference.extends

; attribute (decorator) usages: [ExtendObjectType], [GraphQLName("x")], [System.Obsolete],
; [ExtendObjectType<T>]. The attribute_list nests inside the decorated declaration's
; span, so anchoring the edge on the attribute node attributes it to that symbol.
(attribute name: (identifier) @name) @reference.annotation
(attribute name: (qualified_name name: (identifier) @name)) @reference.annotation
(attribute name: (generic_name (identifier) @name)) @reference.annotation

; type bindings: field / property / parameter / typed-or-var local / new
(field_declaration (variable_declaration type: (_) @bind.type (variable_declarator name: (identifier) @bind.name)))
(property_declaration type: (_) @bind.type name: (identifier) @bind.name)
(parameter type: (_) @bind.type name: (identifier) @bind.name)
(local_declaration_statement (variable_declaration type: (_) @bind.type (variable_declarator name: (identifier) @bind.name)))
(variable_declarator name: (identifier) @bind.name (object_creation_expression type: (_) @bind.type))
