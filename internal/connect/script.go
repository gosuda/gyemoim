package connect

import (
	_ "embed"
)

// The connect script is distributed by the server itself: the admin downloads
// it from the management API, so it must stay in sync with the claim/complete
// endpoints above. Stdlib-only Python, 3.8+.
//
//go:embed gyemoim-connect.py
var scriptPython []byte

// Script returns the embedded Python connect script.
func Script() []byte {
	return scriptPython
}
