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
(base_list (identifier) @name) @reference.extends
(base_list (generic_name (identifier) @name)) @reference.extends

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
