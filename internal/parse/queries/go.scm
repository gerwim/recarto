(function_declaration name: (identifier) @name) @definition.function
(method_declaration receiver: (parameter_list (parameter_declaration type: [(pointer_type (type_identifier) @recv.type) (type_identifier) @recv.type])) name: (field_identifier) @name) @definition.method
(type_spec name: (type_identifier) @name) @definition.type
(call_expression function: (identifier) @name) @reference.call
(call_expression function: (selector_expression operand: (_) @recv field: (field_identifier) @name)) @reference.call
(import_spec path: (interpreted_string_literal) @name) @reference.import

; type bindings: parameter (incl. method receiver) / typed var / := composite literal / := &composite
(parameter_declaration name: (identifier) @bind.name type: [(type_identifier) @bind.type (pointer_type (type_identifier) @bind.type)])
(var_spec name: (identifier) @bind.name type: [(type_identifier) @bind.type (pointer_type (type_identifier) @bind.type)])
(short_var_declaration left: (expression_list (identifier) @bind.name) right: (expression_list (composite_literal type: (type_identifier) @bind.type)))
(short_var_declaration left: (expression_list (identifier) @bind.name) right: (expression_list (unary_expression operand: (composite_literal type: (type_identifier) @bind.type))))
