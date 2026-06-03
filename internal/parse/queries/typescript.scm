(function_declaration name: (identifier) @name) @definition.function
(method_definition name: (property_identifier) @name) @definition.method
(class_declaration name: (type_identifier) @name) @definition.class
(interface_declaration name: (type_identifier) @name) @definition.interface
(call_expression function: (identifier) @name) @reference.call
(call_expression function: (member_expression property: (property_identifier) @name)) @reference.call
(import_statement source: (string) @name) @reference.import
(class_heritage (extends_clause value: (identifier) @name)) @reference.extends
(class_heritage (extends_clause value: (member_expression property: (property_identifier) @name))) @reference.extends
(class_heritage (implements_clause (type_identifier) @name)) @reference.implements
