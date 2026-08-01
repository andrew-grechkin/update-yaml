// Style configuration shared by the render helpers in this package.
//
// The package holds a mutable package-level Active var that callers set
// once before invoking the render helpers (there is no reentrancy concern
// for the CLI). Alternative style values can be constructed by tests and
// library users, but the package-level var is what ShouldWrapStringValue,
// ShouldFold, KeepChompString, and friends actually read at call time.
package style

type Style struct {
	Indent                    int // spaces per indent level (block mappings)
	MaxLineWidth              int // fold budget: 0 disables folding, positive means "fold when plain record exceeds this"
	SpacesBeforeInlineComment int // gap between a scalar value and its inline `#` comment
}

// The active style, set once at the top of run() and read by the render
// helpers below. Package-level because run isn't reentrant. Folding of
// long plain scalars is OFF by default (MaxLineWidth = 0); the tool
// honors data verbatim unless the user opts in.
var Active = Style{
	Indent:                    2,
	MaxLineWidth:              0,
	SpacesBeforeInlineComment: 1,
}
