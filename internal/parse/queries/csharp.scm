(class_declaration name: (identifier) @name) @definition.class
(interface_declaration name: (identifier) @name) @definition.interface
(struct_declaration name: (identifier) @name) @definition.struct
(method_declaration name: (identifier) @name) @definition.method
(namespace_declaration name: (_) @name) @definition.namespace
(invocation_expression function: (member_access_expression name: (identifier) @name)) @reference.call
(invocation_expression function: (identifier) @name) @reference.call
