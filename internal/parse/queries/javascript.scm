(function_declaration name: (identifier) @name) @definition.function
(generator_function_declaration name: (identifier) @name) @definition.function
(method_definition name: (property_identifier) @name) @definition.method
(class_declaration name: (identifier) @name) @definition.class
(class name: (_) @name) @definition.class
(variable_declarator name: (identifier) @name value: [(arrow_function) (function_expression) (generator_function)]) @definition.function
; function-valued assignments and object-literal methods (legacy/module-pattern
; JS: `obj.handler = function () {}`, `{ render: () => {} }`) — per upstream tags.scm
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
(class_heritage (identifier) @name) @reference.extends
(class_heritage (member_expression property: (property_identifier) @name)) @reference.extends
