(function_declaration name: (identifier) @name) @definition.function
(generator_function_declaration name: (identifier) @name) @definition.function
(function_signature name: (identifier) @name) @definition.function
(method_definition name: (property_identifier) @name) @definition.method
(method_signature name: (property_identifier) @name) @definition.method
(abstract_method_signature name: (property_identifier) @name) @definition.method
(class_declaration name: (type_identifier) @name) @definition.class
(abstract_class_declaration name: (type_identifier) @name) @definition.class
(class name: (_) @name) @definition.class
(interface_declaration name: (type_identifier) @name) @definition.interface
(enum_declaration name: (identifier) @name) @definition.enum
(type_alias_declaration name: (type_identifier) @name) @definition.type
(internal_module name: (identifier) @name) @definition.namespace
(module name: (_) @name) @definition.namespace
(variable_declarator name: (identifier) @name value: [(arrow_function) (function_expression) (generator_function)]) @definition.function
; function-valued assignments and object-literal methods (`obj.handler =
; function () {}`, `{ render: () => {} }`) — per upstream tags.scm
(assignment_expression left: (identifier) @name right: [(arrow_function) (function_expression) (generator_function)]) @definition.function
(assignment_expression left: (member_expression property: (property_identifier) @name) right: [(arrow_function) (function_expression) (generator_function)]) @definition.function
(pair key: (property_identifier) @name value: [(arrow_function) (function_expression) (generator_function)]) @definition.method
(call_expression function: (identifier) @name) @reference.call
(call_expression function: (member_expression property: (property_identifier) @name)) @reference.call
; constructor invocations — `new Foo()`, `new ns.Foo()` — are calls: without
; them a class only ever instantiated reports "no callers"
(new_expression constructor: (identifier) @name) @reference.call
(new_expression constructor: (member_expression property: (property_identifier) @name)) @reference.call
(import_statement source: (string) @name) @reference.import
(class_heritage (extends_clause value: (identifier) @name)) @reference.extends
(class_heritage (extends_clause value: (member_expression property: (property_identifier) @name))) @reference.extends
(class_heritage (implements_clause (type_identifier) @name)) @reference.implements

; decorator usages: @Component({...}), @Input(), @core.Module(...), @Injectable
(decorator (identifier) @name) @reference.annotation
(decorator (call_expression function: (identifier) @name)) @reference.annotation
(decorator (call_expression function: (member_expression property: (property_identifier) @name))) @reference.annotation
(decorator (member_expression property: (property_identifier) @name)) @reference.annotation
