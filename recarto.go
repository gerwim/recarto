// Package recarto holds repository-root files the binary embeds.
package recarto

import _ "embed"

// License is Recarto's licence text.
//
//go:embed LICENSE
var License string
