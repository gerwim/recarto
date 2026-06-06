(function_declaration name: (identifier) @name) @definition.function
(method_definition name: (property_identifier) @name) @definition.method
(class_declaration name: (type_identifier) @name) @definition.class
(abstract_class_declaration name: (type_identifier) @name) @definition.class
(interface_declaration name: (type_identifier) @name) @definition.interface
(enum_declaration name: (identifier) @name) @definition.enum
(type_alias_declaration name: (type_identifier) @name) @definition.type
(internal_module name: (identifier) @name) @definition.namespace
(variable_declarator name: (identifier) @name value: (arrow_function)) @definition.function
(variable_declarator name: (identifier) @name value: (function_expression)) @definition.function
(call_expression function: (identifier) @name) @reference.call
(call_expression function: (member_expression property: (property_identifier) @name)) @reference.call
(import_statement source: (string) @name) @reference.import
(class_heritage (extends_clause value: (identifier) @name)) @reference.extends
(class_heritage (extends_clause value: (member_expression property: (property_identifier) @name))) @reference.extends
(class_heritage (implements_clause (type_identifier) @name)) @reference.implements

; decorator usages: @Component({...}), @Input(), @core.Module(...), @Injectable
(decorator (identifier) @name) @reference.annotation
(decorator (call_expression function: (identifier) @name)) @reference.annotation
(decorator (call_expression function: (member_expression property: (property_identifier) @name))) @reference.annotation
(decorator (member_expression property: (property_identifier) @name)) @reference.annotation
